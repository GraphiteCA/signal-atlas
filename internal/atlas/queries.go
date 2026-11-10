package atlas

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"
	"signal-atlas/internal/pycsv"

	"zombiezen.com/go/sqlite"
)

var readCSV = pycsv.ReadFile

// FileSHA256 hashes a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 4<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// kbn is SQLite's sum()/avg()/total() accumulator for REAL values (Kahan-Babuska-Neumaier).
type kbn struct{ sum, err float64 }

func (k *kbn) add(r float64) {
	s := k.sum
	t := s + r
	if math.Abs(s) > math.Abs(r) {
		k.err += (s - t) + r
	} else {
		k.err += (r - t) + s
	}
	k.sum = t
}

func (k *kbn) total() float64 {
	if math.IsNaN(k.err) {
		return k.sum
	}
	return k.sum + k.err
}

// compile parses and binds a filter to the current State.
func (a *Atlas) compile(p py.Params, spatial bool) (*Filter, *obs.State, error) {
	f, err := ParseFilter(p, spatial)
	if err != nil {
		return nil, nil, err
	}
	st := a.Obs.State()
	return f.Compile(st, a.Obs.Dict), st, nil
}

// idset is a set of string ids as a bitmap over the dictionary.
type idset struct {
	bits []uint64
	n    int64
}

func newIDSet(size int) *idset { return &idset{bits: make([]uint64, size/64+1)} }
func (s *idset) add(id uint32) {
	w, b := id/64, uint64(1)<<(id%64)
	if s.bits[w]&b == 0 {
		s.bits[w] |= b
		s.n++
	}
}
func (s *idset) merge(o *idset) {
	s.n = 0
	for i := range s.bits {
		s.bits[i] |= o.bits[i]
	}
	for _, w := range s.bits {
		for ; w != 0; w &= w - 1 {
			s.n++
		}
	}
}

// ---------------------------------------------------------------------------

// Summary is summary(database, params).
func (a *Atlas) Summary(c *sqlite.Conn, p py.Params) (Row, error) {
	f, st, err := a.compile(p, false)
	if err != nil {
		return nil, err
	}
	type acc struct {
		n                        int64
		ssids, radios            *idset
		south, north, west, east float64
		peak                     float64
		dmin, dmax               string
		bands                    map[uint32]int64
	}
	parts := Scan(f, func() *acc {
		return &acc{ssids: newIDSet(len(st.Str)), radios: newIDSet(len(st.Str)), south: math.Inf(1), north: math.Inf(-1), west: math.Inf(1), east: math.Inf(-1), peak: math.Inf(-1), bands: map[uint32]int64{}}
	}, func(x *acc, _ uint32, s *obs.Segment, i int) {
		if x.n == 0 {
			x.dmin, x.dmax = st.Str[s.Date[i]], st.Str[s.Date[i]]
		}
		x.n++
		x.ssids.add(s.SSID[i])
		x.radios.add(s.Radio[i])
		x.south, x.north = math.Min(x.south, s.Lat[i]), math.Max(x.north, s.Lat[i])
		x.west, x.east = math.Min(x.west, s.Lon[i]), math.Max(x.east, s.Lon[i])
		x.peak = math.Max(x.peak, s.RSSI[i])
		if d := st.Str[s.Date[i]]; d < x.dmin {
			x.dmin = d
		} else if d > x.dmax {
			x.dmax = d
		}
		x.bands[s.Band[i]]++
	})
	t := &acc{ssids: newIDSet(len(st.Str)), radios: newIDSet(len(st.Str)), south: math.Inf(1), north: math.Inf(-1), west: math.Inf(1), east: math.Inf(-1), peak: math.Inf(-1), bands: map[uint32]int64{}}
	for _, x := range parts {
		if x.n == 0 {
			continue
		}
		if t.n == 0 || x.dmin < t.dmin {
			t.dmin = x.dmin
		}
		if t.n == 0 || x.dmax > t.dmax {
			t.dmax = x.dmax
		}
		t.n += x.n
		t.ssids.merge(x.ssids)
		t.radios.merge(x.radios)
		t.south, t.north = math.Min(t.south, x.south), math.Max(t.north, x.north)
		t.west, t.east = math.Min(t.west, x.west), math.Max(t.east, x.east)
		t.peak = math.Max(t.peak, x.peak)
		for k, v := range x.bands {
			t.bands[k] += v
		}
	}
	row := Row{"count": t.n, "networks": t.ssids.n, "radios": t.radios.n}
	if t.n == 0 {
		for _, k := range []string{"south", "north", "west", "east", "peak", "first_date", "last_date"} {
			row[k] = nil
		}
	} else {
		row["south"], row["north"], row["west"], row["east"], row["peak"] = t.south, t.north, t.west, t.east, t.peak
		row["first_date"], row["last_date"] = t.dmin, t.dmax
	}
	bands := map[string]any{}
	for k, v := range t.bands {
		bands[st.Str[k]] = v
	}
	row["bands"] = bands
	row["imports"], err = db.All(c, "SELECT filename,valid_rows,invalid_rows,non_wifi_rows FROM imports ORDER BY id")
	return row, err
}

