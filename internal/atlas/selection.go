package atlas

import (
	"encoding/json"
	"sort"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"

	"zombiezen.com/go/sqlite"
)

// Selection-aware fit groups, cached-estimate lookups and comparison snapshots: Graphite's
// selection_estimates.py and area_estimates.py, plus fitted_estimates from serve_survey.py.
// Group identities ('shared:<sha256>', 'radio:<sha256>') hash the same JSON text Python hashes.

func gs(g Group, k string) string { s, _ := g[k].(string); return s }
func gi(g Group, k string) int64  { return db.Int(g[k]) }

func members(g Group) []Group {
	switch m := g["members"].(type) {
	case []Group:
		return m
	case []any:
		out := make([]Group, 0, len(m))
		for _, x := range m {
			if mg, ok := x.(map[string]any); ok {
				out = append(out, mg)
			}
		}
		return out
	}
	return nil
}

func membersAny(list []Group) []any {
	out := make([]any, len(list))
	for i, g := range list {
		out[i] = g
	}
	return out
}

// groupBy keeps first-seen key order, as Python's defaultdict does.
func groupBy(groups []Group, key func(Group) string) ([]string, map[string][]Group) {
	var order []string
	m := map[string][]Group{}
	for _, g := range groups {
		k := key(g)
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = append(m[k], g)
	}
	return order, m
}

func byMAC(g Group) string { return gs(g, "ssid") + "\x00" + gs(g, "mac") }

// UnknownBandAliases is unknown_band_aliases(groups).
func UnknownBandAliases(groups []Group) map[string]string {
	aliases := map[string]string{}
	order, m := groupBy(groups, byMAC)
	for _, k := range order {
		var known []Group
		for _, g := range m[k] {
			if gs(g, "band") != "unknown" {
				known = append(known, g)
			}
		}
		if len(known) == 1 {
			for _, g := range m[k] {
				if gs(g, "band") == "unknown" {
					aliases[gs(g, "radio_key")] = gs(known[0], "radio_key")
				}
			}
		}
	}
	return aliases
}

func shaJSON(v any) string { return py.SHA256Hex(py.MustDumps(v, py.DumpOpts{})) }

// JointGroups is joint_groups(groups, params).
func JointGroups(groups []Group, p py.Params) []Group {
	if !p.Is("shared", "1") {
		return nil
	}
	var out []Group
	order, m := groupBy(groups, func(g Group) string { return gs(g, "ssid") })
	for _, ssid := range order {
		mem := m[ssid]
		// Broadening a map selection must remain usable even with Shared enabled. The UI reports
		// that large SSIDs need narrowing.
		if len(mem) < 2 || len(mem) > 20 {
			continue
		}
		keys := make([]string, len(mem))
		bandSet := map[string]bool{}
		macSet := map[string]bool{}
		var count int64
		for i, g := range mem {
			keys[i] = gs(g, "radio_key")
			bandSet[gs(g, "band")] = true
			macSet[gs(g, "mac")] = true
			count += gi(g, "count")
		}
		sort.Strings(keys)
		aliases := UnknownBandAliases(mem)
		var identity any = keys
		if len(aliases) > 0 {
			pairs := make([][2]string, 0, len(aliases))
			for k, v := range aliases {
				pairs = append(pairs, [2]string{k, v})
			}
			sort.Slice(pairs, func(i, j int) bool {
				if pairs[i][0] != pairs[j][0] {
					return pairs[i][0] < pairs[j][0]
				}
				return pairs[i][1] < pairs[j][1]
			})
			pl := make([]any, len(pairs))
			for i, p := range pairs {
				pl[i] = []string{p[0], p[1]}
			}
			identity = []any{keys, pl}
		}
		bands := make([]string, 0, len(bandSet))
		for b := range bandSet {
			bands = append(bands, b)
		}
		sort.Strings(bands)
		band := "multiple"
		if len(bands) == 1 {
			band = bands[0]
		}
		mac := py.Str(int64(len(macSet))) + " MAC address"
		if len(macSet) != 1 {
			mac += "es"
		}
		out = append(out, Group{"radio_key": "shared:" + shaJSON(identity), "ssid": ssid, "mac": mac, "band": band, "members": membersAny(mem), "count": count})
	}
	return out
}

