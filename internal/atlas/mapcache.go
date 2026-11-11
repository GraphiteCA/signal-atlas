package atlas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"
	"zombiezen.com/go/sqlite"
)

// One active scope and viewport per survey, rather than an unbounded cache of pans.
// Immutable slices can be used after unlocking. Large indexes are served normally but
// not retained. Observation buffers have a separate 64 MiB retention budget.
const mapIndexBudget = 128 << 20

// RebuildCaches preserves saved computations and replaces only derived lookups.
func (a *Atlas) RebuildCaches() {
	a.Obs.Reindex()
	m := &a.mapCache
	m.mu.Lock()
	m.state = nil
	m.raw = nil
	m.visible = nil
	m.targets = nil
	m.summary = nil
	m.view = nil
	m.viewMembers = nil
	m.mu.Unlock()
}

type mapIndex struct {
	rows      []MapFit // longitude order for binary-search viewport queries
	byID      map[string]int
	cellMu    sync.Mutex
	cellZoom  int
	cellNames []string
}

func (idx *mapIndex) cells(z int) []string {
	idx.cellMu.Lock()
	defer idx.cellMu.Unlock()
	if idx.cellNames == nil || idx.cellZoom != z {
		names := make([]string, len(idx.rows))
		size := clusterSize(z)
		for i, r := range idx.rows {
			names[i] = worldCell(r, z, size)
		}
		idx.cellZoom = z
		idx.cellNames = names
	}
	return idx.cellNames
}

type mapCache struct {
	mu                            sync.Mutex
	state                         *obs.State
	epoch                         uint64
	revision                      int64
	rawKey, targetKey, visibleKey string
	raw, visible                  *mapIndex
	targets                       map[string]bool
	summary                       *MapSummary
	viewKey                       string
	view                          *MapPage
	membersKey                    string
	viewMembers                   []int
}

func (m *mapCache) observe(st *obs.State, revision int64) {
	if st != m.state {
		m.state = st
		m.epoch++
		m.targets = nil
		m.raw = nil
		m.visible = nil
		m.summary = nil
		m.view = nil
		m.viewMembers = nil
	}
	if revision != m.revision {
		m.revision = revision
		m.raw = nil
		m.visible = nil
		m.summary = nil
		m.view = nil
		m.viewMembers = nil
	}
}

func paramsKey(p py.Params) string { return url.Values(p).Encode() }
func digest(s string) string       { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:12]) }
func mapModel(p py.Params) (bool, bool) {
	i := p.Is("individual", "1")
	return i, !i || p.Is("shared", "1")
}
func validLocation(lat, lon float64) bool {
	return !math.IsNaN(lat) && !math.IsNaN(lon) && !math.IsInf(lat, 0) && !math.IsInf(lon, 0) && lat >= -85.051129 && lat <= 85.051129 && lon >= -180 && lon <= 180
}

// index uses a committed metadata revision (maintained by SQL triggers, including
// direct SQL writes) plus the immutable observation State identity. A changed fit
// does not reaggregate observations; a new observation State does.
func (a *Atlas) mapIndex(ctx context.Context, c *sqlite.Conn, p py.Params) (*mapIndex, string, error) {
	if !c.AutocommitEnabled() {
		return nil, "", py.Errorf("Map snapshots require a committed connection")
	}
	var idx *mapIndex
	var version string
	err := db.Tx(c, func() error { var err error; idx, version, err = a.buildMapIndex(ctx, c, p); return err })
	return idx, version, err
}

