//go:build windows && amd64 && gpuintegration

package atlas

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"signal-atlas/internal/gpu"
)

func TestOpenCLKnownTruthMatrix(t *testing.T) {
	if status := gpu.Probe(); status["available"] != true {
		t.Fatal(status)
	}
	r := rand.New(rand.NewPCG(159, 391))
	rows := []map[string]any{}
	for trial := 0; trial < 40; trial++ {
		geometry := []string{"loop", "corridor", "line", "area"}[trial%4]
		n := 12 + 32*(trial%4)
		radios := 1 + trial%3
		apx, apy := r.Float64()*180-90, r.Float64()*180-90
		if trial%10 == 8 {
			apx = 480
		}
		noise := 3.
		if trial%3 == 0 {
			noise = 0
		}
		cols := ObsCols{}
		weights, within := make([]float64, n), make([]float64, n)
		idx := make([]int, n)
		for i := range n {
			x, y := r.Float64()*400-200, r.Float64()*300-150
			switch geometry {
			case "loop":
				angle := 2 * math.Pi * float64(i) / float64(n)
				x, y = 180*math.Cos(angle), 140*math.Sin(angle)
			case "corridor":
				y = 20 * math.Sin(x/70)
			case "line":
				y = 0
			}
			cols.Lat = append(cols.Lat, 48.4+y/MetresPerDegree)
			cols.Lon = append(cols.Lon, -123.4+x/(MetresPerDegree*math.Cos(48.4*math.Pi/180)))
			cols.RSSI = append(cols.RSSI, -30-float64(i%radios)*4-30*math.Log10(math.Max(3, math.Hypot(x-apx, y-apy)))+noise*r.NormFloat64())
			cols.MAC = append(cols.MAC, string(rune('A'+i%radios)))
			weights[i] = 1
			if trial%2 == 1 {
				weights[i] += float64(i % 7)
				within[i] = noise * noise * float64(i%3)
			}
			idx[i] = i
		}
		m, e := NewLocationModel(cols, 3, weights, within, nil)
		if e != nil {
			t.Fatal(e)
		}
		truthLat, truthLon := 48.4+apy/MetresPerDegree, -123.4+apx/(MetresPerDegree*math.Cos(48.4*math.Pi/180))
		row := map[string]any{"case": trial, "geometry": geometry, "observations": n, "radios": radios, "noise_sd_db": noise, "weighted": trial%2 == 1, "boundary_case": apx == 480}
		fits := map[string]*Fit{}
		for _, name := range []string{"reference", "fast", GPUReference, GPUFast, "pattern-candidate"} {
			m.accelerated = name == GPUReference || name == GPUFast
			fit := m.Fit
			if name == "fast" || name == GPUFast {
				fit = m.FitFast
			}
			start := time.Now()
			var v *Fit
			if name == "pattern-candidate" {
				v, e = patternCandidate(m, idx)
			} else {
				v, e = fit(idx, 3, 500, 10, false)
			}
			if e != nil {
				t.Fatal(e)
			}
			fits[name] = v
			if m.accelerated && (m.gpuCalls == 0 || m.gpuFallbacks != 0) {
				t.Fatal("Hardware matrix used a CPU fallback", trial, name, m.gpuCalls, m.gpuFallbacks)
			}
			truthError := math.Hypot((v.Latitude-truthLat)*MetresPerDegree, (v.Longitude-truthLon)*MetresPerDegree*math.Cos(truthLat*math.Pi/180))
			row[name] = map[string]any{"seconds": time.Since(start).Seconds(), "truth_error_m": truthError, "sse": v.SSE, "rms_db": v.RMS, "east_m": v.EastM, "north_m": v.NorthM}
			if name == "reference" && noise == 0 && geometry == "loop" && apx != 480 && truthError > 1 {
				t.Fatal("noise-free well-conditioned known location missed", trial, truthError)
			}
		}
		for _, pair := range [][2]string{{"reference", GPUReference}, {"fast", GPUFast}} {
			a, b := fits[pair[0]], fits[pair[1]]
			if a.EastM != b.EastM || a.NorthM != b.NorthM || a.SSE != b.SSE {
				t.Fatal("GPU final result differs", trial, pair, a, b)
			}
		}
		rows = append(rows, row)
	}
	if folder := os.Getenv("SIGNAL_ATLAS_PHASE5_HARDWARE_REPORT"); folder != "" {
		b, e := os.ReadFile(filepath.Join(folder, "owner.json"))
		var v struct {
			Purpose string
			Schema  int
		}
		if e != nil || json.Unmarshal(b, &v) != nil || v.Purpose != "signal-atlas-benchmark" || v.Schema != 1 {
			t.Fatal("Owned report directory required")
		}
		b, e = json.MarshalIndent(map[string]any{"schema": 1, "cases": rows, "notes": "Known synthetic AP positions; fixed seed; loops/corridors/colinear/area, noise, weights, radios and boundaries. Truth error is separate from algorithm agreement. Noisy/ambiguous cases do not guarantee accurate AP positions."}, "", "  ")
		if e != nil {
			t.Fatal(e)
		}
		f, e := os.OpenFile(filepath.Join(folder, "known-truth.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		_, e = f.Write(b)
		f.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
}