// IndependentGroups is independent_groups(groups): unknown band metadata does not create another
// transmitter for a known MAC.
func IndependentGroups(groups []Group) []Group {
	var out []Group
	order, m := groupBy(groups, byMAC)
	for _, k := range order {
		mem := m[k]
		var known []Group
		for _, g := range mem {
			if gs(g, "band") != "unknown" {
				known = append(known, g)
			}
		}
		if len(known) == 1 && len(mem) > 1 {
			keys := make([]string, len(mem))
			var count int64
			for i, g := range mem {
				keys[i] = gs(g, "radio_key")
				count += gi(g, "count")
			}
			sort.Strings(keys)
			g := Group{}
			for k2, v := range known[0] {
				g[k2] = v
			}
			g["radio_key"], g["members"], g["count"], g["co_location_assumed"] = "radio:"+shaJSON(keys), membersAny(mem), count, false
			out = append(out, g)
		} else if len(known) > 0 {
			// With several recorded bands an unknown observation cannot be assigned.
			out = append(out, known...)
		} else {
			out = append(out, mem...)
		}
	}
	return out
}

// EstimationGroups is estimation_groups(groups, params).
func EstimationGroups(groups []Group, p py.Params) []Group {
	shared := JointGroups(groups, p)
	var independent []Group
	if p.Is("shared", "1") && p.Is("individual", "0") {
		counts := map[string]int{}
		for _, g := range groups {
			counts[gs(g, "ssid")]++
		}
		for _, g := range groups {
			if counts[gs(g, "ssid")] == 1 && gi(g, "count") >= 4 {
				independent = append(independent, g)
			}
		}
	} else {
		for _, g := range IndependentGroups(groups) {
			if gi(g, "count") >= 4 {
				independent = append(independent, g)
			}
		}
	}
	return append(independent, shared...)
}

// AnnotateBands is annotate_bands(db, results).
func (a *Atlas) AnnotateBands(results []map[string]any) {
	st := a.Obs.State()
	for _, r := range results {
		ssid, _ := r["ssid"].(string)
		mac, _ := r["mac"].(string)
		r["ssid_seen_bands"] = []any{}
		if id, ok := a.Obs.Dict.Lookup(ssid); ok {
			if ns := st.Net[id]; ns != nil {
				r["ssid_seen_bands"] = bandList(st, ns.Bands)
			}
		}
		var seen []any
		if sid, ok := a.Obs.Dict.Lookup(ssid); ok {
			if mid, ok := a.Obs.Dict.Lookup(mac); ok {
				if t := st.Trans[obs.Pair{sid, mid}]; t != nil {
					seen = bandList(st, t.Bands)
				}
			}
		}
		if seen == nil {
			set := map[string]bool{}
			for _, m := range members(r) {
				set[gs(m, "band")] = true
			}
			l := make([]string, 0, len(set))
			for b := range set {
				l = append(l, b)
			}
			sort.Strings(l)
			seen = []any{}
			for _, b := range l {
				seen = append(seen, b)
			}
		}
		r["seen_bands"] = seen
	}
}

func bandList(st *obs.State, ids []uint32) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = st.Str[id]
	}
	return out
}

