package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/drivedb"
	"signal-atlas/internal/obs"
)

func adminServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Options{DisablePreparation: true, Data: filepath.Join(t.TempDir(), "data"), Version: "dev", UI: os.DirFS("../../ui")})
	if err != nil {
		t.Fatal(err)
	}
	s.AllowHost("admin.test")
	t.Cleanup(s.Close)
	return s
}
func adminRequest(t *testing.T, s *Server, method, path string, body any, id string) *httptest.ResponseRecorder {
	t.Helper()
	raw := ""
	if body != nil {
		b, _ := json.Marshal(body)
		raw = string(b)
	}
	r := httptest.NewRequest(method, "http://admin.test"+path, strings.NewReader(raw))
	if id != "" {
		r.Header.Set("X-Survey-ID", id)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func adminAction(t *testing.T, s *Server, action string, body any) map[string]any {
	t.Helper()
	w := adminRequest(t, s, "POST", "/api/app/admin/"+action, body, "")
	if w.Code != 202 {
		t.Fatalf("%s: %d %s", action, w.Code, w.Body.String())
	}
	s.admin.mu.Lock()
	done := s.admin.done
	s.admin.mu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s timed out", action)
	}
	return s.admin.snapshot()
}
func requireComplete(t *testing.T, state map[string]any) {
	t.Helper()
	if state["status"] != "complete" {
		t.Fatal(state)
	}
}

type cancelledAdminHost struct{}

func (cancelledAdminHost) PickFiles(string, []string, bool) ([]string, error) { return nil, nil }
func (cancelledAdminHost) PickFolder(string) (string, error)                  { return "", nil }
func (cancelledAdminHost) SavePath(string, string, string) (string, bool, error) {
	return "", false, nil
}
func (cancelledAdminHost) ShowFolder(string) {}

func TestAdminDialogsAndOwnedUploads(t *testing.T) {
	s := adminServer(t)
	s.o.Host = cancelledAdminHost{}
	before := s.surveyID
	for _, action := range []string{"new", "open", "convert", "snapshot", "import"} {
		w := adminRequest(t, s, "POST", "/api/app/admin/"+action, map[string]any{}, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"cancelled":true`) {
			t.Fatalf("%s: %d %s", action, w.Code, w.Body.String())
		}
	}
	if s.surveyID != before {
		t.Fatal("cancelled dialogs changed survey")
	}
	s.o.Host = nil
	source := "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n02:00:00:00:00:01,Zero,2026-11-01,2412,-60,0,-0,3,WIFI\n"
	r := httptest.NewRequest("POST", "http://admin.test/api/app/admin/upload", strings.NewReader(source))
	r.Header.Set("X-Filename", "zeros.csv")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var upload map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &upload) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	path := upload["path"].(string)
	native := filepath.Join(t.TempDir(), "original.csv")
	if err := os.WriteFile(native, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	requireComplete(t, adminAction(t, s, "import", map[string]any{"paths": []string{path, native}}))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("owned upload retained", err)
	}
	if _, err := os.Stat(native); err != nil {
		t.Fatal("original source changed", err)
	}
	state := adminAction(t, s, "import", map[string]any{"paths": []string{s.Atlas.MetaPath}})
	if !strings.Contains(fmt.Sprint(state), "workspace metadata only") {
		t.Fatal(state)
	}
}

func TestAdminRejectsStaleCleanupPreview(t *testing.T) {
	s := adminServer(t)
	dirtyAdminRows(t, s)
	w := adminRequest(t, s, "GET", "/api/app/admin/preview", nil, "")
	var preview map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "later.csv")
	text := "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n02:00:00:00:00:FF,Later,2026-11-02,2412,-60,48.5,-123.4,3,WIFI\n"
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	requireComplete(t, adminAction(t, s, "import", map[string]any{"paths": []string{file}}))
	id := s.surveyID
	state := adminAction(t, s, "cleanup", map[string]any{"token": preview["token"]})
	if state["status"] != "failed" || !strings.Contains(fmt.Sprint(state), "preview is stale") || s.surveyID != id || s.Atlas.Obs.State().N != 13 {
		t.Fatal(state)
	}
}
func addAdminRows(t *testing.T, s *Server) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "scan.csv")
	var b strings.Builder
	b.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n")
	for n := 0; n < 3; n++ {
		for i := 0; i < 4; i++ {
			fmt.Fprintf(&b, "02:50:00:00:00:%02X,Net%d,2026-11-01 00:00:0%d,2412,-60,48.4,-123.4,3,WIFI\n", n, n, i)
		}
	}
	os.WriteFile(file, []byte(b.String()), 0644)
	if _, err := s.Atlas.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
}
func dirtyAdminRows(t *testing.T, s *Server) {
	t.Helper()
	addAdminRows(t, s)
	replace := map[*obs.Segment]*obs.Segment{}
	for _, sg := range s.Atlas.Obs.State().Segs {
		c := sg.Range(0, sg.N)
		c.Lat = append([]float64(nil), sg.Lat...)
		c.Lon = append([]float64(nil), sg.Lon...)
		c.Lat[0], c.Lon[0] = 0, 0
		c.Lat[5], c.Lon[5] = 0, 0
		replace[sg] = c
	}
	if err := s.Atlas.Obs.Commit(obs.Change{Replace: replace}); err != nil {
		t.Fatal(err)
	}
}

func TestAdminNewOpenIsolationAndIndependentSessions(t *testing.T) {
	s := adminServer(t)
	addAdminRows(t, s)
	oldRoot, oldID, oldJobs := s.Atlas.Root, s.surveyID, s.Jobs
	if err := s.set.set(func(st *Settings) { st.FastFits = true; st.UpdateChecks = "off" }); err != nil {
		t.Fatal(err)
	}
	cs := s.cmp.create()
	export := filepath.Join(s.o.Data, "exports", "keep.csv")
	os.MkdirAll(filepath.Dir(export), 0755)
	os.WriteFile(export, []byte("keep me"), 0644)
	archive, err := drivedb.Create(filepath.Join(t.TempDir(), "archive.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	s.drv.set(archive)
	release := make(chan struct{})
	if err := s.drv.start("independent", func() (any, error) { <-release; return "done", nil }); err != nil {
		t.Fatal(err)
	}
	defer func() { close(release); s.drv.mu.Lock(); done := s.drv.done; s.drv.mu.Unlock(); <-done }()
	started := make(chan struct{})
	if _, err := oldJobs.start("old-worker", func() (any, error) {
		close(started)
		for !oldJobs.isCancelled() {
			time.Sleep(time.Millisecond)
		}
		return "joined", nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	invalid := filepath.Join(t.TempDir(), "invalid.atlas")
	os.Mkdir(invalid, 0755)
	os.WriteFile(filepath.Join(invalid, "atlas.sqlite3"), []byte("not SQLite"), 0644)
	failed := adminAction(t, s, "open", map[string]any{"path": invalid})
	if failed["status"] != "failed" || s.Atlas.Root != oldRoot || !oldJobs.Running() || oldJobs.isCancelled() {
		t.Fatal("invalid candidate disturbed current worker/survey", failed)
	}
	newRoot := filepath.Join(t.TempDir(), "new.atlas")
	requireComplete(t, adminAction(t, s, "new", map[string]any{"path": newRoot}))
	if s.Atlas.Obs.State().N != 0 || s.surveyID == oldID || s.Jobs == oldJobs {
		t.Fatal("new survey was not isolated")
	}
	if oldJobs.Running() {
		t.Fatal("old worker was not joined")
	}
	if s.cmp.get(cs.id) != cs || s.drv.d != archive || !s.drv.running.Load() {
		t.Fatal("survey action replaced unrelated sessions")
	}
	if _, err := os.Stat(export); err != nil {
		t.Fatal("survey switch deleted exports")
	}
	settings := s.set.get()
	if !settings.FastFits || settings.UpdateChecks != "off" || settings.LastSurvey != newRoot {
		t.Fatal("global preferences were lost", settings)
	}
	w := adminRequest(t, s, "POST", "/api/estimate", nil, oldID)
	if w.Code != 409 {
		t.Fatal("stale client wrote to the new survey")
	}
	failed = adminAction(t, s, "new", map[string]any{"path": newRoot})
	if failed["status"] != "failed" || s.Atlas.Root != newRoot {
		t.Fatal("New overwrote an existing workspace")
	}
	requireComplete(t, adminAction(t, s, "open", map[string]any{"path": oldRoot}))
	if s.Atlas.Obs.State().N != 12 {
		t.Fatal("old survey data lost")
	}
}

func TestAdminCleanupSnapshotRecoveryAndRestart(t *testing.T) {
	s := adminServer(t)
	dirtyAdminRows(t, s)
	root := s.Atlas.Root
	w := adminRequest(t, s, "GET", "/api/app/admin/preview", nil, "")
	var preview map[string]any
	json.Unmarshal(w.Body.Bytes(), &preview)
	if preview["rows"] != float64(2) {
		t.Fatal(preview)
	}
	oldID := s.surveyID
	requireComplete(t, adminAction(t, s, "cleanup", map[string]any{"token": preview["token"]}))
	if s.Atlas.Obs.State().N != 10 || s.surveyID == oldID {
		t.Fatal("cleanup did not publish a distinct complete revision")
	}
	pointer, _ := atlas.ReadWorkspacePointer(root)
	if pointer.Previous != "." {
		t.Fatal("recovery generation missing")
	}
	json.Unmarshal(adminRequest(t, s, "GET", "/api/app/admin/preview", nil, "").Body.Bytes(), &preview)
	before := s.surveyID
	state := adminAction(t, s, "cleanup", map[string]any{"token": preview["token"]})
	requireComplete(t, state)
	if s.surveyID != before {
		t.Fatal("zero-change cleanup replaced the survey")
	}
	backup := filepath.Join(t.TempDir(), "backup.atlas")
	requireComplete(t, adminAction(t, s, "snapshot", map[string]any{"path": backup, "format": "workspace"}))
	copy, err := atlas.Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	if copy.Obs.State().N != 10 {
		t.Fatal("workspace snapshot missing scans")
	}
	copy.Close()
	sqlitePath := filepath.Join(t.TempDir(), "snapshot.sqlite3")
	requireComplete(t, adminAction(t, s, "snapshot", map[string]any{"path": sqlitePath, "format": "sqlite"}))
	c, err := db.OpenReadOnly(sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := db.Scalar(c, "SELECT count(*) FROM observations")
	c.Close()
	if db.Int(n) != 10 {
		t.Fatal("portable snapshot was metadata only")
	}
	requireComplete(t, adminAction(t, s, "restore", map[string]any{}))
	if s.Atlas.Obs.State().N != 12 {
		t.Fatal("recovery did not restore previous complete revision")
	}
	data := s.o.Data
	s.Close()
	restarted, err := New(Options{DisablePreparation: true, Data: data, Version: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.Atlas.Root != root || restarted.Atlas.Obs.State().N != 12 {
		t.Fatal("last survey/generation was not restored")
	}
}

func TestAdminCancelBeforeHandoffAndDuringStaging(t *testing.T) {
	s := adminServer(t)
	dirtyAdminRows(t, s)
	oldID, oldRoot := s.surveyID, s.Atlas.Root
	var preview map[string]any
	json.Unmarshal(adminRequest(t, s, "GET", "/api/app/admin/preview", nil, "").Body.Bytes(), &preview)
	// A long request lease must be cancellable without waiting for its response.
	s.surveyMu.RLock()
	w := adminRequest(t, s, "POST", "/api/app/admin/cleanup", map[string]any{"token": preview["token"]}, "")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	for i := 0; i < 100 && !s.handoff.Load(); i++ {
		time.Sleep(time.Millisecond)
	}
	if !s.handoff.Load() {
		s.surveyMu.RUnlock()
		t.Fatal("handoff did not begin")
	}
	id := s.admin.snapshot()["id"].(string)
	adminRequest(t, s, "POST", "/api/app/admin/cancel", map[string]any{"id": id}, "")
	s.admin.mu.Lock()
	done := s.admin.done
	s.admin.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		s.surveyMu.RUnlock()
		t.Fatal("lease drain cancellation hung")
	}
	s.surveyMu.RUnlock()
	if s.admin.snapshot()["status"] != "cancelled" || s.surveyID != oldID {
		t.Fatal("cancelled handoff changed survey")
	}
	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.admin.observe = func(stage string) {
		if stage == "Copying observation segments" {
			once.Do(func() { close(paused); <-release })
		}
	}
	w = adminRequest(t, s, "POST", "/api/app/admin/cleanup", map[string]any{"token": preview["token"]}, "")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("staging hook not reached")
	}
	id = s.admin.snapshot()["id"].(string)
	adminRequest(t, s, "POST", "/api/app/admin/cancel", map[string]any{"id": id}, "")
	close(release)
	s.admin.mu.Lock()
	done = s.admin.done
	s.admin.mu.Unlock()
	<-done
	if s.admin.snapshot()["status"] != "cancelled" || s.surveyID != oldID || s.Atlas.Obs.State().N != 12 {
		t.Fatal("staging cancellation changed the current revision")
	}
	p, _ := atlas.ReadWorkspacePointer(oldRoot)
	if p.Active != "." {
		t.Fatal("cancelled stage was published")
	}
	entries, _ := os.ReadDir(filepath.Join(oldRoot, "generations"))
	if len(entries) != 0 {
		t.Fatal("owned failed candidate was not removed")
	}
}

func TestAdminConflictsLocksAndFailures(t *testing.T) {
	s := adminServer(t)
	addAdminRows(t, s)
	oldRoot, oldID := s.Atlas.Root, s.surveyID
	if second, err := New(Options{DisablePreparation: true, Data: filepath.Join(t.TempDir(), "other"), Version: "dev"}); err == nil {
		second.Close()
	} // separate app data is allowed
	if _, err := acquireSurveyLock(oldRoot); err == nil {
		t.Fatal("workspace opened twice")
	}
	started := make(chan struct{})
	_, err := s.Jobs.start("busy", func() (any, error) {
		close(started)
		for !s.Jobs.isCancelled() {
			time.Sleep(time.Millisecond)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	state := adminAction(t, s, "snapshot", map[string]any{"format": "sqlite", "path": filepath.Join(t.TempDir(), "no.sqlite3")})
	if state["status"] != "failed" || s.surveyID != oldID {
		t.Fatal("snapshot accepted concurrent writer", state)
	}
	s.Jobs.Cancel()
	s.Jobs.Wait(context.Background())
	prior := filepath.Join(t.TempDir(), "prior.sqlite3")
	os.WriteFile(prior, []byte("prior backup"), 0644)
	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.admin.observe = func(stage string) {
		if stage == "Writing observations" {
			once.Do(func() { close(paused); <-release })
		}
	}
	w := adminRequest(t, s, "POST", "/api/app/admin/snapshot", map[string]any{"format": "sqlite", "path": prior}, "")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	<-paused
	id := s.admin.snapshot()["id"].(string)
	adminRequest(t, s, "POST", "/api/app/admin/cancel", map[string]any{"id": id}, "")
	close(release)
	s.admin.mu.Lock()
	done := s.admin.done
	s.admin.mu.Unlock()
	<-done
	b, _ := os.ReadFile(prior)
	if string(b) != "prior backup" || s.Atlas.Root != oldRoot {
		t.Fatal("failed export overwrote prior snapshot or survey")
	}
	s.admin.observe = nil
	state = adminAction(t, s, "open", map[string]any{"path": filepath.Join(t.TempDir(), "missing")})
	if state["status"] != "failed" || s.surveyID != oldID {
		t.Fatal("failed open changed survey")
	}
	for _, task := range []string{"validate", "rebuild_cache", "checkpoint", "analyze"} {
		requireComplete(t, adminAction(t, s, "maintenance", map[string]any{"task": task}))
	}
}

func TestAdminRecentWorkspaces(t *testing.T) {
	s := adminServer(t)
	root := t.TempDir()
	existing := filepath.Join(root, "kept.atlas")
	missing := filepath.Join(root, "deleted.atlas")
	file := filepath.Join(root, "not-a-folder")
	if err := os.Mkdir(existing, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.set.set(func(st *Settings) {
		st.RecentSurveys = []string{missing, existing, file, s.Atlas.Root}
		st.LastSurvey = existing
	}); err != nil {
		t.Fatal(err)
	}
	before := s.surveyID
	w := adminRequest(t, s, "GET", "/api/app/admin/status", nil, "")
	var status struct {
		Recent []string `json:"recent"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(status.Recent) != 2 || status.Recent[0] != existing {
		t.Fatal(status.Recent)
	}
	w = adminRequest(t, s, "POST", "/api/app/admin/forget", map[string]any{"path": existing}, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	saved := newSettings(filepath.Dir(s.set.path)).get()
	if len(saved.RecentSurveys) != 1 || saved.RecentSurveys[0] != s.Atlas.Root || saved.LastSurvey != "" {
		t.Fatal(saved)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Fatal("forget removed workspace:", err)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "keep" {
		t.Fatal("refresh changed files", err)
	}
	w = adminRequest(t, s, "POST", "/api/app/admin/forget", map[string]any{"path": s.Atlas.Root}, "")
	if w.Code != 200 || s.surveyID != before {
		t.Fatal("forget changed active session", w.Code)
	}
	w = adminRequest(t, s, "GET", "/api/app/admin/status", nil, "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || len(status.Recent) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := s.rememberSurvey(s.Atlas.Root); err != nil {
		t.Fatal(err)
	}
	if len(s.set.get().RecentSurveys) != 1 {
		t.Fatal("reopening did not restore recent entry")
	}
	w = adminRequest(t, s, "POST", "/api/app/admin/forget", map[string]any{}, "")
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAdminDeleteWorkspace(t *testing.T) {
	s := adminServer(t)
	target := filepath.Join(t.TempDir(), "delete-me.atlas")
	if err := s.Atlas.CopyWorkspace(context.Background(), target, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.rememberSurvey(target); err != nil {
		t.Fatal(err)
	}
	reject := func(path string, confirmed bool) {
		t.Helper()
		result := adminAction(t, s, "delete", map[string]any{"path": path, "confirmed": confirmed})
		if result["status"] != "failed" {
			t.Fatal("deletion should fail", result)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("target changed on rejection", err)
		}
	}
	reject(target, false)
	reject(s.Atlas.Root, true)
	reject(filepath.Dir(target), true)
	lock, err := acquireSurveyLock(target)
	if err != nil {
		t.Fatal(err)
	}
	reject(target, true)
	lock.Close()
	invalid := t.TempDir()
	if err := s.rememberSurvey(invalid); err != nil {
		t.Fatal(err)
	}
	reject(invalid, true)
	if err := s.rememberSurvey(target); err != nil {
		t.Fatal(err)
	}
	before := s.surveyID
	result := adminAction(t, s, "delete", map[string]any{"path": target, "confirmed": true})
	requireComplete(t, result)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("workspace still exists", err)
	}
	if s.surveyID != before {
		t.Fatal("active session changed")
	}
	if !atlas.IsWorkspace(s.Atlas.Root) {
		t.Fatal("active workspace damaged")
	}
	saved := newSettings(filepath.Dir(s.set.path)).get()
	if saved.LastSurvey == target {
		t.Fatal("deleted workspace retained for startup")
	}
	for _, path := range saved.RecentSurveys {
		if path == target {
			t.Fatal("deleted workspace retained in list")
		}
	}
	w := adminRequest(t, s, "GET", "/api/app/admin/status", nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
