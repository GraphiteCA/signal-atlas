// qualityaudit reports aggregate observation-quality indicators on owned copies.
// It does not calculate locations, export identities or remove observations.
package main

import (
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"signal-atlas/internal/atlas"
	"signal-atlas/internal/obs"
)

func main() {
	data := flag.String("data", "", "owned benchmark data containing store/")
	flag.Parse()
	root, err := filepath.Abs(*data)
	if err != nil {
		panic(err)
	}
	owned := false
	parent := root
	for range 6 {
		b, e := os.ReadFile(filepath.Join(parent, "owner.json"))
		var o struct {
			Purpose string
			Schema  int
		}
		if e == nil && json.Unmarshal(b, &o) == nil && o.Purpose == "signal-atlas-benchmark" && o.Schema == 1 {
			owned = true
			break
		}
		parent = filepath.Dir(parent)
	}
	if !owned {
		panic("owned benchmark copy required")
	}
	a, err := atlas.Open(filepath.Join(root, "store"))
	if err != nil {
		panic(err)
	}
	defer a.Close()
	st := a.Obs.State()
	counts := map[string]int64{}
	for _, s := range st.Segs {
		for i, acc := range s.Acc {
			counts["rows"]++
			if obs.IsNull(acc) || math.IsNaN(acc) || acc <= 0 {
				counts["accuracy_unknown_or_nonpositive"]++
			} else {
				counts["accuracy_reported_positive"]++
				for _, bin := range []struct {
					name string
					max  float64
				}{{"accuracy_over_25m", 25}, {"accuracy_over_50m", 50}, {"accuracy_over_100m", 100}, {"accuracy_over_250m", 250}} {
					if acc > bin.max {
						counts[bin.name]++
					}
				}
			}
			if s.Lat[i] == 0 && s.Lon[i] == 0 {
				counts["zero_zero"]++
			}
			if s.RSSI[i] >= 0 {
				counts["rssi_nonnegative"]++
			}
			if s.RSSI[i] < -120 {
				counts["rssi_below_minus_120"]++
			}
		}
	}
	extent := func(rows []uint32) float64 {
		la0, la1, lo0, lo1 := 90., -90., 180., -180.
		for _, g := range rows {
			s, i := st.Loc(g)
			la0 = math.Min(la0, s.Lat[i])
			la1 = math.Max(la1, s.Lat[i])
			lo0 = math.Min(lo0, s.Lon[i])
			lo1 = math.Max(lo1, s.Lon[i])
		}
		return math.Max((la1-la0)*atlas.MetresPerDegree, (lo1-lo0)*atlas.MetresPerDegree*math.Cos((la0+la1)*.5*math.Pi/180))
	}
	for sid, rows := range st.BySSID {
		if st.Str[sid] == "" || len(rows) < 4 {
			continue
		}
		counts["named_ssids_with_at_least_four_rows"]++
		d := extent(rows)
		if d > 20000 {
			counts["named_ssids_span_over_20km"]++
		}
		if d > 500 && st.Net[sid].Macs > 1 {
			counts["multi_mac_ssids_span_over_500m"]++
		}
	}
	for _, rows := range st.ByTrans {
		if len(rows) < 4 {
			continue
		}
		counts["ssid_mac_pairs_with_at_least_four_rows"]++
		if extent(rows) > 20000 {
			counts["ssid_mac_pairs_span_over_20km"]++
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "aggregate_only": true, "observations_removed": 0, "counts": counts, "notes": "Raw unfiltered extents are indicators, not proof of bad scans or the count of fits recoverable by filtering. An SSID need not identify one physical AP. No trustworthy capture-time or ground-truth AP labels were assumed."}); err != nil {
		panic(err)
	}
}
