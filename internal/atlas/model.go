package atlas

import (
	"errors"
	"math"
	"sort"

	"signal-atlas/internal/py"
)

// The location model, from Graphite's estimate_ap.py:
//
//	RSSI_i = A_radio - 10 * n * log10(max(distance_i, floor_m))
//
// Each radio gets an independent fitted intercept. Combining radios assumes they are co-located;
// an SSID alone does not establish this. No calibrated probabilities or confidence intervals are
// inferred.
//
// The search is the Python's step for step: a coarse grid over ±extent, the eight best
// well-separated cells refined on a 0.25 m (or step/4) grid, the best refined cell kept. numpy's
// linspace, arange and meshgrid orderings are reproduced so the same candidates are scored in the
// same order and ties resolve the same way. Each candidate is scored in two tight passes over
// the observations with no allocation (numpy builds a candidates × observations matrix).

const (
	EarthRadiusM    = 6371008.8
	MetresPerDegree = math.Pi * EarthRadiusM / 180
)

// ErrCancelled ends a fit when its job is cancelled.
var ErrCancelled = errors.New("Estimation cancelled")

// LocalFrame is local equirectangular metres around an origin, for small survey footprints.
type LocalFrame struct{ Lat, Lon, xScale float64 }

func newFrame(lat, lon float64) LocalFrame {
	return LocalFrame{lat, lon, MetresPerDegree * math.Cos(lat*math.Pi/180)}
}

// ToLatLon converts frame metres to degrees.
func (f LocalFrame) ToLatLon(x, y float64) (float64, float64) {
	return f.Lat + y/MetresPerDegree, f.Lon + x/f.xScale
}

// Linspace is numpy.linspace(start, stop, num).
func Linspace(start, stop float64, num int) []float64 {
	out := make([]float64, num)
	div := num - 1
	if div <= 0 {
		if num == 1 {
			out[0] = start
		}
		return out
	}
	step := (stop - start) / float64(div)
	for i := range out {
		out[i] = float64(i)*step + start
	}
	out[num-1] = stop
	return out
}

// Arange is numpy.arange(start, stop, step) for float64.
func Arange(start, stop, step float64) []float64 {
	n := math.Ceil((stop - start) / step)
	if !(n > 0) {
		return nil
	}
	out := make([]float64, int(n))
	out[0] = start
	if len(out) > 1 {
		out[1] = start + step
	}
	delta := (start + step) - start
	for i := 2; i < len(out); i++ {
		out[i] = start + float64(i)*delta
	}
	return out
}

// ObsCols is what the model needs of each observation.
type ObsCols struct {
	Lat, Lon, RSSI []float64
	MAC            []string // the intercept group: a MAC, or a radio key in shared fits
	Key            []string
}

// LocationModel is estimate_ap.LocationModel.
type LocationModel struct {
	n          int
	cols       ObsCols
	weights    []float64
	within     []float64
	Frame      LocalFrame
	x, y, rssi []float64
	floor      float64
	cancelled  func() bool
}

func wavg(v, w []float64) float64 {
	var s, t float64
	for i := range v {
		s += v[i] * w[i]
		t += w[i]
	}
	return s / t
}

// NewLocationModel validates the observations as the Python constructor does.
func NewLocationModel(cols ObsCols, floor float64, weights, within []float64, cancelled func() bool) (*LocationModel, error) {
	n := len(cols.RSSI)
	if n == 0 {
		return nil, py.Errorf("No observations to fit")
	}
	m := &LocationModel{n: n, cols: cols, floor: floor, cancelled: cancelled}
	if weights == nil {
		weights = make([]float64, n)
		for i := range weights {
			weights[i] = 1
		}
	}
	if within == nil {
		within = make([]float64, n)
	}
	if len(weights) != n || len(within) != n {
		return nil, py.Errorf("Invalid observation weights or within-location residuals")
	}
	for i := 0; i < n; i++ {
		w, s := weights[i], within[i]
		if math.IsNaN(w) || math.IsInf(w, 0) || math.IsNaN(s) || math.IsInf(s, 0) || w <= 0 || s < 0 {
			return nil, py.Errorf("Invalid observation weights or within-location residuals")
		}
	}
	m.weights, m.within = weights, within
	m.Frame = newFrame(wavg(cols.Lat, weights), wavg(cols.Lon, weights))
	m.x, m.y, m.rssi = make([]float64, n), make([]float64, n), cols.RSSI
	x0, x1, y0, y1 := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for i := 0; i < n; i++ {
		x := (cols.Lon[i] - m.Frame.Lon) * m.Frame.xScale
		y := (cols.Lat[i] - m.Frame.Lat) * MetresPerDegree
		m.x[i], m.y[i] = x, y
		x0, x1, y0, y1 = math.Min(x0, x), math.Max(x1, x), math.Min(y0, y), math.Max(y1, y)
	}
	if math.Max(x1-x0, y1-y0) > 20000 {
		return nil, py.Errorf("Survey exceeds 20 km or crosses the dateline; split it into local areas")
	}
	pos := map[[2]float64]bool{}
	for i := 0; i < n && len(pos) < 3; i++ {
		pos[[2]float64{m.x[i], m.y[i]}] = true
	}
	if len(pos) < 3 {
		return nil, py.Errorf("Need at least three distinct receiver positions")
	}
	return m, nil
}

