package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"signal-atlas/internal/drivedb"
	"signal-atlas/internal/py"
)

// ---- the Drive database: one SQLite file on this computer that collects every log and database
// added to it (internal/drivedb), with its own background job so it never waits on the explorer ----

type drives struct {
	done      chan struct{}
	closed    bool
	s         *Server
	mu        sync.Mutex // the open database and the job state
	d         *drivedb.DB
	job       map[string]any
	running   atomic.Bool
	cancelled atomic.Bool
}

func (s *Server) drivesFile() string { return filepath.Join(s.o.Data, "drive-database.json") }

func newDrives(s *Server) *drives {
	v := &drives{s: s, job: map[string]any{"status": "idle"}}
	// reopen the database used last time
	if b, err := os.ReadFile(s.drivesFile()); err == nil {
		var saved struct{ Path string }
		if json.Unmarshal(b, &saved) == nil && saved.Path != "" {
			if d, err := drivedb.Open(saved.Path); err == nil {
				v.d = d
			}
		}
	}
	return v
}

func (v *drives) remember() {
	v.mu.Lock()
	defer v.mu.Unlock()
	path := ""
	if v.d != nil {
		path = v.d.Path
	}
	b, _ := json.Marshal(map[string]string{"Path": path})
	os.WriteFile(v.s.drivesFile(), b, 0o644)
}

func (v *drives) close() {
	v.mu.Lock()
	v.closed = true
	v.cancelled.Store(true)
	done := v.done
	v.mu.Unlock()
	if done != nil {
		<-done
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.d != nil {
		v.d.Close()
		v.d = nil
	}
}

func (v *drives) status() map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	job := map[string]any{}
	for k, x := range v.job {
		job[k] = x
	}
	out := map[string]any{"job": job, "dialogs": v.s.o.Host != nil}
	if v.d != nil && !v.running.Load() {
		info, err := v.d.Info()
		if err != nil {
			out["error"] = err.Error()
		}
		out["database"] = info
	} else if v.d != nil {
		out["database"] = map[string]any{"path": v.d.Path, "name": filepath.Base(v.d.Path), "busy": true}
	}
	return out
}

func (v *drives) update(kv map[string]any) {
	v.mu.Lock()
	for k, x := range kv {
		v.job[k] = x
	}
	v.mu.Unlock()
}

// start runs work as the Drive database's one job.
func (v *drives) start(kind string, work func() (any, error)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return py.Errorf("The Drive database is closing")
	}
	if !v.running.CompareAndSwap(false, true) {
		return py.Errorf("The Drive database is busy; wait for it or cancel")
	}
	v.cancelled.Store(false)
	v.job = map[string]any{"status": "running", "kind": kind}
	done := make(chan struct{})
	v.done = done
	go func() {
		defer close(done)
		r, err := work()
		v.mu.Lock()
		switch {
		case err != nil && (errors.Is(err, drivedb.ErrCancelled) || v.cancelled.Load()):
			v.job["status"], v.job["error"] = "cancelled", "Cancelled; nothing of the unfinished file was kept"
		case err != nil:
			v.job["status"], v.job["error"] = "failed", err.Error()
		default:
			v.job["status"] = "complete"
		}
		v.job["result"] = r
		v.mu.Unlock()
		v.running.Store(false)
	}()
	return nil
}

func (v *drives) need() (*drivedb.DB, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.d == nil {
		return nil, py.Errorf("Open or create a Drive database first")
	}
	return v.d, nil
}

func (v *drives) set(d *drivedb.DB) {
	v.mu.Lock()
	if v.closed {
		if d != nil {
			d.Close()
		}
		v.mu.Unlock()
		return
	}
	if v.d != nil {
		v.d.Close()
	}
	v.d = d
	v.mu.Unlock()
	v.remember()
}

