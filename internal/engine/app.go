package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"signal-atlas/internal/compare"
	"signal-atlas/internal/py"
)

// ---- Wardrive Compare sessions: one engine per compare page, messages over long-polling ----

type queued struct {
	Seq int64           `json:"seq"`
	Msg json.RawMessage `json:"msg"`
}

type compareSession struct {
	id     string
	e      *compare.Engine
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []queued
	seq    int64
	work   chan *compare.Message
	seen   time.Time
	closed bool
}

type compareSessions struct {
	mu  sync.Mutex
	m   map[string]*compareSession
	tmp string
}

func newCompareSessions(tmp string) *compareSessions {
	cs := &compareSessions{m: map[string]*compareSession{}, tmp: tmp}
	go cs.reap()
	return cs
}

// reap ends sessions whose page stopped asking (closed or navigated away).
func (cs *compareSessions) reap() {
	for range time.Tick(time.Minute) {
		cs.mu.Lock()
		for id, s := range cs.m {
			s.mu.Lock()
			idle := time.Since(s.seen) > 10*time.Minute
			s.mu.Unlock()
			if idle {
				s.close()
				delete(cs.m, id)
			}
		}
		cs.mu.Unlock()
	}
}

func (cs *compareSessions) closeAll() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for id, s := range cs.m {
		s.close()
		delete(cs.m, id)
	}
}

func (cs *compareSessions) create() *compareSession {
	s := &compareSession{id: newHex(), work: make(chan *compare.Message, 64), seen: time.Now()}
	s.cond = sync.NewCond(&s.mu)
	s.e = compare.New(s.push, filepath.Join(cs.tmp, s.id))
	go func() {
		for m := range s.work {
			s.e.Handle(m)
		}
	}()
	cs.mu.Lock()
	cs.m[s.id] = s
	cs.mu.Unlock()
	return s
}

func (cs *compareSessions) get(id string) *compareSession {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.m[id]
}

// push queues a message for the page; an unread progress message is replaced by the next one.
func (s *compareSession) push(m map[string]any) {
	raw, err := json.Marshal(m)
	if err != nil {
		raw, _ = json.Marshal(map[string]any{"type": "error", "msg": err.Error()})
	}
	s.mu.Lock()
	if m["type"] == "progress" && len(s.queue) > 0 {
		var last struct {
			Type string `json:"type"`
		}
		json.Unmarshal(s.queue[len(s.queue)-1].Msg, &last)
		if last.Type == "progress" {
			s.queue = s.queue[:len(s.queue)-1]
		}
	}
	s.seq++
	s.queue = append(s.queue, queued{s.seq, raw})
	s.cond.Broadcast()
	s.mu.Unlock()
}

// next waits (up to a timeout) for messages after seq.
func (s *compareSession) next(after int64, wait time.Duration) []queued {
	deadline := time.Now().Add(wait)
	timer := time.AfterFunc(wait, func() { s.mu.Lock(); s.cond.Broadcast(); s.mu.Unlock() })
	defer timer.Stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = time.Now()
	for {
		// drop what the page has acknowledged
		k := 0
		for k < len(s.queue) && s.queue[k].Seq <= after {
			k++
		}
		s.queue = s.queue[k:]
		if len(s.queue) > 0 || s.closed || time.Now().After(deadline) {
			return append([]queued(nil), s.queue...)
		}
		s.cond.Wait()
	}
}

func (s *compareSession) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.work)
		s.cond.Broadcast()
	}
	s.mu.Unlock()
	go func() { time.Sleep(time.Second); s.e.Close(); os.RemoveAll(s.e.Tmp) }()
}

// ---- files dropped on the window: their paths, which a page never learns ----

type dropped struct {
	path, name string
	size       int64
	at         time.Time
}

type dropRegistry struct {
	mu   sync.Mutex
	cond *sync.Cond
	list []dropped
}

func newDropRegistry() *dropRegistry { d := &dropRegistry{}; d.cond = sync.NewCond(&d.mu); return d }

