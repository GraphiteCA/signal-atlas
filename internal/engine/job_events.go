package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/py"
	"zombiezen.com/go/sqlite"
)

const JobEventName = "signal-atlas:job"

type JobEvent struct {
	Sequence      uint64                    `json:"sequence"`
	Kind          string                    `json:"kind"`
	SurveyID      string                    `json:"survey_id"`
	JobID         string                    `json:"job_id"`
	Namespace     string                    `json:"namespace"`
	Solver        string                    `json:"solver"`
	FitMode       string                    `json:"fit_mode"`
	Grouping      string                    `json:"grouping"`
	Targets       []atlas.PreparationTarget `json:"targets,omitempty"`
	Written       int                       `json:"written"`
	Reconcile     bool                      `json:"reconcile,omitempty"`
	CommittedAtMS int64                     `json:"committed_at_ms,omitempty"`
}

// A native sink receives bounded coalesced notifications; the durable database
// and sequence-based reconciliation endpoint remain the source of truth.
type JobEventSink interface{ PublishJobEvent(JobEvent) }
type JobEventSinkFunc func(JobEvent)

func (f JobEventSinkFunc) PublishJobEvent(e JobEvent) { f(e) }

type jobEvents struct {
	mu     sync.Mutex
	seq    uint64
	ring   []JobEvent
	sink   JobEventSink
	latest *JobEvent
	stop   chan struct{}
	once   sync.Once
}

func newJobEvents() *jobEvents {
	h := &jobEvents{stop: make(chan struct{})}
	go h.dispatch()
	return h
}
func (h *jobEvents) publish(e JobEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	e.Sequence = h.seq
	if len(e.Targets) > 32 {
		e.Targets = append([]atlas.PreparationTarget(nil), e.Targets[:32]...)
		e.Reconcile = true
	}
	if len(h.ring) == 128 {
		copy(h.ring, h.ring[1:])
		h.ring = h.ring[:127]
	}
	h.ring = append(h.ring, e)
	if h.latest != nil && h.latest.SurveyID == e.SurveyID && h.latest.JobID == e.JobID && h.latest.Namespace == e.Namespace {
		e.Written += h.latest.Written
		if h.latest.CommittedAtMS > 0 && (e.CommittedAtMS == 0 || h.latest.CommittedAtMS < e.CommittedAtMS) {
			e.CommittedAtMS = h.latest.CommittedAtMS
		}
		e.Reconcile = e.Reconcile || h.latest.Reconcile
		if len(h.latest.Targets)+len(e.Targets) <= 32 {
			e.Targets = append(append([]atlas.PreparationTarget{}, h.latest.Targets...), e.Targets...)
		} else {
			e.Reconcile = true
		}
	} else if h.latest != nil {
		e.Reconcile = true // a pending notification from another job/scope was coalesced away
	}
	h.latest = &e
}
func (h *jobEvents) dispatch() {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-tick.C:
			h.mu.Lock()
			sink, e := h.sink, h.latest
			h.latest = nil
			h.mu.Unlock()
			if sink != nil && e != nil {
				sink.PublishJobEvent(*e)
			}
		}
	}
}
func (h *jobEvents) snapshot(after uint64, survey string) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	reconcile := len(h.ring) > 0 && after+1 < h.ring[0].Sequence
	events := []JobEvent{}
	for _, e := range h.ring {
		if e.Sequence > after && e.SurveyID == survey {
			events = append(events, e)
		}
	}
	if len(events) > 32 {
		events = events[len(events)-32:]
		reconcile = true
	}
	return map[string]any{"events": events, "sequence": h.seq, "survey_id": survey, "reconcile": reconcile}
}
func (h *jobEvents) close() { h.once.Do(func() { close(h.stop) }) }
func (s *Server) SetJobEventSink(sink JobEventSink) {
	s.events.mu.Lock()
	s.events.sink = sink
	s.events.mu.Unlock()
}

// Manual/selected fits use the same committed-change adapter without changing
// their legacy job responses or creating automatic-preparation checkpoints.
func (s *Server) bindFitEvents(j *Jobs, survey string) {
	j.onCommitted = func(c *sqlite.Conn, g atlas.Group, p py.Params, scope string) {
		grouping := "shared"
		if p.Is("individual", "1") {
			grouping = "individual"
			if p.Is("shared", "1") {
				grouping = "both"
			}
		}
		if key, _ := g["radio_key"].(string); strings.HasPrefix(key, "radio:") {
			grouping = "individual"
		} else if strings.HasPrefix(key, "shared:") {
			grouping = "shared"
		}
		solver := p.First("solver", "reference")
		sum := sha256.Sum256([]byte(scope + grouping))
		target, err := j.a.FitInputVersion(c, g)
		e := JobEvent{Kind: "fits-committed", SurveyID: survey, JobID: jobEventID(j), Namespace: "manual:" + hex.EncodeToString(sum[:16]), Solver: solver, FitMode: p.First("fit_mode", "all"), Grouping: grouping, Written: 1, CommittedAtMS: time.Now().UnixMilli()}
		if err != nil {
			e.Reconcile = true
		} else {
			e.Targets = []atlas.PreparationTarget{target}
		}
		s.events.publish(e)
	}
}
func jobEventID(j *Jobs) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	id, _ := j.state["id"].(string)
	return id
}
