package atlas

import (
	"math"
	"reflect"
	"testing"

	"signal-atlas/internal/py"
)

func TestCPUScoringPreservesGenericArithmetic(t *testing.T) {
	for _, weighted := range []bool{false, true} {
		m, idx := scoringModel(128, 1, weighted)
		s := m.subset(idx)
		s.within = 19.5
		x, y := []float64{-1, 0, 18, 500}, []float64{17, 0, -22, -500}
		actual, e := m.sse(x, y, s, 3, nil)
		if e != nil {
			t.Fatal(e)
		}
		for p := range x {
			gs := make([]float64, s.k)
			imp := make([]float64, s.m)
			for j := range s.x {
				dx, dy := x[p]-s.x[j], y[p]-s.y[j]
				d := math.Sqrt(dx*dx + dy*dy)
				if d < m.floor {
					d = m.floor
				}
				v := s.r[j] + 30*math.Log10(d)
				imp[j] = v
				gs[s.g[j]] += v * s.w[j]
			}
			for q := range gs {
				gs[q] /= s.sw[q]
			}
			var total float64
			for j := range s.x {
				e := imp[j] - gs[s.g[j]]
				total += e * e * s.w[j]
			}
			if math.Float64bits(actual[p]) != math.Float64bits(total+s.within) {
				t.Fatal("CPU arithmetic changed", actual[p], total+s.within)
			}
		}
	}
}
func TestAccelerationNamespacesRemainDistinct(t *testing.T) {
	keys := map[string]bool{}
	for _, solver := range []string{"reference", "fast", GPUReference, GPUFast} {
		_, key, e := EstimateScope(py.Params{"solver": {solver}})
		if e != nil || keys[key] {
			t.Fatal(key, e)
		}
		keys[key] = true
		p := DefaultPreparationSettings()
		n, e := NewPreparationNamespace(p, solver == "fast" || solver == GPUFast, solver == GPUReference || solver == GPUFast)
		if e != nil || n.Solver != solver {
			t.Fatal(n, e)
		}
	}
	if _, _, e := EstimateScope(py.Params{"solver": {"unknown-new-algorithm"}}); e == nil {
		t.Fatal("unknown algorithm aliased CPU reference cache")
	}
}
func TestCommonGridIsImmutableAndCancellationReturnsPool(t *testing.T) {
	x, y := coarseGrid(500, 10)
	oldX, oldY := append([]float64(nil), x...), append([]float64(nil), y...)
	m, idx := scoringModel(12, 1, false)
	m.cancelled = func() bool { return true }
	if _, e := m.FitFast(idx, 3, 500, 10, false); e != ErrCancelled {
		t.Fatal(e)
	}
	m.cancelled = nil
	if _, e := m.FitFast(idx, 3, 500, 10, false); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(x, oldX) || !reflect.DeepEqual(y, oldY) {
		t.Fatal("shared candidate grid mutated")
	}
}

func TestOversizedGPUGridFallsBackWithIdenticalCPUScores(t *testing.T) {
	m, idx := scoringModel(4097, 1, true)
	s := m.subset(idx)
	x, y := makeCoarseGrid(50, 4)
	expected, e := m.sse(x, y, s, 3, nil)
	if e != nil {
		t.Fatal(e)
	}
	m.accelerated = true
	actual, e := m.scoreGrid(x, y, s, 3, nil, true, 12)
	if e != nil || !reflect.DeepEqual(expected, actual) || m.gpuCalls != 0 || m.gpuFallbacks != 1 {
		t.Fatal("oversized CPU fallback changed arithmetic", e, m.gpuCalls, m.gpuFallbacks)
	}
}