func strList(x any) []string {
	var out []string
	if l, ok := x.([]any); ok {
		for _, e := range l {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// post answers /api/app/drives/<what>.
func (v *drives) post(w http.ResponseWriter, r *http.Request, what string) error {
	body, err := readJSON(r, 1<<20)
	if err != nil {
		return err
	}
	host := v.s.o.Host
	switch what {
	case "cancel":
		v.cancelled.Store(true)
	case "new", "open":
		if v.running.Load() {
			return py.Errorf("The Drive database is busy; wait for it or cancel")
		}
		path, _ := body["path"].(string)
		if path == "" {
			if host == nil {
				return py.Errorf("There is no file dialog here; give a path")
			}
			if what == "new" {
				p, ok, err := host.SavePath("New Drive database", "drives.sqlite3", "sqlite3")
				if err != nil || !ok {
					sendJSON(w, v.status(), 200)
					return err
				}
				path = p
			} else {
				l, err := host.PickFiles("Open a Drive database", []string{"sqlite3", "sqlite", "db"}, false)
				if err != nil || len(l) == 0 {
					sendJSON(w, v.status(), 200)
					return err
				}
				path = l[0]
			}
		}
		var d *drivedb.DB
		if what == "new" {
			d, err = drivedb.Create(path)
		} else {
			d, err = drivedb.Open(path)
		}
		if err != nil {
			return err
		}
		v.set(d)
	case "close":
		if v.running.Load() {
			return py.Errorf("The Drive database is busy; wait for it or cancel")
		}
		v.set(nil)
	case "add":
		d, err := v.need()
		if err != nil {
			return err
		}
		paths := strList(body["paths"])
		if len(paths) == 0 {
			if host == nil {
				return py.Errorf("There is no file dialog here; give paths")
			}
			paths, err = host.PickFiles("Add logs and databases", []string{"csv", "sqlite3", "sqlite", "db"}, true)
			if err != nil || len(paths) == 0 {
				sendJSON(w, v.status(), 200)
				return err
			}
		}
		err = v.start("add", func() (any, error) {
			v.update(map[string]any{"files": len(paths), "file_index": 0})
			var results []drivedb.Result
			for i, p := range paths {
				if v.cancelled.Load() {
					return results, drivedb.ErrCancelled
				}
				v.update(map[string]any{"file_index": i + 1, "file": filepath.Base(p), "rows": 0, "done": 0, "total": 0})
				res, err := d.Add(p, func(file string, rows, done, total int64) {
					v.update(map[string]any{"rows": rows, "done": done, "total": total})
				}, v.cancelled.Load)
				if errors.Is(err, drivedb.ErrCancelled) {
					return results, err
				}
				if err != nil {
					res.Error = err.Error()
				}
				results = append(results, res)
				v.update(map[string]any{"results": results})
			}
			return results, nil
		})
		if err != nil {
			return err
		}
	case "export":
		d, err := v.need()
		if err != nil {
			return err
		}
		mode, _ := body["mode"].(string)
		dest, _ := body["path"].(string)
		base := strings.TrimSuffix(filepath.Base(d.Path), filepath.Ext(d.Path))
		if dest == "" {
			if host == nil {
				return py.Errorf("There is no file dialog here; give a path")
			}
			var ok bool
			switch mode {
			case "days":
				dest, err = host.PickFolder("Choose a folder for the day files")
				ok = dest != ""
			case "copy":
				dest, ok, err = host.SavePath("Save a copy of the database", base+"-copy.sqlite3", "sqlite3")
			default:
				dest, ok, err = host.SavePath("Export every sighting as WiGLE CSV", base+".csv", "csv")
			}
			if err != nil || !ok {
				sendJSON(w, v.status(), 200)
				return err
			}
		}
		version := v.s.o.Version
		err = v.start("export", func() (any, error) {
			v.update(map[string]any{"mode": mode, "dest": dest})
			progress := func(done, total int64) { v.update(map[string]any{"done": done, "total": total, "rows": done}) }
			switch mode {
			case "days":
				files, err := d.ExportDays(dest, version, progress, v.cancelled.Load)
				return map[string]any{"files": len(files), "folder": dest}, err
			case "copy":
				return map[string]any{"path": dest}, d.ExportCopy(dest)
			}
			n, err := d.ExportCSV(dest, version, progress, v.cancelled.Load)
			return map[string]any{"rows": n, "path": dest}, err
		})
		if err != nil {
			return err
		}
	case "show":
		if host != nil {
			if d, err := v.need(); err == nil {
				host.ShowFolder(filepath.Dir(d.Path))
			}
		}
	default:
		return herr(404, "Not found")
	}
	sendJSON(w, v.status(), 200)
	return nil
}
