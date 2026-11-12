package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/py"
)

// ---- Wardrive Compare results saved in the open survey ----

const compareSavedPath = "/api/app/compare/saved"

// compareSavedLease holds the open survey for one request, as a survey request's own lease does.
func (s *Server) compareSavedLease(w http.ResponseWriter, r *http.Request) (func(), error) {
	if s.handoff.Load() {
		return nil, herr(503, "Survey handoff in progress; retry shortly")
	}
	s.surveyMu.RLock()
	w.Header().Set("X-Survey-ID", s.surveyID)
	if expected := r.Header.Get("X-Survey-ID"); expected != "" && expected != s.surveyID {
		s.surveyMu.RUnlock()
		return nil, herr(409, "The active survey changed; reload this page")
	}
	return s.surveyMu.RUnlock, nil
}

func (s *Server) compareSavedList(w http.ResponseWriter, r *http.Request) error {
	c, put, err := s.Atlas.Conn(r.Context())
	if err != nil {
		return err
	}
	defer put()
	runs, err := atlas.CompareRuns(c)
	if err != nil {
		return err
	}
	sendJSON(w, map[string]any{"runs": runs, "survey_id": s.surveyID}, 200)
	return nil
}

// compareSavedGet lists the saved runs, or answers one with its state and result.
func (s *Server) compareSavedGet(w http.ResponseWriter, r *http.Request, p string) error {
	release, err := s.compareSavedLease(w, r)
	if err != nil {
		return err
	}
	defer release()
	if p == compareSavedPath {
		return s.compareSavedList(w, r)
	}
	c, put, err := s.Atlas.Conn(r.Context())
	if err != nil {
		return err
	}
	defer put()
	run, err := atlas.CompareRun(c, strings.TrimPrefix(p, compareSavedPath+"/"))
	if err != nil {
		return err
	}
	if run == nil {
		return herr(404, "That saved comparison is no longer in this survey")
	}
	// the stored JSON goes out as it is: a result is megabytes, and its numbers stay exact
	sendGoJSON(w, map[string]any{"id": run["id"], "name": run["name"], "created_utc": run["created_utc"], "drives": run["drives"],
		"sources": json.RawMessage(db.Str(run["sources_json"])), "state": json.RawMessage(db.Str(run["state_json"])),
		"result": json.RawMessage(db.Str(run["result_json"]))}, 200)
	return nil
}

// compareSavedPost saves the page's current result, or deletes a saved one; both answer the list.
func (s *Server) compareSavedPost(w http.ResponseWriter, r *http.Request, p string) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, atlas.MaxCompareRunBytes+1<<20))
	if err != nil {
		return err
	}
	var body struct {
		ID      string          `json:"id"`
		Name    string          `json:"name"`
		Drives  int64           `json:"drives"`
		Sources json.RawMessage `json:"sources"`
		State   json.RawMessage `json:"state"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return py.Errorf("That comparison could not be read; run it again and save")
	}
	release, err := s.compareSavedLease(w, r)
	if err != nil {
		return err
	}
	defer release()
	if s.admin.busy.Load() {
		return herr(409, "Survey administration is running; wait or cancel it first")
	}
	c, put, err := s.Atlas.Conn(r.Context())
	if err != nil {
		return err
	}
	if strings.HasSuffix(p, "/delete") {
		err = s.Atlas.DeleteCompareRun(c, body.ID)
	} else {
		_, err = s.Atlas.SaveCompareRun(c, atlas.CompareRunInput{Name: body.Name, Drives: body.Drives, Sources: body.Sources, State: body.State, Result: body.Result})
	}
	put()
	if err != nil {
		return err
	}
	return s.compareSavedList(w, r)
}
