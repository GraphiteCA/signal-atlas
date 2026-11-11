package engine

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/gpu"
	"signal-atlas/internal/py"
)

// ---- settings kept in the data folder, and checking GitHub for a newer release ----

// Settings are the app's own preferences.
type Settings struct {
	LastSurvey    string   `json:"last_survey,omitempty"`
	RecentSurveys []string `json:"recent_surveys,omitempty"`
	// FastFits fits with the fast solver (atlas.FitFast) instead of the exact grid search.
	FastFits bool `json:"fast_fits"`
	// GPUFits is opt-in, versioned float64 OpenCL scoring with CPU fallback.
	GPUFits bool `json:"gpu_fits"`
	// UpdateChecks is "ask" (until the user answers), "on" (once a day) or "off".
	UpdateChecks string `json:"update_checks"`
}

type settingsStore struct {
	mu     sync.Mutex
	path   string
	s      Settings
	update map[string]any // the last check's answer
	client *http.Client
}

func newSettings(dir string) *settingsStore {
	st := &settingsStore{path: filepath.Join(dir, "settings.json"), s: Settings{UpdateChecks: "ask"}, client: &http.Client{Timeout: 30 * time.Second}}
	if b, err := os.ReadFile(st.path); err == nil {
		json.Unmarshal(b, &st.s)
	}
	if st.s.UpdateChecks != "on" && st.s.UpdateChecks != "off" {
		st.s.UpdateChecks = "ask"
	}
	return st
}

func (st *settingsStore) get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

func (st *settingsStore) set(change func(*Settings)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := st.s
	change(&next)
	if err := atlas.WriteJSONAtomic(st.path, next); err != nil {
		return err
	}
	st.s = next
	return nil
}

// releasesURL replaces GitHub's address in tests.
var releasesURL = ""

// releasesAPI is GitHub's answer for the project's newest release (derived from ProjectURL).
func releasesAPI() string {
	if releasesURL != "" {
		return releasesURL
	}
	return "https://api.github.com/repos/" + strings.TrimPrefix(strings.TrimSuffix(ProjectURL, "/"), "https://github.com/") + "/releases/latest"
}

