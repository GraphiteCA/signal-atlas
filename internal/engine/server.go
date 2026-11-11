// Package engine is Signal Atlas's local server. It serves the app's pages (the home page, Wardrive
// Compare and Graphite's Signal Atlas explorer) and the APIs they speak: Signal Atlas's own API
// (serve_survey.py, ported), background jobs, and the app's endpoints (compare engine sessions,
// native dialogs, dropped-file paths, saving exports, map tiles).
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/py"
)

var dbAll = db.All

// Host is what only the app's window can do: native dialogs. Nil when serving to a browser.
type Host interface {
	// PickFiles asks for files of the given extensions.
	PickFiles(title string, exts []string, multiple bool) ([]string, error)
	// SavePath asks where to save a file.
	SavePath(title, defaultName, ext string) (string, bool, error)
	// ShowFolder opens a folder in the file manager.
	ShowFolder(dir string)
	// PickFolder asks for a folder ("" when cancelled).
	PickFolder(title string) (string, error)
}

// Options configure a Server.
type Options struct {
	DisablePreparation bool  // isolated/manual benchmark and test workloads
	UI                 fs.FS // ui/: the home page and the page bridge
	Compare            fs.FS // web/: compare.html
	Explorer           fs.FS // python/templates/: Graphite's explorer
	Data               string
	Version            string
	Hosts              []string // Host header values allowed besides the listening address
	Origins            []string // Origin header values allowed besides http://<host> (wails://wails on macOS and Linux)
	Host               Host
}

// Server is the engine.
type Server struct {
	o              Options
	Atlas          *atlas.Atlas
	Jobs           *Jobs
	cmp            *compareSessions
	drops          *dropRegistry
	drv            *drives
	set            *settingsStore
	surveyMu       sync.RWMutex
	surveyID       string
	handoff        atomic.Bool
	surveyLock     *os.File
	appLock        *os.File
	admin          adminOperations
	adminGate      sync.Mutex
	uploadMu       sync.Mutex
	uploads        map[string]string
	closed         atomic.Bool
	startupWarning string
	preparation    *preparationRunner
	events         *jobEvents
	mapPoints      mapPointsCache
	sql            sqlBrowser
	hostsMu        sync.RWMutex
	hosts          map[string]bool
	origins        map[string]bool
	// OpenURL opens a link in the user's browser (the window sets it).
	OpenURL func(string)
}

