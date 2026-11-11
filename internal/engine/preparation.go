package engine

import (
	"context"
	"encoding/json"
	"errors"
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
	foreground   atomic.Int64
	mapReaders   atomic.Int64
	mapUntil     atomic.Int64
	closed       atomic.Bool
	// Optional deterministic calculation seam for lifecycle tests.
	prepare func(context.Context, atlas.PreparationNamespace, atlas.PreparationTarget, int64) (atlas.PreparedTargetResult, error)
}

func newPreparation(s *Server) *preparationRunner {
	ctx, cancel := context.WithCancel(context.Background())
	p := &preparationRunner{a: s.Atlas, s: s, jobs: s.Jobs, surveyID: s.surveyID, id: newHex(), ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), state: map[string]any{"status": "starting"}}
	p.prepare = p.a.PrepareTarget
	s.bindFitEvents(p.jobs, p.surveyID)
	go p.loop()
	return p
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
	return p.closed.Load() || p.foreground.Load() > 0 || p.jobs.Running() || p.jobs.Retired() || p.s.admin.busy.Load() || p.mapReaders.Load() > 0 || time.Now().UnixNano() < p.mapUntil.Load()
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
	ns, err := atlas.NewPreparationNamespace(settings, p.s.set.get().FastFits)
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
		p.update(map[string]any{"status": "complete"})
		return
	}
	targets, err := p.a.NextPreparationTargets(ctx, ns.ID, perWorker, settings.Workers)
	if err != nil {
		fail(err)
		return
	}
	if len(targets) == 0 {
		p.update(map[string]any{"status": "limited", "reason": "Remaining networks exceed the background memory budget"})
		return
	}
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t atlas.PreparationTarget) {
			defer wg.Done()
			// Cancellation also observes foreground jobs started through legacy APIs.
			watchDone := make(chan struct{})
			go func() {
				tick := time.NewTicker(20 * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-watchDone:
						return
					case <-ctx.Done():
						return
					case <-tick.C:
						if p.busy() {
							cancel()
							return
						}
					}
				}
			}()
			result, err := prepare(ctx, ns, t, perWorker)
			close(watchDone)
			if err != nil {
				if !errors.Is(err, atlas.ErrStaleInput) && !errors.Is(err, atlas.ErrPreparationBudget) && !errors.Is(err, context.Canceled) && !errors.Is(err, atlas.ErrCancelled) {
					fail(err)
					cancel()
				}
				return
			}
			p.s.events.publish(JobEvent{Kind: "fits-committed", SurveyID: p.surveyID, JobID: p.id, Namespace: ns.ID, Solver: ns.Solver, FitMode: ns.FitMode, Grouping: ns.Grouping, Targets: []atlas.PreparationTarget{t}, Written: result.Written, CommittedAtMS: time.Now().UnixMilli()})
		}(t)
	}
	wg.Wait()
	if ctx.Err() == nil && !p.busy() {
		p.signal()
	}
}
func (p *preparationRunner) fail(err error) {
	if !errors.Is(err, context.Canceled) {
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
	ns, err := atlas.NewPreparationNamespace(settings, s.set.get().FastFits)
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