// radioAgg is GROUP BY radio_key over the matching rows.
type radioAgg struct {
	radio, ssid, mac, band uint32
	count                  int64
	peak                   float64
	freq                   float64 // min, NaN if none
	sources                []uint32
	first                  uint32 // first matching row
}

func (a *Atlas) groupRadios(f *Filter, st *obs.State) []*radioAgg {
	parts := Scan(f, func() map[uint32]*radioAgg { return map[uint32]*radioAgg{} }, func(m map[uint32]*radioAgg, g uint32, s *obs.Segment, i int) {
		r := m[s.Radio[i]]
		if r == nil {
			r = &radioAgg{radio: s.Radio[i], ssid: s.SSID[i], mac: s.MAC[i], band: s.Band[i], peak: math.Inf(-1), freq: math.NaN(), first: g}
			m[s.Radio[i]] = r
		}
		r.count++
		r.peak = math.Max(r.peak, s.RSSI[i])
		if fq := s.Freq[i]; !obs.IsNull(fq) && (math.IsNaN(r.freq) || fq < r.freq) {
			r.freq = fq
		}
		src := s.BandSrc[i]
		seen := false
		for _, x := range r.sources {
			seen = seen || x == src
		}
		if !seen {
			r.sources = append(r.sources, src)
		}
	})
	all := map[uint32]*radioAgg{}
	for _, m := range parts {
		for k, r := range m {
			t := all[k]
			if t == nil {
				all[k] = r
				continue
			}
			t.count += r.count
			t.peak = math.Max(t.peak, r.peak)
			if !math.IsNaN(r.freq) && (math.IsNaN(t.freq) || r.freq < t.freq) {
				t.freq = r.freq
			}
			for _, src := range r.sources {
				seen := false
				for _, x := range t.sources {
					seen = seen || x == src
				}
				if !seen {
					t.sources = append(t.sources, src)
				}
			}
		}
	}
	out := make([]*radioAgg, 0, len(all))
	for _, r := range all {
		out = append(out, r)
	}
	return out
}

func joinBands(st *obs.State, ids []uint32) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = st.Str[id]
	}
	return strings.Join(s, ",")
}

