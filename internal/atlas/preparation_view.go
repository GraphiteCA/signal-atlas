package atlas

import (
	"context"
	"encoding/json"
	"math"
	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"sort"
)

// PreparationView walks immutable spatial postings in bounded chunks. It is
// owned by one preparation coordinator, never by an HTTP request or worker.
// Only scheduling uses these bounds; fitting keeps the complete target inputs.
type PreparationView struct {
	state        *obs.State
	bounds       [4]float64
	visible      [4]float64
	visibleCells int
	halo         bool
	cells        []int64
	cell, offset int
	seen         map[obs.Pair]bool
}

func (a *Atlas) NewPreparationView(box [4]float64) *PreparationView {
	for _, x := range box {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
	}
	if box[0] < -180 || box[2] > 180 || box[1] < -85 || box[3] > 85 || box[0] > box[2] || box[1] > box[3] {
		return nil
	}
	st := a.Obs.State()
	cy, cx := (box[1]+box[3])/2, (box[0]+box[2])/2
	dy := math.Max((box[3]-box[1])*.25, 500/MetresPerDegree)
	dx := math.Max((box[2]-box[0])*.25, 500/(MetresPerDegree*math.Cos(cy*math.Pi/180)))
	b := [4]float64{math.Max(-180, box[0]-dx), math.Max(-85, box[1]-dy), math.Min(180, box[2]+dx), math.Min(85, box[3]+dy)}
	la0, la1 := int64(math.Floor(b[1]/obs.GridDeg)), int64(math.Floor(b[3]/obs.GridDeg))
	lo0, lo1 := int64(math.Floor(b[0]/obs.GridDeg)), int64(math.Floor(b[2]/obs.GridDeg))
	if (la1-la0+1)*(lo1-lo0+1) > 4096 {
		return nil
	}
	v := &PreparationView{state: st, bounds: b, visible: box, seen: map[obs.Pair]bool{}}
	for la := la0; la <= la1; la++ {
		for lo := lo0; lo <= lo1; lo++ {
			key := la<<32 | int64(uint32(int32(lo)))
			if len(st.Grid[key]) > 0 {
				v.cells = append(v.cells, key)
			}
		}
	}
	rank := func(key int64) (bool, float64) {
		lat, lon := float64(key>>32)*obs.GridDeg, float64(int32(key))*obs.GridDeg
		outside := lat > box[3] || lat+obs.GridDeg < box[1] || lon > box[2] || lon+obs.GridDeg < box[0]
		x, y := (lon+obs.GridDeg/2-cx)*math.Cos(cy*math.Pi/180), lat+obs.GridDeg/2-cy
		return outside, x*x + y*y
	}
	sort.Slice(v.cells, func(i, j int) bool {
		oi, di := rank(v.cells[i])
		oj, dj := rank(v.cells[j])
		if oi != oj {
			return !oi
		}
		if di != dj {
			return di < dj
		}
		return v.cells[i] < v.cells[j]
	})
	for _, key := range v.cells {
		outside, _ := rank(key)
		if !outside {
			v.visibleCells++
		}
	}
	return v
}
func (v *PreparationView) Done() bool            { return v == nil || v.halo && v.cell >= len(v.cells) }
func (v *PreparationView) Current(a *Atlas) bool { return v != nil && v.state == a.Obs.State() }

func (a *Atlas) NextViewportPreparationTargets(ctx context.Context, ns string, budget int64, v *PreparationView, limit int) ([]PreparationTarget, error) {
	if limit < 1 {
		return nil, nil
	}
	if v.Done() {
		return nil, nil
	}
	if !v.Current(a) {
		return nil, ErrStaleInput
	}
	// Caps both per-refill CPU work and retained scheduling memory. Repeated
	// sightings are deduplicated; the cursor eventually visits every local row.
	capKeys := min(512, limit)
	keys := make([]string, 0, capKeys)
	for scanned := 0; scanned < 16384 && len(keys) < capKeys && !v.Done(); scanned++ {
		if scanned&1023 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !v.halo && v.cell >= v.visibleCells {
			v.halo = true
			v.cell = 0
			v.offset = 0
		}
		if v.Done() {
			break
		}
		rows := v.state.Grid[v.cells[v.cell]]
		if v.offset >= len(rows) {
			v.cell++
			v.offset = 0
			continue
		}
		g := rows[v.offset]
		v.offset++
		s, i := v.state.Loc(g)
		b := v.bounds
		if s.Lat[i] < b[1] || s.Lat[i] > b[3] || s.Lon[i] < b[0] || s.Lon[i] > b[2] || s.RSSI[i] < -150 {
			continue
		}
		visible := s.Lat[i] >= v.visible[1] && s.Lat[i] <= v.visible[3] && s.Lon[i] >= v.visible[0] && s.Lon[i] <= v.visible[2]
		if visible == v.halo {
			continue
		}
		sid, mid := s.SSID[i], s.MAC[i]
		ssid := v.state.Str[sid]
		if ssid != "" {
			mid = 0
		}
		pair := obs.Pair{sid, mid}
		if v.seen[pair] {
			continue
		}
		if len(v.seen) >= 16384 {
			v.seen = map[obs.Pair]bool{}
		}
		v.seen[pair] = true
		mac := ""
		if ssid == "" {
			mac = v.state.Str[mid]
		}
		keys = append(keys, TargetKey(ssid, mac))
	}
	if len(keys) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	c, put, err := a.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer put()
	rows, err := db.All(c, `SELECT t.* FROM json_each(?) j JOIN preparation_pending p ON p.target_key=j.value AND p.namespace=? JOIN preparation_targets t ON t.target_key=p.target_key AND t.version=p.version WHERE p.blocked_bytes<=? ORDER BY cast(j.key AS INTEGER) LIMIT ?`, string(data), ns, budget, limit)
	if err != nil {
		return nil, err
	}
	out := make([]PreparationTarget, 0, len(rows))
	for _, r := range rows {
		out = append(out, PreparationTarget{Key: db.Str(r["target_key"]), SSID: db.Str(r["ssid"]), MAC: db.Str(r["mac"]), Version: db.Int(r["version"])})
	}
	return out, nil
}
