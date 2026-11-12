package compare

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Config is the page's cfg().
type Config struct {
	Mode      string    `json:"mode"`
	Cuts      []float64 `json:"cuts"`
	Joins     []float64 `json:"joins"`
	DriveGapH float64   `json:"driveGapH"`
	LegGapMin float64   `json:"legGapMin"`
	Radius    float64   `json:"radius"`
	Typ       string    `json:"typ"`
	Baseline  *string   `json:"baseline"`
	Excluded  []string  `json:"excluded"`
	TzOff     float64   `json:"tzOff"`
}

var bands = []struct {
	lo, hi float64
	name   string
}{{0, 5, "Stopped, under 5"}, {5, 30, "5–30"}, {30, 50, "30–50"}, {50, 70, "50–70"}, {70, 90, "70–90"}, {90, 1e9, "90 and up"}}

func bandOf(v float64) int {
	for i, b := range bands {
		if v < b.hi {
			return i
		}
	}
	return len(bands) - 1
}

var cuts = []struct {
	lo, hi float64
	name   string
}{{-999, -90, "−90 or weaker"}, {-89, -85, "−89 to −85"}, {-84, -80, "−84 to −80"}, {-79, -75, "−79 to −75"}, {-74, -70, "−74 to −70"}, {-69, -60, "−69 to −60"}, {-59, 0, "−59 or stronger"}}

// drive is buildDrive()'s object.
type drive struct {
	id           int
	key          string
	origin       int
	ra, rb       float64
	partial      bool
	files        []*File
	n            int
	D            cols
	S            int
	sT           []float64
	sStart       []int
	sLa, sLo     []float64
	X, Y, dt, ds []float64
	v            []float32
	legs         []leg
	px, py       []float64
	pl           []float32
	ps           []uint32
	ix           int
}

type leg struct{ a, b int }

func (e *Engine) mx(lo float64) float64 { return lo * 111320 * e.cos }
func (e *Engine) my(la float64) float64 { return la * 110950 }

