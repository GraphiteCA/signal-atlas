package atlas

import (
	"math"

	"signal-atlas/internal/py"
)

// ---------------------------------------------------------------------------
// The fast solver: an option, off by default. Fit scores a coarse grid, then every point of a
// 0.25 m grid around each of the eight best well-separated cells (about 85,000 candidates for
// the app's 500 m / 10 m search). FitFast scores the same coarse grid and starts from the same
// eight cells, but walks downhill from each (a compass search, halving its stride down to the fine
// step) and then scores the 3 × 3 fine-grid points around where it lands: about 10,000 candidates
// in all. On a smooth error surface it lands on the same fine-grid point as Fit; where the
// surface has several dips inside one start's box it can stop in another, so fast fits are kept
// apart from exact ones (EstimateScope's "solver").

// fitSolverFast is the "solver" parameter value that selects FitFast.
const fitSolverFast = "fast"

// FitFast is Fit with a local search in place of the fine grid.
func (m *LocationModel) FitFast(idx []int, exponent, extentM, stepM float64, predictions bool) (*Fit, error) {
	if m.ContrastCount(idx) < 3 {
		return nil, py.Errorf("Need at least three within-radio contrasts (observations minus radios)")
	}
	s := m.subset(idx)
	px, pyy := coarseGrid(extentM, stepM)
	errs, err := m.scoreGrid(px, pyy, s, exponent, nil, true, 3*stepM)
	if err != nil {
		return nil, err
	}
	starts, err := coarseStarts(px, pyy, errs, 3*stepM, m.cancelled)
	if err != nil {
		return nil, err
	}

	refine := math.Min(.25, stepM/4)
	var pointScore [1]float64
	one := func(x, y float64) (float64, error) {
		v, err := m.sse([]float64{x}, []float64{y}, s, exponent, pointScore[:])
		if err != nil {
			return 0, err
		}
		return v[0], nil
	}
	best, bx, by := math.Inf(1), 0.0, 0.0
	first := true
	for _, st := range starts {
		// the box Fit's fine grid covers for this start
		lox, hix := math.Max(-extentM, st[0]-1.2*stepM), math.Min(extentM, st[0]+1.2*stepM)
		loy, hiy := math.Max(-extentM, st[1]-1.2*stepM), math.Min(extentM, st[1]+1.2*stepM)
		x, y := st[0], st[1]
		v, err := one(x, y)
		if err != nil {
			return nil, err
		}
		for h := stepM / 2; h >= refine/2; {
			moved := false
			for _, d := range [4][2]float64{{h, 0}, {-h, 0}, {0, h}, {0, -h}} {
				nx, ny := math.Min(hix, math.Max(lox, x+d[0])), math.Min(hiy, math.Max(loy, y+d[1]))
				if nx == x && ny == y {
					continue
				}
				nv, err := one(nx, ny)
				if err != nil {
					return nil, err
				}
				if nv < v {
					x, y, v, moved = nx, ny, nv, true
				}
			}
			if !moved {
				h /= 2
			}
		}
		// polish on Fit's fine grid: its points nearest the landing spot, and their neighbours
		gx := lox + math.Round((x-lox)/refine)*refine
		gy := loy + math.Round((y-loy)/refine)*refine
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				cx := math.Min(extentM, math.Max(-extentM, math.Min(hix, math.Max(lox, gx+float64(dx)*refine))))
				cy := math.Min(extentM, math.Max(-extentM, math.Min(hiy, math.Max(loy, gy+float64(dy)*refine))))
				cv, err := one(cx, cy)
				if err != nil {
					return nil, err
				}
				if first || cv < best {
					best, bx, by, first = cv, cx, cy, false
				}
			}
		}
	}
	return m.finish(idx, s, exponent, extentM, stepM, best, bx, by, predictions), nil
}