// Radios is radios(database, params).
func (a *Atlas) Radios(p py.Params) (map[string]any, error) {
	f, st, err := a.compile(p, false)
	if err != nil {
		return nil, err
	}
	limit, offset, err := PageLimit(p, 50, 200)
	if err != nil {
		return nil, err
	}
	rs := a.groupRadios(f, st)
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].count != rs[j].count {
			return rs[i].count > rs[j].count
		}
		return st.Str[rs[i].radio] < st.Str[rs[j].radio]
	})
	var rows []Row
	for k := offset; k < int64(len(rs)) && k < offset+limit+1; k++ {
		r := rs[k]
		var seen any
		if t := st.Trans[obs.Pair{r.ssid, r.mac}]; t != nil {
			seen = joinBands(st, t.Bands)
		}
		rows = append(rows, Row{"radio_key": st.Str[r.radio], "ssid": st.Str[r.ssid], "mac": st.Str[r.mac], "band": st.Str[r.band],
			"count": r.count, "peak": r.peak, "frequency_mhz": nullable(nanToNull(r.freq)), "band_sources": joinBands(st, r.sources), "seen_bands": seen})
	}
	return page(rows, limit, offset), nil
}

func nanToNull(v float64) float64 {
	if math.IsNaN(v) {
		return obs.Null
	}
	return v
}

func page(rows []Row, limit, offset int64) map[string]any {
	if rows == nil {
		rows = []Row{}
	}
	more := int64(len(rows)) > limit
	if more {
		rows = rows[:limit]
	}
	return map[string]any{"rows": rows, "more": more, "offset": offset}
}

// orderedRows returns matching rows in id order, from offset, at most n of them.
func orderedRows(f *Filter, offset, n int64) []uint32 {
	parts := Scan(f, func() *[]uint32 { return &[]uint32{} }, func(l *[]uint32, g uint32, _ *obs.Segment, _ int) { *l = append(*l, g) })
	var out []uint32
	skip := offset
	for _, l := range parts {
		for _, g := range *l {
			if skip > 0 {
				skip--
				continue
			}
			if int64(len(out)) >= n {
				return out
			}
			out = append(out, g)
		}
	}
	return out
}

// Observations is observations(database, params).
func (a *Atlas) Observations(p py.Params) (map[string]any, error) {
	f, st, err := a.compile(p, true)
	if err != nil {
		return nil, err
	}
	limit, offset, err := PageLimit(p, 50, 100)
	if err != nil {
		return nil, err
	}
	var rows []Row
	for _, g := range orderedRows(f, offset, limit+1) {
		s, i := st.Loc(g)
		rows = append(rows, fullRow(st, s, i))
	}
	return page(rows, limit, offset), nil
}