func (d *dropRegistry) add(paths []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	keep := d.list[:0]
	for _, x := range d.list {
		if now.Sub(x.at) < 10*time.Minute {
			keep = append(keep, x)
		}
	}
	d.list = keep
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			d.list = append(d.list, dropped{p, filepath.Base(p), st.Size(), now})
		}
	}
	d.cond.Broadcast()
}

// find waits briefly for a dropped (or picked) file with this name and size.
func (d *dropRegistry) find(name string, size int64, wait time.Duration) string {
	deadline := time.Now().Add(wait)
	timer := time.AfterFunc(wait, func() { d.mu.Lock(); d.cond.Broadcast(); d.mu.Unlock() })
	defer timer.Stop()
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		for i := len(d.list) - 1; i >= 0; i-- {
			if x := d.list[i]; x.name == name && x.size == size {
				return x.path
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		d.cond.Wait()
	}
}

// ---- routes ----

func (s *Server) appGet(w http.ResponseWriter, r *http.Request, p string, params py.Params) error {
	switch {
	case p == "/api/app/info":
		sendJSON(w, map[string]any{"version": s.o.Version, "data": s.o.Data, "window": s.o.Host != nil}, 200)
		return nil
	case p == "/api/app/path":
		var size int64
		fmt.Sscan(params.First("size", "0"), &size)
		sendJSON(w, map[string]any{"path": s.drops.find(params.First("name", ""), size, 1500*time.Millisecond)}, 200)
		return nil
	case strings.HasPrefix(p, "/api/app/compare/") && strings.HasSuffix(p, "/next"):
		cs := s.cmp.get(strings.TrimSuffix(strings.TrimPrefix(p, "/api/app/compare/"), "/next"))
		if cs == nil {
			return herr(404, "that compare session has ended; reload the page")
		}
		var after int64
		fmt.Sscan(params.First("after", "0"), &after)
		sendGoJSON(w, cs.next(after, 25*time.Second), 200)
		return nil
	}
	return herr(404, "Not found")
}

func (s *Server) appPost(w http.ResponseWriter, r *http.Request, p string, params py.Params) error {
	switch {
	case p == "/api/app/compare/new":
		sendJSON(w, map[string]any{"id": s.cmp.create().id}, 200)
		return nil
	case strings.HasPrefix(p, "/api/app/compare/"):
		rest := strings.TrimPrefix(p, "/api/app/compare/")
		id, action, _ := strings.Cut(rest, "/")
		cs := s.cmp.get(id)
		if cs == nil {
			return herr(404, "that compare session has ended; reload the page")
		}
		switch action {
		case "post":
			raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
			if err != nil {
				return err
			}
			var m compare.Message
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			for i := range m.Files {
				if f := m.Files[i].Path; f != "" {
					if st, err := os.Stat(f); err != nil || st.IsDir() {
						return py.Errorf("No such file: %s", f)
					}
				}
			}
			cs.work <- &m
			sendJSON(w, map[string]any{"ok": true}, 200)
			return nil
		case "upload":
			// a log with no file behind it (made in the page): kept with the session
			name := filepath.Base(params.First("name", "log.csv"))
			dest := filepath.Join(cs.e.Tmp, "upload-"+newHex()[:8]+"-"+sanitizeName(name))
			if err := saveBody(r, dest, r.ContentLength); err != nil {
				return err
			}
			sendJSON(w, map[string]any{"path": dest}, 200)
			return nil
		case "close":
			cs.close()
			s.cmp.mu.Lock()
			delete(s.cmp.m, id)
			s.cmp.mu.Unlock()
			sendJSON(w, map[string]any{"ok": true}, 200)
			return nil
		}
		return herr(404, "Not found")
	case p == "/api/app/pick-files":
		if s.o.Host == nil {
			return py.Errorf("There is no file dialog here.")
		}
		body, err := readJSON(r, 1<<16)
		if err != nil {
			return err
		}
		var exts []string
		if l, ok := body["exts"].([]any); ok {
			for _, x := range l {
				exts = append(exts, fmt.Sprint(x))
			}
		}
		title, _ := body["title"].(string)
		multiple, _ := body["multiple"].(bool)
		paths, err := s.o.Host.PickFiles(title, exts, multiple)
		if err != nil {
			return err
		}
		out := []any{}
		for _, p := range paths {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				out = append(out, map[string]any{"name": filepath.Base(p), "size": st.Size(), "lastModified": st.ModTime().UnixMilli(), "path": p})
			}
		}
		sendJSON(w, out, 200)
		return nil
	case p == "/api/app/save-export":
		body, err := readJSON(r, 1<<16)
		if err != nil {
			return err
		}
		file, _ := body["file"].(string)
		name, _ := body["name"].(string)
		abs, _ := filepath.Abs(file)
		exports, _ := filepath.Abs(filepath.Join(s.o.Data, "exports"))
		if !strings.HasPrefix(strings.ToLower(abs), strings.ToLower(exports)+string(os.PathSeparator)) {
			return py.Errorf("not an export of this app")
		}
		if s.o.Host == nil {
			return py.Errorf("There is no file dialog here.")
		}
		dest, ok, err := s.o.Host.SavePath("Save CSV", name, "csv")
		if err != nil {
			return err
		}
		if !ok {
			os.Remove(abs)
			sendJSON(w, map[string]any{"saved": false}, 200)
			return nil
		}
		if err := moveFile(abs, dest); err != nil {
			return err
		}
		sendJSON(w, map[string]any{"saved": true, "path": dest}, 200)
		return nil
	case p == "/api/app/save-bytes" && r.Method == http.MethodPost:
		// a file a page made itself (the explorer's GeoJSON export), saved where the user picks:
		// the webviews on macOS and Linux don't download files on their own
		if s.o.Host == nil {
			return py.Errorf("There is no file dialog here.")
		}
		name := filepath.Base(r.URL.Query().Get("name"))
		if name == "." || name == string(os.PathSeparator) || name == "" {
			name = "export"
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<30))
		if err != nil {
			return err
		}
		ext := strings.TrimPrefix(filepath.Ext(name), ".")
		if ext == "" {
			ext = "*"
		}
		dest, ok, err := s.o.Host.SavePath("Save "+name, name, ext)
		if err != nil {
			return err
		}
		if !ok {
			sendJSON(w, map[string]any{"saved": false}, 200)
			return nil
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return err
		}
		sendJSON(w, map[string]any{"saved": true, "path": dest}, 200)
		return nil
	case p == "/api/app/save-atlas":
		if s.o.Host == nil {
			return py.Errorf("There is no file dialog here.")
		}
		dest, ok, err := s.o.Host.SavePath("Save the Signal Atlas database", "signal-atlas.sqlite3", "sqlite3")
		if err != nil {
			return err
		}
		if !ok {
			sendJSON(w, map[string]any{"saved": false}, 200)
			return nil
		}
		tmp := dest + ".part"
		os.Remove(tmp)
		if err := s.Atlas.ExportSQLite(tmp, nil); err != nil {
			return err
		}
		os.Remove(dest)
		if err := os.Rename(tmp, dest); err != nil {
			return err
		}
		sendJSON(w, map[string]any{"saved": true, "path": dest}, 200)
		return nil
	case p == "/api/app/open-url":
		body, err := readJSON(r, 1<<16)
		if err != nil {
			return err
		}
		u, _ := body["url"].(string)
		if !(strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "mailto:")) {
			return py.Errorf("not a web link")
		}
		if s.OpenURL != nil {
			s.OpenURL(u)
		}
		sendJSON(w, map[string]any{"ok": true}, 200)
		return nil
	case p == "/api/app/show-data":
		if s.o.Host != nil {
			s.o.Host.ShowFolder(s.o.Data)
		}
		sendJSON(w, map[string]any{"ok": true}, 200)
		return nil
	}
	return herr(404, "Not found")
}

func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, s)
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	in.Close()
	return os.Remove(src)
}
