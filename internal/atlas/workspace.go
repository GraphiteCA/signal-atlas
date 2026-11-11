package atlas

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"signal-atlas/internal/db"
	"signal-atlas/internal/geo"
	"signal-atlas/internal/obs"
)

const WorkspaceFormat = "signal-atlas-workspace"
const pointerFile = "workspace.json"

type WorkspacePointer struct {
	Format   string `json:"format"`
	Version  int    `json:"version"`
	Active   string `json:"active"`
	Previous string `json:"previous,omitempty"`
}

func ReadWorkspacePointer(root string) (WorkspacePointer, error) {
	p := WorkspacePointer{Format: WorkspaceFormat, Version: 1, Active: "."}
	b, err := os.ReadFile(filepath.Join(root, pointerFile))
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	if p.Format != WorkspaceFormat || p.Version != 1 || p.Active == "" {
		return p, fmt.Errorf("Unsupported survey workspace")
	}
	return p, nil
}
func workspaceChild(root, relative string) (string, error) {
	if filepath.IsAbs(relative) || relative == "" {
		return "", fmt.Errorf("Invalid workspace generation")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, relative)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("Generation escapes workspace")
	}
	if actual, err := filepath.EvalSymlinks(path); err == nil {
		base, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", err
		}
		r, err := filepath.Rel(base, actual)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
			return "", fmt.Errorf("Generation symlink escapes workspace")
		}
	}
	return path, nil
}
func WorkspaceDir(root string) (string, error) {
	p, err := ReadWorkspacePointer(root)
	if err != nil {
		return "", err
	}
	return workspaceChild(root, p.Active)
}
func IsWorkspace(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".incomplete")); err == nil {
		return false
	}
	dir, err := WorkspaceDir(root)
	if err != nil {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, "atlas.sqlite3"))
	if err == nil && !st.IsDir() {
		return true
	}
	p, err := ReadWorkspacePointer(root)
	if err != nil || p.Previous == "" {
		return false
	}
	previous, err := workspaceChild(root, p.Previous)
	if err != nil {
		return false
	}
	st, err = os.Stat(filepath.Join(previous, "atlas.sqlite3"))
	return err == nil && !st.IsDir()
}

// PublishWorkspace is the only cross-file commit point. A complete generation
// must already exist. The previous generation remains available for recovery.
func PublishWorkspace(root, active, previous string) error {
	path, err := workspaceChild(root, active)
	if err != nil {
		return err
	}
	if st, err := os.Stat(filepath.Join(path, "atlas.sqlite3")); err != nil || st.IsDir() {
		return fmt.Errorf("Candidate generation is incomplete")
	}
	if previous != "" {
		if _, err := workspaceChild(root, previous); err != nil {
			return err
		}
	}
	p := WorkspacePointer{WorkspaceFormat, 1, active, previous}
	return WriteJSONAtomic(filepath.Join(root, pointerFile), p)
}
func WriteJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".commit-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func copyFile(ctx context.Context, src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		n, e := in.Read(buf)
		if n > 0 {
			_, err = out.Write(buf[:n])
			if err != nil {
				break
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			err = e
			break
		}
	}
	if err == nil {
		err = out.Sync()
	}
	e := out.Close()
	if err == nil {
		err = e
	}
	return err
}