// Points is points(database, params).
func (a *Atlas) Points(p py.Params) (map[string]any, error) {
	f, st, err := a.compile(p, true)
	if err != nil {
		return nil, err
	}
	l, err := py.Int(p.First("limit", "2000"))
	if err != nil {
		return nil, err
	}
	limit := max(100, min(5000, l))
	type bounds struct {
		n                        int64
		south, north, west, east float64
	}
	bp := Scan(f, func() *bounds {
		return &bounds{south: math.Inf(1), north: math.Inf(-1), west: math.Inf(1), east: math.Inf(-1)}
	},
		func(b *bounds, _ uint32, s *obs.Segment, i int) {
			b.n++
			b.south, b.north = math.Min(b.south, s.Lat[i]), math.Max(b.north, s.Lat[i])
			b.west, b.east = math.Min(b.west, s.Lon[i]), math.Max(b.east, s.Lon[i])
		})
	t := bounds{south: math.Inf(1), north: math.Inf(-1), west: math.Inf(1), east: math.Inf(-1)}
	for _, b := range bp {
		t.n += b.n
		t.south, t.north = math.Min(t.south, b.south), math.Max(t.north, b.north)
		t.west, t.east = math.Min(t.west, b.west), math.Max(t.east, b.east)
	}
	if t.n <= limit {
		var rows []Row
		for _, g := range orderedRows(f, 0, t.n) {
			s, i := st.Loc(g)
			rows = append(rows, Row{"id": s.Base + int64(i), "ssid": st.Str[s.SSID[i]], "mac": st.Str[s.MAC[i]], "band": st.Str[s.Band[i]],
				"latitude": s.Lat[i], "longitude": s.Lon[i], "rssi_dbm": s.RSSI[i], "accuracy_m": nullable(s.Acc[i]), "first_seen": st.Str[s.First[i]]})
		}
		if rows == nil {
			rows = []Row{}
		}
		return map[string]any{"aggregated": false, "count": t.n, "points": rows}, nil
	}
	bins := int64(math.Sqrt(float64(limit)))
	dy := math.Max(t.north-t.south, .0000001) / float64(bins)
	dx := math.Max(t.east-t.west, .0000001) / float64(bins)
	cell := func(v, lo, d float64) int64 { return min(bins-1, int64((v-lo)/d)) } // CAST(... AS INTEGER) truncates
	type agg struct {
		la, lo                   kbn
		n                        int64
		peak                     float64
		south, north, west, east float64
		radios, ssids, bands     map[uint32]bool
		minSSID, minBand         uint32
	}
	parts := Scan(f, func() map[int64]*agg { return map[int64]*agg{} }, func(m map[int64]*agg, _ uint32, s *obs.Segment, i int) {
		k := cell(s.Lat[i], t.south, dy)*bins + cell(s.Lon[i], t.west, dx)
		x := m[k]
		if x == nil {
			x = &agg{peak: math.Inf(-1), south: math.Inf(1), north: math.Inf(-1), west: math.Inf(1), east: math.Inf(-1),
				radios: map[uint32]bool{}, ssids: map[uint32]bool{}, bands: map[uint32]bool{}, minSSID: s.SSID[i], minBand: s.Band[i]}
			m[k] = x
		}
		x.la.add(s.Lat[i])
		x.lo.add(s.Lon[i])
		x.n++
		x.peak = math.Max(x.peak, s.RSSI[i])
		x.south, x.north = math.Min(x.south, s.Lat[i]), math.Max(x.north, s.Lat[i])
		x.west, x.east = math.Min(x.west, s.Lon[i]), math.Max(x.east, s.Lon[i])
		x.radios[s.Radio[i]], x.ssids[s.SSID[i]], x.bands[s.Band[i]] = true, true, true
		if st.Str[s.SSID[i]] < st.Str[x.minSSID] {
			x.minSSID = s.SSID[i]
		}
		if st.Str[s.Band[i]] < st.Str[x.minBand] {
			x.minBand = s.Band[i]
		}
	})
	all := map[int64]*agg{}
	for _, m := range parts {
		for k, x := range m {
			t := all[k]
			if t == nil {
				all[k] = x
				continue
			}
			t.la.add(x.la.sum)
			t.la.add(x.la.err)
			t.lo.add(x.lo.sum)
			t.lo.add(x.lo.err)
			t.n += x.n
			t.peak = math.Max(t.peak, x.peak)
			t.south, t.north = math.Min(t.south, x.south), math.Max(t.north, x.north)
			t.west, t.east = math.Min(t.west, x.west), math.Max(t.east, x.east)
			for id := range x.radios {
				t.radios[id] = true
			}
			for id := range x.ssids {
				t.ssids[id] = true
			}
			for id := range x.bands {
				t.bands[id] = true
			}
			if st.Str[x.minSSID] < st.Str[t.minSSID] {
				t.minSSID = x.minSSID
			}
			if st.Str[x.minBand] < st.Str[t.minBand] {
				t.minBand = x.minBand
			}
		}
	}
	keys := make([]int64, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	rows := make([]Row, 0, len(keys))
	for _, k := range keys {
		x := all[k]
		rows = append(rows, Row{"latitude": x.la.total() / float64(x.n), "longitude": x.lo.total() / float64(x.n), "count": x.n,
			"rssi_dbm": x.peak, "south": x.south, "north": x.north, "west": x.west, "east": x.east,
			"radios": int64(len(x.radios)), "networks": int64(len(x.ssids)), "ssid": st.Str[x.minSSID],
			"bands": int64(len(x.bands)), "band": st.Str[x.minBand]})
	}
	return map[string]any{"aggregated": true, "count": t.n, "points": rows}, nil
}

// AvailableDates is available_dates(database, params).
func (a *Atlas) AvailableDates(p py.Params) (map[string]any, error) {
	q := py.Params{}
	for k, v := range p {
		if k != "date" && k != "date_from" && k != "date_to" {
			q[k] = v
		}
	}
	f, st, err := a.compile(q, false)
	if err != nil {
		return nil, err
	}
	parts := Scan(f, func() map[uint32]int64 { return map[uint32]int64{} }, func(m map[uint32]int64, _ uint32, s *obs.Segment, i int) { m[s.Date[i]]++ })
	all := map[string]int64{}
	for _, m := range parts {
		for k, v := range m {
			all[st.Str[k]] += v
		}
	}
	days := make([]string, 0, len(all))
	for d := range all {
		if d != "" && py.ISODateKind(d) == "ok" {
			days = append(days, d)
		}
	}
	sort.Strings(days)
	out := make([]Row, 0, len(days))
	for _, d := range days {
		out = append(out, Row{"date": d, "count": all[d]})
	}
	return map[string]any{"dates": out}, nil
}

// Networks is networks(database, params): network_catalog filtered by name.
func (a *Atlas) Networks(p py.Params) (map[string]any, error) {
	limit, offset, err := PageLimit(p, 50, 200)
	if err != nil {
		return nil, err
	}
	st := a.Obs.State()
	needle := asciiLower(p.First("q", ""))
	type item struct {
		ssid uint32
		ns   *obs.NetStat
	}
	var list []item
	for id, ns := range st.Net {
		if strings.Contains(asciiLower(st.Str[id]), needle) {
			list = append(list, item{id, ns})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].ns.Count != list[j].ns.Count {
			return list[i].ns.Count > list[j].ns.Count
		}
		return st.Str[list[i].ssid] < st.Str[list[j].ssid]
	})
	var rows []Row
	for k := offset; k < int64(len(list)) && k < offset+limit+1; k++ {
		it := list[k]
		rows = append(rows, Row{"ssid": st.Str[it.ssid], "count": it.ns.Count, "macs": it.ns.Macs, "radios": it.ns.Radios, "peak": it.ns.Peak, "bands": joinBands(st, it.ns.Bands)})
	}
	return page(rows, limit, offset), nil
}

