package atlas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func viewportFixture(t *testing.T) *Atlas {
	a, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	var b strings.Builder
	b.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n")
	for _, row := range []struct {
		ssid, mac string
		lat, lon  float64
		n         int
	}{{"A Far", "02:00:00:00:00:01", 49, -124, 8}, {"Z Town", "02:00:00:00:00:02", 48.4, -123.4, 8}, {"Z Town", "02:00:00:00:00:02", 48.41, -123.4, 8}, {"Y Halo", "02:00:00:00:00:03", 48.402, -123.4, 8}, {"", "02:00:00:00:00:04", 48.4, -123.4, 8}} {
		for i := 0; i < row.n; i++ {
			fmt.Fprintf(&b, "%s,%s,2026-11-01,2412,%d,%.7f,%.7f,3,WIFI\n", row.mac, row.ssid, -50-i, row.lat+float64(i%3)*.00001, row.lon+float64(i%2)*.00001)
		}
	}
	file := filepath.Join(t.TempDir(), "view.csv")
	writeFile(t, file, b.String())
	if _, err := a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	return a
}
func TestViewportPreparationPrioritizesNearbyWithoutCroppingInputs(t *testing.T) {
	a := viewportFixture(t)
	ns := preparationNamespace(t, a, "sample", "shared", true)
	ctx := context.Background()
	v := a.NewPreparationView([4]float64{-123.4001, 48.3999, -123.3999, 48.4001})
	got := map[string]bool{}
	for !v.Done() {
		targets, err := a.NextViewportPreparationTargets(ctx, ns.ID, 32<<20, v, 512)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range targets {
			if got[target.Key] {
				t.Fatal("repeated local target")
			}
			got[target.Key] = true
			if target.SSID == "Z Town" {
				result, err := a.ComputePreparationTarget(ctx, ns, target, 32<<20)
				if err != nil || len(result.Writes) != 1 {
					t.Fatal(result, err)
				}
				var fit map[string]any
				if err := json.Unmarshal([]byte(result.Writes[0].Text), &fit); err != nil || fit["count"] != float64(16) {
					t.Fatal("viewport cropped full fitting inputs", fit, err)
				}
			}
		}
	}
	for _, key := range []string{TargetKey("Z Town", ""), TargetKey("Y Halo", ""), TargetKey("", "02:00:00:00:00:04")} {
		if !got[key] {
			t.Fatal("missing nearby target", key)
		}
	}
	if got[TargetKey("A Far", "")] {
		t.Fatal("distant target prioritized")
	}
}
func TestViewportPreparationSkipsCompletedAndRejectsLargeOrStaleViews(t *testing.T) {
	a := viewportFixture(t)
	ns := preparationNamespace(t, a, "sample", "shared", true)
	drainPreparation(t, a, ns)
	v := a.NewPreparationView([4]float64{-123.401, 48.399, -123.399, 48.401})
	targets, err := a.NextViewportPreparationTargets(context.Background(), ns.ID, 32<<20, v, 512)
	if err != nil || len(targets) != 0 {
		t.Fatal(targets, err)
	}
	if a.NewPreparationView([4]float64{-180, -85, 180, 85}) != nil {
		t.Fatal("world view must not trigger full-survey priority scan")
	}
	v = a.NewPreparationView([4]float64{-123.401, 48.399, -123.399, 48.401})
	a.Obs.Reindex()
	if _, err := a.NextViewportPreparationTargets(context.Background(), ns.ID, 32<<20, v, 512); !errors.Is(err, ErrStaleInput) {
		t.Fatal(err)
	}
	v = a.NewPreparationView([4]float64{-123.401, 48.399, -123.399, 48.401})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.NextViewportPreparationTargets(ctx, ns.ID, 32<<20, v, 512); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestViewportPreparationSmallPagesKeepVisibleTargetsAheadOfHalo(t *testing.T) {
	a := viewportFixture(t)
	ns := preparationNamespace(t, a, "sample", "shared", true)
	v := a.NewPreparationView([4]float64{-123.4001, 48.3999, -123.3999, 48.4001})
	var targets []PreparationTarget
	for !v.Done() {
		page, err := a.NextViewportPreparationTargets(context.Background(), ns.ID, 32<<20, v, 1)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, page...)
	}
	if len(targets) != 3 || targets[2].SSID != "Y Halo" {
		t.Fatal("lost targets or prioritized halo before visible rows", targets)
	}
}
