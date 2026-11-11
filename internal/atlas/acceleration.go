package atlas

import (
	"errors"
	"math"

	"signal-atlas/internal/gpu"
	"signal-atlas/internal/py"
)

const GPUReference = "opencl64-reference-v1"
const GPUFast = "opencl64-fast-v1"

func SolverName(fast, accelerated bool) string {
	if accelerated {
		if fast {
			return GPUFast
		}
		return GPUReference
	}
	if fast {
		return "fast"
	}
	return "reference"
}
func validSolver(s string) bool {
	return s == "reference" || s == "fast" || s == GPUReference || s == GPUFast
}
func (m *LocationModel) scoreGrid(x, y []float64, s *subset, exponent float64, out []float64, coarse bool, separation float64) ([]float64, error) {
	if !m.accelerated || len(x) < 512 {
		return m.sse(x, y, s, exponent, out)
	}
	groups := make([]int32, len(s.g))
	for i, g := range s.g {
		groups[i] = int32(g)
	}
	v, e := gpu.Score(gpu.Input{X: s.x, Y: s.y, RSSI: s.r, Weights: s.w, WeightSums: s.sw, Groups: groups, CandidateX: x, CandidateY: y, Floor: m.floor, Factor: 10 * exponent, Within: s.within}, m.cancelled)
	if errors.Is(e, gpu.ErrCancelled) {
		return nil, ErrCancelled
	}
	if e != nil {
		m.gpuFallbacks++
		return m.sse(x, y, s, exponent, out)
	}
	m.gpuCalls++
	cut := math.Inf(1)
	for _, value := range v {
		cut = math.Min(cut, value)
	}
	if coarse {
		starts, err := coarseStarts(x, y, v, separation, m.cancelled)
		if err != nil {
			return nil, err
		}
		for _, st := range starts {
			for i := range x {
				if x[i] == st[0] && y[i] == st[1] {
					cut = math.Max(cut, v[i])
					break
				}
			}
		}
	}
	// Resolve competitive cells and nearly equal minima using the original
	// float64 CPU arithmetic/order. Flat or ill-conditioned grids may require
	// many CPU checks; numerical behavior takes priority over a kernel speedup.
	margin := 1e-10 * math.Max(1, math.Abs(cut))
	var cx, cy []float64
	var indices []int
	for i, value := range v {
		if value <= cut+margin {
			cx = append(cx, x[i])
			cy = append(cy, y[i])
			indices = append(indices, i)
		}
	}
	exact, err := m.sse(cx, cy, s, exponent, nil)
	if err != nil {
		return nil, err
	}
	for j, i := range indices {
		if math.Abs(v[i]-exact[j]) > 1e-8*math.Max(1, math.Abs(exact[j])) {
			gpu.Disable("GPU score validation differed from CPU; CPU fallback is active")
			m.gpuFallbacks++
			return m.sse(x, y, s, exponent, out)
		}
		v[i] = exact[j]
	}
	if out != nil {
		copy(out, v)
		return out, nil
	}
	return v, nil
}

func solverFromParams(p py.Params) (string, error) {
	s := p.First("solver", "reference")
	if !validSolver(s) {
		return "", py.Errorf("Choose reference, fast, or a supported versioned GPU solver")
	}
	return s, nil
}
