package atlas

import (
	"context"
	"math"

	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"

	"zombiezen.com/go/sqlite"
)

// ---------------------------------------------------------------------------
// Data for the GPU map (ui/map.html): every observation in view as packed numbers, thinned evenly
// past a limit, and every saved fit in view. The page draws them with deck.gl.

// Points are observations in view: XY holds (east, north) offsets in degrees from (Lon0, Lat0)
// as float32 pairs, which keeps them to a fraction of a metre; RSSI is each one's signal.
type Points struct {
	Lon0, Lat0   float64
	XY, RSSI     []float32
	Total, Shown int
}

// Bounds is the box around every observation (west, south, east, north), or false when empty.
func (a *Atlas) Bounds() ([4]float64, bool) {
	st := a.Obs.State()
	b := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for _, s := range st.Segs {
		for i := 0; i < s.N; i++ {
			b[0], b[2] = math.Min(b[0], s.Lon[i]), math.Max(b[2], s.Lon[i])
			b[1], b[3] = math.Min(b[1], s.Lat[i]), math.Max(b[3], s.Lat[i])
		}
	}
	return b, b[0] <= b[2]
}

// PointsIn returns the observations the filters keep (bbox included), at most max of them: past
// that, every k-th, so the whole view is covered evenly.
func (a *Atlas) PointsIn(p py.Params, max int) (*Points, error) {
	return a.PointsInContext(context.Background(), p, max)
}

func (a *Atlas) PointsInContext(ctx context.Context, p py.Params, max int) (*Points, error) {
	f, _, err := a.compile(p, true)
	if err != nil {
		return nil, err
	}
	counts := ScanContext(ctx, f, func() *int { return new(int) }, func(n *int, _ uint32, _ *obs.Segment, _ int) { *n++ })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	total := 0
	for _, n := range counts {
		total += *n
	}
	out := &Points{Total: total}
	if total == 0 {
		return out, nil
	}
	every := 1
	if max > 0 && total > max {
		every = (total + max - 1) / max
	}
	if b, err := Bbox(p.First("bbox", "")); err == nil {
		out.Lon0, out.Lat0 = (b[0]+b[2])/2, (b[1]+b[3])/2
	}
	type acc struct {
		k        int
		xy, rssi []float32
	}
	lon0, lat0 := out.Lon0, out.Lat0
	parts := ScanContext(ctx, f, func() *acc { return &acc{} }, func(a *acc, _ uint32, s *obs.Segment, i int) {
		a.k++
		if a.k%every != 0 {
			return
		}
		a.xy = append(a.xy, float32(s.Lon[i]-lon0), float32(s.Lat[i]-lat0))
		a.rssi = append(a.rssi, float32(s.RSSI[i]))
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n := 0
	for _, pt := range parts {
		n += len(pt.rssi)
	}
	out.XY, out.RSSI = make([]float32, 0, 2*n), make([]float32, 0, n)
	for _, pt := range parts {
		out.XY = append(out.XY, pt.xy...)
		out.RSSI = append(out.RSSI, pt.rssi...)
	}
	out.Shown = n
	return out, nil
}

// MapFit is one saved fit for the GPU map.
type MapFit struct {
	ID              string  `json:"id"`
	Band            string  `json:"band"`
	Lat             float64 `json:"lat"`
	Lon             float64 `json:"lon"`
	SSID            string  `json:"ssid"`
	MAC             string  `json:"mac"`
	Shared          bool    `json:"shared"`
	Used            int64   `json:"used"`
	RMS             float64 `json:"rms"`
	BoundaryLimited bool    `json:"edge"`
}

// FitsIn returns every fitted location in the bbox saved under the filters' nearby-fit scope
// (sample mode, or all-observations fits where there are both). No cap: the page draws them on
// the GPU.
func (a *Atlas) FitsIn(c *sqlite.Conn, p py.Params) ([]MapFit, error) {
	idx, _, err := a.mapIndex(context.Background(), c, p)
	if err != nil {
		return nil, err
	}
	b := [4]float64{-180, -85.051129, 180, 85.051129}
	if p.Has("bbox") {
		b, err = Bbox(p.First("bbox", ""))
		if err != nil {
			return nil, err
		}
	}
	out := []MapFit{}
	idx.in(b, func(r MapFit) { out = append(out, r) })
	return out, nil
}