func (e *Engine) buildDrive(fl []*File, cfg *Config, id int, ra, rb float64, pre, sfx string, origin int) *drive {
	n := 0
	for _, f := range fl {
		for i := 0; i < f.N; i++ {
			if t := float64(f.C.t[i]); t >= ra && t < rb {
				n++
			}
		}
	}
	if n == 0 {
		return nil
	}
	// the rows in range, then a stable sort by time (the worker's idx.sort((a,b)=>tt[a]-tt[b]||a-b))
	type ref struct {
		f *File
		i int32
	}
	refs := make([]ref, 0, n)
	for _, f := range fl {
		for i := 0; i < f.N; i++ {
			if t := float64(f.C.t[i]); t >= ra && t < rb {
				refs = append(refs, ref{f, int32(i)})
			}
		}
	}
	sort.SliceStable(refs, func(a, b int) bool { return refs[a].f.C.t[refs[a].i] < refs[b].f.C.t[refs[b].i] })
	var D cols
	D.grow(n)
	for _, r := range refs {
		c, i := &r.f.C, r.i
		D.t, D.mac, D.rssi, D.node = append(D.t, c.t[i]), append(D.mac, c.mac[i]), append(D.rssi, c.rssi[i]), append(D.node, c.node[i])
		D.chn, D.band, D.auth, D.flags = append(D.chn, c.chn[i]), append(D.band, c.band[i]), append(D.auth, c.auth[i]), append(D.flags, c.flags[i])
		D.ssid, D.lat, D.lon = append(D.ssid, c.ssid[i]), append(D.lat, c.lat[i]), append(D.lon, c.lon[i])
	}
	refs = nil
	d := &drive{id: id, origin: origin, ra: ra, rb: rb, n: n, D: D}
	cur := -1.0
	for i := 0; i < n; i++ {
		if t := float64(D.t[i]); t != cur {
			cur = t
			d.sT = append(d.sT, t)
			d.sLa = append(d.sLa, D.lat[i])
			d.sLo = append(d.sLo, D.lon[i])
			d.sStart = append(d.sStart, i)
		}
	}
	d.sStart = append(d.sStart, n)
	S := len(d.sT)
	d.S = S
	d.X, d.Y, d.dt, d.ds, d.v = make([]float64, S), make([]float64, S), make([]float64, S), make([]float64, S), make([]float32, S)
	for i := 0; i < S; i++ {
		d.X[i], d.Y[i] = e.mx(d.sLo[i]), e.my(d.sLa[i])
		if i > 0 {
			d.dt[i] = d.sT[i] - d.sT[i-1]
			d.ds[i] = math.Hypot(d.X[i]-d.X[i-1], d.Y[i]-d.Y[i-1])
		}
	}
	sT, dt, ds := d.sT, d.dt, d.ds
	for i := 0; i < S; i++ {
		a, b, dd := i, i, 0.0
		for a > 0 && i-a < 3 && dt[a] <= 5 && sT[i]-sT[a-1] <= 8 {
			dd += ds[a]
			a--
		}
		for b < S-1 && b-i < 3 && dt[b+1] <= 5 && sT[b+1]-sT[i] <= 8 {
			b++
			dd += ds[b]
		}
		el := sT[b] - sT[a]
		switch {
		case el > 0:
			d.v[i] = float32(dd / el * 3.6)
		case i > 0 && dt[i] < 60 && dt[i] > 0:
			d.v[i] = float32(ds[i] / dt[i] * 3.6)
		default:
			d.v[i] = 0
		}
	}
	ls := 0
	for i := 1; i <= S; i++ {
		if i == S || dt[i] >= cfg.LegGapMin*60 {
			d.legs = append(d.legs, leg{ls, i - 1})
			ls = i
		}
	}
	// densified points, no bridging of gaps
	for i := 1; i < S; i++ {
		if dt[i] > 60 && ds[i] > 200 {
			continue
		}
		m := int(math.Max(1, math.Floor(ds[i]/10)))
		for k := 0; k < m; k++ {
			d.px = append(d.px, d.X[i-1]+(d.X[i]-d.X[i-1])*float64(k)/float64(m))
			d.py = append(d.py, d.Y[i-1]+(d.Y[i]-d.Y[i-1])*float64(k)/float64(m))
			d.pl = append(d.pl, float32(ds[i]/float64(m)))
			d.ps = append(d.ps, uint32(i))
		}
	}
	for _, f := range fl {
		if f.Tmin < rb && f.Tmax >= ra {
			d.files = append(d.files, f)
			if f.Tmin < ra || f.Tmax >= rb {
				d.partial = true
			}
		}
	}
	d.key = pre + jsKey(sT[0]) + sfx
	return d
}

// hash is the worker's hashOf/near spatial hash.
type hash struct {
	h      map[int64][]int
	size   float64
	xs, ys []float64
}

func hkey(x, y, size float64) int64 {
	return int64(math.Floor(x/size))*4194304 + int64(math.Floor(y/size))
}

func hashOf(xs, ys []float64, size float64) *hash {
	h := &hash{h: map[int64][]int{}, size: size, xs: xs, ys: ys}
	for i := range xs {
		k := hkey(xs[i], ys[i], size)
		h.h[k] = append(h.h[k], i)
	}
	return h
}

func (H *hash) add(x, y float64) {
	i := len(H.xs)
	H.xs, H.ys = append(H.xs, x), append(H.ys, y)
	k := hkey(x, y, H.size)
	H.h[k] = append(H.h[k], i)
}

func near(H *hash, x, y, r float64) bool {
	cx, cy := int64(math.Floor(x/H.size)), int64(math.Floor(y/H.size))
	r2 := r * r
	R := int64(math.Ceil(r / H.size))
	for i := -R; i <= R; i++ {
		for j := -R; j <= R; j++ {
			l := H.h[(cx+i)*4194304+(cy+j)]
			for _, d := range l {
				dx, dy := H.xs[d]-x, H.ys[d]-y
				if dx*dx+dy*dy <= r2 {
					return true
				}
			}
		}
	}
	return false
}

func med(a []float64) any {
	if len(a) == 0 {
		return nil
	}
	s := append([]float64(nil), a...)
	sort.Float64s(s)
	m := len(s) >> 1
	if len(s)%2 == 1 {
		return Num(s[m])
	}
	return Num((s[m-1] + s[m]) / 2)
}

func medv(a []float64) (float64, bool) {
	v := med(a)
	if v == nil {
		return 0, false
	}
	return float64(v.(Num)), true
}

