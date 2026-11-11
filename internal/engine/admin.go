package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/drivedb"
	"signal-atlas/internal/py"
)

// Administration has an app-owned runner: replacing a survey never replaces the
// operation that reports the handoff, nor the Compare/Drive sessions.
type adminOperations struct {
	observe func(string) // optional diagnostics/fault-injection seam
	mu      sync.Mutex
	busy    atomic.Bool
	state   map[string]any
	cancel  context.CancelFunc
	done    chan struct{}
}

func (o *adminOperations) snapshot() map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state == nil {
		return map[string]any{"status": "idle"}
	}
	v := map[string]any{}
	for k, x := range o.state {
		v[k] = x
	}
	return v
}
func (o *adminOperations) progress(stage string, done, total int64, cancelable bool) {
	o.mu.Lock()
	o.state["stage"] = stage
	o.state["completed"] = done
	o.state["total"] = total
	o.state["can_cancel"] = cancelable
	hook := o.observe
	o.mu.Unlock()
	if hook != nil {
		hook(stage)
	}
}
func (o *adminOperations) beginCommit(ctx context.Context, stage string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	o.state["can_cancel"] = false
	o.state["stage"] = stage
	return nil
}
func (o *adminOperations) start(kind string, work func(context.Context) (any, error)) (map[string]any, error) {
	if !o.busy.CompareAndSwap(false, true) {
		return nil, herr(409, "Another administration operation is running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	o.mu.Lock()
	o.cancel = cancel
	o.done = done
	o.state = map[string]any{"id": newHex(), "kind": kind, "status": "running", "stage": "Preparing", "can_cancel": true, "completed": int64(0), "total": int64(0)}
	o.mu.Unlock()
	go func() {
		defer close(done)
		defer cancel()
		result, err := work(ctx)
		o.mu.Lock()
		if result != nil {
			o.state["result"] = result
		}
		o.state["can_cancel"] = false
		if err != nil {
			if ctx.Err() != nil {
				o.state["status"] = "cancelled"
			} else {
				o.state["status"] = "failed"
			}
			o.state["error"] = err.Error()
		} else {
			o.state["status"] = "complete"
			o.state["result"] = result
		}
		o.busy.Store(false)
		o.mu.Unlock()
	}()
	return o.snapshot(), nil
}
func (o *adminOperations) requestCancel(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.busy.Load() && o.state["id"] == id && o.state["can_cancel"] == true {
		o.cancel()
		o.state["stage"] = "Cancelling at the next safe boundary"
		return true
	}
	return false
}
func (o *adminOperations) stop() {
	o.mu.Lock()
	if o.cancel != nil {
		o.cancel()
	}
	done := o.done
	o.mu.Unlock()
	if done != nil {
		<-done
	}
}

func zeroToken(id string, a *atlas.Atlas) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%p", id, a.Obs.State())))
	return hex.EncodeToString(h[:16])
}
func (s *Server) adminGet(w http.ResponseWriter, r *http.Request, action string) error {
	if action == "operation" {
		sendGoJSON(w, s.admin.snapshot(), 200)
		return nil
	}
	s.surveyMu.RLock()
	defer s.surveyMu.RUnlock()
	w.Header().Set("X-Survey-ID", s.surveyID)
	if expected := r.Header.Get("X-Survey-ID"); action != "status" && expected != "" && expected != s.surveyID {
		return herr(409, "The active survey changed; reload this page")
	}
	switch action {
	case "status":
		recent, err := s.set.recentSurveys("")
		if err != nil {
			return err
		}
		c, put, err := s.Atlas.Conn(r.Context())
		if err != nil {
			return err
		}
		info, err := s.Atlas.DatabaseInfo(c)
		put()
		if err != nil {
			return err
		}
		pointer, err := atlas.ReadWorkspacePointer(s.Atlas.Root)
		if err != nil {
			return err
		}
		sendGoJSON(w, map[string]any{"survey_id": s.surveyID, "path": s.Atlas.Root, "generation": pointer.Active, "recovery_available": pointer.Previous != "", "database": info, "job": s.Jobs.Snapshot(), "operation": s.admin.snapshot(), "recent": recent, "dialogs": s.o.Host != nil, "warning": s.startupWarning, "capabilities": map[string]bool{"workspace_snapshot": true, "portable_sqlite": true, "full_observation_sql": true}}, 200)
		return nil
	case "imports":
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		offset = max(0, offset)
		c, put, err := s.Atlas.Conn(r.Context())
		if err != nil {
			return err
		}
		defer put()
		rows, err := db.All(c, "SELECT * FROM imports ORDER BY id DESC LIMIT 100 OFFSET ?", offset)
		if err != nil {
			return err
		}
		total, err := db.Scalar(c, "SELECT count(*) FROM imports")
		if err != nil {
			return err
		}
		sendGoJSON(w, map[string]any{"rows": rows, "total": db.Int(total), "offset": offset, "limit": 100}, 200)
		return nil
	case "preview":
		state := s.Atlas.Obs.State()
		token := zeroToken(s.surveyID, s.Atlas)
		v, err := s.Atlas.ZeroPreview(r.Context())
		if err != nil {
			return err
		}
		if state != s.Atlas.Obs.State() {
			return herr(409, "The observations changed; preview the active survey again")
		}
		sendGoJSON(w, map[string]any{"rows": v.Rows, "imports": v.Imports, "networks": v.Networks, "token": token, "survey_id": s.surveyID}, 200)
		return nil
	}
	return herr(404, "Unknown administration request")
}

func (s *Server) adminPost(w http.ResponseWriter, r *http.Request, action string) error {
	if action == "upload" {
		return s.adminUpload(w, r)
	}
	body, err := readJSON(r, 1<<20)
	if err != nil {
		return err
	}
	if action == "cancel" {
		id, _ := body["id"].(string)
		s.admin.requestCancel(id)
		sendGoJSON(w, s.admin.snapshot(), 200)
		return nil
	}
	if action == "forget" {
		path, _ := body["path"].(string)
		if strings.TrimSpace(path) == "" {
			return herr(400, "Choose a workspace to remove from the recent list")
		}
		recent, err := s.set.recentSurveys(path)
		if err != nil {
			return err
		}
		sendGoJSON(w, map[string]any{"recent": recent}, 200)
		return nil
	}
	s.surveyMu.RLock()
	expected := s.surveyID
	s.surveyMu.RUnlock()
	if h := r.Header.Get("X-Survey-ID"); h != "" {
		expected = h
	}
	if err := s.expectedSurvey(expected); err != nil {
		return err
	}
	path, _ := body["path"].(string)
	source, _ := body["source"].(string)
	format, _ := body["format"].(string)
	if r.URL.Path == "/api/app/save-atlas" && format == "" {
		format = "sqlite"
	}
	if action == "new" || action == "open" || action == "snapshot" || action == "convert" {
		if action == "convert" && source == "" && s.o.Host != nil {
			files, e := s.o.Host.PickFiles("Convert a saved Signal Atlas SQLite survey", []string{"sqlite3", "sqlite", "db"}, false)
			if e != nil {
				return e
			}
			if len(files) == 0 {
				sendGoJSON(w, map[string]any{"cancelled": true}, 200)
				return nil
			}
			source = files[0]
		}
		if path == "" && s.o.Host != nil {
			if action == "open" {
				path, err = s.o.Host.PickFolder("Open a Signal Atlas survey workspace folder")
				if path == "" && err == nil {
					sendGoJSON(w, map[string]any{"cancelled": true}, 200)
					return nil
				}
			} else {
				ext := "atlas"
				title := "New survey workspace"
				if action == "snapshot" {
					title = "Save complete workspace snapshot"
					if format == "sqlite" {
						ext = "sqlite3"
						title = "Export portable SQLite snapshot"
					}
				}
				var ok bool
				path, ok, err = s.o.Host.SavePath(title, "survey."+ext, ext)
				if !ok && err == nil {
					sendGoJSON(w, map[string]any{"cancelled": true}, 200)
					return nil
				}
			}
			if err != nil {
				return err
			}
		}
		if path == "" {
			return py.Errorf("Choose a workspace or destination path")
		}
	}
	if action == "wigle" {
		token, _ := body["token"].(string)
		token = strings.TrimSpace(token)
		if token == "" || strings.ContainsAny(token, "\r\n") {
			return herr(400, "Enter the WiGLE token's Encoded for use value")
		}
		skip, _ := body["skip_invalid"].(bool)
		return s.startAdmin(w, "wigle", expected, func(ctx context.Context) (any, error) {
			return s.adminWiGLE(ctx, token, skip, newWiGLEClient())
		})
	}
	if action == "import" {
		paths := strList(body["paths"])
		if len(paths) == 0 && s.o.Host != nil {
			paths, err = s.o.Host.PickFiles("Add scans from CSV, logger or WiGLE databases", []string{"csv", "sqlite3", "sqlite", "db"}, true)
			if err != nil {
				return err
			}
		}
		if len(paths) == 0 {
			sendGoJSON(w, map[string]any{"cancelled": true}, 200)
			return nil
		}
		skip, _ := body["skip_invalid"].(bool)
		return s.startAdmin(w, "import", expected, func(ctx context.Context) (any, error) { return s.adminImports(ctx, paths, skip) })
	}
	return s.startAdmin(w, action, expected, func(ctx context.Context) (any, error) {
		switch action {
		case "new", "open", "convert":
			return s.adminOpen(ctx, action, path, source)
		case "snapshot":
			return s.adminSnapshot(ctx, path, format)
		case "delete":
			confirmed, _ := body["confirmed"].(bool)
			if !confirmed {
				return nil, herr(400, "Confirm workspace deletion first")
			}
			return s.adminDeleteWorkspace(ctx, path)
		case "cleanup":
			token, _ := body["token"].(string)
			return s.adminCleanup(ctx, token)
		case "restore":
			return s.adminRestore(ctx)
		case "maintenance":
			task, _ := body["task"].(string)
			return s.adminMaintenance(ctx, task)
		}
		return nil, py.Errorf("Unknown administration action")
	})
}
func (s *Server) startAdmin(w http.ResponseWriter, kind, expected string, work func(context.Context) (any, error)) error {
	s.adminGate.Lock()
	defer s.adminGate.Unlock()
	if s.closed.Load() {
		return herr(503, "The application is closing")
	}
	job, err := s.admin.start(kind, func(ctx context.Context) (any, error) {
		if err := s.expectedSurvey(expected); err != nil {
			return nil, err
		}
		return work(ctx)
	})
	if err != nil {
		return err
	}
	sendGoJSON(w, job, 202)
	return nil
}

func validateWorkspacePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if !atlas.IsWorkspace(path) {
		return "", py.Errorf("Choose a complete survey workspace folder. Saved SQLite files must be converted instead.")
	}
	if filepath.Base(filepath.Dir(path)) == "generations" && atlas.IsWorkspace(filepath.Dir(filepath.Dir(path))) {
		return "", py.Errorf("Open the workspace root, not an internal generation folder")
	}
	return filepath.EvalSymlinks(path)
}

