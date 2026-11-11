package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"signal-atlas/internal/atlas"
)

// Each runner belongs to one survey session. It never holds a request lease
// while calculating; lifecycle callers cancel/join it before closing that Atlas.
type preparationRunner struct {
	a            *atlas.Atlas
	s            *Server
	jobs         *Jobs
	surveyID, id string
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	wake         chan struct{}
	mu           sync.Mutex
	workCancel   context.CancelFunc
	workDone     chan struct{}
	state        map[string]any
	viewBounds   [4]float64
	viewRevision uint64
	viewClient   string
	viewSerial   uint64
	foreground   atomic.Int64
	mapReaders   atomic.Int64
	closed       atomic.Bool
	// Optional deterministic calculation seam for lifecycle tests.
	prepare func(context.Context, atlas.PreparationNamespace, atlas.PreparationTarget, int64) (atlas.PreparedTarget, error)
}

func newPreparation(s *Server) *preparationRunner {
	ctx, cancel := context.WithCancel(context.Background())
	p := &preparationRunner{a: s.Atlas, s: s, jobs: s.Jobs, surveyID: s.surveyID, id: newHex(), ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), state: map[string]any{"status": "starting"}}
	p.prepare = p.a.ComputePreparationTarget
	s.bindFitEvents(p.jobs, p.surveyID)
	go p.loop()
	return p
}

// A changed camera changes scheduling, not solver scope or completed results.
func (p *preparationRunner) setViewport(b [4]float64) { p.setViewportRequest(b, "", 0) }
func (p *preparationRunner) setViewportRequest(b [4]float64, client string, serial uint64) {
	p.mu.Lock()
	if client != "" && client == p.viewClient && serial < p.viewSerial {
		p.mu.Unlock()
		return
	}
	if client != "" {
		p.viewClient = client
		p.viewSerial = serial
	}
	changed := p.viewRevision == 0 || p.viewBounds != b
	if changed {
		p.viewBounds = b
		p.viewRevision++
	}
	p.mu.Unlock()
	if changed {
		p.signal()
	}
}
func (p *preparationRunner) viewport() ([4]float64, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.viewBounds, p.viewRevision
}
func withoutPreparationKey(q []atlas.PreparationTarget, key string) []atlas.PreparationTarget {
	out := q[:0]
	for _, t := range q {
		if t.Key != key {
			out = append(out, t)
		}
	}
	return out
}

