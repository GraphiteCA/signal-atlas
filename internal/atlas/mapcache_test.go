package atlas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"signal-atlas/internal/db"
	"signal-atlas/internal/py"
	"zombiezen.com/go/sqlite"
)

func TestLargeIndividualViewUsesCompactPagination(t *testing.T) {
	// Exercise the large-view delivery path without building a second observation
	// store: eligibility and revisions are covered by the public API tests above.
	idx := &mapIndex{}
	const n = 110000
	for i := 0; i < n; i++ {
		idx.rows = append(idx.rows, MapFit{ID: fmt.Sprintf("ap-%06d", i), SSID: "Dense survey", Band: "5", Lon: -123.4, Lat: 48.4})
	}
	a := &Atlas{}
	p := py.Params{"clustering": {"0"}, "limit": {"1500"}}
	b := [4]float64{-124, 48, -123, 49}
	start := 0
	seen := map[string]bool{}
	for {
		page, err := a.mapIndividuals(context.Background(), idx, "revision", "query", start, 15, b, p)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != n || len(page.Features) > 1500 {
			t.Fatalf("large page budget: %d/%d", len(page.Features), page.Total)
		}
		for _, r := range page.Features {
			if seen[r.ID] {
				t.Fatal("duplicate AP")
			}
			seen[r.ID] = true
		}
		if page.Next == "" {
			break
		}
		start, _ = strconv.Atoi(strings.Split(page.Next, ".")[1])
	}
	if len(seen) != n || len(a.mapCache.viewMembers) != n || a.mapCache.view != nil {
		t.Fatalf("large view lost APs or retained full feature payload: %d", len(seen))
	}
}

func TestMapFullInputPreferenceSolverIsolationAndRollback(t *testing.T) {
	a, c, done := mapFixture(t, 3)
	defer done()
	ctx := context.Background()
	p := py.Params{"bbox": {"-124,48,-123,49"}}
	idx, _, err := a.mapIndex(ctx, c, p)
	if err != nil {
		t.Fatal(err)
	}
	id := idx.rows[0].ID
	_, sample, _ := EstimateScope(py.Params{"fit_mode": {"sample"}})
	_, full, _ := EstimateScope(py.Params{"fit_mode": {"all"}})
	if _, err := db.Exec(c, "INSERT INTO estimates SELECT radio_key,?,json_set(result,'$.longitude',-122) FROM estimates WHERE radio_key=? AND scope_key=?", full, id, sample); err != nil {
		t.Fatal(err)
	}
	page, err := a.MapFeatures(ctx, c, p)
	if err != nil || page.Total != 2 {
		t.Fatalf("full input did not replace sampled location: %+v %v", page, err)
	}
	_, fast, _ := EstimateScope(py.Params{"fit_mode": {"sample"}, "solver": {"fast"}})
	if _, err := db.Exec(c, "INSERT INTO estimates SELECT radio_key,?,json_set(result,'$.used_observations',777) FROM estimates WHERE radio_key=? AND scope_key=?", fast, id, sample); err != nil {
		t.Fatal(err)
	}
	fastP := p.With("solver", []string{"fast"}).With("id", []string{id})
	detail, err := a.MapDetail(ctx, c, fastP)
	if err != nil || detail.Used != 777 {
		t.Fatalf("solver namespace: %+v %v", detail, err)
	}
	page, err = a.MapFeatures(ctx, c, fastP)
	if err != nil || page.Total != 1 {
		t.Fatalf("reference fits leaked into fast scope: %+v %v", page, err)
	}
	before, _, _ := a.mapIndex(ctx, c, p)
	sentinel := errors.New("rollback")
	if err := db.Tx(c, func() error {
		_, err := db.Exec(c, "DELETE FROM estimates")
		if err != nil {
			return err
		}
		if _, _, err := a.mapIndex(ctx, c, p); err == nil {
			t.Fatal("uncommitted transaction published a map snapshot")
		}
		return sentinel
	}); err != sentinel {
		t.Fatal(err)
	}
	after, _, _ := a.mapIndex(ctx, c, p)
	if before != after {
		t.Fatal("rolled-back writes invalidated committed cache")
	}
}

func TestMapBandAssignmentInvalidatesLocations(t *testing.T) {
	a, c, done := mapFixture(t, 2)
	defer done()
	file := filepath.Join(t.TempDir(), "unknown.csv")
	writeFile(t, file, strings.ReplaceAll(surveyCSV([]string{"Editable"}, 30), ",2437,", ",,"))
	if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	p := py.Params{}
	before, _, err := a.mapIndex(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.rows) != 2 {
		t.Fatal("fixture locations")
	}
	if _, err := a.AssignBand(c, "Editable", "02:5A:00:00:00:00", "5"); err != nil {
		t.Fatal(err)
	}
	page, err := a.MapFeatures(context.Background(), c, p)
	if err != nil || page.Total != 0 {
		t.Fatalf("band edit kept stale cached locations: %+v %v", page, err)
	}
}