func (s *Server) adminDeleteWorkspace(ctx context.Context, path string) (any, error) {
	if !filepath.IsAbs(path) {
		return nil, herr(400, "Choose a workspace from the recent list")
	}
	listed := false
	for _, recent := range s.set.get().RecentSurveys {
		listed = listed || strings.EqualFold(filepath.Clean(recent), filepath.Clean(path))
	}
	if !listed {
		return nil, herr(400, "Choose a workspace from the recent list")
	}
	root, err := validateWorkspacePath(path)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(root, filepath.Clean(path)) {
		return nil, herr(400, "Open the actual workspace folder instead of a linked folder before deleting")
	}
	s.surveyMu.RLock()
	defer s.surveyMu.RUnlock()
	// Never delete the active survey, its parents, or app settings/data.
	for _, protected := range []string{s.Atlas.Root, filepath.Dir(s.set.path)} {
		actual, err := filepath.EvalSymlinks(protected)
		if err != nil {
			return nil, err
		}
		base := strings.ToLower(filepath.Clean(root))
		protectedPath := strings.ToLower(filepath.Clean(actual))
		if protectedPath == base || strings.HasPrefix(protectedPath, base+string(os.PathSeparator)) {
			return nil, herr(409, "Cannot delete the active workspace or application data. Open another survey first.")
		}
	}
	lock, err := acquireSurveyLock(root)
	if err != nil {
		return nil, err
	}
	defer func() {
		if lock != nil {
			lock.Close()
		}
	}()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	if err := s.admin.beginCommit(ctx, "Deleting workspace permanently"); err != nil {
		return nil, err
	}
	// Keep the process lock held while deleting contents. Windows requires the
	// lock handle to be closed before removing the lock file and empty folder.
	for _, entry := range entries {
		if entry.Name() == ".signal-atlas.lock" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return nil, fmt.Errorf("Workspace deletion was incomplete: %w", err)
		}
	}
	err = lock.Close()
	lock = nil // Close consumes the handle even when it reports an error.
	if err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(root, ".signal-atlas.lock")); err != nil {
		return nil, err
	}
	if err := os.Remove(root); err != nil {
		return nil, err
	}
	if _, err := s.set.recentSurveys(path); err != nil {
		return nil, fmt.Errorf("Workspace deleted, but the recent list could not be saved: %w", err)
	}
	return map[string]any{"deleted": root}, nil
}
func (s *Server) adminOpen(ctx context.Context, action, path, source string) (any, error) {
	var err error
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	s.surveyMu.RLock()
	same := strings.EqualFold(path, s.Atlas.Root)
	s.surveyMu.RUnlock()
	if action == "open" && same {
		return map[string]any{"opened": path, "unchanged": true}, nil
	}
	created := action != "open"
	if created {
		if err := os.Mkdir(path, 0755); err != nil {
			return nil, fmt.Errorf("New workspace target must not exist: %w", err)
		}
	} else {
		path, err = validateWorkspacePath(path)
		if err != nil {
			return nil, err
		}
	}
	consumed := false
	var candidate *atlas.Atlas
	var lock *os.File
	defer func() {
		if !consumed {
			if candidate != nil {
				candidate.Close()
			}
			if lock != nil {
				lock.Close()
			}
			if created {
				removeOwnedCandidate(path, filepath.Dir(path))
			}
		}
	}()
	lock, err = acquireSurveyLock(path)
	if err != nil {
		return nil, err
	}
	s.admin.progress("Opening candidate workspace", 0, 0, true)
	candidate, err = atlas.Open(path)
	if err != nil {
		return nil, err
	}
	var converted map[string]any
	if action == "convert" {
		if err := portableSurvey(source); err != nil {
			return nil, err
		}
		converted, err = candidate.OpenDatabase(source, func(n int64) error { s.admin.progress("Converting saved SQLite survey", n, 0, true); return ctx.Err() }, func(stage string) { s.admin.progress(stage, 0, 0, true) })
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.admin.progress("Validating candidate workspace", 0, 0, true)
	if err := candidate.Validate(ctx); err != nil {
		return nil, err
	}
	if created {
		if err := atlas.PublishWorkspace(path, ".", ""); err != nil {
			return nil, err
		}
	}
	s.admin.progress("Cancelling and joining old survey workers", 0, 0, true)
	_, j, err := s.freezeSurvey(ctx, true)
	if err != nil {
		return nil, err
	}
	if err := s.installSurvey(ctx, candidate, lock, ""); err != nil {
		j.Resume()
		return nil, err
	}
	consumed = true
	return map[string]any{"opened": path, "created": created, "conversion": converted, "reload": true}, nil
}
func portableSurvey(path string) error {
	kind, err := atlas.DatabaseKind(path)
	if err != nil {
		return err
	}
	if kind != atlas.KindAtlas {
		return py.Errorf("This is not a saved Signal Atlas SQLite survey")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "observations")); err == nil && strings.EqualFold(filepath.Base(path), "atlas.sqlite3") {
		return py.Errorf("That file is workspace metadata only. Open its workspace folder instead.")
	}
	return nil
}