func (p *preparationRunner) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
func (p *preparationRunner) snapshot() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := map[string]any{}
	for k, v := range p.state {
		m[k] = v
	}
	m["job_id"] = p.id
	m["survey_id"] = p.surveyID
	return m
}
func (p *preparationRunner) update(m map[string]any) {
	p.mu.Lock()
	for k, v := range m {
		p.state[k] = v
	}
	p.mu.Unlock()
}
func (p *preparationRunner) interrupt() {
	p.mu.Lock()
	if p.workCancel != nil {
		p.workCancel()
	}
	p.mu.Unlock()
}
func (p *preparationRunner) join(ctx context.Context) error {
	p.interrupt()
	p.mu.Lock()
	done := p.workDone
	p.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *preparationRunner) close() {
	if p.closed.CompareAndSwap(false, true) {
		p.cancel()
		p.interrupt()
	}
	<-p.done
}
func (p *preparationRunner) busy() bool {
	return p.closed.Load() || p.foreground.Load() > 0 || p.jobs.Running() || p.jobs.Retired() || p.s.admin.busy.Load() || p.mapReaders.Load() > 0
}
func (p *preparationRunner) loop() {
	defer close(p.done)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	// Let the first foreground map finish before catalogue/queue work begins.
	select {
	case <-p.ctx.Done():
		return
	case <-time.After(750 * time.Millisecond):
	}
	for {
		if p.ctx.Err() != nil {
			return
		}
		if !p.busy() {
			p.step()
		} else {
			p.update(map[string]any{"status": "waiting", "reason": "Foreground work has priority"})
		}
		select {
		case <-p.ctx.Done():
			return
		case <-tick.C:
		case <-p.wake:
		}
	}
}
func (p *preparationRunner) step() {
	ctx, cancel := context.WithCancel(p.ctx)
	fail := func(err error) {
		if ctx.Err() == nil {
			p.fail(err)
		}
	}
	done := make(chan struct{})
	p.mu.Lock()
	p.workCancel = cancel
	p.workDone = done
	prepare := p.prepare
	p.mu.Unlock()
	defer func() { cancel(); p.mu.Lock(); close(done); p.workCancel = nil; p.mu.Unlock() }()
	if p.busy() {
		return
	}
	c, put, err := p.a.Conn(ctx)
	if err != nil {
		fail(err)
		return
	}
	settings, err := p.a.PreparationSettings(c)
	put()
	if err != nil {
		fail(err)
		return
	}
	if !settings.Enabled || settings.Paused || p.s.o.DisablePreparation {
		status := "paused"
		if !settings.Enabled || p.s.o.DisablePreparation {
			status = "disabled"
		}
		p.update(map[string]any{"status": status, "settings": settings, "reason": "", "error": ""})
		return
	}
	appSettings := p.s.set.get()
	ns, err := atlas.NewPreparationNamespace(settings, appSettings.FastFits, appSettings.GPUFits)
	if err != nil {
		fail(err)
		return
	}
	if err = p.a.EnsurePreparationNamespace(ctx, ns); err != nil {
		fail(err)
		return
	}
	budget := int64(settings.MemoryMB) << 20
	perWorker := budget / int64(settings.Workers)
	progress, err := p.a.PreparationProgress(ctx, ns.ID, perWorker)
	if err != nil {
		fail(err)
		return
	}
	p.update(map[string]any{"status": "running", "settings": settings, "namespace": ns, "progress": progress, "error": "", "reason": ""})
	if progress.Pending == 0 {
		p.update(map[string]any{"status": "complete", "view_priority": false})
		return
	}
	p.runPool(ctx, cancel, ns, settings, perWorker, prepare)
}