// Transmitters is transmitters(database, params): one SSID's MAC addresses.
func (a *Atlas) Transmitters(c *sqlite.Conn, p py.Params) (map[string]any, error) {
	ssids := p.Get("ssid", nil)
	if len(ssids) != 1 {
		return nil, py.Errorf("Expand exactly one SSID to list its MAC addresses")
	}
	limit, offset, err := PageLimit(p, 25, 200)
	if err != nil {
		return nil, err
	}
	st := a.Obs.State()
	needle := asciiLower(strings.ToUpper(py.Strip(p.First("q", ""))))
	type item struct {
		mac uint32
		t   *obs.TransStat
	}
	var list []item
	if sid, ok := a.Obs.Dict.Lookup(ssids[0]); ok {
		for k, t := range st.Trans {
			if k[0] == sid && strings.Contains(asciiLower(st.Str[k[1]]), needle) {
				list = append(list, item{k[1], t})
			}
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].t.Count != list[j].t.Count {
			return list[i].t.Count > list[j].t.Count
		}
		return st.Str[list[i].mac] < st.Str[list[j].mac]
	})
	var rows []Row
	for k := offset; k < int64(len(list)) && k < offset+limit+1; k++ {
		it := list[k]
		manual, err := db.Scalar(c, "SELECT band FROM band_assignments WHERE ssid=? AND mac=?", ssids[0], st.Str[it.mac])
		if err != nil {
			return nil, err
		}
		rows = append(rows, Row{"ssid": ssids[0], "mac": st.Str[it.mac], "count": it.t.Count, "radio_count": int64(len(it.t.Bands)),
			"bands": joinBands(st, it.t.Bands), "peak": it.t.Peak, "manual_band": manual, "editable_band_count": it.t.Editable})
	}
	out := page(rows, limit, offset)
	out["ssid"] = ssids[0]
	return out, nil
}