// New opens the store and builds the server.
func New(o Options) (*Server, error) {
	if err := os.MkdirAll(o.Data, 0o755); err != nil {
		return nil, err
	}
	appLock, err := acquireSurveyLock(o.Data)
	if err != nil {
		return nil, fmt.Errorf("Application data directory is already in use: %w", err)
	}
	opened := false
	defer func() {
		if !opened {
			appLock.Close()
		}
	}()
	settings := newSettings(o.Data)
	root := settings.get().LastSurvey
	warning := ""
	if root != "" && !atlas.IsWorkspace(root) {
		warning = "The last workspace is unavailable; opened the default survey instead."
		root = ""
	}
	if root == "" {
		root = filepath.Join(o.Data, "store")
		if err := os.MkdirAll(root, 0755); err != nil {
			return nil, err
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	lock, err := acquireSurveyLock(root)
	if err != nil {
		return nil, err
	}
	a, err := atlas.Open(root)
	if err != nil {
		lock.Close()
		failedRoot := root
		fallback, _ := filepath.Abs(filepath.Join(o.Data, "store"))
		if strings.EqualFold(root, fallback) {
			fallback, _ = filepath.Abs(filepath.Join(o.Data, "recovery-store"))
		}
		if e := os.MkdirAll(fallback, 0755); e != nil {
			return nil, fmt.Errorf("Signal Atlas could not open %s: %w", failedRoot, err)
		}
		lock, err = acquireSurveyLock(fallback)
		if err != nil {
			return nil, err
		}
		a, err = atlas.Open(fallback)
		if err != nil {
			lock.Close()
			return nil, err
		}
		root = fallback
		warning = "Could not open " + failedRoot + ". Using " + fallback + "; the unavailable workspace was kept unchanged."
	}
	if a.Recovered {
		warning = "Recovered the previous complete survey revision after an incomplete generation."
	}
	os.RemoveAll(filepath.Join(o.Data, "exports")) // leftovers of an earlier run
	s := &Server{o: o, Atlas: a, Jobs: newJobs(a), drops: newDropRegistry(), hosts: map[string]bool{}, origins: map[string]bool{}, surveyID: newHex(), surveyLock: lock, appLock: appLock, startupWarning: warning}
	s.cmp = newCompareSessions(filepath.Join(o.Data, "exports"))
	for _, org := range o.Origins {
		s.origins[org] = true
	}
	for _, h := range o.Hosts {
		s.AllowHost(h)
	}
	s.set = settings
	if warning == "" {
		if err := s.rememberSurvey(root); err != nil {
			s.startupWarning = "Could not remember this workspace: " + err.Error()
		}
	}
	go s.autoCheck()
	s.drv = newDrives(s)
	s.events = newJobEvents()
	s.preparation = newPreparation(s)
	opened = true
	return s, nil
}

// AllowHost lets requests with this Host header (and Origin http://host) in.
func (s *Server) AllowHost(h string) {
	s.hostsMu.Lock()
	s.hosts[h] = true
	s.origins["http://"+h] = true
	s.hostsMu.Unlock()
}

// RecordDrop notes files dropped on the window (their paths, which a page never learns).
func (s *Server) RecordDrop(paths []string) { s.drops.add(paths) }

// Close closes the store.
func (s *Server) Close() {
	s.adminGate.Lock()
	if !s.closed.CompareAndSwap(false, true) {
		s.adminGate.Unlock()
		return
	}
	s.adminGate.Unlock()
	s.sql.stop("")
	s.admin.stop()
	s.preparation.close()
	s.events.close()
	s.releaseUploads(nil)
	s.surveyMu.Lock()
	defer s.surveyMu.Unlock()
	s.Jobs.Retire()
	s.Jobs.Wait(context.Background())
	s.cmp.closeAll()
	s.drv.close()
	s.Atlas.Close()
	if s.surveyLock != nil {
		s.surveyLock.Close()
	}
	if s.appLock != nil {
		s.appLock.Close()
	}
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func herr(status int, f string, a ...any) error { return &httpError{status, fmt.Sprintf(f, a...)} }

func sendJSON(w http.ResponseWriter, v any, status int) {
	text, err := py.Dumps(v, py.DumpOpts{NoEnsureASCII: true})
	if err != nil {
		b, _ := json.Marshal(v)
		text = string(b)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	io.WriteString(w, text)
}

func sendGoJSON(w http.ResponseWriter, v any, status int) {
	b, err := json.Marshal(v)
	if err != nil {
		sendJSON(w, map[string]any{"error": err.Error()}, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b)
}

func sendErr(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		sendJSON(w, map[string]any{"error": he.msg}, he.status)
		return
	}
	sendJSON(w, map[string]any{"error": err.Error()}, 400)
}

// allowed: a loopback or app Host header (no DNS rebinding) and no Origin but our own, so no other
// website can reach the engine.
func (s *Server) allowed(r *http.Request) bool {
	s.hostsMu.RLock()
	defer s.hostsMu.RUnlock()
	if !s.hosts[r.Host] {
		return false
	}
	o := r.Header.Get("Origin")
	return o == "" || s.origins[o]
}

var explorerAssets = map[string]bool{"/explorer.js": true, "/explorer.css": true, "/satellite.js": true, "/network_tree.js": true, "/observation_scope.js": true,
	"/estimate_comparison.js": true, "/online_map.js": true, "/follow_estimate.js": true, "/area_estimates.js": true, "/network_tree.css": true}

// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.closed.Load() {
		sendErr(w, herr(503, "The application is closing"))
		return
	}
	defer func() {
		if p := recover(); p != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Fprintf(os.Stderr, "panic serving %s: %v\n%s\n", r.URL.Path, p, buf[:n])
			sendJSON(w, map[string]any{"error": fmt.Sprint(p)}, 500)
		}
	}()
	if !s.allowed(r) {
		sendJSON(w, map[string]any{"error": "not allowed"}, 403)
		return
	}
	// Foreground writes cancel/join background preparation before taking their
	// survey lease. The lease then protects the captured runner through dispatch.
	pagedMap := r.Method == http.MethodGet && r.URL.Path == "/api/app/map/features" && (r.URL.Query().Get("clustering") == "0" || r.URL.Query().Get("cursor") != "")
	if pagedMap || (r.Method == http.MethodPost && (surveyRequest(r.URL.Path) || r.URL.Path == "/api/app/settings")) {
		s.surveyMu.RLock()
		prep := s.preparation
		prep.foreground.Add(1)
		s.surveyMu.RUnlock()
		defer func() { prep.foreground.Add(-1); prep.signal() }()
		if err := prep.join(r.Context()); err != nil {
			sendErr(w, err)
			return
		}
	}
	if surveyRequest(r.URL.Path) {
		if s.handoff.Load() {
			sendErr(w, herr(503, "Survey handoff in progress; retry shortly"))
			return
		}
		s.surveyMu.RLock()
		defer s.surveyMu.RUnlock()
		// Clustered AP reads use WAL snapshots and immutable observation state.
		// Pausing after every live update can starve the preparation that
		// generated that update. Only large sighting-buffer reads yield CPU.
		if r.Method == http.MethodGet && r.URL.Path == "/api/app/map/points" {
			prep := s.preparation
			prep.mapReaders.Add(1)
			defer func() {
				prep.mapReaders.Add(-1)
				prep.signal()
			}()
		}
		w.Header().Set("X-Survey-ID", s.surveyID)
		if expected := r.Header.Get("X-Survey-ID"); expected != "" && expected != s.surveyID {
			sendErr(w, herr(409, "The active survey changed; reload this page"))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path != "/api/cancel" && s.admin.busy.Load() {
			sendErr(w, herr(409, "Survey administration is running; wait or cancel it first"))
			return
		}
	}
	if err := s.route(w, r); err != nil {
		sendErr(w, err)
	}
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) error {
	p := r.URL.Path
	params := py.ParseQs(r.URL.RawQuery)
	// the fast solver, when chosen in the settings, for every fit and every cached-fit lookup
	if strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/api/app/compare") && !params.Has("solver") {
		settings := s.set.get()
		if solver := atlas.SolverName(settings.FastFits, settings.GPUFits); solver != "reference" {
			params["solver"] = []string{solver}
		}
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		switch {
		case p == "/" || p == "/index.html":
			return s.serveFS(w, s.o.UI, "index.html", false)
		case p == "/app/bridge.js":
			return s.serveFS(w, s.o.UI, "bridge.js", false)
		case p == "/app/native.js":
			return s.serveFS(w, s.o.UI, "native.js", false)
		case strings.HasPrefix(p, "/app/map/"):
			return s.serveFS(w, s.o.UI, "map/"+strings.TrimPrefix(p, "/app/map/"), false)
		case p == "/app/admin.js" || p == "/app/admin.css" || p == "/app/sql.js" || p == "/app/sql.css":
			return s.serveFS(w, s.o.UI, strings.TrimPrefix(p, "/app/"), false)
		case strings.HasPrefix(p, "/app/vendor/"):
			return s.serveFS(w, s.o.UI, "vendor/"+strings.TrimPrefix(p, "/app/vendor/"), false)
		case p == "/compare.html" || p == "/compare":
			return s.serveFS(w, s.o.Compare, "compare.html", true)
		case p == "/map" || p == "/map.html":
			return s.serveFS(w, s.o.UI, "map.html", true)
		case p == "/sql" || p == "/sql.html":
			return s.serveFS(w, s.o.UI, "sql.html", true)
		case p == "/admin" || p == "/admin.html":
			return s.serveFS(w, s.o.UI, "admin.html", true)
		case p == "/database" || p == "/database.html":
			http.Redirect(w, r, "/admin?tab=survey", http.StatusTemporaryRedirect)
			return nil
		case p == "/atlas" || p == "/atlas/":
			return s.serveFS(w, s.o.Explorer, "explorer.html", true)
		case explorerAssets[p]:
			return s.serveFS(w, s.o.Explorer, strings.TrimPrefix(p, "/"), false)
		case p == "/api/status":
			sendJSON(w, s.status(), 200)
			return nil
		case p == "/api/job":
			sendJSON(w, s.Jobs.Snapshot(), 200)
			return nil
		case p == "/api/database/download":
			return s.downloadStore(w, r.Context())
		case tilePath.MatchString(p):
			return s.tile(w, r, tilePath.FindStringSubmatch(p))
		case strings.HasPrefix(p, "/api/app/"):
			return s.appGet(w, r, p, params)
		}
		if v, ok, err := s.atlasGet(r.Context(), p, params); ok {
			if err != nil {
				return err
			}
			if raw, isRaw := v.(py.Raw); isRaw {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				io.WriteString(w, string(raw))
				return nil
			}
			sendJSON(w, v, 200)
			return nil
		}
		return herr(404, "Not found")
	}
	if r.Method != http.MethodPost {
		return herr(404, "Not found")
	}
	switch {
	case p == "/api/cancel":
		s.Jobs.Cancel()
		sendJSON(w, s.Jobs.Snapshot(), 200)
		return nil
	case strings.HasPrefix(p, "/api/app/"):
		return s.appPost(w, r, p, params)
	}
	return s.atlasPost(w, r, p, params)
}

func (s *Server) status() map[string]any {
	st := map[string]any{"version": s.o.Version, "go": runtime.Version(), "sqlite": atlas.SQLiteVersion, "data": s.o.Data,
		"window": s.o.Host != nil, "threads": map[string]any{"fits": s.Jobs.Threads, "cores": runtime.NumCPU()}, "error": nil}
	c, put, err := s.Atlas.Conn(context.Background())
	if err != nil {
		st["error"] = err.Error()
		return st
	}
	defer put()
	info, err := s.Atlas.DatabaseInfo(c)
	if err != nil {
		st["error"] = err.Error()
	}
	st["atlas"], st["job"] = info, s.Jobs.Snapshot()
	return st
}

// atlasGet is Signal Atlas's GET API. ok is false for other paths.
func (s *Server) atlasGet(ctx context.Context, p string, params py.Params) (any, bool, error) {
	a := s.Atlas
	switch p {
	case "/api/networks":
		v, err := a.Networks(params)
		return v, true, err
	case "/api/radios":
		v, err := a.Radios(params)
		return v, true, err
	case "/api/dates":
		v, err := a.AvailableDates(params)
		return v, true, err
	case "/api/points":
		v, err := a.Points(params)
		return v, true, err
	case "/api/observations":
		v, err := a.Observations(params)
		return v, true, err
	case "/api/summary", "/api/transmitters", "/api/area-estimates", "/api/estimates", "/api/database", "/api/basemap", "/api/comparisons":
	default:
		return nil, false, nil
	}
	c, put, err := a.Conn(ctx)
	if err != nil {
		return nil, true, err
	}
	defer put()
	var v any
	switch p {
	case "/api/summary":
		v, err = a.Summary(c, params)
	case "/api/transmitters":
		v, err = a.Transmitters(c, params)
	case "/api/area-estimates":
		v, err = a.AreaEstimates(c, params)
	case "/api/estimates":
		v, err = a.FittedEstimates(c, params)
	case "/api/database":
		v, err = a.DatabaseInfo(c)
	case "/api/basemap":
		v, err = a.LoadBasemap(c)
	case "/api/comparisons":
		v, err = atlas.Comparisons(c)
	}
	return v, true, err
}

// readJSON parses a request body, numbers kept exact (whole numbers as int64).
func readJSON(r *http.Request, limit int64) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, herr(400, "request too large")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, py.Errorf("Expecting value: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	for k, x := range m {
		if n, ok := x.(json.Number); ok {
			if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
				m[k] = i
			} else if f, err := n.Float64(); err == nil {
				m[k] = f
			}
		}
	}
	return m, nil
}

