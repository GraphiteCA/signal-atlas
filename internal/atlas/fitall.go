package atlas

import (
	"math"
	"sort"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"

	"zombiezen.com/go/sqlite"
)

// ---------------------------------------------------------------------------
// Fitting every access point in the store, not just the ones near the map's view. Each network is
// fitted exactly as the map's nearby fits would fit it (same groups, same scope), so the results
// land in the cache the map draws from. The work is listed first as a light list of networks and
// fitted a batch at a time, so memory stays flat however big the store is.

// FitTarget is one network to fit: a named network (MAC empty unless fitting each radio on its
// own), or a hidden network by its MAC.
type FitTarget struct{ SSID, MAC string }

// AllTargets lists every network the explorer's current filters keep, strongest first, with the
// scope the fits are saved under.
func (a *Atlas) AllTargets(p py.Params) (py.Params, string, string, []FitTarget, error) {
	scoped := AreaScope(p)
	_, sampleKey, err := EstimateScope(scoped)
	if err != nil {
		return nil, "", "", nil, err
	}
	_, exactKey, err := EstimateScope(scoped.With("fit_mode", []string{"all"}))
	if err != nil {
		return nil, "", "", nil, err
	}
	f, st, err := a.compile(scoped, true)
	if err != nil {
		return nil, "", "", nil, err
	}
	individual := p.Is("individual", "1")
	type cand struct {
		ssid, mac uint32
		peak      float64
	}
	key := func(s *obs.Segment, i int) [2]uint32 {
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
		if s.RSSI[i] > c.peak {
			c.peak, c.mac = s.RSSI[i], s.MAC[i]
		}
	})
	all := map[[2]uint32]*cand{}
	for _, m := range parts {
		for k, c := range m {
			if t := all[k]; t == nil {
				all[k] = c
			} else if c.peak > t.peak {
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
		if x.peak != y.peak {
			return x.peak > y.peak
		}
		if st.Str[x.ssid] != st.Str[y.ssid] {
			return st.Str[x.ssid] < st.Str[y.ssid]
		}
		return st.Str[x.mac] < st.Str[y.mac]
	})
	out := make([]FitTarget, len(list))
	for i, c := range list {
		out[i] = FitTarget{SSID: st.Str[c.ssid], MAC: st.Str[c.mac]}
	}
	return scoped, sampleKey, exactKey, out, nil
}

// TargetGroups is the groups the map would fit for one network (what nearbyGroups picks for it).
func (a *Atlas) TargetGroups(scoped py.Params, t FitTarget, individual, shared bool) ([]Group, error) {
	hidden := ""
	if t.SSID == "" {
		hidden = t.MAC
	}
	tm, err := a.overviewTargets(scoped, t.SSID, hidden, individual, shared)
	if err != nil {
		return nil, err
	}
	var out []Group
	for _, k := range tm.keys {
		g := tm.m[k]
		coLoc := true
		if v, ok := g["co_location_assumed"].(bool); ok {
			coLoc = v
		}
		if !individual || gs(g, "mac") == t.MAC || (len(members(g)) > 0 && coLoc) {
			out = append(out, g)
		}
	}
	return out, nil
}

// CachedFits is which of these groups already have a fit the map would show (in sample mode, or a
// fitted result in all-observations mode).
func CachedFits(c *sqlite.Conn, sampleKey, exactKey string, groups []Group) (map[string]bool, error) {
	cached := map[string]bool{}
	for start := 0; start < len(groups); start += 500 {
		part := groups[start:min(len(groups), start+500)]
		keys := make([]any, len(part))
		for i, g := range part {
			keys[i] = gs(g, "radio_key")
		}
		rows, err := db.All(c, `SELECT radio_key FROM estimates
            WHERE (scope_key=? OR (scope_key=? AND json_extract(result,'$.status')='fitted'))
            AND radio_key IN (`+db.Marks(len(keys))+`)`, append([]any{sampleKey, exactKey}, keys...)...)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			cached[db.Str(r["radio_key"])] = true
		}
	}
	return cached, nil
}
