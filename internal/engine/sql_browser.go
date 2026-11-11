package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"signal-atlas/internal/atlas"
)

// Queries own a survey read lease. Handoff/shutdown cancel them before waiting
// for that lease, and Close never frees their observation State underneath them.
type sqlBrowser struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	id     string
}

func (b *sqlBrowser) stop(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil && (id == "" || id == b.id) {
		b.cancel()
	}
}
func (s *Server) sqlQuery(w http.ResponseWriter, r *http.Request) error {
	var request atlas.SQLRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 110_000)).Decode(&request); err != nil {
		return err
	}
	b := &s.sql
	b.mu.Lock()
	if s.closed.Load() || s.handoff.Load() {
		b.mu.Unlock()
		return herr(503, "Survey handoff or shutdown in progress")
	}
	if b.cancel != nil {
		b.mu.Unlock()
		return herr(409, "A SQL query is running; cancel it or wait for it to finish")
	}
	ctx, cancel := context.WithCancel(r.Context())
	b.cancel = cancel
	b.id = request.ID
	b.mu.Unlock()
	defer func() { cancel(); b.mu.Lock(); b.cancel = nil; b.id = ""; b.mu.Unlock() }()
	result, err := s.Atlas.SQLQuery(ctx, request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return herr(400, "SQL query timed out; narrow the query or increase its timeout")
		}
		if errors.Is(err, context.Canceled) {
			return herr(400, "SQL query cancelled")
		}
		return herr(400, "%s", err.Error())
	}
	sendJSON(w, result, 200)
	return nil
}
