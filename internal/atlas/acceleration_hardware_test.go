//go:build windows && amd64 && gpuintegration

package atlas

import (
	"math"
	"testing"

	"signal-atlas/internal/gpu"
)

// Explicit hardware suite: unsupported hardware fails instead of passing/skipping.
func TestOpenCLHardwareScoringAndSolvers(t *testing.T) {
	status := gpu.Probe()
	if status["available"] != true {
		t.Fatal(status)
	}
	for _, tc := range []struct {
		n, k     int
		weighted bool
	}{{8, 1, false}, {50, 1, false}, {256, 3, false}, {256, 1, true}, {2048, 4, true}} {
		m, idx := scoringModel(tc.n, tc.k, tc.weighted)
		s := m.subset(idx)
		s.within = 17
		x, y := coarseGrid(500, 10)
		expected, e := m.sse(x, y, s, 3, nil)
		if e != nil {
			t.Fatal(e)
		}
		groups := make([]int32, len(s.g))
		for i, g := range s.g {
			groups[i] = int32(g)
		}
		actual, e := gpu.Score(gpu.Input{X: s.x, Y: s.y, RSSI: s.r, Weights: s.w, WeightSums: s.sw, Groups: groups, CandidateX: x, CandidateY: y, Floor: 3, Factor: 30, Within: s.within}, nil)
		if e != nil {
			t.Fatal(e)
		}
		for i, v := range actual {
			if math.Abs(v-expected[i]) > 1e-10*math.Max(1, math.Abs(expected[i])) {
				t.Fatal("hardware score mismatch", i, v, expected[i])
			}
		}
		if tc.n > 256 {
			continue
		}
		for _, fast := range []bool{false, true} {
			fit := m.Fit
			if fast {
				fit = m.FitFast
			}
			m.accelerated = false
			cpu, e := fit(idx, 3, 500, 10, false)
			if e != nil {
				t.Fatal(e)
			}
			m.accelerated = true
			accelerated, e := fit(idx, 3, 500, 10, false)
			if e != nil {
				t.Fatal(e)
			}
			if cpu.EastM != accelerated.EastM || cpu.NorthM != accelerated.NorthM || cpu.SSE != accelerated.SSE || m.gpuCalls == 0 || m.gpuFallbacks > 0 {
				t.Fatal("final hardware fit mismatch/fallback", tc, fast, cpu, accelerated, m.gpuCalls, m.gpuFallbacks)
			}
		}
	}
}
