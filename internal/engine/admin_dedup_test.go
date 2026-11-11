package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"strings"
	"testing"
)

func TestAdminSurveyDedupAndCSV(t *testing.T) {
	s := adminServer(t)
	dir := t.TempDir()
	header := "MAC,SSID,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n"
	row := "02:00:00:00:00:01,Test,2026-01-02 12:00:00,1,2412,-60,49,-123,3,WIFI\n"
	source := filepath.Join(dir, "input.csv")
	if err := os.WriteFile(source, []byte(header+row+row), 0644); err != nil {
		t.Fatal(err)
	}
	requireComplete(t, adminAction(t, s, "import", map[string]any{"paths": []string{source}}))
	legacySurveyDuplicate(t, s)
	var preview map[string]any
	w := adminRequest(t, s, "GET", "/api/app/admin/duplicates", nil, "")
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview["rows"] != float64(1) {
		t.Fatal(preview)
	}
	if state := adminAction(t, s, "deduplicate", map[string]any{"token": "stale"}); state["status"] != "failed" {
		t.Fatal(state)
	}
	if s.Atlas.Obs.State().N != 2 {
		t.Fatal("stale preview changed rows")
	}
	requireComplete(t, adminAction(t, s, "deduplicate", map[string]any{"token": preview["token"]}))
	if s.Atlas.Obs.State().N != 1 {
		t.Fatal("duplicate survived")
	}
	dest := filepath.Join(dir, "survey.csv")
	requireComplete(t, adminAction(t, s, "export-csv", map[string]any{"path": dest, "format": "csv"}))
	b, err := os.ReadFile(dest)
	if err != nil || strings.Count(string(b), "02:00:00:00:00:01") != 1 {
		t.Fatalf("csv: %s %v", b, err)
	}
	days := filepath.Join(dir, "days")
	requireComplete(t, adminAction(t, s, "export-csv", map[string]any{"path": days, "format": "days"}))
	if _, err = os.Stat(filepath.Join(days, "wigle_2026-01-02.csv")); err != nil {
		t.Fatal(err)
	}
	for _, body := range []map[string]any{{"path": days, "format": "days"}, {"path": filepath.Join(s.Atlas.Root, "bad.csv"), "format": "csv"}} {
		if state := adminAction(t, s, "export-csv", body); state["status"] != "failed" {
			t.Fatal(state)
		}
	}
	requireComplete(t, adminAction(t, s, "restore", nil))
	if s.Atlas.Obs.State().N != 2 {
		t.Fatal("recovery did not restore duplicates")
	}
	// A stale preview from before the revision change cannot remove rows.
	if state := adminAction(t, s, "deduplicate", map[string]any{"token": preview["token"]}); state["status"] != "failed" {
		t.Fatal(state)
	}
}

func TestAdminSurveyExportAndDedupCancellation(t *testing.T) {
	s := adminServer(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "input.csv")
	row := "02:00:00:00:00:01,Test,2026-01-02 12:00:00,1,2412,-60,49,-123,3,WIFI\n"
	os.WriteFile(source, []byte("MAC,SSID,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n"+row+row), 0644)
	requireComplete(t, adminAction(t, s, "import", map[string]any{"paths": []string{source}}))
	legacySurveyDuplicate(t, s)
	dest := filepath.Join(dir, "keep.csv")
	os.WriteFile(dest, []byte("existing export"), 0644)
	s.admin.observe = func(stage string) {
		if stage == "Exporting survey observations" {
			s.admin.requestCancel(s.admin.snapshot()["id"].(string))
		}
	}
	state := adminAction(t, s, "export-csv", map[string]any{"path": dest, "format": "csv"})
	if state["status"] != "cancelled" {
		t.Fatal(state)
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "existing export" {
		t.Fatal("cancel replaced existing export")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".survey-csv-*"))
	if len(matches) != 0 {
		t.Fatal("staging leaked", matches)
	}
	s.admin.observe = nil
	var preview map[string]any
	json.Unmarshal(adminRequest(t, s, "GET", "/api/app/admin/duplicates", nil, "").Body.Bytes(), &preview)
	s.admin.observe = func(stage string) {
		if stage == "Removing duplicate observations in candidate" {
			s.admin.requestCancel(s.admin.snapshot()["id"].(string))
		}
	}
	before := s.surveyID
	state = adminAction(t, s, "deduplicate", map[string]any{"token": preview["token"]})
	if state["status"] != "cancelled" {
		t.Fatal(state)
	}
	if s.surveyID != before || s.Atlas.Obs.State().N != 2 {
		t.Fatal("cancel changed active survey")
	}
	s.admin.observe = nil
}

// Simulate data stored before imports automatically skipped duplicate sightings.
func legacySurveyDuplicate(t *testing.T, s *Server) {
	t.Helper()
	st := s.Atlas.Obs.State()
	copy := st.Segs[0].Range(0, 1)
	copy.Base = st.MaxID + 1
	if err := s.Atlas.Obs.Commit(obs.Change{Add: []*obs.Segment{copy}}); err != nil {
		t.Fatal(err)
	}
	c, put, err := s.Atlas.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer put()
	if _, err = db.Exec(c, "UPDATE imports SET active_rows=active_rows+1,valid_rows=valid_rows+1,duplicate_rows=0 WHERE id=?", copy.ImportID); err != nil {
		t.Fatal(err)
	}
}

func TestAdminAutomaticImportDeduplication(t *testing.T) {
	s := adminServer(t)
	dir := t.TempDir()
	header := "MAC,SSID,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n"
	row := "02:00:00:00:00:01,Test,2026-01-02 12:00:00,1,2412,-60,49,-123,3,WIFI\n"
	paths := []string{filepath.Join(dir, "one.csv"), filepath.Join(dir, "two.csv")}
	os.WriteFile(paths[0], []byte(header+row+row), 0644)
	os.WriteFile(paths[1], []byte(header+row+row+row), 0644)
	requireComplete(t, adminAction(t, s, "import", map[string]any{"paths": paths}))
	if s.Atlas.Obs.State().N != 1 {
		t.Fatal("batch stored duplicates")
	}
	var history struct {
		Rows []map[string]any `json:"rows"`
	}
	response := adminRequest(t, s, "GET", "/api/app/admin/imports", nil, "")
	if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Rows) != 2 || history.Rows[0]["duplicate_rows"] != float64(3) || history.Rows[1]["duplicate_rows"] != float64(1) || history.Rows[0]["active_rows"] != float64(0) {
		t.Fatalf("history: %v", history)
	}
}
