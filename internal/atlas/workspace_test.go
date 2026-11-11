package atlas

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"
)

func TestZeroPairImportCountsAndEmptyHistory(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "mixed.csv")
	writeFile(t, file, "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n"+
		"02:00:00:00:00:01,Zero,,2412,-60,0,0,3,WIFI\n"+
		"02:00:00:00:00:01,Zero,,2412,, -0.0,0e2,3,WIFI\n"+
		"02:00:00:00:00:02,Equator,,2412,-60,0,10,3,WIFI\n"+
		"02:00:00:00:00:03,Meridian,,2412,-60,10,-0,3,WIFI\n"+
		"02:00:00:00:00:04,Invalid,,2412,-60,,10,3,WIFI\n"+
		"02:00:00:00:00:04,Invalid,,2412,-60,NaN,10,3,WIFI\n"+
		"02:00:00:00:00:05,Other,,2412,-60,0,0,3,BLE\n")
	if _, err := a.ImportCSV(file, false, nil, "", nil); err == nil {
		t.Fatal("invalid coordinates must still fail strict import")
	}
	if a.Obs.State().N != 0 || len(table(t, a, "SELECT * FROM imports")) != 0 {
		t.Fatal("failed import changed store")
	}
	r, err := a.ImportCSV(file, true, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int64{"valid_rows": 2, "invalid_rows": 2, "non_wifi_rows": 1, "zero_excluded_rows": 2} {
		if r[key] != want {
			t.Fatalf("%s: %v", key, r)
		}
	}
	if a.Obs.State().N != 2 {
		t.Fatal("single zeros were dropped")
	}
	empty := filepath.Join(dir, "zero.csv")
	writeFile(t, empty, "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n02:00:00:00:00:01,Zero,,2412,-60,-0,0,3,WIFI\n")
	r, err = a.ImportCSV(empty, false, nil, "", nil)
	if err != nil || r["empty"] != true || r["valid_rows"] != int64(0) || r["zero_excluded_rows"] != int64(1) {
		t.Fatalf("all excluded: %v %v", r, err)
	}
	root := a.Root
	a.Close()
	a, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if len(table(t, a, "SELECT * FROM imports")) != 2 {
		t.Fatal("zero-row import history was lost on reopen")
	}
	r, err = a.ImportCSV(empty, false, nil, "", nil)
	if err != nil || r["duplicate"] != true {
		t.Fatal("all-excluded file deduplication")
	}
}

