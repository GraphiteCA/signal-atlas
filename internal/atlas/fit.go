package atlas

import (
	"sort"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"

	"zombiezen.com/go/sqlite"
)

// Keep the reference sort order, with cancellation boundaries inside large
// sample sorts. Only our private sentinel is recovered.
func fitSortStable(values any, less func(int, int) bool, cancelled func() bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if r == ErrCancelled {
				err = ErrCancelled
			} else {
				panic(r)
			}
		}
	}()
	n := 0
	sort.SliceStable(values, func(i, j int) bool {
		n++
		if n&4095 == 0 && cancelled != nil && cancelled() {
			panic(ErrCancelled)
		}
		return less(i, j)
	})
	if cancelled != nil && cancelled() {
		return ErrCancelled
	}
	return nil
}

// FitRadio is serve_survey.Jobs.fit_radio(group, params): one radio, or one shared group, fitted
// from the store. Many run at once, one per core.
func (a *Atlas) FitRadio(group Group, p py.Params, cancelled func() bool) (map[string]any, error) {
	check := func() error {
		if cancelled != nil && cancelled() {
			return ErrCancelled
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, err
	}
	scope, _, err := EstimateScope(p)
	if err != nil {
		return nil, err
	}
	f, st, err := a.compile(p, false)
	if err != nil {
		return nil, err
	}
	mem := members(group)
	if mem == nil {
		mem = []Group{group}
	}
	keys := make([]string, len(mem))
	for i, m := range mem {
		keys[i] = gs(m, "radio_key")
	}
	shared, isBool := group["co_location_assumed"].(bool)
	if !isBool {
		_, shared = group["members"]
	}
	mode := scope["fit_mode"].(string)
	result := map[string]any{}
	for k, v := range group {
		result[k] = v
	}
	var sampleLimit any
	if mode == "sample" {
		sampleLimit = int64(256 * len(keys))
	}
	result["status"], result["co_location_assumed"], result["sample_limit"], result["fit_mode"], result["observation_scope"] = "insufficient", shared, sampleLimit, mode, scope
	var match []uint32
	var count int64
	var dmin, dmax any
	if mode == "sample" {
		match, count, dmin, dmax, err = selectSampleRows(st, a.Obs.Dict, f, keys, cancelled)
		if err != nil {
			return nil, err
		}
	} else {
		// the matching rows of these radios, in id order
		var rows []uint32
		for _, k := range keys {
			if id, ok := a.Obs.Dict.Lookup(k); ok {
				rows = append(rows, st.ByRadio[id]...)
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i] < rows[j] })
		match = rows[:0:0]
		for n, g := range rows {
			if n&4095 == 0 {
				if err := check(); err != nil {
					return nil, err
				}
			}
			s, i := st.Loc(g)
			if !f.Match(s, i) {
				continue
			}
			match = append(match, g)
			d := st.Str[s.Date[i]]
			if dmin == nil || d < dmin.(string) {
				dmin = d
			}
			if dmax == nil || d > dmax.(string) {
				dmax = d
			}
		}
		count = int64(len(match))
	}
	result["count"], result["first_date"], result["last_date"] = count, dmin, dmax
	if count < 4 {
		result["reason"] = "Fewer than four observations within the selected dates and filters"
		return result, nil
	}
	type smp struct {
		id             int64
		radio, mac     uint32
		lat, lon, rssi float64
		weight, within float64
	}
	var sample []smp
	var weights, within []float64
	switch {
	case mode == "all":
		// GROUP BY radio_key,latitude,longitude ORDER BY min(id), identical positions aggregated
		type gk struct {
			radio    uint32
			lat, lon float64
		}
		type agg struct {
			minID int64
			mac   uint32
			r, r2 kbn
			n     int64
		}
		m := map[gk]*agg{}
		var order []gk
		for n, g := range match {
			if n&4095 == 0 {
				if err := check(); err != nil {
					return nil, err
				}
			}
			s, i := st.Loc(g)
			k := gk{s.Radio[i], s.Lat[i], s.Lon[i]}
			x := m[k]
			if x == nil {
				x = &agg{minID: s.ID(i), mac: s.MAC[i]}
				m[k] = x
				order = append(order, k)
			}
			x.r.add(s.RSSI[i])
			x.r2.add(s.RSSI[i] * s.RSSI[i])
			x.n++
		}
		for _, k := range order { // first appearance in id order is min(id) order
			x := m[k]
			t, t2 := x.r.total(), x.r2.total()
			w := t2 - t*t/float64(x.n)
			if w < 0 {
				w = 0
			}
			sample = append(sample, smp{id: x.minID, radio: k.radio, mac: x.mac, lat: k.lat, lon: k.lon, rssi: t / float64(x.n), weight: float64(x.n), within: w})
			weights, within = append(weights, float64(x.n)), append(within, w)
		}
	default:
		for _, g := range match {
			s, i := st.Loc(g)
			sample = append(sample, smp{id: s.ID(i), radio: s.Radio[i], mac: s.MAC[i], lat: s.Lat[i], lon: s.Lon[i], rssi: s.RSSI[i]})
		}
	}
	var aliases map[string]string
	if shared {
		aliases = UnknownBandAliases(mem)
	}
	cols := ObsCols{}
	for _, x := range sample {
		cols.Lat, cols.Lon, cols.RSSI = append(cols.Lat, x.lat), append(cols.Lon, x.lon), append(cols.RSSI, x.rssi)
		// Independent power intercept for every BSSID/band, including when the same MAC was
		// reported on more than one band.
		g := st.Str[x.mac]
		if shared {
			g = st.Str[x.radio]
			if al, ok := aliases[g]; ok {
				g = al
			}
		}
		cols.MAC, cols.Key = append(cols.MAC, g), append(cols.Key, py.Str(x.id))
	}
	used := int64(len(sample))
	if mode == "all" {
		used = count
	}
	result["sampled_observations"], result["used_observations"], result["coordinate_groups"] = used, used, int64(len(sample))
	model, err := NewLocationModel(cols, 3, weights, within, cancelled)
	if err == nil {
		solver := p.First("solver", "reference")
		model.accelerated = solver == GPUReference || solver == GPUFast
		idx := make([]int, len(sample))
		for i := range idx {
			idx[i] = i
		}
		var fit *Fit
		fitter := model.Fit
		if solver == fitSolverFast || solver == GPUFast {
			fitter = model.FitFast
		}
		if fit, err = fitter(idx, 3.0, 500, 10, false); err == nil {
			note := "Fast sampled estimate. "
			if mode == "all" {
				note = "All matching observations; identical GPS positions are exactly aggregated. "
			}
			result["status"], result["latitude"], result["longitude"] = "fitted", fit.Latitude, fit.Longitude
			result["residual_rms_db"], result["boundary_limited"], result["path_loss_exponent"] = fit.RMS, fit.BoundaryLimited, 3.0
			result["note"] = note + "Fixed-power, stationary-radio model. Planning radius is not a confidence interval."
			if model.accelerated {
				backend := "opencl64-v1"
				if model.gpuCalls == 0 {
					backend = "cpu-fallback"
				}
				result["acceleration"] = map[string]any{"backend": backend, "gpu_grids": model.gpuCalls, "cpu_fallback_grids": model.gpuFallbacks, "solver": solver}
			}
		}
	}
	if err != nil {
		if !py.IsValueError(err) {
			return nil, err
		}
		result["reason"] = err.Error()
	}
	return result, nil
}

// FitAndStore fits a group and saves the result, as the Python job loop does after each fit.
func (a *Atlas) FitAndStore(c *sqlite.Conn, group Group, p py.Params, scopeKey string, cancelled func() bool) (string, error) {
	a.WriteMu.Lock()
	key := groupTarget(group)
	version, err := targetVersion(c, key)
	a.WriteMu.Unlock()
	if err != nil {
		return "", err
	}
	r, err := a.FitRadio(group, p, cancelled)
	if err != nil {
		return "", err
	}
	text, err := py.Dumps(r, py.DumpOpts{})
	if err != nil {
		return "", err
	}
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	if cancelled != nil && cancelled() {
		return "", ErrCancelled
	}
	current, err := targetVersion(c, key)
	if err != nil {
		return "", err
	}
	if current != version {
		return "", ErrStaleInput
	}
	if _, err := db.Exec(c, "INSERT OR REPLACE INTO estimates(radio_key,scope_key,result) VALUES(?,?,?)", gs(group, "radio_key"), scopeKey, text); err != nil {
		return "", err
	}
	return r["status"].(string), nil
}

var _ = obs.Null