func (s *Server) atlasPost(w http.ResponseWriter, r *http.Request, p string, params py.Params) error {
	a := s.Atlas
	switch p {
	case "/api/comparisons", "/api/comparisons/delete":
		if !(r.ContentLength > 0 && r.ContentLength <= 65536) {
			return py.Errorf("Invalid comparison request size")
		}
		body, err := readJSON(r, 65536)
		if err != nil {
			return err
		}
		c, put, err := a.Conn(r.Context())
		if err != nil {
			return err
		}
		defer put()
		if strings.HasSuffix(p, "/delete") {
			id := ""
			if v, ok := body["id"]; ok && v != nil {
				id = fmt.Sprint(v)
			}
			a.WriteMu.Lock()
			err := atlas.DeleteComparison(c, id)
			a.WriteMu.Unlock()
			if err != nil {
				return err
			}
		} else if _, err := a.SaveComparison(c, params, body["name"]); err != nil {
			return err
		}
		v, err := atlas.Comparisons(c)
		if err != nil {
			return err
		}
		sendJSON(w, v, 200)
		return nil
	}
	if s.Jobs.Running() {
		sendJSON(w, map[string]any{"error": "A background job is already running"}, 409)
		return nil
	}
	switch p {
	case "/api/band-assignment":
		if !(r.ContentLength > 0 && r.ContentLength <= 4096) {
			return py.Errorf("Invalid band assignment size")
		}
		body, err := readJSON(r, 4096)
		if err != nil {
			return err
		}
		c, put, err := a.Conn(r.Context())
		if err != nil {
			return err
		}
		defer put()
		v, err := a.AssignBand(c, body["ssid"], body["mac"], body["band"])
		if err != nil {
			return err
		}
		sendJSON(w, v, 200)
		return nil
	case "/api/import":
		skip := r.Header.Get("X-Skip-Invalid") == "1"
		if local := r.Header.Get("X-App-Path"); local != "" {
			// the app: the CSV is read where it is, whatever its size
			io.Copy(io.Discard, r.Body)
			file := py.Unquote(local)
			if st, err := os.Stat(file); err != nil || st.IsDir() {
				return py.Errorf("No such file: %s", file)
			}
			job, err := s.Jobs.ImportFile(file, filepath.Base(file), skip, false)
			if err != nil {
				return err
			}
			sendJSON(w, job, 202)
			return nil
		}
		if !(r.ContentLength > 0 && r.ContentLength <= 2_000_000_000) {
			return py.Errorf("Choose a CSV between 1 byte and 2 GB")
		}
		name := filepath.Base(py.Unquote(r.Header.Get("X-Filename")))
		if name == "." || name == "" {
			name = "survey.csv"
		}
		tmp := filepath.Join(s.o.Data, "upload-"+newHex()[:10]+".csv")
		if err := saveBody(r, tmp, r.ContentLength); err != nil {
			return err
		}
		job, err := s.Jobs.ImportFile(tmp, name, skip, true)
		if err != nil {
			os.Remove(tmp)
			return err
		}
		sendJSON(w, job, 202)
		return nil
	case "/api/area-estimate":
		c, put, err := a.Conn(r.Context())
		if err != nil {
			return err
		}
		plan, scoped, groups, err := a.AreaPlan(c, params, false, true)
		put()
		if err != nil {
			return err
		}
		if len(groups) == 0 {
			out := s.Jobs.Snapshot()
			out["area_more"] = plan["more"]
			sendJSON(w, out, 200)
			return nil
		}
		job, err := s.Jobs.Estimate(scoped, groups, "area-estimate")
		if err != nil {
			return err
		}
		job["area_more"], job["area_started"] = plan["more"], true
		sendJSON(w, job, 202)
		return nil
	case "/api/estimate":
		if _, err := atlas.ParseFilter(params, false); err != nil { // validate before queuing a long operation
			return err
		}
		job, err := s.Jobs.Estimate(params, nil, "estimate")
		if err != nil {
			return err
		}
		sendJSON(w, job, 202)
		return nil
	}
	return herr(404, "Not found")
}