func (s *Server) adminCleanup(ctx context.Context, token string) (any, error) {
	a, j, err := s.freezeSurvey(ctx, false)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			j.Resume()
		}
	}()
	if token == "" || token != zeroToken(s.surveyID, a) {
		return nil, py.Errorf("The preview is stale; preview the active survey again")
	}
	preview, err := a.ZeroPreview(ctx)
	if err != nil {
		return nil, err
	}
	if preview.Rows == 0 {
		j.Resume()
		return map[string]any{"removed_rows": int64(0), "unchanged": true}, nil
	}
	parent, stage, err := generationPath(a.Root)
	if err != nil {
		return nil, err
	}
	var candidate *atlas.Atlas
	defer func() {
		if !committed {
			if candidate != nil {
				candidate.Close()
			}
			removeOwnedCandidate(stage, parent)
		}
	}()
	candidate, result, err := a.PrepareCleanup(ctx, stage, func(done, total int64, stage string) { s.admin.progress(stage, done, total, true) })
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	relative, _ := filepath.Rel(a.Root, stage)
	if err := s.installSurvey(ctx, candidate, nil, relative); err != nil {
		return nil, err
	}
	committed = true
	result["recovery_available"] = true
	result["reload"] = true
	return result, nil
}
func (s *Server) adminRestore(ctx context.Context) (any, error) {
	a, j, err := s.freezeSurvey(ctx, false)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			j.Resume()
		}
	}()
	p, err := atlas.ReadWorkspacePointer(a.Root)
	if err != nil {
		return nil, err
	}
	if p.Previous == "" {
		return nil, py.Errorf("No previous generation is available")
	}
	candidate, err := atlas.OpenGeneration(a.Root, p.Previous)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !committed {
			candidate.Close()
		}
	}()
	if err := candidate.Validate(ctx); err != nil {
		return nil, err
	}
	if err := s.installSurvey(ctx, candidate, nil, p.Previous); err != nil {
		return nil, err
	}
	committed = true
	return map[string]any{"restored": true, "reload": true}, nil
}