func (m *LocationModel) uniqueMACs(idx []int) []string {
	set := map[string]bool{}
	for _, i := range idx {
		set[m.cols.MAC[i]] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ContrastCount is observations (by weight) minus radios.
func (m *LocationModel) ContrastCount(idx []int) int {
	var w float64
	for _, i := range idx {
		w += m.weights[i]
	}
	return int(w) - len(m.uniqueMACs(idx))
}

type subset struct {
	m, k           int
	x, y, r, w, sw []float64
	g              []int
	within         float64
	imp, gs        []float64
	macs           []string
}

func (m *LocationModel) subset(idx []int) *subset {
	macs := m.uniqueMACs(idx)
	gid := map[string]int{}
	for i, s := range macs {
		gid[s] = i
	}
	s := &subset{m: len(idx), k: len(macs), x: make([]float64, len(idx)), y: make([]float64, len(idx)), r: make([]float64, len(idx)),
		w: make([]float64, len(idx)), g: make([]int, len(idx)), sw: make([]float64, len(macs)), imp: make([]float64, len(idx)), gs: make([]float64, len(macs)), macs: macs}
	for j, i := range idx {
		s.x[j], s.y[j], s.r[j], s.w[j], s.g[j] = m.x[i], m.y[i], m.rssi[i], m.weights[i], gid[m.cols.MAC[i]]
		s.sw[s.g[j]] += m.weights[i]
		s.within += m.within[i]
	}
	return s
}

// sse profiles out the per-radio intercepts: the weighted squared residual at each candidate.
func (m *LocationModel) sse(px, py_ []float64, s *subset, exponent float64, out []float64) ([]float64, error) {
	if out == nil {
		out = make([]float64, len(px))
	}
	c := 10 * exponent
	floor := m.floor
	x, y, r, w, g, imp, gs, sw := s.x, s.y, s.r, s.w, s.g, s.imp, s.gs, s.sw
	for p := range px {
		if p&2047 == 0 && m.cancelled != nil && m.cancelled() {
			return nil, ErrCancelled
		}
		X, Y := px[p], py_[p]
		for q := range gs {
			gs[q] = 0
		}
		for j := 0; j < s.m; j++ {
			dx, dy := X-x[j], Y-y[j]
			d := math.Sqrt(dx*dx + dy*dy)
			if d < floor {
				d = floor
			}
			v := r[j] + c*math.Log10(d)
			imp[j] = v
			gs[g[j]] += v * w[j]
		}
		for q := range gs {
			gs[q] /= sw[q]
		}
		var t float64
		for j := 0; j < s.m; j++ {
			e := imp[j] - gs[g[j]]
			t += e * e * w[j]
		}
		out[p] = t + s.within
	}
	return out, nil
}

// Fit is one fit's result.
type Fit struct {
	Latitude, Longitude, EastM, NorthM, Exponent, SSE, RMS float64
	Intercepts                                             map[string]float64
	BoundaryLimited                                        bool
	ObservationCount, Informative                          int64
	Predictions                                            []map[string]any
}

// Map is the fit as the Python's dict.
func (f *Fit) Map() map[string]any {
	ic := map[string]any{}
	for k, v := range f.Intercepts {
		ic[k] = v
	}
	preds := make([]any, len(f.Predictions))
	for i, p := range f.Predictions {
		preds[i] = p
	}
	return map[string]any{"latitude": f.Latitude, "longitude": f.Longitude, "east_m": f.EastM, "north_m": f.NorthM,
		"path_loss_exponent": f.Exponent, "sse_db2": f.SSE, "residual_rms_db": f.RMS, "radio_intercepts_dbm_at_1m": ic,
		"boundary_limited": f.BoundaryLimited, "observation_count": f.ObservationCount, "informative_observation_count": f.Informative,
		"predictions": preds}
}

// Fit is LocationModel.fit(indices, exponent, extent_m, step_m, include_predictions).
func (m *LocationModel) Fit(idx []int, exponent, extentM, stepM float64, predictions bool) (*Fit, error) {
	if m.ContrastCount(idx) < 3 {
		return nil, py.Errorf("Need at least three within-radio contrasts (observations minus radios)")
	}
	s := m.subset(idx)
	axis := Linspace(-extentM, extentM, int(math.Ceil(2*extentM/stepM))+1)
	A := len(axis)
	px, pyy := make([]float64, A*A), make([]float64, A*A)
	for r := 0; r < A; r++ {
		for c := 0; c < A; c++ {
			px[r*A+c], pyy[r*A+c] = axis[c], axis[r]
		}
	}
	errs, err := m.sse(px, pyy, s, exponent, nil)
	if err != nil {
		return nil, err
	}
	order := make([]int, len(errs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return errs[order[a]] < errs[order[b]] })
	var starts [][2]float64
	sep := 3 * stepM
	for _, j := range order {
		if len(starts) == 8 {
			break
		}
		ok := true
		for _, st := range starts {
			dx, dy := px[j]-st[0], pyy[j]-st[1]
			if !(math.Sqrt(dx*dx+dy*dy) > sep) {
				ok = false
				break
			}
		}
		if ok {
			starts = append(starts, [2]float64{px[j], pyy[j]})
		}
	}
	refine := math.Min(.25, stepM/4)
	best, bx, by := math.Inf(1), 0.0, 0.0
	first := true
	for _, st := range starts {
		ax := Arange(math.Max(-extentM, st[0]-1.2*stepM), math.Min(extentM, st[0]+1.2*stepM)+refine/2, refine)
		ay := Arange(math.Max(-extentM, st[1]-1.2*stepM), math.Min(extentM, st[1]+1.2*stepM)+refine/2, refine)
		fx, fy := make([]float64, len(ax)*len(ay)), make([]float64, len(ax)*len(ay))
		for r := range ay {
			for c := range ax {
				fx[r*len(ax)+c] = math.Min(extentM, math.Max(-extentM, ax[c]))
				fy[r*len(ax)+c] = math.Min(extentM, math.Max(-extentM, ay[r]))
			}
		}
		scores, err := m.sse(fx, fy, s, exponent, nil)
		if err != nil {
			return nil, err
		}
		j := 0
		for i := 1; i < len(scores); i++ {
			if scores[i] < scores[j] {
				j = i
			}
		}
		if first || scores[j] < best {
			best, bx, by, first = scores[j], fx[j], fy[j], false
		}
	}
	dist := make([]float64, s.m)
	for j := 0; j < s.m; j++ {
		dx, dy := s.x[j]-bx, s.y[j]-by
		d := math.Sqrt(dx*dx + dy*dy)
		if d < m.floor {
			d = m.floor
		}
		dist[j] = d
	}
	f := &Fit{Exponent: exponent, SSE: best, EastM: bx, NorthM: by, Intercepts: map[string]float64{}}
	pred := make([]float64, s.m)
	var informative, wsum float64
	for gi, mac := range s.macs {
		var num, den float64
		for j := 0; j < s.m; j++ {
			if s.g[j] == gi {
				num += (s.r[j] + 10*exponent*math.Log10(dist[j])) * s.w[j]
				den += s.w[j]
			}
		}
		icpt := num / den
		f.Intercepts[mac] = icpt
		for j := 0; j < s.m; j++ {
			if s.g[j] == gi {
				pred[j] = icpt - 10*exponent*math.Log10(dist[j])
			}
		}
		if den > 1 {
			informative += den
		}
	}
	for j := 0; j < s.m; j++ {
		wsum += s.w[j]
	}
	f.Informative = int64(informative)
	f.ObservationCount = int64(wsum)
	f.RMS = math.Sqrt(best / float64(f.Informative))
	f.Latitude, f.Longitude = m.Frame.ToLatLon(bx, by)
	f.BoundaryLimited = math.Max(math.Abs(bx), math.Abs(by)) >= extentM-stepM
	f.Predictions = []map[string]any{}
	if predictions {
		for j, i := range idx {
			key := ""
			if m.cols.Key != nil {
				key = m.cols.Key[i]
			}
			f.Predictions = append(f.Predictions, map[string]any{"key": key, "distance_m": dist[j], "predicted_rssi_dbm": pred[j], "residual_db": m.rssi[i] - pred[j]})
		}
	}
	return f, nil
}

func stripFit(f *Fit) map[string]any {
	m := f.Map()
	delete(m, "predictions")
	delete(m, "radio_intercepts_dbm_at_1m")
	return m
}

// EstimateOpts are estimate()'s keyword arguments.
type EstimateOpts struct {
	Exponent, ExtentM, StepM, FloorM, RadiusM float64
	Sensitivity                               bool
}

// DefaultEstimateOpts are estimate()'s defaults.
func DefaultEstimateOpts() EstimateOpts {
	return EstimateOpts{Exponent: 3, ExtentM: 500, StepM: 5, FloorM: 3, RadiusM: 30, Sensitivity: true}
}

// Estimate is estimate_ap.estimate: the single-network report (primary fit, sensitivity to the
// exponent, leave-one-out shifts, per-radio fits, a descriptive RMS landscape, warnings).
func Estimate(observations []*Observation, o EstimateOpts) (map[string]any, error) {
	for _, v := range []float64{o.Exponent, o.ExtentM, o.StepM, o.FloorM, o.RadiusM} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return nil, py.Errorf("Exponent, grid dimensions, distance floor and radius must be positive and finite")
		}
	}
	if len(observations) > 5000 {
		return nil, py.Errorf("Use import_survey.py and serve_survey.py for more than 5,000 observations; the explorer uses bounded per-radio fitting")
	}
	if math.Ceil(2*o.ExtentM/o.StepM) > 1000 {
		return nil, py.Errorf("Grid exceeds 1,002,001 candidates; increase --grid-step-m")
	}
	cols := ObsCols{}
	for _, ob := range observations {
		cols.Lat, cols.Lon, cols.RSSI = append(cols.Lat, ob.Latitude), append(cols.Lon, ob.Longitude), append(cols.RSSI, ob.RSSI)
		cols.MAC, cols.Key = append(cols.MAC, ob.MAC), append(cols.Key, ob.Key)
	}
	m, err := NewLocationModel(cols, o.FloorM, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	idx := make([]int, len(observations))
	for i := range idx {
		idx[i] = i
	}
	primary, err := m.Fit(idx, o.Exponent, o.ExtentM, o.StepM, true)
	if err != nil {
		return nil, err
	}
	trials, omissions, radioFits := []any{}, []any{}, []any{}
	if o.Sensitivity {
		ns := map[float64]bool{2: true, 2.5: true, 3: true, 3.5: true, 4: true, o.Exponent: true}
		var list []float64
		for n := range ns {
			list = append(list, n)
		}
		sort.Float64s(list)
		for _, n := range list {
			f := primary
			if n != o.Exponent {
				if f, err = m.Fit(idx, n, o.ExtentM, o.StepM, true); err != nil {
					return nil, err
				}
			}
			trials = append(trials, stripFit(f))
		}
		for _, i := range idx {
			keep := make([]int, 0, len(idx)-1)
			for _, j := range idx {
				if j != i {
					keep = append(keep, j)
				}
			}
			if m.ContrastCount(keep) < 3 {
				continue
			}
			f, err := m.Fit(keep, o.Exponent, o.ExtentM, o.StepM, true)
			if err != nil {
				return nil, err
			}
			omissions = append(omissions, map[string]any{"omitted_key": observations[i].Key, "omitted_id": observations[i].ID, "latitude": f.Latitude, "longitude": f.Longitude,
				"shift_m": math.Hypot(f.EastM-primary.EastM, f.NorthM-primary.NorthM), "boundary_limited": f.BoundaryLimited})
		}
		for _, mac := range m.uniqueMACs(idx) {
			var keep []int
			for _, j := range idx {
				if cols.MAC[j] == mac {
					keep = append(keep, j)
				}
			}
			if m.ContrastCount(keep) >= 3 {
				f, err := m.Fit(keep, o.Exponent, o.ExtentM, o.StepM, true)
				if err != nil {
					return nil, err
				}
				r := stripFit(f)
				r["mac"] = mac
				radioFits = append(radioFits, r)
			}
		}
	}
	peak := math.Inf(-1)
	for _, r := range m.rssi {
		peak = math.Max(peak, r)
	}
	var sw, sx, sy float64
	for i := 0; i < m.n; i++ {
		w := math.Pow(10, (m.rssi[i]-peak)/10)
		sw, sx, sy = sw+w, sx+m.x[i]*w, sy+m.y[i]*w
	}
	cLat, cLon := m.Frame.ToLatLon(sx/sw, sy/sw)
	// Descriptive surface, deliberately not normalized into a probability density.
	ax := Linspace(-200, 200, 41)
	lx, ly := make([]float64, 41*41), make([]float64, 41*41)
	for r := 0; r < 41; r++ {
		for c := 0; c < 41; c++ {
			lx[r*41+c], ly[r*41+c] = primary.EastM+ax[c], primary.NorthM+ax[r]
		}
	}
	flat, err := m.sse(lx, ly, m.subset(idx), o.Exponent, nil)
	if err != nil {
		return nil, err
	}
	rms := make([]any, 41)
	for r := 0; r < 41; r++ {
		row := make([]any, 41)
		for c := 0; c < 41; c++ {
			row[c] = math.RoundToEven(math.Sqrt(flat[r*41+c]/float64(primary.Informative))*1000) / 1000
		}
		rms[r] = row
	}
	warnings := []any{
		"The search radius is a user-chosen planning radius, not a confidence interval.",
		"RSSI-to-distance parameters are uncalibrated; walls, antenna patterns and changes between surveys can bias the fit.",
		"GPS accuracy describes receiver positions, not access point location uncertainty.",
		"Measurements are 2D; receiver altitudes do not establish transmitter height.",
	}
	macs := m.uniqueMACs(idx)
	if len(macs) > 1 {
		warnings = append([]any{"Combined fit assumes all selected MAC addresses are stationary and co-located; a shared SSID does not prove this."}, warnings...)
	}
	dates := map[string]bool{}
	for _, ob := range observations {
		dates[ob.Date] = true
	}
	if len(dates) > 1 {
		warnings = append(warnings, "Survey dates differ; constant transmitter position and per-radio power are assumptions.")
	}
	if primary.BoundaryLimited {
		warnings = append(warnings, "Best fit is near the search boundary: enlarge --extent-m before interpreting it.")
	}
	if len(observations) > 2 {
		n := float64(m.n)
		var mx, my float64
		for i := 0; i < m.n; i++ {
			mx, my = mx+m.x[i], my+m.y[i]
		}
		mx, my = mx/n, my/n
		var a, b, d float64
		for i := 0; i < m.n; i++ {
			u, v := m.x[i]-mx, m.y[i]-my
			a, b, d = a+u*u, b+u*v, d+v*v
		}
		a, b, d = a/(n-1), b/(n-1), d/(n-1)
		tr, disc := (a+d)/2, math.Sqrt(math.Pow((a-d)/2, 2)+b*b)
		e0, e1 := tr-disc, tr+disc
		if e0 <= 0 || e1/math.Max(e0, 1e-9) > 20 {
			warnings = append(warnings, "Receiver positions have weak cross-track geometry; alternative locations may fit similarly.")
		}
	}
	var singles []string
	for _, mac := range macs {
		k := 0
		for _, x := range cols.MAC {
			if x == mac {
				k++
			}
		}
		if k == 1 {
			singles = append(singles, mac)
		}
	}
	if len(singles) > 0 {
		s := ""
		for i, x := range singles {
			if i > 0 {
				s += ", "
			}
			s += x
		}
		warnings = append(warnings, "Single-observation radios provide no position information with free intercepts: "+s)
	}
	obsList := make([]any, len(observations))
	for i, ob := range observations {
		obsList[i] = ob.Map()
	}
	swLat, swLon := m.Frame.ToLatLon(primary.EastM-200, primary.NorthM-200)
	neLat, neLon := m.Frame.ToLatLon(primary.EastM+200, primary.NorthM+200)
	return map[string]any{
		"schema_version": int64(1), "ssid": observations[0].SSID, "observations": obsList,
		"estimate": primary.Map(), "search_radius_m": o.RadiusM,
		"method": "Profile least squares of log-distance RSSI, with one intercept per MAC",
		"settings": map[string]any{"path_loss_exponent": o.Exponent, "distance_floor_m": o.FloorM, "extent_m": o.ExtentM, "coarse_grid_step_m": o.StepM,
			"refined_grid_step_m": math.Min(.25, o.StepM/4), "origin_latitude": m.Frame.Lat, "origin_longitude": m.Frame.Lon},
		"sensitivity":                map[string]any{"path_loss_exponents": trials, "leave_one_out": omissions, "individual_radios": radioFits},
		"weighted_receiver_centroid": map[string]any{"latitude": cLat, "longitude": cLon, "note": "Signal-power-weighted receiver centroid; not a triangulated AP position"},
		"landscape": map[string]any{"width": int64(41), "height": int64(41), "southwest": []any{swLat, swLon}, "northeast": []any{neLat, neLon},
			"rms_db_south_to_north": rms},
		"warnings": warnings,
	}, nil
}