// newer is whether version a (1.2.3) is newer than b.
func newer(a, b string) bool {
	parse := func(v string) [3]int {
		var out [3]int
		v = strings.TrimPrefix(strings.SplitN(v, "-", 2)[0], "v")
		for i, p := range strings.SplitN(v, ".", 3) {
			if i < 3 {
				out[i], _ = strconv.Atoi(p)
			}
		}
		return out
	}
	x, y := parse(a), parse(b)
	for i := 0; i < 3; i++ {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// checkUpdate asks GitHub for the newest release. It sends the app's version as its user agent
// and nothing else.
func (s *Server) checkUpdate(ctx context.Context) (map[string]any, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", releasesAPI(), nil)
	req.Header.Set("User-Agent", "SignalAtlas/"+s.o.Version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.set.client.Do(req)
	if err != nil {
		return nil, py.Errorf("Couldn't reach GitHub: %s", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, py.Errorf("GitHub answered %d", resp.StatusCode)
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Name   string `json:"name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	out := map[string]any{"current": s.o.Version, "latest": latest, "url": rel.URL, "checked": time.Now().UTC().Format(time.RFC3339),
		"newer": s.o.Version != "dev" && newer(latest, s.o.Version), "installer": false}
	for _, a := range rel.Assets {
		switch {
		case strings.HasSuffix(a.Name, "-windows-amd64-setup.exe"):
			out["installer_url"], out["installer_name"], out["installer_size"] = a.URL, a.Name, a.Size
		case a.Name == "SHA256SUMS.txt":
			out["sums_url"] = a.URL
		}
	}
	// the app installs an update itself only on Windows, from the release's installer, checked
	// against the release's checksums; elsewhere it points at the release page
	out["installer"] = runtime.GOOS == "windows" && s.o.Host != nil && out["installer_url"] != nil && out["sums_url"] != nil
	s.set.mu.Lock()
	s.set.update = out
	s.set.mu.Unlock()
	return out, nil
}

// autoCheck checks once soon after start and then daily, while the user has said yes.
func (s *Server) autoCheck() {
	time.Sleep(5 * time.Second)
	for {
		if s.set.get().UpdateChecks == "on" && s.o.Version != "dev" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.checkUpdate(ctx)
			cancel()
		}
		time.Sleep(24 * time.Hour)
	}
}

func (s *Server) updateState() map[string]any {
	s.set.mu.Lock()
	defer s.set.mu.Unlock()
	out := map[string]any{"current": s.o.Version, "setting": s.set.s.UpdateChecks}
	for k, v := range s.set.update {
		out[k] = v
	}
	return out
}

// download fetches url to path (at most limit bytes).
func (s *Server) download(ctx context.Context, url, path string, limit int64) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("User-Agent", "SignalAtlas/"+s.o.Version)
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("the download answered %d", resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, io.LimitReader(resp.Body, limit))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// installUpdate downloads the release's Windows installer, checks it against the release's
// SHA256SUMS.txt, starts it (it installs quietly and opens the new version), and quits.
func (s *Server) installUpdate(ctx context.Context) error {
	u := s.updateState()
	if u["installer"] != true || u["newer"] != true {
		return py.Errorf("There is no newer installer to install; check for updates first")
	}
	dir := filepath.Join(s.o.Data, "updates")
	os.MkdirAll(dir, 0o755)
	sums := filepath.Join(dir, "SHA256SUMS.txt")
	if err := s.download(ctx, u["sums_url"].(string), sums, 1<<20); err != nil {
		return err
	}
	name := filepath.Base(u["installer_name"].(string))
	want := ""
	if f, err := os.Open(sums); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if parts := strings.Fields(sc.Text()); len(parts) == 2 && strings.TrimPrefix(parts[1], "*") == name {
				want = strings.ToLower(parts[0])
			}
		}
		f.Close()
	}
	if want == "" {
		return py.Errorf("The release's checksums don't list %s", name)
	}
	path := filepath.Join(dir, name)
	if err := s.download(ctx, u["installer_url"].(string), path+".part", 512<<20); err != nil {
		return err
	}
	f, err := os.Open(path + ".part")
	if err != nil {
		return err
	}
	h := sha256.New()
	io.Copy(h, f)
	f.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(path + ".part")
		return py.Errorf("The downloaded installer doesn't match the release's checksum; nothing was installed")
	}
	os.Remove(path)
	if err := os.Rename(path+".part", path); err != nil {
		return err
	}
	if err := exec.Command(path, "/S").Start(); err != nil {
		return err
	}
	if q, ok := s.o.Host.(interface{ Quit() }); ok {
		go func() { time.Sleep(700 * time.Millisecond); q.Quit() }()
	}
	return nil
}

// settingsPost answers /api/app/settings and /api/app/update/*.
func (s *Server) settingsPost(w http.ResponseWriter, r *http.Request, p string) error {
	body, err := readJSON(r, 1<<16)
	if err != nil {
		return err
	}
	switch p {
	case "/api/app/settings":
		previous := s.set.get()
		err = s.set.set(func(st *Settings) {
			if v, ok := body["fast_fits"].(bool); ok {
				st.FastFits = v
			}
			if v, ok := body["gpu_fits"].(bool); ok {
				st.GPUFits = v
			}
			if v, ok := body["update_checks"].(string); ok && (v == "on" || v == "off" || v == "ask") {
				st.UpdateChecks = v
			}
		})
		if err != nil {
			return err
		}
		next := s.set.get()
		if next.GPUFits {
			gpu.Probe()
		}
		if (next.FastFits != previous.FastFits || next.GPUFits != previous.GPUFits) && s.events != nil {
			solver := atlas.SolverName(next.FastFits, next.GPUFits)
			s.surveyMu.RLock()
			sid := s.surveyID
			s.surveyMu.RUnlock()
			s.events.publish(JobEvent{Kind: "solver-changed", SurveyID: sid, JobID: "settings:" + newHex(), Solver: solver, Reconcile: true})
		}
		if s.set.get().UpdateChecks == "on" {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				s.checkUpdate(ctx)
			}()
		}
		sendJSON(w, map[string]any{"settings": s.set.get(), "update": s.updateState(), "acceleration": gpu.Status()}, 200)
		return nil
	case "/api/app/update/check":
		if _, err := s.checkUpdate(r.Context()); err != nil {
			return err
		}
		sendJSON(w, s.updateState(), 200)
		return nil
	case "/api/app/update/install":
		if err := s.installUpdate(r.Context()); err != nil {
			return err
		}
		sendJSON(w, map[string]any{"installing": true}, 200)
		return nil
	}
	return herr(404, "Not found")
}