func (s *Server) adminSnapshot(ctx context.Context, path, format string) (any, error) {
	a, j, err := s.freezeSurvey(ctx, false)
	if err != nil {
		return nil, err
	}
	defer j.Resume()
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if format == "sqlite" {
		if strings.EqualFold(filepath.Base(path), "atlas.sqlite3") {
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), "observations")); err == nil {
				return nil, py.Errorf("Choose a portable snapshot file outside live workspace metadata")
			}
		}
		s.drv.mu.Lock()
		archivePath := ""
		if s.drv.d != nil {
			archivePath = s.drv.d.Path
		}
		s.drv.mu.Unlock()
		if archivePath != "" && strings.EqualFold(path, archivePath) {
			return nil, py.Errorf("A survey snapshot cannot replace the open Drive database")
		}
		if strings.EqualFold(path, a.MetaPath) {
			return nil, py.Errorf("A portable export cannot replace live workspace metadata")
		}
		f, err := os.CreateTemp(filepath.Dir(path), ".survey-export-*")
		if err != nil {
			return nil, err
		}
		part := f.Name()
		f.Close()
		os.Remove(part)
		defer os.Remove(part)
		if err := a.ExportSQLiteContext(ctx, part, func(stage string) { s.admin.progress(stage, 0, int64(a.Obs.State().N), true) }, func(done, total int64) { s.admin.progress("Writing observations", done, total, true) }); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.admin.beginCommit(ctx, "Publishing complete SQLite snapshot"); err != nil {
			return nil, err
		}
		if err := os.Rename(part, path); err != nil {
			return nil, err
		}
		return map[string]any{"saved": true, "path": path, "format": "sqlite", "observations": a.Obs.State().N}, nil
	}
	// Reserve the final name so a race cannot overwrite a pre-existing workspace.
	if err := os.Mkdir(path, 0755); err != nil {
		return nil, fmt.Errorf("Snapshot workspace target must not exist: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			removeOwnedCandidate(path, filepath.Dir(path))
		}
	}()
	if err := os.WriteFile(filepath.Join(path, ".incomplete"), []byte("snapshot in progress"), 0600); err != nil {
		return nil, err
	}
	parent := filepath.Join(path, "generations")
	if err := os.Mkdir(parent, 0755); err != nil {
		return nil, err
	}
	stage := filepath.Join(parent, "g-"+newHex())
	if err := a.CopyWorkspace(ctx, stage, func(stage string) { s.admin.progress(stage, 0, 0, true) }); err != nil {
		return nil, err
	}
	candidate, err := a.ForkWorkspace(stage)
	if err != nil {
		return nil, err
	}
	err = candidate.Validate(ctx)
	candidate.Close()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.admin.beginCommit(ctx, "Publishing complete workspace snapshot"); err != nil {
		return nil, err
	}
	relative, _ := filepath.Rel(path, stage)
	if err := atlas.PublishWorkspace(path, relative, ""); err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(path, ".incomplete")); err != nil {
		return nil, err
	}
	complete = true
	return map[string]any{"saved": true, "path": path, "format": "workspace", "observations": a.Obs.State().N}, nil
}

