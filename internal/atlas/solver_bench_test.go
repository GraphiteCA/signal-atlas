package atlas

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Fixed numerical workloads, independent of storage and cache reuse.
func scoringModel(n, radios int, weighted bool) (*LocationModel, []int) {
	r := rand.New(rand.NewPCG(91, 16))
	c := ObsCols{}
	w := make([]float64, n)
	idx := make([]int, n)
	for i := range n {
		x, y := r.Float64()*400-200, r.Float64()*180-90
		c.Lat = append(c.Lat, 48.4+y/MetresPerDegree)
		c.Lon = append(c.Lon, -123.4+x/(MetresPerDegree*math.Cos(48.4*math.Pi/180)))
		c.RSSI = append(c.RSSI, -32-float64(i%radios)*4-30*math.Log10(math.Max(3, math.Hypot(x-18, y+22)))+r.NormFloat64()*3)
		c.MAC = append(c.MAC, string(rune('A'+i%radios)))
		w[i], idx[i] = 1, i
		if weighted {
			w[i] += float64(i % 7)
		}
	}
	m, e := NewLocationModel(c, 3, w, nil, nil)
	if e != nil {
		panic(e)
	}
	return m, idx
}

func BenchmarkPhase5Solver(b *testing.B) {
	for _, tc := range []struct {
		name      string
		n, radios int
		weighted  bool
	}{{"single50", 50, 1, false}, {"multi256", 256, 3, false}, {"weighted256", 256, 1, true}} {
		for _, fast := range []bool{false, true} {
			name := "reference"
			if fast {
				name = "fast"
			}
			b.Run(tc.name+"/"+name, func(b *testing.B) {
				m, idx := scoringModel(tc.n, tc.radios, tc.weighted)
				b.ReportAllocs()
				for b.Loop() {
					var e error
					if fast {
						_, e = m.FitFast(idx, 3, 500, 10, false)
					} else {
						_, e = m.Fit(idx, 3, 500, 10, false)
					}
					if e != nil {
						b.Fatal(e)
					}
				}
			})
		}
	}
}

func BenchmarkPhase5Scoring(b *testing.B) {
	for _, tc := range []struct {
		name      string
		n, radios int
		weighted  bool
	}{{"single50", 50, 1, false}, {"multi256", 256, 3, false}, {"weighted256", 256, 1, true}, {"large2048", 2048, 4, true}} {
		b.Run(tc.name, func(b *testing.B) {
			m, idx := scoringModel(tc.n, tc.radios, tc.weighted)
			s := m.subset(idx)
			axis := Linspace(-500, 500, 101)
			x, y := make([]float64, 10201), make([]float64, 10201)
			for i := range x {
				x[i], y[i] = axis[i%101], axis[i/101]
			}
			out := make([]float64, len(x))
			b.ReportAllocs()
			for b.Loop() {
				if _, e := m.sse(x, y, s, 3, out); e != nil {
					b.Fatal(e)
				}
			}
			if folder := os.Getenv("SIGNAL_ATLAS_PHASE5_EXPORT"); folder != "" {
				marker, e := os.ReadFile(filepath.Join(folder, "owner.json"))
				var owner struct {
					Purpose string
					Schema  int
				}
				if e != nil || json.Unmarshal(marker, &owner) != nil || owner.Purpose != "signal-atlas-benchmark" || owner.Schema != 1 {
					b.Fatal("Owned output directory required")
				}
				start := time.Now()
				for range 20 {
					if _, e := m.sse(x, y, s, 3, out); e != nil {
						b.Fatal(e)
					}
				}
				v := map[string]any{"name": tc.name, "x": s.x, "y": s.y, "rssi": s.r, "weights": s.w, "groups": s.g, "weight_sums": s.sw, "within": s.within, "candidate_x": x, "candidate_y": y, "scores": out, "cpu_seconds_per_call": time.Since(start).Seconds() / 20}
				data, e := json.Marshal(v)
				if e != nil {
					b.Fatal(e)
				}
				f, e := os.OpenFile(filepath.Join(folder, "gpu-input-"+tc.name+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if e != nil {
					b.Fatal(e)
				}
				_, e = f.Write(data)
				f.Close()
				if e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}
