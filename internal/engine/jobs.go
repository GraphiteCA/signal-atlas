package engine

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"runtime"
	"sync"
	"sync/atomic"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/py"
)

// Jobs is Signal Atlas's one-background-job-at-a-time runner (serve_survey.Jobs), polled at
// /api/job and cancellable. An estimate job fits one radio per core at once.
type Jobs struct {
	a         *atlas.Atlas
	mu        sync.Mutex
	state     map[string]any
	cancelled atomic.Bool
	Threads   int
}

func newJobs(a *atlas.Atlas) *Jobs {
	return &Jobs{a: a, state: map[string]any{"id": nil, "status": "idle", "kind": nil}, Threads: max(1, runtime.NumCPU()-2)}
}

// Snapshot is the job's state.
func (j *Jobs) Snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[string]any, len(j.state))
	for k, v := range j.state {
		out[k] = v
	}
	return out
}

func (j *Jobs) update(kv map[string]any) {
	j.mu.Lock()
	for k, v := range kv {
		j.state[k] = v
	}
	j.mu.Unlock()
}

// Running reports a job in progress.
func (j *Jobs) Running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state["status"] == "running"
}

// Cancel asks the running job to stop.
func (j *Jobs) Cancel() { j.cancelled.Store(true) }

func (j *Jobs) isCancelled() bool { return j.cancelled.Load() }

func newHex() string { var b [16]byte; rand.Read(b[:]); return hex.EncodeToString(b[:]) }

func (j *Jobs) start(kind string, work func() (any, error)) (map[string]any, error) {
	j.mu.Lock()
	if j.state["status"] == "running" {
		j.mu.Unlock()
		return nil, py.Errorf("Another background job is running; wait or cancel it first")
	}
	j.cancelled.Store(false)
	id := newHex()
	j.state = map[string]any{"id": id, "status": "running", "kind": kind, "completed": 0, "total": 0}
	j.mu.Unlock()
	go func() {
		r, err := work()
		j.mu.Lock()
		defer j.mu.Unlock()
		if j.state["id"] != id {
			return
		}
		switch {
		case err != nil && j.cancelled.Load():
			j.state["status"], j.state["error"] = "cancelled", err.Error()
		case err != nil:
			j.state["status"], j.state["error"] = "failed", err.Error()
		case j.cancelled.Load():
			j.state["status"], j.state["result"] = "cancelled", r
		default:
			j.state["status"], j.state["result"] = "complete", r
		}
	}()
	return j.Snapshot(), nil
}

// ImportFile imports a CSV (deleting it afterwards when it is an uploaded copy).
func (j *Jobs) ImportFile(path, filename string, skip, deleteAfter bool) (map[string]any, error) {
	return j.start("import", func() (any, error) {
		if deleteAfter {
			defer os.Remove(path)
		}
		return j.a.ImportCSV(path, skip, func(valid, invalid, read, total int64) error {
			if j.isCancelled() {
				return py.Errorf("Import cancelled; changes rolled back")
			}
			j.update(map[string]any{"completed": valid, "invalid": invalid, "bytes_read": read, "bytes_total": total})
			return nil
		}, filename, func(stage string) { j.update(map[string]any{"stage": stage}) })
	})
}

// Estimate validates and picks the groups (errors go back to the request), then fits them.
// OpenDatabase runs a database from the user's computer as an import job: a Signal Atlas database
// replaces the explorer's data, a WiGLE app backup is added to it.
func (j *Jobs) OpenDatabase(path, kind string, skip bool) (map[string]any, error) {
	total := atlas.Count(path, kind)
	return j.start("import", func() (any, error) {
		j.update(map[string]any{"bytes_total": max(total, 1), "bytes_read": 0, "invalid": 0, "database_kind": kind})
		say := func(stage string) { j.update(map[string]any{"stage": stage}) }
		if kind == atlas.KindAtlas {
			return j.a.OpenDatabase(path, func(done int64) error {
				if j.isCancelled() {
					return py.Errorf("Opening the database was cancelled; nothing changed")
				}
				j.update(map[string]any{"completed": done, "bytes_read": done})
				return nil
			}, say)
		}
		return j.a.ImportWiGLE(path, skip, func(valid, invalid int64) error {
			if j.isCancelled() {
				return py.Errorf("Import cancelled; changes rolled back")
			}
			j.update(map[string]any{"completed": valid, "invalid": invalid, "bytes_read": valid + invalid})
			return nil
		}, say)
	})
}

func (j *Jobs) Estimate(p py.Params, groups []atlas.Group, kind string) (map[string]any, error) {
	if j.Running() {
		return nil, py.Errorf("Another background job is running; wait or cancel it first")
	}
	scope, scopeKey, err := atlas.EstimateScope(p)
	if err != nil {
		return nil, err
	}
	if groups == nil {
		sel, err := j.a.SelectedGroups(p)
		if err != nil {
			return nil, err
		}
		groups = atlas.EstimationGroups(sel, p)
	}
	eligible := len(groups)
	if p.Is("missing", "1") {
		c, put, err := j.a.Conn(nil)
		if err != nil {
			return nil, err
		}
		cached := map[string]bool{}
		rows, err := dbAll(c, "SELECT radio_key FROM estimates WHERE scope_key=?", scopeKey)
		put()
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			cached[r["radio_key"].(string)] = true
		}
		var keep []atlas.Group
		for _, g := range groups {
			if !cached[g["radio_key"].(string)] {
				keep = append(keep, g)
			}
		}
		groups = keep
	}
	return j.start(kind, func() (any, error) {
		j.update(map[string]any{"total": len(groups), "observation_scope": scope})
		var fitted, skipped, done atomic.Int64
		var next atomic.Int64
		var firstErr error
		var errMu sync.Mutex
		var stop atomic.Bool // a failed fit stops the others without calling it a cancel
		var wg sync.WaitGroup
		for w := 0; w < max(1, min(j.Threads, len(groups))); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, put, err := j.a.Conn(nil)
				if err != nil {
					errMu.Lock()
					firstErr = err
					errMu.Unlock()
					return
				}
				defer put()
				for !j.isCancelled() && !stop.Load() {
					i := int(next.Add(1)) - 1
					if i >= len(groups) {
						return
					}
					status, err := j.a.FitAndStore(c, groups[i], p, scopeKey, j.isCancelled)
					if err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						errMu.Unlock()
						stop.Store(true)
						return
					}
					if status == "fitted" {
						fitted.Add(1)
					} else {
						skipped.Add(1)
					}
					j.update(map[string]any{"completed": done.Add(1), "fitted": fitted.Load(), "skipped": skipped.Load()})
				}
			}()
		}
		wg.Wait()
		if firstErr != nil && firstErr != atlas.ErrCancelled {
			return nil, firstErr
		}
		return map[string]any{"fitted": fitted.Load(), "skipped": skipped.Load(), "eligible_radios": eligible, "attempted_fits": len(groups)}, nil
	})
}