func (s *Server) adminMaintenance(ctx context.Context, task string) (any, error) {
	a, j, err := s.freezeSurvey(ctx, false)
	if err != nil {
		return nil, err
	}
	defer j.Resume()
	s.admin.progress("Running "+task, 0, 0, true)
	switch task {
	case "validate":
		if err := a.Validate(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"valid": true, "observations": a.Obs.State().N}, nil
	case "rebuild_cache":
		a.RebuildCaches()
		return map[string]any{"rebuilt": "map lookups and observation indexes", "saved_fits_preserved": true}, nil
	case "checkpoint", "analyze":
		c, put, err := a.Conn(ctx)
		if err != nil {
			return nil, err
		}
		defer put()
		var v any
		if task == "checkpoint" {
			v, err = db.All(c, "PRAGMA wal_checkpoint(TRUNCATE)")
		} else {
			_, err = db.Exec(c, "PRAGMA optimize")
			v = map[string]any{"optimized": "metadata query statistics"}
		}
		return v, err
	}
	return nil, py.Errorf("Unsupported maintenance action")
}

func (s *Server) adminImports(ctx context.Context, paths []string, skip bool) (any, error) {
	defer s.releaseUploads(paths)
	a, j, err := s.freezeSurvey(ctx, false)
	if err != nil {
		return nil, err
	}
	defer j.Resume()
	results := []map[string]any{}
	for index, path := range paths {
		if err := ctx.Err(); err != nil {
			return map[string]any{"files": results}, err
		}
		name := filepath.Base(path)
		s.uploadMu.Lock()
		if uploaded := s.uploads[path]; uploaded != "" {
			name = uploaded
		}
		s.uploadMu.Unlock()
		s.admin.progress(fmt.Sprintf("Importing %d of %d: %s", index+1, len(paths), name), 0, 0, true)
		progress := func(valid, invalid, read, total int64) error {
			s.admin.progress(fmt.Sprintf("%s · %d accepted · %d invalid", name, valid, invalid), read, total, true)
			return ctx.Err()
		}
		var result map[string]any
		var importErr error
		if !drivedb.IsDatabase(path) {
			result, importErr = a.ImportCSV(path, skip, progress, name, func(stage string) { s.admin.progress(name+" · "+stage, 0, 0, true) })
		} else {
			kind, e := atlas.DatabaseKind(path)
			if e == nil && kind == atlas.KindAtlas && portableSurvey(path) != nil {
				importErr = portableSurvey(path)
			} else if e == nil && kind == atlas.KindWiGLE {
				result, importErr = a.ImportWiGLE(path, skip, func(v, i int64) error { return progress(v, i, v+i, 0) }, nil)
			} else {
				if e := os.MkdirAll(filepath.Join(s.o.Data, "uploads"), 0755); e != nil {
					return nil, e
				}
				tmp := filepath.Join(s.o.Data, "uploads", "convert-"+newHex()+".csv")
				_, _, _, importErr = drivedb.ToCSVContext(ctx, path, tmp, "signal-atlas", false, func(rows, done, total int64) { s.admin.progress("Converting "+name, done, total, true) })
				if importErr == nil {
					result, importErr = a.ImportCSV(tmp, skip, progress, name, nil)
				}
				os.Remove(tmp)
			}
		}
		if result == nil {
			result = map[string]any{}
		}
		result["filename"] = name
		if importErr != nil {
			result["error"] = importErr.Error()
		}
		results = append(results, result)
		if ctx.Err() != nil {
			return map[string]any{"files": results, "reload": true}, ctx.Err()
		}
	}
	return map[string]any{"files": results, "reload": true}, nil
}