func qt(a []float64, q float64) any {
	if len(a) == 0 {
		return nil
	}
	s := append([]float64(nil), a...)
	sort.Float64s(s)
	return Num(s[min(len(s)-1, int(math.Floor(q*float64(len(s)))))])
}

func mean(a []float64) any {
	if len(a) == 0 {
		return nil
	}
	t := 0.0
	for _, x := range a {
		t += x
	}
	return Num(t / float64(len(a)))
}

func hStat(h []int32) map[string]any {
	n := 0
	for i := 1; i < len(h); i++ {
		n += int(h[i])
	}
	if n == 0 {
		return map[string]any{"n": 0, "med": nil, "p90": nil, "fast": nil}
	}
	c, f := 0, 0
	var m, p any
	for i := 1; i < len(h); i++ {
		c += int(h[i])
		if i <= 2 {
			f += int(h[i])
		}
		if m == nil && float64(c) >= float64(n)/2 {
			m = i
		}
		if p == nil && float64(c) >= float64(n)*.9 {
			p = i
		}
	}
	return map[string]any{"n": n, "med": m, "p90": p, "fast": Num(float64(f) / float64(n) * 100)}
}

func ratio(ok bool, a, b float64) any {
	if !ok {
		return nil
	}
	return Num(a / b)
}

type bandAcc struct {
	sec, km                                  float64
	rows, distinct, dup, exact, newAps, once int
	enc, encKm                               []float64
}