// AssignBand is assign_band(database, ssid, mac, band).
func (a *Atlas) AssignBand(c *sqlite.Conn, ssid, mac, band any) (map[string]any, error) {
	s, ok1 := ssid.(string)
	m, ok2 := mac.(string)
	if !ok1 || !ok2 {
		return nil, py.Errorf("Choose an SSID and MAC address")
	}
	m = strings.ToUpper(py.Strip(m))
	b, isStr := band.(string)
	if !isStr || (b != "" && b != "2.4" && b != "5" && b != "6") {
		return nil, py.Errorf("Choose 2.4, 5, or 6 GHz, or clear the manual assignment")
	}
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	st := a.Obs.State()
	sid, ok1 := a.Obs.Dict.Lookup(s)
	mid, ok2 := a.Obs.Dict.Lookup(m)
	rows := st.ByTrans[obs.Pair{sid, mid}]
	if !ok1 || !ok2 || len(rows) == 0 {
		return nil, py.Errorf("This SSID/MAC is not in the database")
	}
	assigned, source := "unknown", "frequency/band unavailable"
	if b != "" {
		assigned, source = b, "manual assignment"
	}
	bandID, srcID, radioID := a.Obs.Dict.ID(assigned), a.Obs.Dict.ID(source), a.Obs.Dict.ID(RadioKey(s, m, assigned))
	// the changed rows, by segment
	changed := map[*obs.Segment]*obs.Segment{}
	var n int64
	for _, g := range rows {
		sg, i := st.Loc(g)
		if !obs.Editable(st.Str[sg.BandSrc[i]]) {
			continue
		}
		c2 := changed[sg]
		if c2 == nil {
			c2 = sg.Clone()
			changed[sg] = c2
		}
		c2.Band[i], c2.BandSrc[i], c2.Radio[i] = bandID, srcID, radioID
		n++
	}
	err := db.Tx(c, func() error {
		var err error
		if b != "" {
			_, err = db.Exec(c, "INSERT OR REPLACE INTO band_assignments VALUES(?,?,?,?)", s, m, b, py.UTCNowISO())
		} else {
			_, err = db.Exec(c, "DELETE FROM band_assignments WHERE ssid=? AND mac=?", s, m)
		}
		if err != nil {
			return err
		}
		// These are disposable computations. Historical comparison pins remain immutable,
		// including their original band declarations.
		if n > 0 {
			if _, err := db.Exec(c, "DELETE FROM estimates"); err != nil {
				return err
			}
			return a.Obs.Commit(obs.Change{Replace: changed})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"updated": n, "band": assigned, "manual": b != ""}, nil
}

// Group is a fit group: a radio, or several (members) fitted together.
type Group = map[string]any

// SelectedGroups is selected_groups(database, params).
func (a *Atlas) SelectedGroups(p py.Params) ([]Group, error) {
	f, st, err := a.compile(p, false)
	if err != nil {
		return nil, err
	}
	rs := a.groupRadios(f, st)
	sort.Slice(rs, func(i, j int) bool { return st.Str[rs[i].radio] < st.Str[rs[j].radio] })
	out := make([]Group, 0, len(rs))
	for _, r := range rs {
		out = append(out, Group{"radio_key": st.Str[r.radio], "ssid": st.Str[r.ssid], "mac": st.Str[r.mac], "band": st.Str[r.band], "count": r.count})
	}
	return out, nil
}
