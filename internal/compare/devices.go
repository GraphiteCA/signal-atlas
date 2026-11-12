package compare

import (
	"sort"
	"strings"

	"signal-atlas/internal/devices"
)

// ---- Devices: what the drives' networks and Bluetooth devices are, and who made them ----
//
// Only the app answers this (internal/devices holds the tables); the page's worker doesn't, so the
// analyse result the two compare stays as it was. The answer covers the drives of the last analyse,
// less the excluded ones.

// DeviceFilter narrows the device list (the counts follow Typ only).
type DeviceFilter struct {
	Typ      string `json:"typ"`      // all | wifi | ble
	Category string `json:"category"` // a category code, "notable", or "" for any
	Maker    string `json:"maker"`    // an exact manufacturer, "-" for none known, or "" for any
	Drive    string `json:"drive"`    // a drive key, or "" for any
	Text     string `json:"text"`     // in the MAC, name, manufacturer or detail
	Limit    int    `json:"limit"`
}

// devClass is one MAC's classification, kept while its name and manufacturer id stay the same.
type devClass struct {
	ssid uint32
	name bool
	mfgr int32
	r    devices.Result
}

type devAcc struct {
	mac        uint32
	n          int32
	best       int8
	lat, lon   float64
	first      uint32
	last       uint32
	lastNamed  uint32
	ssid       uint32
	named, ble bool
	drives     []uint16
	cls        devices.Result
}

type devState struct {
	gen      int
	excluded string
	keys     []string
	accs     []devAcc
}

const maxDeviceRows = 2000

// ssidNames turns the SSID ids back into names (the parser keeps only name -> id).
func (e *Engine) ssidNames() []string {
	names := make([]string, len(e.ssidIdx))
	for s, id := range e.ssidIdx {
		if int(id) < len(names) {
			names[id] = s
		}
	}
	return names
}