// CopyWorkspace requires a quiescent writer. It captures complete metadata and
// column files, rather than exporting the empty metadata observations table.
// Immutable segments may be hardlinked; the append-only dictionary is copied.
func (a *Atlas) CopyWorkspace(ctx context.Context, dest string, progress func(string)) error {
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Mkdir(dest, 0755); err != nil {
		return err
	}
	if progress != nil {
		progress("Copying committed metadata")
	}
	c, put, err := a.Conn(ctx)
	if err != nil {
		return err
	}
	_, err = db.Exec(c, "VACUUM INTO ?", filepath.Join(dest, "atlas.sqlite3"))
	put()
	if err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(dest, "observations"), 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(a.Dir, "observations"))
	if err != nil {
		return err
	}
	if progress != nil {
		progress("Copying observation segments")
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		if e.IsDir() || !(name == "manifest.json" || name == "strings.bin" || strings.HasPrefix(name, "seg-") && strings.HasSuffix(name, ".bin")) {
			continue
		}
		src := filepath.Join(a.Dir, "observations", name)
		dst := filepath.Join(dest, "observations", name)
		if strings.HasPrefix(name, "seg-") {
			if err := os.Link(src, dst); err == nil {
				continue
			}
		}
		if err := copyFile(ctx, src, dst); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// ForkWorkspace opens metadata from a completed CopyWorkspace without loading
// a second copy of all observation columns. The source stays quiescent until
// the fork is closed, or until it replaces the source during cleanup.
func (a *Atlas) ForkWorkspace(dest string) (*Atlas, error) {
	return openGenerationWith(dest, dest, a.Obs.ForkTo(filepath.Join(dest, "observations")))
}

type ZeroPreview struct {
	Rows     int64 `json:"rows"`
	Imports  int   `json:"imports"`
	Networks int   `json:"networks"`
}

func (a *Atlas) ZeroPreview(ctx context.Context) (ZeroPreview, error) {
	var out ZeroPreview
	imports := map[int64]bool{}
	nets := map[string]bool{}
	st := a.Obs.State()
	for _, s := range st.Segs {
		for i := 0; i < s.N; i++ {
			if i&4095 == 0 && ctx.Err() != nil {
				return out, ctx.Err()
			}
			if geo.ZeroPair(s.Lat[i], s.Lon[i]) {
				out.Rows++
				imports[s.ImportID] = true
				k := st.Str[s.SSID[i]]
				if k == "" {
					k = "\x00" + st.Str[s.MAC[i]]
				}
				nets[k] = true
			}
		}
	}
	out.Imports = len(imports)
	out.Networks = len(nets)
	return out, nil
}

// PrepareCleanup modifies only a newly created candidate directory. Until the
// caller publishes it, a crash/failure/cancel leaves the old generation intact.
func (a *Atlas) PrepareCleanup(ctx context.Context, dest string, progress func(int64, int64, string)) (*Atlas, map[string]any, error) {
	say := func(stage string) {
		if progress != nil {
			progress(0, int64(a.Obs.State().N), stage)
		}
	}
	if err := a.CopyWorkspace(ctx, dest, say); err != nil {
		return nil, nil, err
	}
	candidate, err := a.ForkWorkspace(dest)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*Atlas, map[string]any, error) { candidate.Close(); return nil, nil, err }
	st := candidate.Obs.State()
	segs := make([]*obs.Segment, 0, len(st.Segs))
	counts := map[int64]int64{}
	names, hidden := map[string]bool{}, map[string]bool{}
	var removed, done int64
	for _, s := range st.Segs {
		start := 0
		changed := false
		for i := 0; i < s.N; i++ {
			if i&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return fail(err)
				}
				if progress != nil {
					progress(done+int64(i), int64(st.N), "Removing 0,0 rows in candidate")
				}
			}
			if !geo.ZeroPair(s.Lat[i], s.Lon[i]) {
				continue
			}
			changed = true
			if start < i {
				segs = append(segs, s.Range(start, i))
			}
			start = i + 1
			counts[s.ImportID]++
			removed++
			ssid := st.Str[s.SSID[i]]
			if ssid == "" {
				hidden[st.Str[s.MAC[i]]] = true
			} else {
				names[ssid] = true
			}
		}
		if !changed {
			segs = append(segs, s)
		} else if start < s.N {
			segs = append(segs, s.Range(start, s.N))
		}
		done += int64(s.N)
	}
	c, put, err := candidate.Conn(ctx)
	if err != nil {
		return fail(err)
	}
	err = db.WriteTx(c, func() error {
		for id, n := range counts {
			if _, err := db.Exec(c, "UPDATE imports SET active_rows=active_rows-?,zero_removed_rows=zero_removed_rows+? WHERE id=?", n, n, id); err != nil {
				return err
			}
		}
		if err := dropChangedFits(c, names, hidden); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		say("Writing cleaned observation generation")
		return candidate.Obs.Commit(obs.Change{Segments: segs})
	})
	put()
	if err != nil {
		return fail(err)
	}
	if err := candidate.Validate(ctx); err != nil {
		return fail(err)
	}
	return candidate, map[string]any{"removed_rows": removed, "affected_imports": len(counts), "remaining_rows": candidate.Obs.State().N}, nil
}

// Validate checks actual column rows, provenance counts and metadata integrity.
func (a *Atlas) Validate(ctx context.Context) error {
	c, put, err := a.Conn(ctx)
	if err != nil {
		return err
	}
	defer put()
	v, err := db.Scalar(c, "PRAGMA quick_check")
	if err != nil {
		return err
	}
	if db.Str(v) != "ok" {
		return fmt.Errorf("Metadata check: %v", v)
	}
	counts := map[int64]int64{}
	last := int64(0)
	haveLast := false
	st := a.Obs.State()
	for _, s := range st.Segs {
		if s.N == 0 {
			continue
		}
		if haveLast && s.Base <= last {
			return fmt.Errorf("Observation IDs overlap")
		}
		last = s.Base + int64(s.N) - 1
		haveLast = true
		for i := 0; i < s.N; i++ {
			if i&4095 == 0 && ctx.Err() != nil {
				return ctx.Err()
			}
			if math.IsNaN(s.Lat[i]) || math.IsNaN(s.Lon[i]) || math.IsInf(s.Lat[i], 0) || math.IsInf(s.Lon[i], 0) || math.Abs(s.Lat[i]) > 90 || math.Abs(s.Lon[i]) > 180 {
				return fmt.Errorf("Invalid coordinate at observation %d", s.Base+int64(i))
			}
			counts[s.ImportID]++
		}
	}
	rows, err := db.All(c, "SELECT id,active_rows FROM imports")
	if err != nil {
		return err
	}
	for _, r := range rows {
		id := db.Int(r["id"])
		if db.Int(r["active_rows"]) != counts[id] {
			return fmt.Errorf("Import %d active count differs from stored observations", id)
		}
		delete(counts, id)
	}
	if len(counts) != 0 {
		return fmt.Errorf("Observations have missing imports")
	}
	return nil
}
