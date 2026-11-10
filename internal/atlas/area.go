package atlas

import (
	"encoding/json"
	"math"
	"sort"
	"strings"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"

	"zombiezen.com/go/sqlite"
)

// area_estimates.py: saved AP locations in the viewport, and bounded nearby fits.
const (
	MinZoom      = 16
	MaxLocations = 60 // limit discovery work, never the number of already fitted APs on the map
	BatchSize    = 8
)

type targetMap struct {
	keys []string
	m    map[string]Group
}

func (t *targetMap) set(k string, g Group) {
	if _, ok := t.m[k]; !ok {
		t.keys = append(t.keys, k)
	}
	t.m[k] = g
}

// overviewTargets is overview_targets(db, scoped, ssid, hidden_mac, individual, shared).
func (a *Atlas) overviewTargets(scoped py.Params, ssid, hiddenMAC string, individual, shared bool) (*targetMap, error) {
	f, st, err := a.compile(scoped, false)
	if err != nil {
		return nil, err
	}
	sid, ok := a.Obs.Dict.Lookup(ssid)
	var mid uint32
	okMac := true
	if ssid == "" {
		mid, okMac = a.Obs.Dict.Lookup(hiddenMAC)
	}
	t := &targetMap{m: map[string]Group{}}
	if !ok || !okMac {
		return t, nil
	}
	type key struct{ mac, band uint32 }
	agg := map[key]*radioAgg{}
	for _, g := range st.BySSID[sid] {
		s, i := st.Loc(g)
		if ssid == "" && s.MAC[i] != mid {
			continue
		}
		if !f.Match(s, i) {
			continue
		}
		k := key{s.MAC[i], s.Band[i]}
		r := agg[k]
		if r == nil {
			r = &radioAgg{radio: s.Radio[i], ssid: s.SSID[i], mac: s.MAC[i], band: s.Band[i]}
			agg[k] = r
		}
		r.count++
	}
	list := make([]*radioAgg, 0, len(agg))
	for _, r := range agg {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		if st.Str[list[i].mac] != st.Str[list[j].mac] {
			return st.Str[list[i].mac] < st.Str[list[j].mac]
		}
		return st.Str[list[i].band] < st.Str[list[j].band]
	})
	mem := make([]Group, len(list))
	for i, r := range list {
		mem[i] = Group{"radio_key": st.Str[r.radio], "ssid": st.Str[r.ssid], "mac": st.Str[r.mac], "band": st.Str[r.band], "count": r.count}
	}
	if individual {
		for _, g := range IndependentGroups(mem) {
			t.set(gs(g, "radio_key"), g)
		}
	}
	if shared {
		for _, g := range EstimationGroups(mem, py.Params{"shared": {"1"}, "individual": {"0"}}) {
			t.set(gs(g, "radio_key"), g)
		}
	}
	return t, nil
}

// AreaScope is the filters a nearby fit honours, in sample mode: the scope its fits are saved under.
func AreaScope(p py.Params) py.Params {
	scoped := py.Params{}
	for k, v := range p {
		switch k {
		case "ssid_query", "band", "min_rssi", "date", "date_from", "date_to", "none", "solver":
			scoped[k] = v
		}
	}
	scoped["fit_mode"] = []string{"sample"}
	return scoped
}