// Continuous bounded workers feed a single batch writer. A slow target cannot
// hold up the next wave. Progress/events are published only after a commit.
func (p *preparationRunner) runPool(ctx context.Context, cancel context.CancelFunc, ns atlas.PreparationNamespace, settings atlas.PreparationSettings, budget int64, prepare func(context.Context, atlas.PreparationNamespace, atlas.PreparationTarget, int64) (atlas.PreparedTarget, error)) {
	type outcome struct {
		value   atlas.PreparedTarget
		err     error
		seconds float64
	}
	jobs := make(chan atlas.PreparationTarget)
	results := make(chan outcome, settings.Workers*2)
	var wg sync.WaitGroup
	for range settings.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case t, ok := <-jobs:
					if !ok {
						return
					}
					start := time.Now()
					value, err := prepare(ctx, ns, t, budget)
					select {
					case results <- outcome{value, err, time.Since(start).Seconds()}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	defer func() { cancel(); close(jobs); wg.Wait() }()
	flushTick := time.NewTicker(100 * time.Millisecond)
	defer flushTick.Stop()
	priority := time.NewTicker(20 * time.Millisecond)
	defer priority.Stop()
	claimed := map[string]bool{}
	var queue, viewQueue []atlas.PreparationTarget
	var view *atlas.PreparationView
	var viewRevision uint64
	var nextViewCheck time.Time
	var dispatched, nearDispatched int
	exhausted := false
	var batch []atlas.PreparedTarget
	batchBytes := 0
	var computeSeconds, commitSeconds float64
	var committed, written, transactions int
	fail := func(err error) {
		if ctx.Err() == nil {
			p.fail(err)
		}
		cancel()
	}
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		start := time.Now()
		accepted, err := p.a.CommitPreparationBatch(ctx, ns, batch)
		commitSeconds += time.Since(start).Seconds()
		if err != nil {
			fail(err)
			return false
		}
		transactions++
		for _, item := range batch {
			delete(claimed, item.Target.Key)
		}
		targets := make([]atlas.PreparationTarget, 0, len(accepted))
		count := 0
		for _, item := range accepted {
			if item.BlockedBytes == 0 {
				committed++
				written += item.Result.Written
				count += item.Result.Written
				targets = append(targets, item.Target)
			}
		}
		if len(targets) > 0 {
			p.s.events.publish(JobEvent{Kind: "fits-committed", SurveyID: p.surveyID, JobID: p.id, Namespace: ns.ID, Solver: ns.Solver, FitMode: ns.FitMode, Grouping: ns.Grouping, Targets: targets, Written: count, CommittedAtMS: time.Now().UnixMilli()})
		}
		p.update(map[string]any{"compute_seconds": computeSeconds, "commit_seconds": commitSeconds, "commit_batches": transactions, "targets_committed": committed, "locations_written": written, "viewport_targets_dispatched": nearDispatched, "view_priority": !view.Done() || len(viewQueue) > 0})
		batch = nil
		batchBytes = 0
		exhausted = false
		return true
	}
	for ctx.Err() == nil {
		bounds, revision := p.viewport()
		if revision != viewRevision || view != nil && !view.Current(p.a) {
			viewRevision = revision
			view = nil
			if revision > 0 {
				view = p.a.NewPreparationView(bounds)
			}
			// Unclaimed work remains in the database. Running fits finish;
			// newly available workers can immediately take the new viewport.
			viewQueue = nil
			queue = nil
			exhausted = false
			nextViewCheck = time.Time{}
		}
		if len(viewQueue) == 0 && !view.Done() && !time.Now().Before(nextViewCheck) {
			targets, err := p.a.NextViewportPreparationTargets(ctx, ns.ID, budget, view, 512)
			if err != nil {
				if errors.Is(err, atlas.ErrStaleInput) {
					view = nil
					viewRevision = 0
					continue
				}
				fail(err)
				return
			}
			for _, t := range targets {
				if !claimed[t.Key] {
					viewQueue = append(viewQueue, t)
				}
			}
			nextViewCheck = time.Now().Add(50 * time.Millisecond)
			p.update(map[string]any{"view_priority": !view.Done() || len(viewQueue) > 0})
		}
		for len(queue) > 0 && claimed[queue[0].Key] {
			queue = queue[1:]
		}
		for len(viewQueue) > 0 && claimed[viewQueue[0].Key] {
			viewQueue = viewQueue[1:]
		}
		if len(queue) == 0 && !exhausted {
			// Pending includes work already in flight and awaiting commit. Read
			// past those claims rather than repeatedly fetching the same head.
			targets, err := p.a.NextPreparationTargets(ctx, ns.ID, budget, 256+len(claimed))
			if err != nil {
				fail(err)
				return
			}
			for _, t := range targets {
				if !claimed[t.Key] {
					queue = append(queue, t)
				}
			}
			exhausted = len(queue) == 0
		}
		if len(queue) == 0 && len(viewQueue) == 0 && len(claimed) == 0 {
			progress, err := p.a.PreparationProgress(ctx, ns.ID, budget)
			if err != nil {
				fail(err)
				return
			}
			status := "complete"
			if progress.Pending > 0 {
				status = "limited"
			}
			p.update(map[string]any{"status": status, "progress": progress, "view_priority": false})
			return
		}
		var send chan atlas.PreparationTarget
		var next atlas.PreparationTarget
		near := len(viewQueue) > 0 && (dispatched%4 != 3 || len(queue) == 0)
		if !p.busy() {
			if near {
				send = jobs
				next = viewQueue[0]
			} else if len(queue) > 0 {
				send = jobs
				next = queue[0]
			}
		}
		select {
		case <-ctx.Done():
			return
		case send <- next:
			claimed[next.Key] = true
			dispatched++
			if near {
				nearDispatched++
				viewQueue = viewQueue[1:]
				queue = withoutPreparationKey(queue, next.Key)
			} else {
				queue = queue[1:]
				viewQueue = withoutPreparationKey(viewQueue, next.Key)
			}
		case result := <-results:
			if result.err != nil {
				if !errors.Is(result.err, context.Canceled) && !errors.Is(result.err, atlas.ErrCancelled) && !errors.Is(result.err, atlas.ErrStaleInput) {
					fail(result.err)
				}
				return
			}
			computeSeconds += result.seconds
			batch = append(batch, result.value)
			for _, write := range result.value.Writes {
				batchBytes += len(write.Key) + len(write.Text)
			}
			if (len(batch) >= 512 || batchBytes >= 2<<20) && !flush() {
				return
			}
		case <-flushTick.C:
			if !flush() {
				return
			}
		case <-priority.C:
			// Explicit foreground writes, paginated maps, switch and shutdown cancel
			// and join. Ordinary clustered map reads run alongside preparation
			// without cancelling calculations or resetting a cooldown.
			if p.closed.Load() || p.foreground.Load() > 0 || p.jobs.Running() || p.jobs.Retired() || p.s.admin.busy.Load() {
				cancel()
				return
			}
		}
	}
}