func mapFixture(t *testing.T, n int) (*Atlas, *sqlite.Conn, func()) {
	t.Helper()
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n")
	for i := 0; i < n; i++ {
		for j := 0; j < 4; j++ {
			fmt.Fprintf(&b, "02:51:00:00:%02X:%02X,Net%d,2026-05-01 10:00:0%d,2437,-65,48.4,-123.4,4,WIFI\n", i/256, i%256, i, j)
		}
	}
	file := filepath.Join(dir, "data.csv")
	writeFile(t, file, b.String())
	if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := a.SelectedGroups(py.Params{})
	if err != nil {
		t.Fatal(err)
	}
	_, scope, _ := EstimateScope(py.Params{"fit_mode": {"sample"}})
	if err := db.Tx(c, func() error {
		for _, g := range groups {
			r := map[string]any{}
			for k, v := range g {
				r[k] = v
			}
			r["status"] = "fitted"
			r["latitude"] = 48.4
			r["longitude"] = -123.4
			r["used_observations"] = 4
			r["residual_rms_db"] = 3
			raw, _ := json.Marshal(r)
			if _, err := db.Exec(c, "INSERT INTO estimates VALUES(?,?,?)", g["radio_key"], scope, string(raw)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, c, func() { put(); a.Close() }
}

func TestMapExactClustersPaginationAndCache(t *testing.T) {
	a, c, done := mapFixture(t, 1601)
	defer done()
	ctx := context.Background()
	p := py.Params{"bbox": {"-124,48,-123,49"}, "zoom": {"15"}, "limit": {"2"}}
	page, err := a.MapFeatures(ctx, c, p)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1601 || len(page.Features) != 1 || page.Features[0].Count != 1601 {
		t.Fatalf("sampled cluster: %+v", page)
	}
	idx, _, _ := a.mapIndex(ctx, c, p)
	again, _, _ := a.mapIndex(ctx, c, p)
	if idx != again {
		t.Fatal("warm index rebuilt")
	}
	targets := a.mapCache.targets
	individual := p.With("clustering", []string{"0"}).With("limit", []string{"137"})
	seen := map[string]bool{}
	pages := 0
	for {
		page, err = a.MapFeatures(ctx, c, individual)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(page.Features) > 137 {
			t.Fatal("page budget")
		}
		for _, f := range page.Features {
			if f.Count != 1 || seen[f.ID] {
				t.Fatal("sampled/duplicate individual")
			}
			seen[f.ID] = true
		}
		if page.Next == "" {
			break
		}
		individual = individual.With("cursor", []string{page.Next})
	}
	if len(seen) != 1601 || pages < 2 {
		t.Fatalf("lost APs: %d", len(seen))
	}
	// Pins remain available for details while excluded from the cluster count.
	id := idx.rows[0].ID
	pinned := p.With("pin", []string{id})
	page, err = a.MapFeatures(ctx, c, pinned)
	if err != nil || page.Total != 1600 {
		t.Fatalf("pin count: %+v %v", page, err)
	}
	detail, err := a.MapDetail(ctx, c, pinned.With("id", []string{id}))
	if err != nil || detail.ID != id || detail.Band != "2.4" {
		t.Fatalf("detail: %+v %v", detail, err)
	}
	// A committed direct SQL update invalidates typed fits and cursors, without
	// rebuilding the much more expensive observation aggregation.
	first, err := a.MapFeatures(ctx, c, p.With("clustering", []string{"0"}))
	if err != nil || first.Next == "" {
		t.Fatal(err)
	}
	if _, err := db.Exec(c, "UPDATE estimates SET result=json_set(result,'$.longitude',-122.0) WHERE radio_key=?", id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.MapFeatures(ctx, c, p.With("clustering", []string{"0"}).With("cursor", []string{first.Next})); err == nil || !strings.Contains(err.Error(), "Map changed") {
		t.Fatalf("stale cursor accepted: %v", err)
	}
	page, err = a.MapFeatures(ctx, c, p)
	if err != nil || page.Total != 1600 {
		t.Fatalf("stale positions: %+v %v", page, err)
	}
	if fmt.Sprintf("%p", targets) != fmt.Sprintf("%p", a.mapCache.targets) {
		t.Fatal("fit edit reaggregated observations")
	}
	// Presentation changes do not alter the fit revision or cached index.
	before := a.mapCache.visible
	if err := a.SaveMapDisplay(c, MapDisplay{false, 2, 50, false}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.MapFeatures(ctx, c, p); err != nil {
		t.Fatal(err)
	}
	if before != a.mapCache.visible {
		t.Fatal("display settings invalidated fits")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.MapFeatures(cancelled, c, p); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestMapCountsInvalidationAndPreferenceRoundTrip(t *testing.T) {
	a, c, done := mapFixture(t, 3)
	defer done()
	s, err := a.MapSummary(c)
	if err != nil || s.LocatedSSIDs != 3 || s.ObservedSSIDs != 3 || s.Observations != 12 {
		t.Fatalf("summary: %+v %v", s, err)
	}
	// Missing coordinates, unavailable fits and hidden names are not located SSIDs.
	if _, err := db.Exec(c, "UPDATE estimates SET result=json_remove(result,'$.latitude') WHERE json_extract(result,'$.ssid')='Net0'"); err != nil {
		t.Fatal(err)
	}
	s, err = a.MapSummary(c)
	if err != nil || s.LocatedSSIDs != 2 {
		t.Fatalf("invalid coordinate counted: %+v %v", s, err)
	}
	p := py.Params{"ssid_query": {"Net1"}}
	page, err := a.MapFeatures(context.Background(), c, p)
	if err != nil || page.Total != 1 {
		t.Fatalf("eligibility: %+v %v", page, err)
	}
	s2, _ := a.MapSummary(c)
	if s2 != s {
		t.Fatal("selection changed whole-survey counts")
	}
	// Import drops a touched fit and adds a new network. Cached results must
	// reflect both the metadata revision and the new immutable observation State.
	file := filepath.Join(t.TempDir(), "more.csv")
	writeFile(t, file, surveyCSV([]string{"Net1", "Fresh"}, 30))
	if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	s2, err = a.MapSummary(c)
	if err != nil || s2.LocatedSSIDs != 1 || s2.ObservedSSIDs != 4 || s2.Observations != 28 || s2.Revision == s.Revision {
		t.Fatalf("import invalidation: %+v %v", s2, err)
	}
	page, err = a.MapFeatures(context.Background(), c, p)
	if err != nil || page.Total != 0 {
		t.Fatalf("stale imported fit: %+v %v", page, err)
	}
	d := MapDisplay{false, 2, 50, false}
	if err := a.SaveMapDisplay(c, d); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []MapDisplay{{true, 1, 10, true}, {true, 23, 10, true}, {true, 18, 0, true}, {true, 18, 51, true}} {
		if a.SaveMapDisplay(c, bad) == nil {
			t.Fatal("invalid preference accepted")
		}
	}
	export := filepath.Join(t.TempDir(), "survey.sqlite3")
	if err := a.ExportSQLite(export, nil); err != nil {
		t.Fatal(err)
	}
	other, err := Open(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	oc, put, _ := other.Conn(nil)
	defer put()
	def, err := other.MapDisplay(oc)
	if err != nil || def != DefaultMapDisplay() {
		t.Fatal("defaults/isolation")
	}
	if _, err := other.OpenDatabase(export, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := other.MapDisplay(oc)
	if err != nil || got != d {
		t.Fatalf("export/import preferences: %+v %v", got, err)
	}
}

func TestMapModelEligibilityMatchesReferenceTargets(t *testing.T) {
	a, c, done := mapFixture(t, 6)
	defer done()
	// Introduce a second MAC into a named SSID and change one to a hidden name.
	file := filepath.Join(t.TempDir(), "groups.csv")
	writeFile(t, file, strings.ReplaceAll(surveyCSV([]string{"Net0", ""}, 30), "02:5A:", "02:5B:"))
	if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []py.Params{{}, {"individual": {"1"}}, {"individual": {"1"}, "shared": {"1"}}, {"band": {"5"}}, {"ssid_query": {"Net0"}}, {"min_rssi": {"-60"}}} {
		if _, _, err := a.mapIndex(context.Background(), c, p); err != nil {
			t.Fatal(err)
		}
		_, _, _, targets, err := a.AllTargets(p)
		if err != nil {
			t.Fatal(err)
		}
		i, s := mapModel(p)
		want := map[string]bool{}
		for _, target := range targets {
			groups, err := a.TargetGroups(AreaScope(p), target, i, s)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range groups {
				want[gs(g, "radio_key")] = true
			}
		}
		got := a.mapCache.targets
		if len(got) != len(want) {
			t.Fatalf("target count %v: got %d want %d", p, len(got), len(want))
		}
		for k := range want {
			if !got[k] {
				t.Fatalf("missing target %v: %s", p, k)
			}
		}
	}
}