// releaseUploads removes only scratch files this engine created, never source files.
func (s *Server) releaseUploads(paths []string) {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	if paths == nil {
		for path := range s.uploads {
			os.Remove(path)
			delete(s.uploads, path)
		}
		return
	}
	for _, path := range paths {
		if _, owned := s.uploads[path]; owned {
			os.Remove(path)
			delete(s.uploads, path)
		}
	}
}
func (s *Server) adminUpload(w http.ResponseWriter, r *http.Request) error {
	s.adminGate.Lock()
	defer s.adminGate.Unlock()
	if s.closed.Load() {
		return herr(503, "The application is closing")
	}
	if r.ContentLength <= 0 || r.ContentLength > 2_000_000_000 {
		return py.Errorf("Choose a file between 1 byte and 2 GB, or use the native file picker")
	}
	dir := filepath.Join(s.o.Data, "uploads")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	name := sanitizeName(filepath.Base(py.Unquote(r.Header.Get("X-Filename"))))
	path := filepath.Join(dir, newHex()+"-"+name)
	if err := saveBody(r, path, r.ContentLength); err != nil {
		return err
	}
	s.uploadMu.Lock()
	if s.uploads == nil {
		s.uploads = map[string]string{}
	}
	s.uploads[path] = name
	s.uploadMu.Unlock()
	sendGoJSON(w, map[string]any{"path": path, "name": name}, 200)
	return nil
}