// Legacy fixtures intentionally bypass the new import policy. Real inputs are
// never edited; only newly created test segments are changed here.
func dirtyWorkspace(t *testing.T) (*Atlas, map[int64]Row) {
	t.Helper()
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "survey.atlas"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "legacy.csv")
	writeFile(t, file, surveyCSV([]string{"Touched", "Untouched", ""}, 0))
	if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	st := a.Obs.State()
	replace := map[*obs.Segment]*obs.Segment{}
	for _, s := range st.Segs {
		c := s.Range(0, s.N)
		c.Lat = append([]float64(nil), s.Lat...)
		c.Lon = append([]float64(nil), s.Lon...)
		c.Lat[0], c.Lon[0] = 0, 0
		c.Lat[16], c.Lon[16] = 0, 0
		c.Lat[1] = 0
		c.Lon[2] = 0
		replace[s] = c
	}
	if err := a.Obs.Commit(obs.Change{Replace: replace}); err != nil {
		t.Fatal(err)
	}
	c, put, _ := a.Conn(nil)
	groups, _ := a.SelectedGroups(py.Params{})
	for _, mode := range []string{"sample", "all"} {
		for _, solver := range []string{"reference", "fast"} {
			_, key, _ := EstimateScope(py.Params{"fit_mode": {mode}, "solver": {solver}})
			for _, g := range groups {
				result := map[string]any{}
				for k, v := range g {
					result[k] = v
				}
				result["status"] = "fitted"
				result["latitude"] = 48.4
				result["longitude"] = -123.4
				raw, _ := json.Marshal(result)
				if _, err := db.Exec(c, "INSERT INTO estimates VALUES(?,?,?)", g["radio_key"], key, string(raw)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if _, err := db.Exec(c, "INSERT INTO comparisons VALUES('pin','Historical pin','2026-11-10','{\"immutable\":true}')"); err != nil {
		t.Fatal(err)
	}
	put()
	before := map[int64]Row{}
	for _, r := range allRows(a) {
		before[db.Int(r["id"])] = r
	}
	return a, before
}

func TestStagedCleanupPreservesIDsFitsHistoryAndRecovery(t *testing.T) {
	a, before := dirtyWorkspace(t)
	root := a.Root
	defer func() {
		if a != nil {
			a.Close()
		}
	}()
	oldManifest, err := os.ReadFile(filepath.Join(a.Dir, "observations", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := a.ZeroPreview(context.Background())
	if err != nil || preview.Rows != 2 {
		t.Fatalf("preview %+v %v", preview, err)
	}
	untouched := table(t, a, "SELECT * FROM estimates WHERE json_extract(result,'$.ssid')='Untouched' ORDER BY scope_key")
	pins := table(t, a, "SELECT * FROM comparisons")
	parent := filepath.Join(root, "generations")
	os.Mkdir(parent, 0755)
	stage := filepath.Join(parent, "g-clean")
	candidate, result, err := a.PrepareCleanup(context.Background(), stage, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	if result["removed_rows"] != int64(2) || candidate.Obs.State().N != 22 || a.Obs.State().N != 24 {
		t.Fatalf("staged count %v", result)
	}
	for _, r := range allRows(candidate) {
		id := db.Int(r["id"])
		if !reflect.DeepEqual(r, before[id]) {
			t.Fatalf("surviving record %d changed", id)
		}
	}
	if !reflect.DeepEqual(untouched, table(t, candidate, "SELECT * FROM estimates WHERE json_extract(result,'$.ssid')='Untouched' ORDER BY scope_key")) {
		t.Fatal("untouched fits changed")
	}
	if len(table(t, candidate, "SELECT * FROM estimates")) != 4 {
		t.Fatal("affected estimates survived across solver/mode scopes")
	}
	if !reflect.DeepEqual(pins, table(t, candidate, "SELECT * FROM comparisons")) {
		t.Fatal("historical comparison changed")
	}
	history := table(t, candidate, "SELECT valid_rows,active_rows,zero_removed_rows FROM imports")[0]
	if db.Int(history["valid_rows"]) != 24 || db.Int(history["active_rows"]) != 22 || db.Int(history["zero_removed_rows"]) != 2 {
		t.Fatal(history)
	}
	manifest, _ := os.ReadFile(filepath.Join(a.Dir, "observations", "manifest.json"))
	if !reflect.DeepEqual(oldManifest, manifest) {
		t.Fatal("live segments/manifest edited in place")
	}
	// A restart before handoff still sees the old, complete generation.
	a.Close()
	a = nil
	old, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if old.Obs.State().N != 24 {
		t.Fatal("unpublished generation became active")
	}
	old.Close()
	if err := PublishWorkspace(root, filepath.Join("generations", "g-clean"), "."); err != nil {
		t.Fatal(err)
	}
	// A restart after the one-file handoff sees the new, complete generation.
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	again, _ := reopened.ZeroPreview(context.Background())
	if again.Rows != 0 || reopened.Obs.State().N != 22 {
		t.Fatal("cleanup not durable/idempotent")
	}
	recovery, err := OpenGeneration(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()
	if recovery.Obs.State().N != 24 {
		t.Fatal("previous revision unavailable")
	}
	portable := filepath.Join(t.TempDir(), "clean.sqlite3")
	if err := reopened.ExportSQLite(portable, nil); err != nil {
		t.Fatal(err)
	}
	c, err := db.OpenReadOnly(portable)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	n, _ := db.Scalar(c, "SELECT count(*) FROM observations")
	if db.Int(n) != 22 {
		t.Fatal("portable export omitted observations")
	}
}

func TestCleanupCancelAndAllZeroHistory(t *testing.T) {
	a, _ := dirtyWorkspace(t)
	defer a.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stage := filepath.Join(t.TempDir(), "cancelled")
	if _, _, err := a.PrepareCleanup(ctx, stage, func(_, _ int64, phase string) {
		if phase == "Removing 0,0 rows in candidate" {
			cancel()
		}
	}); err == nil {
		t.Fatal("cancelled cleanup succeeded")
	}
	if a.Obs.State().N != 24 {
		t.Fatal("cancel changed live observations")
	}
	// Make every row a legacy zero, then clean to an empty but attributable survey.
	replace := map[*obs.Segment]*obs.Segment{}
	for _, s := range a.Obs.State().Segs {
		c := s.Range(0, s.N)
		c.Lat = make([]float64, s.N)
		c.Lon = make([]float64, s.N)
		replace[s] = c
	}
	if err := a.Obs.Commit(obs.Change{Replace: replace}); err != nil {
		t.Fatal(err)
	}
	candidate, _, err := a.PrepareCleanup(context.Background(), filepath.Join(t.TempDir(), "empty"), nil)
	if err != nil {
		t.Fatal(err)
	}
	path := candidate.Root
	candidate.Close()
	empty, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if empty.Obs.State().N != 0 || len(table(t, empty, "SELECT * FROM imports")) != 1 {
		t.Fatal("all-zero cleanup lost history")
	}
}

func TestLegacySQLiteConversionFiltersZerosAndRetainsIDs(t *testing.T) {
	a, _ := dirtyWorkspace(t)
	defer a.Close()
	source := filepath.Join(t.TempDir(), "legacy.sqlite3")
	if err := a.ExportSQLite(source, nil); err != nil {
		t.Fatal(err)
	}
	b, err := Open(filepath.Join(t.TempDir(), "converted"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	r, err := b.OpenDatabase(source, nil, nil)
	if err != nil || r["zero_excluded_rows"] != int64(2) || b.Obs.State().N != 22 {
		t.Fatalf("conversion %v %v", r, err)
	}
	if err := b.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(table(t, b, "SELECT * FROM estimates")) != 4 || len(table(t, b, "SELECT * FROM comparisons")) != 1 {
		t.Fatal("conversion fit invalidation/history")
	}
	c, err := db.OpenReadOnly(source)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	n, _ := db.Scalar(c, "SELECT count(*) FROM observations WHERE latitude=0 AND longitude=0")
	if db.Int(n) != 2 {
		t.Fatal("original SQLite file was modified")
	}
}

func TestIncompleteGenerationRecoversWithoutDeletingCommittedHistory(t *testing.T) {
	a, _ := dirtyWorkspace(t)
	root := a.Root
	parent := filepath.Join(root, "generations")
	os.Mkdir(parent, 0755)
	broken := filepath.Join(parent, "broken")
	if err := a.CopyWorkspace(context.Background(), broken, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate a missing manifest at a persisted handoff boundary. The metadata
	// still declares committed rows; opening it must not erase its import history.
	if err := os.Remove(filepath.Join(broken, "observations", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := PublishWorkspace(root, filepath.Join("generations", "broken"), "."); err != nil {
		t.Fatal(err)
	}
	a.Close()
	recovered, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if !recovered.Recovered || recovered.Obs.State().N != 24 {
		t.Fatal("prior complete generation not recovered")
	}
	c, err := db.OpenReadOnly(filepath.Join(broken, "atlas.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	n, _ := db.Scalar(c, "SELECT count(*) FROM imports")
	if db.Int(n) != 1 {
		t.Fatal("opening an incomplete candidate erased source history")
	}
	files, _ := filepath.Glob(filepath.Join(broken, "observations", "seg-*.bin"))
	if len(files) == 0 {
		t.Fatal("opening an incomplete candidate removed committed segments")
	}
}

func TestMissingPublishedGenerationRecovers(t *testing.T) {
	a, _ := dirtyWorkspace(t)
	root := a.Root
	a.Close()
	if err := WriteJSONAtomic(filepath.Join(root, pointerFile), WorkspacePointer{WorkspaceFormat, 1, "generations/missing", "."}); err != nil {
		t.Fatal(err)
	}
	if !IsWorkspace(root) {
		t.Fatal("recoverable root rejected")
	}
	b, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !b.Recovered || b.Obs.State().N != 24 {
		t.Fatal("missing revision replaced with empty data")
	}
	if _, err := os.Stat(filepath.Join(root, "generations", "missing")); !os.IsNotExist(err) {
		t.Fatal("missing generation was created", err)
	}
}

func TestIncompleteManifestPreservesCommittedFiles(t *testing.T) {
	a, _ := dirtyWorkspace(t)
	root := a.Root
	a.Close()
	path := filepath.Join(root, "observations", "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["segments"] = []any{}
	if err := WriteJSONAtomic(path, manifest); err != nil {
		t.Fatal(err)
	}
	if broken, err := Open(root); err == nil {
		broken.Close()
		t.Fatal("incomplete manifest opened")
	}
	files, _ := filepath.Glob(filepath.Join(root, "observations", "seg-*.bin"))
	if len(files) == 0 {
		t.Fatal("incomplete manifest caused committed files to be swept")
	}
}