// AreaPlan is area_plan(database, params, display, discover): the visible saved fits, the scoped
// params a fit job uses, and the next batch of nearby groups still to fit.
func (a *Atlas) AreaPlan(c *sqlite.Conn, p py.Params, display, discover bool) (map[string]any, py.Params, []Group, error) {
	scoped := AreaScope(p)
	scope, sampleKey, err := EstimateScope(scoped)
	if err != nil {
		return nil, nil, nil, err
	}
	_, exactKey, err := EstimateScope(scoped.With("fit_mode", []string{"all"}))
	if err != nil {
		return nil, nil, nil, err
	}
	zoom, err := py.Float(p.First("zoom", "0"))
	if err != nil {
		return nil, nil, nil, err
	}
	if math.IsNaN(zoom) || math.IsInf(zoom, 0) {
		return nil, nil, nil, py.Errorf("Invalid zoom")
	}
	result := map[string]any{"estimates": []any{}, "more": false, "missing_estimates": int64(0), "min_zoom": int64(MinZoom),
		"limit": int64(MaxLocations), "observation_scope": scope, "zoom_required": zoom < MinZoom}
	if !p.Has("bbox") {
		if zoom < MinZoom {
			return result, scoped, nil, nil
		}
		return nil, nil, nil, py.Errorf("Map bounds are required")
	}
	if _, err := ParseFilter(scoped.With("bbox", p["bbox"]), true); err != nil {
		return nil, nil, nil, err
	}
	if scoped.Is("none", "1") {
		return result, scoped, nil, nil
	}
	b, _ := Bbox(p["bbox"][0])
	west, south, east, north := b[0], b[1], b[2], b[3]
	dy := 500.0 / 111195
	dx := dy / math.Max(.08, math.Cos((south+north)/2*math.Pi/180))
	str := func(v float64) string {
		if v == math.Trunc(v) && (v == -180 || v == 180 || v == -85 || v == 85) {
			return py.Str(int64(v))
		}
		return py.FloatRepr(v)
	}
	nearby := scoped.With("bbox", []string{strings.Join([]string{str(math.Max(-180, west-dx)), str(math.Max(-85, south-dy)), str(math.Min(180, east+dx)), str(math.Min(85, north+dy))}, ",")})
	individual := p.Is("individual", "1")
	shared := !individual || p.Is("shared", "1")
	cache := map[string]*targetMap{}
	targetsFor := func(ssid, mac string) (*targetMap, error) {
		hidden := ""
		if ssid == "" {
			hidden = mac
		}
		id := ssid + "\x00" + hidden
		if t, ok := cache[id]; ok {
			return t, nil
		}
		t, err := a.overviewTargets(scoped, ssid, hidden, individual, shared)
		cache[id] = t
		return t, err
	}
	if display {
		fits, err := visibleFits(c, exactKey, sampleKey, b, targetsFor)
		if err != nil {
			return nil, nil, nil, err
		}
		result["estimates"] = fits
	}
	if !discover || zoom < MinZoom {
		return result, scoped, nil, nil
	}
	groups, more, err := a.nearbyGroups(nearby, b, individual, targetsFor)
	if err != nil {
		return nil, nil, nil, err
	}
	result["more"] = more
	cached := map[string]bool{}
	if len(groups) > 0 {
		keys := make([]any, len(groups))
		for i, g := range groups {
			keys[i] = gs(g, "radio_key")
		}
		rows, err := db.All(c, `SELECT radio_key FROM estimates
            WHERE (scope_key=? OR (scope_key=? AND json_extract(result,'$.status')='fitted'))
            AND radio_key IN (`+db.Marks(len(keys))+`)`, append([]any{sampleKey, exactKey}, keys...)...)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, r := range rows {
			cached[db.Str(r["radio_key"])] = true
		}
	}
	var missing []Group
	for _, g := range groups {
		if !cached[gs(g, "radio_key")] {
			missing = append(missing, g)
		}
	}
	result["missing_estimates"] = int64(len(missing))
	return result, scoped, missing[:min(len(missing), BatchSize)], nil
}

// MaxDisplayFits is the most saved fits one map view loads. A wider view (after "Fit every AP" on a
// big survey, a zoomed-out map can hold hundreds of thousands) gets an even sample of about this
// many instead, so the page never has to hold them all; zooming in shows every fit in the area.
const MaxDisplayFits = 1500

