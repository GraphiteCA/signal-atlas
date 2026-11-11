package engine

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitSQL(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.sql.mu.Lock()
		active := s.sql.cancel != nil
		s.sql.mu.Unlock()
		if active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("query never started")
}
func TestSQLBrowserRoutesCancelAndSurveyHandoff(t *testing.T) {
	s := adminServer(t)
	id := s.surveyID
	for _, path := range []string{"/sql", "/app/sql.js", "/app/sql.css", "/api/app/sql/schema"} {
		w := adminRequest(t, s, "GET", path, nil, id)
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	query := map[string]any{"sql": "WITH RECURSIVE n(v) AS (VALUES(1) UNION ALL SELECT v+1 FROM n) SELECT sum(v) FROM n", "id": "first", "timeout_seconds": 120}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- adminRequest(t, s, "POST", "/api/app/sql/query", query, id) }()
	waitSQL(t, s)
	w := adminRequest(t, s, "POST", "/api/app/sql/query", map[string]any{"sql": "SELECT 1"}, id)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	// A stale cancellation must not cancel a newer query in another tab.
	adminRequest(t, s, "POST", "/api/app/sql/cancel?id=stale", nil, "")
	select {
	case <-done:
		t.Fatal("stale token cancelled query")
	case <-time.After(20 * time.Millisecond):
	}
	start := time.Now()
	adminRequest(t, s, "POST", "/api/app/sql/cancel?id=first", nil, "")
	select {
	case w = <-done:
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt native SQL")
	}
	if time.Since(start) > time.Second {
		t.Fatal("slow cancellation")
	}
	go func() { done <- adminRequest(t, s, "POST", "/api/app/sql/query", query, id) }()
	waitSQL(t, s)
	next := filepath.Join(t.TempDir(), "new.atlas")
	state := adminAction(t, s, "new", map[string]any{"path": next})
	requireComplete(t, state)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handoff left SQL running")
	}
	if w = adminRequest(t, s, "POST", "/api/app/sql/query", map[string]any{"sql": "SELECT 1"}, id); w.Code != 409 {
		t.Fatal("stale survey query accepted", w.Code)
	}
	if w = adminRequest(t, s, "POST", "/api/app/sql/query", map[string]any{"sql": "SELECT count(*) FROM observations"}, s.surveyID); w.Code != 200 || !strings.Contains(w.Body.String(), `"rows":[["0"]]`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestSQLBrowserShutdownCancelsQuery(t *testing.T) {
	s := adminServer(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		adminRequest(t, s, "POST", "/api/app/sql/query", map[string]any{"sql": "WITH RECURSIVE n(v) AS (VALUES(1) UNION ALL SELECT v+1 FROM n) SELECT sum(v) FROM n", "id": "shutdown"}, s.surveyID)
	}()
	waitSQL(t, s)
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join SQL")
	}
	<-done
}
func TestSQLSnapshotWriterWaitIsCancellable(t *testing.T) {
	s := adminServer(t)
	s.Atlas.WriteMu.Lock()
	defer s.Atlas.WriteMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := s.Atlas.SQLSchema(ctx)
	if err == nil {
		t.Fatal("snapshot ignored cancellation while waiting for writer")
	}
}