func saveBody(r *http.Request, dest string, length int64) error {
	os.MkdirAll(filepath.Dir(dest), 0o755)
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r.Body)
	f.Close()
	if err == nil && n < length {
		err = py.Errorf("Upload ended before the whole file arrived (%d of %d bytes)", n, length)
	}
	if err != nil {
		os.Remove(dest)
	}
	return err
}

// downloadStore streams a Graphite-format snapshot of the store (browser use; the app's window
// saves it straight to a chosen file instead).
func (s *Server) downloadStore(w http.ResponseWriter, ctx context.Context) error {
	if s.admin.busy.Load() || !s.Jobs.suspendIdle() {
		return herr(409, "Finish the current operation, then save a snapshot from Admin")
	}
	defer s.Jobs.Resume()
	dir, err := os.MkdirTemp(s.o.Data, "snapshot-")
	if err != nil {
		return err
	}
	defer removeOwnedCandidate(dir, s.o.Data)
	file := filepath.Join(dir, "survey.sqlite3")
	if err := s.Atlas.ExportSQLiteContext(ctx, file, nil, nil); err != nil {
		return err
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="signal-atlas.sqlite3"`)
	w.Header().Set("Content-Length", fmt.Sprint(st.Size()))
	_, err = io.Copy(w, f)
	return err
}

// ---- static files: the compare page and the explorer get the app bridge first thing in <head> ----

var bridgeTag = []byte(`<script src="/app/bridge.js"></script>`)

func (s *Server) serveFS(w http.ResponseWriter, fsys fs.FS, rel string, inject bool) error {
	data, err := fs.ReadFile(fsys, rel)
	if err != nil {
		return herr(404, "Not found")
	}
	ext := strings.ToLower(path.Ext(rel))
	ctype := mime.TypeByExtension(ext)
	switch ext {
	case ".js":
		ctype = "text/javascript; charset=utf-8"
	case ".html":
		ctype = "text/html; charset=utf-8"
		if inject {
			// Keep the Python reference files unchanged while making its fallback
			// map usable without a CDN connection.
			data = bytes.ReplaceAll(data, []byte("https://unpkg.com/leaflet@1.9.4/dist/"), []byte("/app/vendor/leaflet/"))
			if i := bytes.Index(bytes.ToLower(data), []byte("<head>")); i >= 0 {
				tags := bridgeTag
				native := s.o.Host != nil
				for _, origin := range s.o.Origins {
					native = native || origin == "wails://wails"
				}
				if native {
					tags = append([]byte(`<script src="/app/native.js"></script>`), bridgeTag...)
				}
				data = append(append(append([]byte{}, data[:i+6]...), tags...), data[i+6:]...)
			}
		}
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Write(data)
	return nil
}
