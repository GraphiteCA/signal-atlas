package atlas

import (
	"math"
	"sort"
)

// Benchmark-only adaptation of WifiLateration/internal/atlas/fit.go's donor
// multistart-pattern-v1. Reuse the upstream prepared observations/weights so
// this compares optimizers, rather than different grouping/sample policies.
// It has no production dispatch, cache namespace, setting or DB dependency.
func patternCandidate(m *LocationModel, idx []int) (*Fit, error) {
	s := m.subset(idx)
	means, weights := make([]float64, s.k), make([]float64, s.k)
	score := func(x, y float64) (float64, error) {
		if m.cancelled != nil && m.cancelled() {
			return 0, ErrCancelled
		}
		clear(means)
		clear(weights)
		total := s.within
		for i := range s.x {
			dx, dy := x-s.x[i], y-s.y[i]
			z := s.r[i] + (15/math.Ln10)*math.Log(math.Max(9, dx*dx+dy*dy))
			g := s.g[i]
			delta := z - means[g]
			next := weights[g] + s.w[i]
			means[g] += delta * s.w[i] / next
			total += s.w[i] * delta * (z - means[g])
			weights[g] = next
		}
		return math.Max(0, total), nil
	}
	type point struct{ x, y, v float64 }
	grid := make([]point, 0, 289)
	for y := -500.; y <= 500; y += 62.5 {
		for x := -500.; x <= 500; x += 62.5 {
			v, e := score(x, y)
			if e != nil {
				return nil, e
			}
			grid = append(grid, point{x, y, v})
		}
	}
	sort.Slice(grid, func(i, j int) bool { return grid[i].v < grid[j].v })
	center, e := score(0, 0)
	if e != nil {
		return nil, e
	}
	starts := []point{{0, 0, center}}
	for _, p := range grid {
		separate := true
		for _, q := range starts {
			if math.Hypot(p.x-q.x, p.y-q.y) < 90 {
				separate = false
				break
			}
		}
		if separate {
			starts = append(starts, p)
		}
		if len(starts) == 6 {
			break
		}
	}
	best := grid[0]
	for _, p := range starts {
		for step, iter := 31.25, 0; step >= .125 && iter < 200; iter++ {
			next := p
			for _, delta := range [][2]float64{{1, 0}, {-1, 0}, {0, 1}, {0, -1}, {1, 1}, {1, -1}, {-1, 1}, {-1, -1}} {
				x, y := math.Max(-500, math.Min(500, p.x+delta[0]*step)), math.Max(-500, math.Min(500, p.y+delta[1]*step))
				v, e := score(x, y)
				if e != nil {
					return nil, e
				}
				if v < next.v {
					next = point{x, y, v}
				}
			}
			if next.v >= p.v {
				step /= 2
			} else {
				p = next
			}
		}
		if p.v < best.v {
			best = p
		}
	}
	// Compare final objective using the production arithmetic, independent of
	// Welford/log-squared differences in the candidate's search implementation.
	v, e := m.sse([]float64{best.x}, []float64{best.y}, s, 3, nil)
	if e != nil {
		return nil, e
	}
	return m.finish(idx, s, 3, 500, 10, v[0], best.x, best.y, false), nil
}