// Read the revision and fit rows from the same SQLite snapshot, so a concurrent
// fit commit cannot label a different row ordering with an earlier revision.
func (a *Atlas) buildMapIndex(ctx context.Context, c *sqlite.Conn, p py.Params) (*mapIndex, string, error) {
	m := &a.mapCache
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	v, err := db.Scalar(c, "SELECT value FROM map_revision WHERE id=1")
	if err != nil {
		return nil, "", err
	}
	revision, st := db.Int(v), a.Obs.State()
	m.observe(st, revision)
	scoped := AreaScope(p)
	_, sample, err := EstimateScope(scoped)
	if err != nil {
		return nil, "", err
	}
	_, exact, err := EstimateScope(scoped.With("fit_mode", []string{"all"}))
	if err != nil {
		return nil, "", err
	}
	individual, shared := mapModel(p)
	tk := paramsKey(scoped) + fmt.Sprint(individual, shared)
	if m.targets == nil || m.targetKey != tk {
		// Aggregate only once per observation revision/filter/model, never per pan
		// or saved fit. Hidden networks are grouped by MAC, as in overviewTargets.
		f, err := ParseFilter(scoped, false)
		if err != nil {
			return nil, "", err
		}
		f.Compile(st, a.Obs.Dict)
		type radio struct {
			sid, mid, band uint32
			n              int64
		}
		parts := ScanContext(ctx, f, func() map[uint32]*radio { return map[uint32]*radio{} }, func(rs map[uint32]*radio, _ uint32, s *obs.Segment, i int) {
			r := rs[s.Radio[i]]
			if r == nil {
				r = &radio{sid: s.SSID[i], mid: s.MAC[i], band: s.Band[i]}
				rs[s.Radio[i]] = r
			}
			r.n++
		})
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		all := map[uint32]*radio{}
		for _, rs := range parts {
			for k, r := range rs {
				if prev := all[k]; prev != nil {
					prev.n += r.n
				} else {
					all[k] = r
				}
			}
		}
		groups := map[string][]Group{}
		for k, r := range all {
			ssid, mac := st.Str[r.sid], st.Str[r.mid]
			id := ssid
			if ssid == "" {
				id = "\x00" + mac
			}
			groups[id] = append(groups[id], Group{"radio_key": st.Str[k], "ssid": ssid, "mac": mac, "band": st.Str[r.band], "count": r.n})
		}
		targets := map[string]bool{}
		for _, g := range groups {
			if err := ctx.Err(); err != nil {
				return nil, "", err
			}
			sort.Slice(g, func(i, j int) bool { return gs(g[i], "radio_key") < gs(g[j], "radio_key") })
			if individual {
				for _, v := range IndependentGroups(g) {
					targets[gs(v, "radio_key")] = true
				}
			}
			if shared {
				for _, v := range EstimationGroups(g, py.Params{"shared": {"1"}, "individual": {"0"}}) {
					targets[gs(v, "radio_key")] = true
				}
			}
		}
		m.targets = targets
		m.targetKey = tk
		m.visible = nil
		m.view = nil
	}
	rk := sample + exact
	if m.raw == nil || m.rawKey != rk {
		idx := &mapIndex{byID: map[string]int{}}
		// A single statement gives a consistent committed snapshot. Full-input fits
		// replace sampled fits of the same identity within the solver namespace.
		err := db.Each(c, `SELECT e.radio_key,e.result FROM estimates e WHERE e.scope_key IN (?,?)
		AND json_extract(e.result,'$.status')='fitted' AND (e.scope_key=? OR NOT EXISTS
		(SELECT 1 FROM estimates x WHERE x.radio_key=e.radio_key AND x.scope_key=? AND json_extract(x.result,'$.status')='fitted'))`, []any{sample, exact, exact, exact}, func(s *sqlite.Stmt) error {
			var r struct {
				Latitude, Longitude *float64
				SSID, MAC, Band     string
				Shared              bool    `json:"co_location_assumed"`
				Used                int64   `json:"used_observations"`
				RMS                 float64 `json:"residual_rms_db"`
				Edge                bool    `json:"boundary_limited"`
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if json.Unmarshal([]byte(s.ColumnText(1)), &r) != nil || r.Latitude == nil || r.Longitude == nil || !validLocation(*r.Latitude, *r.Longitude) {
				return nil
			}
			idx.rows = append(idx.rows, MapFit{ID: s.ColumnText(0), Lat: *r.Latitude, Lon: *r.Longitude, SSID: r.SSID, MAC: r.MAC, Band: r.Band, Shared: r.Shared, Used: r.Used, RMS: r.RMS, BoundaryLimited: r.Edge})
			return nil
		})
		if err != nil {
			return nil, "", err
		}
		sort.Slice(idx.rows, func(i, j int) bool {
			a, b := idx.rows[i], idx.rows[j]
			if a.Lon != b.Lon {
				return a.Lon < b.Lon
			}
			return a.ID < b.ID
		})
		m.raw = idx
		m.rawKey = rk
		m.visible = nil
		m.view = nil
	}
	if m.visible == nil || m.visibleKey != tk {
		idx := &mapIndex{byID: map[string]int{}}
		for _, r := range m.raw.rows {
			if m.targets[r.ID] {
				idx.byID[r.ID] = len(idx.rows)
				idx.rows = append(idx.rows, r)
			}
		}
		m.visible = idx
		m.visibleKey = tk
		m.view = nil
	}
	idx := m.visible
	version := fmt.Sprintf("%d:%d:%s", m.epoch, revision, digest(tk+rk))
	// Bound retention, not the number of APs returned. Oversized surveys still
	// receive every identity through pagination; they rebuild on later queries.
	size := 0
	for _, r := range m.raw.rows {
		size += 144 + len(r.ID) + len(r.SSID) + len(r.MAC) + len(r.Band)
	}
	if size*2 > mapIndexBudget {
		m.raw = nil
		m.visible = nil
		m.targets = nil
	}
	return idx, version, nil
}

func (idx *mapIndex) in(b [4]float64, visit func(MapFit)) {
	idx.each(b, func(_ int, r MapFit) { visit(r) })
}
func (idx *mapIndex) each(b [4]float64, visit func(int, MapFit)) {
	i := sort.Search(len(idx.rows), func(i int) bool { return idx.rows[i].Lon >= b[0] })
	for ; i < len(idx.rows) && idx.rows[i].Lon <= b[2]; i++ {
		r := idx.rows[i]
		if r.Lat >= b[1] && r.Lat <= b[3] {
			visit(i, r)
		}
	}
}

type MapFeature struct {
	memberID string
	ID       string     `json:"id"`
	Lon      float64    `json:"lon"`
	Lat      float64    `json:"lat"`
	Count    int        `json:"count"`
	Bounds   [4]float64 `json:"bounds"`
	Label    string     `json:"label,omitempty"`
	Band     string     `json:"band,omitempty"`
}
type MapPage struct {
	Features     []MapFeature `json:"features"`
	Total        int          `json:"total"` // exact eligible AP identities in this viewport/cell
	FeatureTotal int          `json:"feature_total"`
	Revision     string       `json:"revision"`
	Next         string       `json:"next"`
}

func worldCell(r MapFit, z int, size float64) string {
	scale := 512 * math.Exp2(float64(z))
	la := math.Max(-85.051128, math.Min(85.051128, r.Lat)) * math.Pi / 180
	x := (r.Lon + 180) / 360 * scale
	y := (1 - math.Asinh(math.Tan(la))/math.Pi) / 2 * scale
	return fmt.Sprintf("%d:%d:%d", z, int64(math.Floor(x/size)), int64(math.Floor(y/size)))
}
func clusterSize(z int) float64 {
	switch {
	case z <= 16:
		return 64
	case z == 17:
		return 52
	case z == 18:
		return 48
	case z == 19:
		return 44
	default:
		return 40
	}
}

func cellCenter(cell string, z int) (float64, float64) {
	parts := strings.Split(cell, ":")
	x, _ := strconv.ParseFloat(parts[1], 64)
	y, _ := strconv.ParseFloat(parts[2], 64)
	scale := 512 * math.Exp2(float64(z))
	size := clusterSize(z)
	lon := (x+.5)*size/scale*360 - 180
	lat := math.Atan(math.Sinh(math.Pi*(1-2*(y+.5)*size/scale))) * 180 / math.Pi
	return math.Max(-180, math.Min(180, lon)), lat
}

// MapFeatures returns exact world-grid clusters or real individuals in bounded
// pages. Its cursor binds the whole query to a data revision; stale pages fail.
func (a *Atlas) MapFeatures(ctx context.Context, c *sqlite.Conn, p py.Params) (MapPage, error) {
	idx, version, err := a.mapIndex(ctx, c, p)
	if err != nil {
		return MapPage{}, err
	}
	b := [4]float64{-180, -85.051129, 180, 85.051129}
	if p.Has("bbox") {
		b, err = Bbox(p.First("bbox", ""))
		if err != nil {
			return MapPage{}, err
		}
	}
	zf, err := strconv.ParseFloat(p.First("zoom", "15"), 64)
	if err != nil || math.IsNaN(zf) || math.IsInf(zf, 0) || zf < 0 || zf > 22 {
		return MapPage{}, py.Errorf("Zoom must be between 0 and 22")
	}
	z := int(math.Floor(zf))
	q := py.Params{}
	for k, v := range p {
		if k != "cursor" && k != "limit" {
			q[k] = v
		}
	}
	key := digest(version + paramsKey(q))
	start := 0
	if cursor := p.First("cursor", ""); cursor != "" {
		parts := strings.Split(cursor, ".")
		if len(parts) != 2 || parts[0] != key {
			return MapPage{}, py.Errorf("Map changed; restart pagination")
		}
		start, err = strconv.Atoi(parts[1])
		if err != nil || start < 0 {
			return MapPage{}, py.Errorf("Invalid map cursor")
		}
	}
	// An uncapped individual view may contain hundreds of thousands of APs.
	// Keep compact membership indices rather than rebuilding an oversized full
	// feature payload for every page after the response-cache budget is exceeded.
	if (p.Is("clustering", "0") || p.Has("cell")) && len(idx.rows)*160 >= 16<<20 {
		return a.mapIndividuals(ctx, idx, version, key, start, z, b, p)
	}
	m := &a.mapCache
	m.mu.Lock()
	cached := m.view
	if m.viewKey != key {
		cached = nil
	}
	m.mu.Unlock()
	if cached == nil {
		page := &MapPage{Features: []MapFeature{}, Revision: version}
		clusters := map[string]int{}
		pins := map[string]bool{}
		for _, id := range p["pin"] {
			pins[id] = true
		}
		var cells []string
		if !p.Is("clustering", "0") || p.Has("cell") {
			cells = idx.cells(z)
		}
		idx.each(b, func(index int, r MapFit) {
			if pins[r.ID] || ctx.Err() != nil {
				return
			}
			cell := ""
			if cells != nil {
				cell = cells[index]
			}
			if only := p.First("cell", ""); only != "" && only != cell {
				return
			}
			page.Total++
			if p.Is("clustering", "0") || p.Has("cell") {
				page.Features = append(page.Features, MapFeature{ID: r.ID, Lon: r.Lon, Lat: r.Lat, Count: 1, Bounds: [4]float64{r.Lon, r.Lat, r.Lon, r.Lat}, Label: r.SSID, Band: r.Band})
				return
			}
			if i, ok := clusters[cell]; ok {
				g := &page.Features[i]
				g.Lon += r.Lon
				g.Lat += r.Lat
				g.Count++
				g.Bounds[0] = math.Min(g.Bounds[0], r.Lon)
				g.Bounds[1] = math.Min(g.Bounds[1], r.Lat)
				g.Bounds[2] = math.Max(g.Bounds[2], r.Lon)
				g.Bounds[3] = math.Max(g.Bounds[3], r.Lat)
			} else {
				clusters[cell] = len(page.Features)
				page.Features = append(page.Features, MapFeature{ID: cell, memberID: r.ID, Lon: r.Lon, Lat: r.Lat, Count: 1, Bounds: [4]float64{r.Lon, r.Lat, r.Lon, r.Lat}, Label: r.SSID, Band: r.Band})
			}
		})
		if err := ctx.Err(); err != nil {
			return MapPage{}, err
		}
		if !p.Is("clustering", "0") && !p.Has("cell") {
			// Singletons need the real AP identity, not a cluster cell identity.
			for i := range page.Features {
				g := &page.Features[i]
				g.Lon /= float64(g.Count)
				g.Lat /= float64(g.Count)
				if g.Count == 1 {
					g.ID = g.memberID
				} else {
					// Fixed world-cell centres keep neighbouring cluster badges apart
					// and stable while panning. Bounds retain the actual AP extent.
					g.Lon, g.Lat = cellCenter(g.ID, z)
					g.Label = ""
					g.Band = ""
				}
			}
		}
		sort.Slice(page.Features, func(i, j int) bool { return page.Features[i].ID < page.Features[j].ID })
		page.FeatureTotal = len(page.Features)
		// Retain one bounded viewport, sharing no mutable page state with callers.
		m.mu.Lock()
		if len(page.Features)*160 < 16<<20 {
			m.view = page
			m.viewKey = key
			m.viewMembers = nil
			m.membersKey = ""
		}
		m.mu.Unlock()
		cached = page
	}
	if start > len(cached.Features) {
		return MapPage{}, py.Errorf("Invalid map cursor")
	}
	limit := 1500
	if v, e := strconv.Atoi(p.First("limit", "")); e == nil && v > 0 {
		limit = min(v, 1500)
	}
	end := min(len(cached.Features), start+limit)
	// Target ~900 KB; a single imported identity is never split between pages.
	bytes := 0
	for i := start; i < end; i++ {
		r := cached.Features[i]
		bytes += 200 + 6*(len(r.ID)+len(r.Label)+len(r.Band))
		if bytes > 900000 && i > start {
			end = i
			break
		}
	}
	out := *cached
	out.Features = cached.Features[start:end]
	if end < len(cached.Features) {
		out.Next = key + "." + strconv.Itoa(end)
	}
	return out, nil
}

func (a *Atlas) mapIndividuals(ctx context.Context, idx *mapIndex, version, key string, start, z int, b [4]float64, p py.Params) (MapPage, error) {
	m := &a.mapCache
	m.mu.Lock()
	rows := m.viewMembers
	if m.membersKey != key {
		rows = nil
	}
	m.mu.Unlock()
	if rows == nil {
		rows = []int{}
		pins := map[string]bool{}
		for _, id := range p["pin"] {
			pins[id] = true
		}
		var cells []string
		if p.First("cell", "") != "" {
			cells = idx.cells(z)
		}
		idx.each(b, func(i int, r MapFit) {
			if !pins[r.ID] && ctx.Err() == nil && (cells == nil || cells[i] == p.First("cell", "")) {
				rows = append(rows, i)
			}
		})
		if err := ctx.Err(); err != nil {
			return MapPage{}, err
		}
		sort.Slice(rows, func(i, j int) bool { return idx.rows[rows[i]].ID < idx.rows[rows[j]].ID })
		m.mu.Lock()
		if len(rows)*8 <= 16<<20 {
			m.viewMembers = rows
			m.membersKey = key
			m.view = nil
			m.viewKey = ""
		}
		m.mu.Unlock()
	}
	if start > len(rows) {
		return MapPage{}, py.Errorf("Invalid map cursor")
	}
	limit := 1500
	if v, e := strconv.Atoi(p.First("limit", "")); e == nil && v > 0 {
		limit = min(v, 1500)
	}
	out := MapPage{Features: []MapFeature{}, Total: len(rows), FeatureTotal: len(rows), Revision: version}
	end := min(len(rows), start+limit)
	bytes := 0
	for i := start; i < end; i++ {
		r := idx.rows[rows[i]]
		size := 200 + 6*(len(r.ID)+len(r.SSID)+len(r.Band))
		if bytes+size > 900000 && i > start {
			end = i
			break
		}
		bytes += size
		out.Features = append(out.Features, MapFeature{ID: r.ID, Lon: r.Lon, Lat: r.Lat, Count: 1, Bounds: [4]float64{r.Lon, r.Lat, r.Lon, r.Lat}, Label: r.SSID, Band: r.Band})
	}
	if end < len(rows) {
		out.Next = key + "." + strconv.Itoa(end)
	}
	return out, nil
}

func (a *Atlas) MapDetail(ctx context.Context, c *sqlite.Conn, p py.Params) (*MapFit, error) {
	idx, _, err := a.mapIndex(ctx, c, p)
	if err != nil {
		return nil, err
	}
	if i, ok := idx.byID[p.First("id", "")]; ok {
		r := idx.rows[i]
		return &r, nil
	}
	return nil, py.Errorf("AP is no longer available in this scope")
}

type MapSummary struct {
	LocatedSSIDs  int    `json:"located_ssids"`
	ObservedSSIDs int    `json:"observed_ssids"`
	Observations  int    `json:"observations"`
	Revision      string `json:"revision"`
}

// Counts describe the entire survey, independent of map selection and viewport.
// Located SSIDs are nonempty names with a valid saved fit in any scope.
func (a *Atlas) MapSummary(c *sqlite.Conn) (MapSummary, error) {
	m := &a.mapCache
	m.mu.Lock()
	defer m.mu.Unlock()
	v, err := db.Scalar(c, "SELECT value FROM map_revision WHERE id=1")
	if err != nil {
		return MapSummary{}, err
	}
	st := a.Obs.State()
	rev := db.Int(v)
	m.observe(st, rev)
	version := fmt.Sprintf("%d:%d", m.epoch, rev)
	if m.summary != nil && m.state == st && m.summary.Revision == version {
		return *m.summary, nil
	}
	out := MapSummary{Observations: st.N, Revision: version}
	names := map[string]bool{}
	for id := range st.Net {
		if st.Str[id] != "" {
			names[st.Str[id]] = true
			out.ObservedSSIDs++
		}
	}
	err = db.Each(c, `SELECT DISTINCT json_extract(result,'$.ssid') FROM estimates WHERE
	json_extract(result,'$.status')='fitted' AND json_type(result,'$.latitude') IN ('integer','real')
	AND json_type(result,'$.longitude') IN ('integer','real') AND json_extract(result,'$.latitude') BETWEEN -85.051129 AND 85.051129
	AND json_extract(result,'$.longitude') BETWEEN -180 AND 180`, nil, func(s *sqlite.Stmt) error {
		if names[s.ColumnText(0)] {
			out.LocatedSSIDs++
		}
		return nil
	})
	if err == nil && m.state == st {
		m.summary = &out
	}
	return out, err
}

type MapDisplay struct {
	Clustering   bool `json:"clustering"`
	LabelMinZoom int  `json:"label_min_zoom"`
	Radius       int  `json:"radius_m"`
	Rings        bool `json:"rings"`
}

func DefaultMapDisplay() MapDisplay { return MapDisplay{true, 18, 10, true} }
func (d MapDisplay) Validate() error {
	if d.LabelMinZoom < 2 || d.LabelMinZoom > 22 {
		return py.Errorf("AP label zoom must be between 2 and 22")
	}
	if d.Radius < 5 || d.Radius > 50 {
		return py.Errorf("Planning radius must be between 5 and 50 metres")
	}
	return nil
}
func (a *Atlas) MapDisplay(c *sqlite.Conn) (MapDisplay, error) {
	d := DefaultMapDisplay()
	v, err := db.Scalar(c, "SELECT payload FROM map_assets WHERE name='map_display'")
	if err != nil {
		return d, err
	}
	if v != nil {
		var version struct {
			Version int `json:"version"`
		}
		if err = json.Unmarshal([]byte(db.Str(v)), &version); err != nil {
			return d, err
		}
		if version.Version > 1 || version.Version < 0 {
			return d, py.Errorf("Unsupported map display preferences version")
		}
		if err = json.Unmarshal([]byte(db.Str(v)), &d); err != nil {
			return d, err
		}
	}
	return d, d.Validate()
}
func (a *Atlas) SaveMapDisplay(c *sqlite.Conn, d MapDisplay) error {
	if err := d.Validate(); err != nil {
		return err
	}
	b, _ := json.Marshal(struct {
		Version int `json:"version"`
		MapDisplay
	}{1, d})
	_, err := db.Exec(c, "INSERT OR REPLACE INTO map_assets(name,payload) VALUES('map_display',?)", string(b))
	return err
}