func (e *Engine) devState(cfg *Config) *devState {
	ex := map[string]bool{}
	var exl []string
	if cfg != nil {
		for _, k := range cfg.Excluded {
			ex[k] = true
			exl = append(exl, k)
		}
	}
	sort.Strings(exl)
	exKey := strings.Join(exl, "\x00")
	if e.dev != nil && e.dev.gen == e.unitsGen && e.dev.excluded == exKey {
		return e.dev
	}
	var keys []string
	for k := range e.lastUnits {
		if !ex[k] {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(a, b int) bool {
		ua, ub := e.lastUnits[keys[a]], e.lastUnits[keys[b]]
		if ua.a != ub.a {
			return ua.a < ub.a
		}
		return keys[a] < keys[b]
	})
	st := &devState{gen: e.unitsGen, excluded: exKey, keys: keys}
	pos := make([]int32, len(e.macStr))
	for i := range pos {
		pos[i] = -1
	}
	for di, k := range keys {
		e.say(map[string]any{"type": "progress", "text": "Identifying devices", "frac": float64(di) / float64(max(len(keys), 1))})
		u := e.lastUnits[k]
		for _, f := range u.files {
			C := &f.C
			for i := 0; i < f.N; i++ {
				t := C.t[i]
				if float64(t) < u.a || float64(t) >= u.b {
					continue
				}
				m := C.mac[i]
				p := pos[m]
				if p < 0 {
					p = int32(len(st.accs))
					pos[m] = p
					st.accs = append(st.accs, devAcc{mac: m, best: -128, first: t, last: t})
				}
				a := &st.accs[p]
				a.n++
				if C.rssi[i] > a.best {
					a.best, a.lat, a.lon = C.rssi[i], C.lat[i], C.lon[i]
				}
				a.first, a.last = min(a.first, t), max(a.last, t)
				fl := C.flags[i]
				a.ble = a.ble || fl&1 != 0
				// the latest name the device gave
				if fl&2 == 0 && (!a.named || t >= a.lastNamed) {
					a.ssid, a.named, a.lastNamed = C.ssid[i], true, t
				}
				if n := len(a.drives); n == 0 || a.drives[n-1] != uint16(di) {
					a.drives = append(a.drives, uint16(di))
				}
			}
		}
	}
	// classify each device once; a name or manufacturer id that hasn't changed keeps its answer
	names := e.ssidNames()
	if e.devCls == nil {
		e.devCls = map[uint32]devClass{}
	}
	for i := range st.accs {
		a := &st.accs[i]
		mf := int32(-1)
		if int(a.mac) < len(e.macMfgr) {
			mf = e.macMfgr[a.mac]
		}
		if c, ok := e.devCls[a.mac]; ok && c.ssid == a.ssid && c.name == a.named && c.mfgr == mf {
			a.cls = c.r
			continue
		}
		name := ""
		if a.named {
			name = names[a.ssid]
		}
		a.cls = devices.Classify(e.macStr[a.mac], name, a.ble, int(mf))
		e.devCls[a.mac] = devClass{ssid: a.ssid, name: a.named, mfgr: mf, r: a.cls}
	}
	e.dev = st
	return st
}

// Devices answers the device counts for each drive and the devices the filter picks.
func (e *Engine) Devices(cfg *Config, f *DeviceFilter) map[string]any {
	if f == nil {
		f = &DeviceFilter{}
	}
	out := map[string]any{"type": "devices", "filter": f}
	if len(e.files) == 0 || len(e.lastUnits) == 0 {
		out["drives"], out["categories"], out["makers"], out["list"], out["matched"] = []any{}, []any{}, []any{}, []any{}, 0
		return out
	}
	st := e.devState(cfg)
	names := e.ssidNames()
	nameOf := func(a *devAcc) string {
		if !a.named || int(a.ssid) >= len(names) {
			return ""
		}
		return names[a.ssid]
	}
	typOK := func(a *devAcc) bool {
		switch f.Typ {
		case "wifi":
			return !a.ble
		case "ble":
			return a.ble
		}
		return true
	}
	nd := len(st.keys)
	type tally struct {
		n  int
		by []int
	}
	cats := map[string]*tally{}
	makers := map[string]*tally{}
	drv := make([]struct{ devices, notable, named int }, nd)
	add := func(m map[string]*tally, k string, a *devAcc) {
		t := m[k]
		if t == nil {
			t = &tally{by: make([]int, nd)}
			m[k] = t
		}
		t.n++
		for _, d := range a.drives {
			t.by[d]++
		}
	}
	text := strings.ToLower(strings.TrimSpace(f.Text))
	driveIx := -1
	for i, k := range st.keys {
		if k == f.Drive {
			driveIx = i
		}
	}
	var picked []*devAcc
	for i := range st.accs {
		a := &st.accs[i]
		if !typOK(a) {
			continue
		}
		add(cats, a.cls.Category, a)
		mk := a.cls.Maker
		if mk == "" {
			mk = "-"
		}
		add(makers, mk, a)
		for _, d := range a.drives {
			drv[d].devices++
			if a.cls.Notable {
				drv[d].notable++
			}
			if devices.Lookup(a.cls.Category).Group != "Unclassified" {
				drv[d].named++
			}
		}
		// the list
		switch {
		case f.Category == "notable" && !a.cls.Notable,
			f.Category != "" && f.Category != "notable" && a.cls.Category != f.Category,
			f.Maker != "" && mk != f.Maker:
			continue
		}
		if driveIx >= 0 {
			in := false
			for _, d := range a.drives {
				in = in || int(d) == driveIx
			}
			if !in {
				continue
			}
		} else if f.Drive != "" {
			continue
		}
		if text != "" {
			hay := strings.ToLower(e.macStr[a.mac] + "\x00" + nameOf(a) + "\x00" + a.cls.Maker + "\x00" + a.cls.Detail)
			if !strings.Contains(hay, text) {
				continue
			}
		}
		picked = append(picked, a)
	}
	drives := make([]map[string]any, nd)
	for i, k := range st.keys {
		drives[i] = map[string]any{"key": k, "devices": drv[i].devices, "notable": drv[i].notable, "classified": drv[i].named}
	}
	byKey := func(by []int) map[string]int {
		m := map[string]int{}
		for i, n := range by {
			if n > 0 {
				m[st.keys[i]] = n
			}
		}
		return m
	}
	var catsOut []map[string]any
	for _, c := range devices.Categories {
		if t := cats[c.Code]; t != nil {
			catsOut = append(catsOut, map[string]any{"code": c.Code, "name": c.Name, "group": c.Group, "notable": c.Notable, "n": t.n, "by": byKey(t.by)})
		}
	}
	type mk struct {
		name string
		t    *tally
	}
	var ms []mk
	for n, t := range makers {
		ms = append(ms, mk{n, t})
	}
	sort.Slice(ms, func(a, b int) bool {
		if ms[a].t.n != ms[b].t.n {
			return ms[a].t.n > ms[b].t.n
		}
		return ms[a].name < ms[b].name
	})
	var makersOut []map[string]any
	for i, m := range ms {
		if i >= 300 && m.name != f.Maker {
			continue
		}
		makersOut = append(makersOut, map[string]any{"name": m.name, "n": m.t.n, "by": byKey(m.t.by)})
	}
	// notable first, then the most often seen
	sort.SliceStable(picked, func(a, b int) bool {
		x, y := picked[a], picked[b]
		if x.cls.Notable != y.cls.Notable {
			return x.cls.Notable
		}
		if x.n != y.n {
			return x.n > y.n
		}
		return e.macStr[x.mac] < e.macStr[y.mac]
	})
	limit := f.Limit
	if limit <= 0 || limit > maxDeviceRows {
		limit = 500
	}
	var list []map[string]any
	for i, a := range picked {
		if i >= limit {
			break
		}
		ks := make([]string, len(a.drives))
		for j, d := range a.drives {
			ks[j] = st.keys[d]
		}
		typ := "WIFI"
		if a.ble {
			typ = "BLE"
		}
		list = append(list, map[string]any{"mac": e.macStr[a.mac], "name": nameOf(a), "type": typ, "maker": a.cls.Maker,
			"category": a.cls.Category, "detail": a.cls.Detail, "notable": a.cls.Notable, "random": a.cls.Random,
			"n": a.n, "best": a.best, "lat": a.lat, "lon": a.lon, "first": a.first, "last": a.last, "drives": ks})
	}
	if list == nil {
		list = []map[string]any{}
	}
	if catsOut == nil {
		catsOut = []map[string]any{}
	}
	if makersOut == nil {
		makersOut = []map[string]any{}
	}
	out["drives"], out["categories"], out["makers"], out["list"], out["matched"] = drives, catsOut, makersOut, list, len(picked)
	out["makerCount"] = len(ms)
	return out
}
