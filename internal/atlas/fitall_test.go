package atlas

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"signal-atlas/internal/py"
)

// After fitting every network the way "Fit every AP" does, the map's nearby fits find nothing left
// to fit and draw every one of them: the fits land in the cache the map reads.
func TestFitEveryNetworkFillsTheMapCache(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// twelve named networks (two with two radios) and three hidden ones, around one spot
	var b strings.Builder
	b.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n")
	for n := 0; n < 15; n++ {
		ssid := fmt.Sprintf("Net%02d", n)
		if n >= 12 {
			ssid = ""
		}
		apLat, apLon := 48.449+float64(n%5)*0.0004, 2.668+float64(n/5)*0.0006
		for r := 0; r < 1+n%2; r++ {
			mac := fmt.Sprintf("02:5A:00:00:%02X:%02X", n, r)
			for k := 0; k < 9; k++ {
				ang := float64(k) * 0.7
				d := 15 + float64(k*7%40)
				lat := apLat + d*math.Sin(ang)/111195
				lon := apLon + d*math.Cos(ang)/(111195*math.Cos(apLat*math.Pi/180))
				rssi := int(-40 - 25*math.Log10(d))
				fmt.Fprintf(&b, "%s,%s,2026-05-0%d 10:00:%02d,%d,%d,%.7f,%.7f,4,WIFI\n", mac, ssid, 1+k%3, k, []int{2437, 5180}[r], rssi, lat, lon)
			}
		}
	}
	csv := filepath.Join(dir, "survey.csv")
	writeFile(t, csv, b.String())
	if _, err := a.ImportCSV(csv, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, individual := range []string{"0", "1"} {
		p := py.Params{"individual": {individual}, "shared": {"1"}, "min_rssi": {"-100"}}
		scoped, sampleKey, exactKey, targets, err := a.AllTargets(p)
		if err != nil {
			t.Fatal(err)
		}
		want := 16 // twelve named networks, and the hidden ones by MAC (one has two radios)
		if individual == "1" {
			want = 22 // every radio of a named network, and the hidden ones by MAC
		}
		if len(targets) != want {
			t.Fatalf("individual=%s: %d networks, want %d", individual, len(targets), want)
		}
		c, put, _ := a.Conn(nil)
		fitted := 0
		for _, tg := range targets {
			groups, err := a.TargetGroups(scoped, tg, individual == "1", true)
			if err != nil {
				t.Fatal(err)
			}
			have, err := CachedFits(c, sampleKey, exactKey, groups)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range groups {
				if have[gs(g, "radio_key")] {
					continue
				}
				status, err := a.FitAndStore(c, g, scoped, sampleKey, func() bool { return false })
				if err != nil {
					t.Fatal(err)
				}
				if status == "fitted" {
					fitted++
				}
			}
		}
		view := p.With("bbox", []string{"2.660,48.445,2.680,48.455"}).With("zoom", []string{"17"})
		plan, _, missing, err := a.AreaPlan(c, view, true, true)
		put()
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) != 0 || plan["missing_estimates"] != int64(0) {
			t.Fatalf("individual=%s: the map still wants %v fits", individual, plan["missing_estimates"])
		}
		if shown := len(plan["estimates"].([]any)); fitted == 0 || shown < fitted {
			t.Fatalf("individual=%s: fitted %d, the map shows %d", individual, fitted, shown)
		}
	}
}