func visibleFits(c *sqlite.Conn, exact, sample string, b [4]float64, targetsFor func(string, string) (*targetMap, error)) ([]any, error) {
	where := `FROM estimates e
        WHERE e.scope_key IN (?, ?) AND json_extract(e.result,'$.status')='fitted'
        AND json_extract(e.result,'$.longitude') BETWEEN ? AND ?
        AND json_extract(e.result,'$.latitude') BETWEEN ? AND ?
        AND (e.scope_key=? OR NOT EXISTS (SELECT 1 FROM estimates x
            WHERE x.radio_key=e.radio_key AND x.scope_key=?
            AND json_extract(x.result,'$.status')='fitted'))`
	args := []any{exact, sample, b[0], b[2], b[1], b[3], exact, exact}
	n, err := db.Scalar(c, "SELECT count(*) "+where, args...)
	if err != nil {
		return nil, err
	}
	if every := (db.Int(n) + MaxDisplayFits - 1) / MaxDisplayFits; every > 1 {
		where += " AND e.rowid % ? = 0"
		args = append(args, every)
	}
	rows, err := db.All(c, "SELECT e.radio_key,e.result "+where+" ORDER BY e.radio_key", args...)
	if err != nil {
		return nil, err
	}
	fits := []any{}
	for _, r := range rows {
		var fit map[string]any
		if err := json.Unmarshal([]byte(db.Str(r["result"])), &fit); err != nil {
			return nil, err
		}
		mac, _ := fit["mac"].(string)
		if ms := members(fit); ms != nil && len(ms) > 0 {
			mac = gs(ms[0], "mac")
		}
		ssid, _ := fit["ssid"].(string)
		t, err := targetsFor(ssid, mac)
		if err != nil {
			return nil, err
		}
		if _, ok := t.m[db.Str(r["radio_key"])]; ok {
			fits = append(fits, fit)
		}
	}
	return fits, nil
}

func (a *Atlas) nearbyGroups(nearby py.Params, b [4]float64, individual bool, targetsFor func(string, string) (*targetMap, error)) ([]Group, bool, error) {
	f, st, err := a.compile(nearby, true)
	if err != nil {
		return nil, false, err
	}
	west, south, east, north := b[0], b[1], b[2], b[3]
	type cand struct {
		ssid, mac uint32
		inView    bool
		peak      float64
	}
	key := func(s *obs.Segment, i int) [2]uint32 {
		// Discover individual MACs only on request. Hidden networks use a MAC identity.
		if individual || st.Str[s.SSID[i]] == "" {
			return [2]uint32{s.SSID[i], s.MAC[i]}
		}
		return [2]uint32{s.SSID[i], math.MaxUint32}
	}
	parts := Scan(f, func() map[[2]uint32]*cand { return map[[2]uint32]*cand{} }, func(m map[[2]uint32]*cand, _ uint32, s *obs.Segment, i int) {
		k := key(s, i)
		c := m[k]
		if c == nil {
			c = &cand{ssid: s.SSID[i], mac: s.MAC[i], peak: math.Inf(-1)}
			m[k] = c
		}
		lo, la := s.Lon[i], s.Lat[i]
		if lo >= west && lo <= east && la >= south && la <= north {
			c.inView = true
		}
		if s.RSSI[i] > c.peak {
			c.peak, c.mac = s.RSSI[i], s.MAC[i]
		}
	})
	all := map[[2]uint32]*cand{}
	for _, m := range parts {
		for k, c := range m {
			t := all[k]
			if t == nil {
				all[k] = c
				continue
			}
			t.inView = t.inView || c.inView
			if c.peak > t.peak {
				t.peak, t.mac = c.peak, c.mac
			}
		}
	}
	list := make([]*cand, 0, len(all))
	for _, c := range all {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		x, y := list[i], list[j]
		if x.inView != y.inView {
			return x.inView
		}
		if x.peak != y.peak {
			return x.peak > y.peak
		}
		if st.Str[x.ssid] != st.Str[y.ssid] {
			return st.Str[x.ssid] < st.Str[y.ssid]
		}
		return st.Str[x.mac] < st.Str[y.mac]
	})
	more := len(list) > MaxLocations
	list = list[:min(len(list), MaxLocations)]
	out := &targetMap{m: map[string]Group{}}
	for _, c := range list {
		t, err := targetsFor(st.Str[c.ssid], st.Str[c.mac])
		if err != nil {
			return nil, false, err
		}
		for _, k := range t.keys {
			g := t.m[k]
			coLoc := true
			if v, ok := g["co_location_assumed"].(bool); ok {
				coLoc = v
			}
			if !individual || gs(g, "mac") == st.Str[c.mac] || (len(members(g)) > 0 && coLoc) {
				out.set(k, g)
			}
		}
	}
	groups := make([]Group, 0, len(out.keys))
	for _, k := range out.keys {
		groups = append(groups, out.m[k])
	}
	return groups, more, nil
}

// AreaEstimates is area_estimates(database, params).
func (a *Atlas) AreaEstimates(c *sqlite.Conn, p py.Params) (map[string]any, error) {
	r, _, _, err := a.AreaPlan(c, p, true, !p.Is("saved_only", "1"))
	return r, err
}
