package atlas

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"signal-atlas/internal/db"
)

func preparationFixture(t *testing.T) *Atlas {
	t.Helper()
	a, err := Open(filepath.Join(t.TempDir(), "survey.atlas"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	file := filepath.Join(t.TempDir(), "survey.csv")
	writeFile(t, file, surveyCSV([]string{"One", "Two", ""}, 0))
	if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	return a
}
func preparationNamespace(t *testing.T, a *Atlas, mode, grouping string, fast bool) PreparationNamespace {
	t.Helper()
	p := DefaultPreparationSettings()
	p.FitMode = mode
	p.Grouping = grouping
	n, err := NewPreparationNamespace(p, fast)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.EnsurePreparationNamespace(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	return n
}
func drainPreparation(t *testing.T, a *Atlas, n PreparationNamespace) int {
	t.Helper()
	done := 0
	for {
		targets, err := a.NextPreparationTargets(context.Background(), n.ID, 256<<20, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(targets) == 0 {
			break
		}
		for _, target := range targets {
			if _, err := a.PrepareTarget(context.Background(), n, target, 256<<20); err != nil {
				t.Fatal(err)
			}
			done++
		}
	}
	return done
}
func TestPreparationDurableNamespacesAndUnavailable(t *testing.T) {
	a := preparationFixture(t)
	namespaces := map[string]bool{}
	for _, fast := range []bool{false, true} {
		for _, mode := range []string{"sample", "all"} {
			for _, grouping := range []string{"shared", "individual", "both"} {
				n := preparationNamespace(t, a, mode, grouping, fast)
				if namespaces[n.ID] {
					t.Fatal("namespace collision")
				}
				namespaces[n.ID] = true
				if count := drainPreparation(t, a, n); count != 3 {
					t.Fatal("target count", count, n)
				}
				p, err := a.PreparationProgress(context.Background(), n.ID, 256<<20)
				if err != nil || p.Pending != 0 || p.Completed != 3 {
					t.Fatal(p, err)
				}
				if again := drainPreparation(t, a, n); again != 0 {
					t.Fatal("completed sweep revisited targets")
				}
			}
		}
	}
	// A saved unavailable result is a terminal outcome for these inputs too.
	n := preparationNamespace(t, a, "sample", "shared", true)
	if rows := table(t, a, "SELECT * FROM preparation_checkpoints WHERE namespace=?", n.ID); len(rows) != 3 {
		t.Fatal(rows)
	}
	root := a.Root
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if drainPreparation(t, b, n) != 0 {
		t.Fatal("reopen recalculated completed targets")
	}
	// Missing/limited geometry outcomes are persisted, not retried every pan.
	file := filepath.Join(t.TempDir(), "insufficient.csv")
	writeFile(t, file, "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n02:00:00:00:00:01,Short,2026-11-01,2412,-60,48.4,-123.4,3,WIFI\n")
	if _, err := b.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if drainPreparation(t, b, n) != 1 {
		t.Fatal("new insufficient target not queued")
	}
	if rows := table(t, b, "SELECT unavailable FROM preparation_checkpoints WHERE namespace=? AND target_key=?", n.ID, TargetKey("Short", "")); len(rows) != 1 || db.Int(rows[0]["unavailable"]) != 1 {
		t.Fatal(rows)
	}
}
func TestPreparationSelectiveVersionsAndStaleCommit(t *testing.T) {
	a := preparationFixture(t)
	n := preparationNamespace(t, a, "sample", "shared", true)
	drainPreparation(t, a, n)
	untouched := table(t, a, "SELECT * FROM preparation_checkpoints WHERE target_key=?", TargetKey("Two", ""))
	file := filepath.Join(t.TempDir(), "one.csv")
	writeFile(t, file, surveyCSV([]string{"One"}, 30))
	if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(untouched, table(t, a, "SELECT * FROM preparation_checkpoints WHERE target_key=?", TargetKey("Two", ""))) {
		t.Fatal("untouched checkpoint changed")
	}
	targets, err := a.NextPreparationTargets(context.Background(), n.ID, 256<<20, 10)
	if err != nil || len(targets) != 1 || targets[0].SSID != "One" {
		t.Fatal(targets, err)
	}
	paused, release := make(chan struct{}), make(chan struct{})
	a.beforePreparationCommit = func() { close(paused); <-release }
	done := make(chan error, 1)
	go func() { _, err := a.PrepareTarget(context.Background(), n, targets[0], 256<<20); done <- err }()
	<-paused
	file = filepath.Join(t.TempDir(), "newer.csv")
	writeFile(t, file, surveyCSV([]string{"One"}, 60))
	if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrStaleInput) {
		t.Fatal("stale worker committed", err)
	}
	a.beforePreparationCommit = nil
	if !reflect.DeepEqual(untouched, table(t, a, "SELECT * FROM preparation_checkpoints WHERE target_key=?", TargetKey("Two", ""))) {
		t.Fatal("unrelated target invalidated")
	}
	if count := drainPreparation(t, a, n); count != 1 {
		t.Fatal(count)
	}
}
func TestPreparationBudgetCancellationAndSnapshot(t *testing.T) {
	a := preparationFixture(t)
	n := preparationNamespace(t, a, "all", "both", true)
	targets, _ := a.NextPreparationTargets(context.Background(), n.ID, 256<<20, 3)
	if _, err := a.PrepareTarget(context.Background(), n, targets[0], 1); !errors.Is(err, ErrPreparationBudget) {
		t.Fatal(err)
	}
	progress, _ := a.PreparationProgress(context.Background(), n.ID, 1)
	if progress.Blocked != 1 || progress.Completed != 0 {
		t.Fatal(progress)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.PrepareTarget(ctx, n, targets[1], 256<<20); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if drainPreparation(t, a, n) != 3 {
		t.Fatal("blocked/cancelled targets were marked done")
	}
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultPreparationSettings()
	p.Paused = true
	p.FitMode = "all"
	if err := a.SavePreparationSettings(c, p); err != nil {
		t.Fatal(err)
	}
	put()
	path := filepath.Join(t.TempDir(), "portable.sqlite3")
	if err := a.ExportSQLite(path, nil); err != nil {
		t.Fatal(err)
	}
	b, err := Open(filepath.Join(t.TempDir(), "converted"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.OpenDatabase(path, nil, nil); err != nil {
		t.Fatal(err)
	}
	if drainPreparation(t, b, n) != 0 {
		t.Fatal("portable snapshot lost complete checkpoints")
	}
	c, put, err = b.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := b.PreparationSettings(c)
	put()
	if err != nil || saved != p {
		t.Fatal(saved, err)
	}
}
func TestPreparationCleanupAndManualBandEdit(t *testing.T) {
	a, _ := dirtyWorkspace(t)
	defer a.Close()
	n := preparationNamespace(t, a, "sample", "shared", true)
	drainPreparation(t, a, n)
	untouched := table(t, a, "SELECT * FROM preparation_checkpoints WHERE target_key=?", TargetKey("Untouched", ""))
	b, _, err := a.PrepareCleanup(context.Background(), filepath.Join(t.TempDir(), "clean"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !reflect.DeepEqual(untouched, table(t, b, "SELECT * FROM preparation_checkpoints WHERE target_key=?", TargetKey("Untouched", ""))) {
		t.Fatal("cleanup lost unaffected checkpoint")
	}
	if drainPreparation(t, b, n) != 2 {
		t.Fatal("cleanup should queue named and hidden affected targets")
	}
	file := filepath.Join(t.TempDir(), "editable.csv")
	writeFile(t, file, strings.ReplaceAll(surveyCSV([]string{"Editable"}, 90), ",2437,", ",,"))
	if _, err := b.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	drainPreparation(t, b, n)
	c, put, _ := b.Conn(nil)
	_, err = b.AssignBand(c, "Editable", "02:5A:00:00:00:00", "5")
	put()
	if err != nil {
		t.Fatal(err)
	}
	if drainPreparation(t, b, n) != 1 {
		t.Fatal("band assignment dirtied unrelated work")
	}
	if !reflect.DeepEqual(untouched, table(t, b, "SELECT * FROM preparation_checkpoints WHERE target_key=?", TargetKey("Untouched", ""))) {
		t.Fatal("band assignment lost checkpoint")
	}
}

func TestLargeSampleSortCancellation(t *testing.T) {
	values := make([]int, 1000000)
	for i := range values {
		values[i] = len(values) - i
	}
	checks := 0
	start := time.Now()
	err := fitSortStable(values, func(i, j int) bool { return values[i] < values[j] }, func() bool { checks++; return checks >= 2 })
	if !errors.Is(err, ErrCancelled) || time.Since(start) > time.Second {
		t.Fatal("large sample sort did not cancel promptly", err, time.Since(start))
	}
	values = []int{4, 2, 2, 3, 1}
	if err := fitSortStable(values, func(i, j int) bool { return values[i] < values[j] }, nil); err != nil || !sort.IntsAreSorted(values) {
		t.Fatal(values, err)
	}
}
