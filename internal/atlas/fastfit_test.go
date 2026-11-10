package atlas

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// The fast solver lands where the exact grid search does on almost every network, several times
// faster.
func TestFastSolverAgreesWithExact(t *testing.T) {
	rnd := rand.New(rand.NewSource(11))
	var same, near, total int
	var worst float64
	var tExact, tFast time.Duration
	for n := 0; n < 300; n++ {
		// an access point somewhere within 150 m, heard 20-120 times from a drive past it, with
		// one to three radios and realistic noise
		apx, apy := rnd.Float64()*300-150, rnd.Float64()*300-150
		radios := 1 + rnd.Intn(3)
		k := 20 + rnd.Intn(100)
		cols := ObsCols{}
		var weights []float64
		for i := 0; i < k; i++ {
			x := rnd.Float64()*400 - 200
			y := apy + rnd.Float64()*120 - 60 + 40*math.Sin(x/90)
			d := math.Max(3, math.Hypot(x-apx, y-apy))
			r := rnd.Intn(radios)
			rssi := -30 - float64(r)*4 - 30*math.Log10(d) + rnd.NormFloat64()*4
			lat := 48.449 + y/MetresPerDegree
			lon := 2.668 + x/(MetresPerDegree*math.Cos(48.449*math.Pi/180))
			cols.Lat = append(cols.Lat, lat)
			cols.Lon = append(cols.Lon, lon)
			cols.RSSI = append(cols.RSSI, rssi)
			cols.MAC = append(cols.MAC, []string{"A", "B", "C"}[r])
			weights = append(weights, 1)
		}
		m, err := NewLocationModel(cols, 3, weights, make([]float64, k), nil)
		if err != nil {
			continue
		}
		idx := make([]int, k)
		for i := range idx {
			idx[i] = i
		}
		t0 := time.Now()
		ex, err := m.Fit(idx, 3, 500, 10, false)
		tExact += time.Since(t0)
		if err != nil {
			continue
		}
		t0 = time.Now()
		fa, err := m.FitFast(idx, 3, 500, 10, false)
		tFast += time.Since(t0)
		if err != nil {
			t.Fatal(err)
		}
		total++
		d := math.Hypot(ex.EastM-fa.EastM, ex.NorthM-fa.NorthM)
		if d == 0 {
			same++
		}
		if d <= 1 {
			near++
		}
		worst = math.Max(worst, d)
		if fa.SSE < ex.SSE-1e-9 {
			t.Fatalf("network %d: the fast fit beat the exact grid (%v < %v)", n, fa.SSE, ex.SSE)
		}
	}
	speed := float64(tExact) / float64(tFast)
	t.Logf("%d networks: %d at the same point, %d within 1 m, worst %.1f m; exact %v, fast %v (%.1fx)", total, same, near, worst, tExact, tFast, speed)
	if float64(near) < 0.95*float64(total) {
		t.Fatalf("only %d of %d within 1 m of the exact fit", near, total)
	}
	if speed < 3 {
		t.Fatalf("only %.1fx faster", speed)
	}
}