func (p *preparationRunner) fail(err error) {
	if !errors.Is(err, context.Canceled) {
		log.Printf("preparation: %v", err)
		p.update(map[string]any{"status": "failed", "error": err.Error()})
	}
}
func (s *Server) preparationGet(w http.ResponseWriter, r *http.Request, action string) error {
	if action == "events" {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		sendGoJSON(w, s.events.snapshot(after, s.surveyID), 200)
		return nil
	}
	c, put, err := s.Atlas.Conn(r.Context())
	if err != nil {
		return err
	}
	settings, err := s.Atlas.PreparationSettings(c)
	put()
	if err != nil {
		return err
	}
	m := s.preparation.snapshot()
	appSettings := s.set.get()
	ns, err := atlas.NewPreparationNamespace(settings, appSettings.FastFits, appSettings.GPUFits)
	if err != nil {
		return err
	}
	if old, ok := m["namespace"].(atlas.PreparationNamespace); !ok || old.ID != ns.ID {
		m["status"] = "starting"
	}
	progress, err := s.Atlas.PreparationProgress(r.Context(), ns.ID, (int64(settings.MemoryMB)<<20)/int64(settings.Workers))
	if err != nil {
		return err
	}
	m["namespace"] = ns
	m["progress"] = progress
	if settings.Paused {
		m["status"] = "paused"
		m["error"] = ""
	}
	if !settings.Enabled || s.o.DisablePreparation {
		m["status"] = "disabled"
		m["error"] = ""
	}
	m["settings"] = settings
	m["transport"] = map[string]any{"native_event": JobEventName, "poll_ms": 400}
	sendGoJSON(w, m, 200)
	return nil
}
func (s *Server) preparationPost(w http.ResponseWriter, r *http.Request) error {
	body, err := readJSON(r, 65536)
	if err != nil {
		return err
	}
	if err := s.preparation.join(r.Context()); err != nil {
		return err
	}
	c, put, err := s.Atlas.Conn(r.Context())
	if err != nil {
		return err
	}
	defer put()
	settings, err := s.Atlas.PreparationSettings(c)
	if err != nil {
		return err
	}
	// Merge explicit fields, retaining defaults and unrelated survey preferences.
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, &settings); err != nil {
		return err
	}
	s.Atlas.WriteMu.Lock()
	err = s.Atlas.SavePreparationSettings(c, settings)
	s.Atlas.WriteMu.Unlock()
	if err != nil {
		return err
	}
	s.preparation.signal()
	sendGoJSON(w, map[string]any{"settings": settings, "saved": true}, 200)
	return nil
}
