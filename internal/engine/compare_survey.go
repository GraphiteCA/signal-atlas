package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"signal-atlas/internal/compare"
	"signal-atlas/internal/db"
	"signal-atlas/internal/py"
)

// ---- Wardrive Compare, fed from the open survey: its days or its imports, one drive each ----

// maxSurveyDrives bounds one request; Compare itself stops comparing legs above 80.
const maxSurveyDrives = 200

// ended reports whether the page behind the session has gone.
func (s *compareSession) ended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// enqueue hands the engine a message unless the session has ended. It never waits holding the
// session's lock, which the engine takes to answer the page.
func (s *compareSession) enqueue(m *compare.Message) bool {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return false
		}
		select {
		case s.work <- m:
			s.mu.Unlock()
			return true
		default:
		}
		s.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
}

// compareSurveyList answers what the open survey can give Compare.
func (s *Server) compareSurveyList(w http.ResponseWriter, r *http.Request) error {
	if s.handoff.Load() {
		return herr(503, "Survey handoff in progress; retry shortly")
	}
	s.surveyMu.RLock()
	defer s.surveyMu.RUnlock()
	list, err := s.Atlas.SurveyDrives(r.Context())
	if err != nil {
		return err
	}
	list["survey"] = filepath.Base(s.Atlas.Root)
	list["survey_id"] = s.surveyID
	w.Header().Set("X-Survey-ID", s.surveyID)
	sendJSON(w, list, 200)
	return nil
}

// compareSurveyAdd writes the chosen days or imports of the open survey as WiGLE CSV logs in the
// session's folder and adds them to its engine, each as a drive of its own. The survey is only read.
func (s *Server) compareSurveyAdd(w http.ResponseWriter, r *http.Request, cs *compareSession) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	var body struct {
		By   string          `json:"by"`
		Keys []string        `json:"keys"`
		Cfg  *compare.Config `json:"cfg"`
		// Duplicates writes back the exact duplicates the survey set aside (the default).
		Duplicates *bool `json:"duplicates"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	if len(body.Keys) == 0 {
		return py.Errorf("Tick at least one day or import")
	}
	if len(body.Keys) > maxSurveyDrives {
		return py.Errorf("Add at most %d survey drives at a time", maxSurveyDrives)
	}
	if s.handoff.Load() {
		return herr(503, "Survey handoff in progress; retry shortly")
	}
	s.surveyMu.RLock()
	defer s.surveyMu.RUnlock()
	w.Header().Set("X-Survey-ID", s.surveyID)
	if expected := r.Header.Get("X-Survey-ID"); expected != "" && expected != s.surveyID {
		return herr(409, "The active survey changed; reload this page")
	}
	a := s.Atlas
	list, err := a.SurveyDrives(r.Context())
	if err != nil {
		return err
	}
	which := "days"
	if body.By == "import" {
		which = "imports"
	}
	names := map[string]string{}
	rows, _ := list[which].([]db.Row)
	for _, d := range rows {
		names[db.Str(d["key"])] = db.Str(d["name"])
	}
	dir := filepath.Join(cs.e.Tmp, "survey-"+newHex()[:8])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	duplicates := body.Duplicates == nil || *body.Duplicates
	files, err := a.ExportSurveyDrives(ctx, dir, s.o.Version, body.By, body.Keys, duplicates, func(done, total int64) {
		if cs.ended() {
			cancel()
			return
		}
		cs.push(map[string]any{"type": "progress", "text": "Reading the survey", "frac": float64(done) / float64(max(total, 1))})
	})
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	var add []compare.FileIn
	for _, f := range files {
		name := "Survey " + f.Key
		if body.By == "import" {
			// the import's id keeps two imports of one filename apart
			stem := strings.TrimSuffix(names[f.Key], filepath.Ext(names[f.Key]))
			name = "Survey " + stem + " (import " + f.Key + ")"
		}
		add = append(add, compare.FileIn{Name: name + ".csv", Size: f.Size, Path: f.Path})
	}
	if len(add) == 0 {
		os.RemoveAll(dir)
		return py.Errorf("The survey has no dated sightings for that choice")
	}
	if !cs.enqueue(&compare.Message{Type: "add", Files: add, Cfg: body.Cfg, Solo: true}) {
		os.RemoveAll(dir)
		return herr(404, "that compare session has ended; reload the page")
	}
	sendJSON(w, map[string]any{"ok": true, "drives": len(add)}, 200)
	return nil
}