// FittedEstimates is serve_survey.fitted_estimates(database, params).
func (a *Atlas) FittedEstimates(c *sqlite.Conn, p py.Params) (map[string]any, error) {
	scope, scopeKey, err := EstimateScope(p)
	if err != nil {
		return nil, err
	}
	l, err := py.Int(p.First("limit", "200"))
	if err != nil {
		return nil, err
	}
	limit := max(1, min(500, l))
	groups, err := a.SelectedGroups(p)
	if err != nil {
		return nil, err
	}
	targets := EstimationGroups(groups, p)
	// Date windows, selected days, signal thresholds and sampling modes get separate cache entries.
	// A full-history fit never stands in for a dated fit.
	keys := make([]string, len(targets))
	for i, g := range targets {
		keys[i] = gs(g, "radio_key")
	}
	sort.Strings(keys)
	results := []map[string]any{}
	found := int64(0)
	for start := 0; start < len(keys); start += 400 {
		batch := keys[start:min(len(keys), start+400)]
		args := append([]any{scopeKey}, anyStrings(batch)...)
		rows, err := db.All(c, "SELECT radio_key FROM estimates WHERE scope_key=? AND radio_key IN ("+db.Marks(len(batch))+") ORDER BY radio_key", args...)
		if err != nil {
			return nil, err
		}
		found += int64(len(rows))
		n := min(int64(len(rows)), limit-int64(len(results)))
		if n > 0 {
			pg := make([]string, n)
			for i := range pg {
				pg[i] = db.Str(rows[i]["radio_key"])
			}
			res, err := db.All(c, "SELECT result FROM estimates WHERE scope_key=? AND radio_key IN ("+db.Marks(len(pg))+") ORDER BY radio_key", append([]any{scopeKey}, anyStrings(pg)...)...)
			if err != nil {
				return nil, err
			}
			for _, r := range res {
				var v map[string]any
				if err := json.Unmarshal([]byte(db.Str(r["result"])), &v); err != nil {
					return nil, err
				}
				results = append(results, v)
			}
		}
	}
	a.AnnotateBands(results)
	shared := false
	if p.Is("shared", "1") {
		c := map[string]int{}
		for _, g := range groups {
			c[gs(g, "ssid")]++
		}
		for _, n := range c {
			shared = shared || n > 20
		}
	}
	warn := ""
	if shared {
		warn = "Shared fits skipped for SSIDs with more than 20 selected radios; narrow those selections."
	}
	var eligible int64
	for _, g := range groups {
		if gi(g, "count") >= 4 {
			eligible++
		}
	}
	return map[string]any{"estimates": results, "more": found > limit, "observation_scope": scope, "shared_warning": warn,
		"eligible_radios": eligible, "eligible_fits": int64(len(targets)), "selected_radios": int64(len(groups)), "missing_estimates": int64(len(keys)) - found}, nil
}

func anyStrings(s []string) []any {
	out := make([]any, len(s))
	for i, x := range s {
		out[i] = x
	}
	return out
}

// Comparisons is comparisons(database).
func Comparisons(c *sqlite.Conn) (map[string]any, error) {
	rows, err := db.All(c, "SELECT * FROM comparisons ORDER BY created_utc,id")
	if err != nil {
		return nil, err
	}
	list := make([]any, 0, len(rows))
	for _, r := range rows {
		var v map[string]any
		if err := json.Unmarshal([]byte(db.Str(r["payload"])), &v); err != nil {
			return nil, err
		}
		v["id"], v["name"], v["created_utc"] = r["id"], r["name"], r["created_utc"]
		list = append(list, v)
	}
	return map[string]any{"comparisons": list}, nil
}

// SaveComparison is save_comparison(database, params, name, fitted_estimates).
func (a *Atlas) SaveComparison(c *sqlite.Conn, p py.Params, name any) (string, error) {
	n := ""
	if name != nil {
		n = py.Strip(py.Str(name))
		if s, ok := name.(string); ok {
			n = py.Strip(s)
		}
	}
	n = py.Head(n, 100)
	if n == "" {
		return "", py.Errorf("Name this comparison, for example January or Before move")
	}
	res, err := a.FittedEstimates(c, p.With("limit", []string{"61"}))
	if err != nil {
		return "", err
	}
	if db.Int(res["missing_estimates"]) > 0 {
		return "", py.Errorf("Wait for the selected estimates to finish before pinning this date range")
	}
	ests := res["estimates"].([]map[string]any)
	var fits []any
	for _, r := range ests {
		if r["status"] == "fitted" {
			fits = append(fits, r)
		}
	}
	if res["more"].(bool) || len(ests) > 60 {
		return "", py.Errorf("Narrow the selection to at most 60 candidates before pinning a comparison")
	}
	if len(fits) == 0 {
		return "", py.Errorf("Estimate the selected radios before pinning a comparison")
	}
	id := py.UUIDHex()
	payload, err := py.Dumps(map[string]any{"estimates": fits, "observation_scope": res["observation_scope"], "filters": p}, py.DumpOpts{})
	if err != nil {
		return "", err
	}
	err = db.Tx(c, func() error {
		v, err := db.Scalar(c, "SELECT count(*) FROM comparisons")
		if err != nil {
			return err
		}
		if db.Int(v) >= 6 {
			return py.Errorf("Keep up to six pinned comparisons; remove one before adding another")
		}
		_, err = db.Exec(c, "INSERT INTO comparisons VALUES(?,?,?,?)", id, n, py.UTCNowISO(), payload)
		return err
	})
	return id, err
}

// DeleteComparison is delete_comparison.
func DeleteComparison(c *sqlite.Conn, id string) error {
	_, err := db.Exec(c, "DELETE FROM comparisons WHERE id=?", id)
	return err
}