// Analyse is the worker's analyse(cfg): the whole result object the page renders.
func (e *Engine) Analyse(cfg *Config) map[string]any {
	t0 := time.Now()
	if len(e.files) == 0 {
		return nil
	}
	la := 0.0
	for _, f := range e.files {
		la += f.C.lat[0]
	}
	e.lat0 = la / float64(len(e.files))
	e.cos = math.Cos(e.lat0 * math.Pi / 180)
	fs := append([]*File(nil), e.files...)
	sort.SliceStable(fs, func(a, b int) bool { return fs[a].Tmin < fs[b].Tmin })
	type group struct {
		device string
		tmax   float64
		fl     []*File
		solo   bool
	}
	var grp []*group
	// A solo file is a drive of its own, never lumped with another file.
	for _, f := range fs {
		var g *group
		if !f.Solo {
			for i := len(grp) - 1; i >= 0; i-- {
				x := grp[i]
				if !x.solo && x.device == f.Device && f.Tmin-x.tmax <= cfg.DriveGapH*3600 {
					g = x
					break
				}
			}
		}
		if g == nil {
			g = &group{device: f.Device, tmax: f.Tmax, solo: f.Solo}
			grp = append(grp, g)
		}
		g.fl = append(g.fl, f)
		g.tmax = math.Max(g.tmax, f.Tmax)
	}
	sort.SliceStable(grp, func(a, b int) bool { return grp[a].fl[0].Tmin < grp[b].fl[0].Tmin })
	mode := cfg.Mode
	if mode == "" {
		mode = "drive"
	}
	type unit struct {
		fl       []*File
		a, b     float64
		origin   int
		pre, sfx string
	}
	var units []unit
	indexOf := func(f *File) int {
		for gi, g := range grp {
			for _, x := range g.fl {
				if x == f {
					return gi
				}
			}
		}
		return -1
	}
	if mode == "file" {
		for _, f := range fs {
			units = append(units, unit{[]*File{f}, f.Tmin, f.Tmax + 1, indexOf(f), "f", "x" + fmt.Sprint(f.Size)})
		}
	} else {
		for gi, g := range grp {
			a, b := math.Inf(1), math.Inf(-1)
			for _, f := range g.fl {
				a, b = math.Min(a, f.Tmin), math.Max(b, f.Tmax)
			}
			b++
			if mode == "leg" {
				if t := e.buildDrive(g.fl, cfg, 0, a, b, "l", "", gi); t != nil {
					for _, l := range t.legs {
						units = append(units, unit{g.fl, t.sT[l.a], t.sT[l.b] + 1, gi, "l", ""})
					}
				}
			} else {
				units = append(units, unit{g.fl, a, b, gi, "d", ""})
			}
		}
	}
	cs := append([]float64(nil), cfg.Cuts...)
	sort.Float64s(cs)
	var split []unit
	for _, u := range units {
		s := u.a
		for _, c := range cs {
			if c > s && c < u.b {
				x := u
				x.a, x.b = s, c
				split = append(split, x)
				s = c
			}
		}
		x := u
		x.a = s
		split = append(split, x)
	}
	units = split
	sort.SliceStable(units, func(a, b int) bool { return units[a].a < units[b].a })
	joins := map[float64]bool{}
	for _, j := range cfg.Joins {
		joins[j] = true
	}
	var merged []unit
	for _, u := range units {
		if k := len(merged); k > 0 && joins[u.a] && merged[k-1].fl[0].Device == u.fl[0].Device {
			p := &merged[k-1]
			seen := map[*File]bool{}
			var fl []*File
			for _, f := range append(append([]*File(nil), p.fl...), u.fl...) {
				if !seen[f] {
					seen[f] = true
					fl = append(fl, f)
				}
			}
			p.fl = fl
			p.b = math.Max(p.b, u.b)
		} else {
			merged = append(merged, u)
		}
	}
	var all []*drive
	for i, u := range merged {
		e.say(map[string]any{"type": "progress", "text": fmt.Sprintf("Building %d of %d", i+1, len(merged)), "frac": .05})
		if d := e.buildDrive(u.fl, cfg, i, u.a, u.b, u.pre, u.sfx, u.origin); d != nil {
			all = append(all, d)
		}
	}
	e.lastUnits = map[string]lastUnit{}
	e.unitsGen++
	for _, d := range all {
		e.lastUnits[d.key] = lastUnit{files: d.files, a: d.ra, b: d.rb}
	}
	ex := map[string]bool{}
	for _, k := range cfg.Excluded {
		ex[k] = true
	}
	var ds []*drive
	for _, d := range all {
		if !ex[d.key] {
			d.ix = len(ds)
			ds = append(ds, d)
		}
	}
	N := len(ds)
	mC := len(e.macStr)
	typ := cfg.Typ
	keep := func(fl uint8) bool {
		switch typ {
		case "all":
			return true
		case "wifi":
			return fl&1 == 0
		}
		return fl&1 != 0
	}
	firstDrive, lastDrive, genStamp := fill32(mC, -1), fill32(mC, -1), fill32(mC, -1)
	gen := int32(0)
	R := cfg.Radius
	var summaries []map[string]any
	seenInDrive := fill32(mC, -1)
	for di, d := range ds {
		e.say(map[string]any{"type": "progress", "text": fmt.Sprintf("Working out speed and visibility for drive %d of %d", di+1, N), "frac": .1 + .25*float64(di)/math.Max(1, float64(N))})
		B := make([]*bandAcc, len(bands))
		for i := range B {
			B[i] = &bandAcc{}
		}
		hasNodeD := false
		for _, f := range d.files {
			hasNodeD = hasNodeD || f.HasNode
		}
		chRows, chAps, chNew := map[uint16]int{}, map[uint16]int{}, map[uint16]int{}
		chBand := map[uint16]uint8{}
		chSeen := map[uint64]bool{}
		var chOrder []uint16
		pk := make([]int8, mC)
		for i := range pk {
			pk[i] = -128
		}
		fr := make([]int8, mC)
		var macsSeen []uint32
		firstB := [4][]float64{}
		nOf := make([]int32, mC)
		nBoth := 0
		D, S, sStart, dtv, sd, v, X, Y, sT := &d.D, d.S, d.sStart, d.dt, d.ds, d.v, d.X, d.Y, d.sT
		rows, dist, dup, exact, uniq := 0, 0, 0, 0, 0
		el, kmAll := 0.0, 0.0
		for s := 0; s < S; s++ {
			b := bandOf(float64(v[s]))
			ee := 0.0
			if dtv[s] < 60 {
				ee = dtv[s]
			}
			B[b].sec += ee
			el += ee
			if dtv[s] < 60 {
				B[b].km += sd[s]
				kmAll += sd[s]
			}
			gen++
			ex2 := map[float64]bool{}
			for i := sStart[s]; i < sStart[s+1]; i++ {
				if !keep(D.flags[i]) {
					continue
				}
				m := D.mac[i]
				rows++
				B[b].rows++
				k := float64(m)*1e9 + float64(D.node[i]+1)*1e6 + float64(int(D.rssi[i])+128)*1000 + float64(D.chn[i])
				if genStamp[m] == gen {
					dup++
					B[b].dup++
					if hasNodeD && nOf[m] >= 0 && nOf[m] != int32(D.node[i]) {
						nBoth++
						nOf[m] = -1
					}
				} else {
					genStamp[m] = gen
					dist++
					B[b].distinct++
					if hasNodeD {
						nOf[m] = int32(D.node[i])
					}
				}
				ch := D.chn[i]
				if _, ok := chRows[ch]; !ok {
					chOrder = append(chOrder, ch)
				}
				chRows[ch]++
				if _, ok := chBand[ch]; !ok {
					chBand[ch] = D.band[i]
				}
				ck := uint64(m)*256 + uint64(ch&255)
				if !chSeen[ck] {
					chSeen[ck] = true
					chAps[ch]++
				}
				if D.rssi[i] > pk[m] {
					pk[m] = D.rssi[i]
				}
				if ex2[k] {
					exact++
					B[b].exact++
				} else {
					ex2[k] = true
				}
				if seenInDrive[m] != int32(di) {
					seenInDrive[m] = int32(di)
					uniq++
					B[b].newAps++
					if firstDrive[m] < 0 {
						firstDrive[m] = int32(di)
					}
					lastDrive[m] = int32(di)
					chNew[ch]++
					fr[m] = D.rssi[i]
					macsSeen = append(macsSeen, m)
					firstB[D.band[i]] = append(firstB[D.band[i]], float64(D.rssi[i]))
				}
			}
		}
		const RV = 61
		rvAll := make([]int32, RV)
		rvB := [4][]int32{make([]int32, RV), make([]int32, RV), make([]int32, RV), make([]int32, RV)}
		nLT := map[int16][]uint32{}
		rvN := map[int16][]int32{}
		fT, lT, fS, lS, cnt := fill32(mC, -1), make([]int32, mC), make([]int32, mC), make([]int32, mC), make([]int32, mC)
		const ENC = 60
		var encMoving, encAll, encDist []float64
		encOnce := 0
		fin := func(m uint32) {
			du := float64(lT[m] - fT[m])
			sp := (float64(v[fS[m]]) + float64(v[lS[m]])) / 2
			b := bandOf(sp)
			km := math.Hypot(X[lS[m]]-X[fS[m]], Y[lS[m]]-Y[fS[m]])
			B[b].enc = append(B[b].enc, du)
			B[b].encKm = append(B[b].encKm, km)
			if cnt[m] == 1 {
				B[b].once++
				encOnce++
			}
			encAll = append(encAll, du)
			if sp >= 5 {
				encMoving = append(encMoving, du)
				encDist = append(encDist, km)
			}
		}
		for s := 0; s < S; s++ {
			tf := sT[s]
			t := int32(toInt(tf, 32))
			for i := sStart[s]; i < sStart[s+1]; i++ {
				if !keep(D.flags[i]) {
					continue
				}
				m := D.mac[i]
				if fT[m] >= 0 && tf > float64(lT[m]) && tf-float64(lT[m]) <= ENC {
					g := int(tf - float64(lT[m]))
					rvAll[g]++
					rvB[D.band[i]][g]++
				}
				if hasNodeD && D.node[i] >= 0 {
					nd := D.node[i]
					a := nLT[nd]
					if a == nil {
						a = make([]uint32, mC)
						nLT[nd] = a
					}
					p := a[m]
					if p != 0 && tf > float64(p) && tf-float64(p) <= ENC {
						h := rvN[nd]
						if h == nil {
							h = make([]int32, RV)
							rvN[nd] = h
						}
						h[int(tf-float64(p))]++
					}
					a[m] = uint32(toUint(tf, 32))
				}
				if fT[m] >= 0 && tf-float64(lT[m]) > ENC {
					fin(m)
					fT[m] = -1
				}
				if fT[m] < 0 {
					fT[m], fS[m], cnt[m] = t, int32(s), 0
				}
				lT[m], lS[m] = t, int32(s)
				cnt[m]++
			}
		}
		for m := 0; m < mC; m++ {
			if fT[m] >= 0 {
				fin(uint32(m))
			}
		}
		mS, mK := 0.0, 0.0
		for _, x := range B[1:] {
			mS += x.sec
		}
		for _, x := range B[1:] {
			mK += x.km
		}
		u := map[uint32]bool{}
		var uOrder []uint32
		open, hidden, rnd, b5, b24, b6, bleRows, wep, wpa3 := 0, 0, 0, 0, 0, 0, 0, 0, 0
		ssids := map[uint32]bool{}
		var rs []float64
		for i := 0; i < d.n; i++ {
			if D.flags[i]&1 != 0 {
				bleRows++
			}
			if !keep(D.flags[i]) {
				continue
			}
			if len(rs) < 500000 {
				rs = append(rs, float64(D.rssi[i]))
			}
			m := D.mac[i]
			if D.ssid[i] != 0 {
				ssids[D.ssid[i]] = true
			}
			if !u[m] {
				u[m] = true
				uOrder = append(uOrder, m)
				a := ""
				if int(D.auth[i]) < len(e.authNames) {
					a = e.authNames[D.auth[i]]
				}
				if a == "OPEN" {
					open++
				}
				if strings.HasPrefix(a, "WEP") {
					wep++
				}
				if strings.Contains(a, "WPA3") {
					wpa3++
				}
				if D.flags[i]&2 != 0 {
					hidden++
				}
				if e.macRand[m] != 0 {
					rnd++
				}
				switch D.band[i] {
				case 1:
					b24++
				case 2:
					b5++
				case 3:
					b6++
				}
			}
		}
		bleSet := map[uint32]bool{}
		for i := 0; i < d.n; i++ {
			if D.flags[i]&1 != 0 {
				bleSet[D.mac[i]] = true
			}
		}
		type nodeAcc struct {
			rows int
			set  map[uint32]bool
			chs  map[uint16]bool
			only int
		}
		nodes := map[int16]*nodeAcc{}
		var nodeOrder []int16
		anyNode := false
		for _, f := range d.files {
			anyNode = anyNode || f.HasNode
		}
		if anyNode {
			only := fill32(mC, -2)
			for i := 0; i < d.n; i++ {
				if !keep(D.flags[i]) || D.node[i] < 0 {
					continue
				}
				nd := D.node[i]
				na := nodes[nd]
				if na == nil {
					na = &nodeAcc{set: map[uint32]bool{}, chs: map[uint16]bool{}}
					nodes[nd] = na
					nodeOrder = append(nodeOrder, nd)
				}
				na.rows++
				na.set[D.mac[i]] = true
				na.chs[D.chn[i]] = true
				m := D.mac[i]
				switch {
				case only[m] == -2:
					only[m] = int32(nd)
				case only[m] == int32(nd):
				default:
					only[m] = -1
				}
			}
			uo := map[int32]int{}
			for _, m := range uOrder {
				if only[m] >= 0 {
					uo[only[m]]++
				}
			}
			for k, na := range nodes {
				na.only = uo[int32(k)]
			}
		}
		km := kmAll / 1000
		bandsOut := make([]any, len(B))
		for i, x := range B {
			bandsOut[i] = map[string]any{"band": bands[i].name, "sec": Num(x.sec), "km": Num(x.km / 1000),
				"rowsPerS": ratio(x.sec != 0, float64(x.rows), x.sec), "netsPerS": ratio(x.sec != 0, float64(x.distinct), x.sec),
				"dupPerS": ratio(x.sec != 0, float64(x.dup), x.sec), "newPerS": ratio(x.sec != 0, float64(x.newAps), x.sec),
				"newPerKm": ratio(x.km >= 1000, float64(x.newAps), x.km/1000), "visMean": mean(x.enc), "visMedian": med(x.enc),
				"visKm": mean(x.encKm), "encounters": len(x.enc), "oncePct": orNull(len(x.enc) > 0, float64(x.once)/float64(len(x.enc))*100)}
		}
		legsOut := make([]any, len(d.legs))
		for i, l := range d.legs {
			k := 0.0
			for s := l.a + 1; s <= l.b; s++ {
				if d.dt[s] < 60 {
					k += d.ds[s]
				}
			}
			legsOut[i] = map[string]any{"i": i, "start": Num(sT[l.a]), "end": Num(sT[l.b]), "km": Num(k / 1000)}
		}
		pct := func(n int) Num {
			if len(u) == 0 {
				return 0
			}
			return Num(float64(n) / float64(len(u)) * 100)
		}
		mix := map[string]any{"ssids": len(ssids), "medianRssi": med(rs), "b24": pct(b24), "b5": pct(b5), "b6": pct(b6), "open": pct(open),
			"hidden": pct(hidden), "rand": pct(rnd), "wpa3": pct(wpa3), "wep": pct(wep), "bleDev": len(bleSet), "bleRows": bleRows}
		sort.SliceStable(nodeOrder, func(a, b int) bool { return len(nodes[nodeOrder[a]].set) > len(nodes[nodeOrder[b]].set) })
		nodesOut := []any{}
		for _, k := range nodeOrder {
			na := nodes[k]
			chList := make([]int, 0, len(na.chs))
			for c := range na.chs {
				chList = append(chList, int(c))
			}
			sort.Ints(chList)
			h := rvN[k]
			if h == nil {
				h = make([]int32, RV)
			}
			name := ""
			if int(k) < len(e.nodeNames) && k >= 0 {
				name = e.nodeNames[k]
			}
			nodesOut = append(nodesOut, map[string]any{"node": name, "rows": na.rows, "aps": len(na.set), "only": na.only,
				"perS": zeroOr(el != 0, float64(na.rows)/el), "chans": len(na.chs), "chList": chList, "revisit": hStat(h)})
		}
		sort.Slice(chOrder, func(a, b int) bool { return chOrder[a] < chOrder[b] })
		chansOut := make([]any, len(chOrder))
		for i, ch := range chOrder {
			chansOut[i] = map[string]any{"ch": int(ch), "band": int(chBand[ch]), "rows": chRows[ch], "aps": chAps[ch], "newAps": chNew[ch]}
		}
		frs, gains := make([]float64, len(macsSeen)), make([]float64, len(macsSeen))
		atPeak := 0
		for i, m := range macsSeen {
			frs[i], gains[i] = float64(fr[m]), float64(int(pk[m])-int(fr[m]))
			if pk[m] == fr[m] {
				atPeak++
			}
		}
		var atPeakV any
		if len(macsSeen) > 0 {
			atPeakV = Num(float64(atPeak) / float64(len(macsSeen)) * 100)
		}
		var overlap any
		if hasNodeD && dist != 0 {
			overlap = Num(float64(nBoth) / float64(dist) * 100)
		}
		fileNames := make([]string, len(d.files))
		for i, f := range d.files {
			fileNames[i] = f.Name
		}
		summaries = append(summaries, map[string]any{"key": d.key, "start": Num(sT[0]), "end": Num(sT[S-1]), "device": d.files[0].Device, "files": fileNames,
			"km": Num(km), "elapsed": Num(el), "movingS": Num(mS), "movingKm": Num(mK / 1000), "avgMoving": zeroOr(mS != 0, mK/mS*3.6), "avgAll": zeroOr(el != 0, kmAll/el*3.6), "rows": rows, "aps": uniq,
			"netsPerS": zeroOr(el != 0, float64(dist)/el), "dupPerS": zeroOr(el != 0, float64(dup)/el), "exactPerS": zeroOr(el != 0, float64(exact)/el), "rowsPerS": zeroOr(el != 0, float64(rows)/el),
			"newPerS": zeroOr(el != 0, float64(uniq)/el), "apsPerKm": zeroOr(km != 0, float64(uniq)/km), "visMean": mean(encMoving), "visMedian": med(encMoving), "visAll": mean(encAll), "visKm": mean(encDist),
			"encounters": len(encAll), "oncePct": zeroOr(len(encAll) > 0, float64(encOnce)/float64(len(encAll))*100),
			"bands": bandsOut, "legs": legsOut, "mix": mix, "nodes": nodesOut, "hasNode": anyNode, "chans": chansOut,
			"first":   map[string]any{"med": med(frs), "gain": med(gains), "atPeak": atPeakV, "b24": med(firstB[1]), "b5": med(firstB[2])},
			"revisit": map[string]any{"all": hStat(rvAll), "b24": hStat(rvB[1]), "b5": hStat(rvB[2]), "b6": hStat(rvB[3])}, "nodeOverlap": overlap})
		_ = mK
	}
	return e.analyseRest(cfg, t0, all, ds, summaries, firstDrive, lastDrive, keep, R, mode)
}

func zeroOr(ok bool, v float64) Num {
	if !ok {
		return 0
	}
	return Num(v)
}

func fill32(n int, v int32) []int32 {
	s := make([]int32, n)
	for i := range s {
		s[i] = v
	}
	return s
}
