package compare

import (
	"fmt"
	"math"
	"sort"
	"time"
)

type legItem struct {
	d, origin, li, a, b int
	key                 string
	km, start           float64
	H                   *hash
}

type legPair struct {
	x, y   int
	j      any // nil or Num
	jv     float64
	hasJ   bool
	km     float64
	da, db int
	na, nb int
	ka, kb float64
	ra, rb int
}

func (e *Engine) analyseRest(cfg *Config, t0 time.Time, all, ds []*drive, summaries []map[string]any, firstDrive, lastDrive []int32, keep func(uint8) bool, R float64, mode string) map[string]any {
	N := len(ds)
	mC := len(e.macStr)
	// near flags
	e.say(map[string]any{"type": "progress", "text": "Matching tracks between drives", "frac": .4})
	H := make([]*hash, N)
	for i, d := range ds {
		H[i] = hashOf(d.px, d.py, math.Max(R, 20))
	}
	nearF := make([][][]uint8, N)
	for i := range nearF {
		nearF[i] = make([][]uint8, N)
	}
	for i := 0; i < N; i++ {
		for j := 0; j < N; j++ {
			if i == j {
				continue
			}
			d := ds[i]
			f := make([]uint8, d.S)
			for s := 0; s < d.S; s++ {
				if near(H[j], d.X[s], d.Y[s], R) {
					f[s] = 1
				}
			}
			nearF[i][j] = f
		}
		e.say(map[string]any{"type": "progress", "text": fmt.Sprintf("Matching tracks: drive %d of %d", i+1, N), "frac": .4 + .2*float64(i)/math.Max(1, float64(N))})
	}
	// point-level coverage count
	cov := make([][]uint16, N)
	for i, d := range ds {
		c := make([]uint16, len(d.px))
		for p := range c {
			n := 1
			for j := 0; j < N; j++ {
				if j != i && near(H[j], d.px[p], d.py[p], R) {
					n++
				}
			}
			c[p] = uint16(n)
		}
		cov[i] = c
	}
	// road length at each K, deduped between legs at 40 m
	walk := func(K int) (float64, []float64) {
		H2 := &hash{h: map[int64][]int{}, size: 40}
		tot := 0.0
		var per []float64
		for i, d := range ds {
			legOf := func(p int) int {
				s := int(d.ps[p])
				for l, lg := range d.legs {
					if s >= lg.a && s <= lg.b {
						return l
					}
				}
				return 0
			}
			cl := -1
			var pend []int
			flush := func() {
				for _, p := range pend {
					H2.add(d.px[p], d.py[p])
				}
				pend = pend[:0]
			}
			for p := range d.px {
				if int(cov[i][p]) < K {
					continue
				}
				l := legOf(p)
				if l != cl {
					flush()
					cl = l
				}
				if !near(H2, d.px[p], d.py[p], 40) {
					tot += float64(d.pl[p])
				}
				pend = append(pend, p)
			}
			flush()
			per = append(per, tot/1000)
		}
		return tot / 1000, per
	}
	roadKm := map[string]any{}
	var cumRoad []float64
	if N > 0 {
		_, cumRoad = walk(1)
	}
	for K := 2; K <= N; K++ {
		k, _ := walk(K)
		roadKm[fmt.Sprint(K)] = Num(k)
	}
	// thin tracks for the map
	tracks := make([]any, N)
	for i, d := range ds {
		var la, lo []Num
		var cv, seg []int
		var tm []Num
		sg, acc := 0, 0.0
		for s := 0; s < d.S; s++ {
			n := 1
			for j := 0; j < N; j++ {
				if j != i && nearF[i][j][s] != 0 {
					n++
				}
			}
			if s > 0 && d.dt[s] > 60 {
				sg++
			}
			if d.dt[s] <= 60 {
				acc += d.ds[s]
			}
			pc := 0
			if len(cv) > 0 {
				pc = cv[len(cv)-1]
			}
			if s == 0 || s == d.S-1 || acc >= 60 || n != pc || (s > 0 && d.dt[s] > 60) {
				la, lo, cv, tm, seg = append(la, Num(d.sLa[s])), append(lo, Num(d.sLo[s])), append(cv, n), append(tm, Num(d.sT[s])), append(seg, sg)
				acc = 0
			}
		}
		tracks[i] = map[string]any{"key": d.key, "lat": nonNil(la), "lon": nonNil(lo), "cov": nonNilI(cv), "t": nonNil(tm), "seg": nonNilI(seg)}
	}
	// share of driving on shared road
	sharedShare := make([]any, N)
	for i, d := range ds {
		on, to := 0.0, 0.0
		for s := 1; s < d.S; s++ {
			if d.dt[s] >= 60 {
				continue
			}
			to += d.ds[s]
			hit := false
			for j := 0; j < N && !hit; j++ {
				hit = j != i && nearF[i][j][s] != 0
			}
			if hit {
				on += d.ds[s]
			}
		}
		sharedShare[i] = map[string]any{"key": d.key, "onKm": Num(on / 1000), "pct": zeroOr(to != 0, on/to*100)}
	}
	// pair matrix on shared road
	e.say(map[string]any{"type": "progress", "text": "Comparing APs between drives", "frac": .65})
	markA, markB := fill32(mC, -1), fill32(mC, -1)
	pid := int32(0)
	pairs := []any{}
	for i := 0; i < N; i++ {
		for j := i + 1; j < N; j++ {
			pid++
			a, b, fa, fb := ds[i], ds[j], nearF[i][j], nearF[j][i]
			na, nb, m := 0, 0, 0
			for s := 0; s < a.S; s++ {
				if fa[s] == 0 {
					continue
				}
				for r := a.sStart[s]; r < a.sStart[s+1]; r++ {
					if !keep(a.D.flags[r]) {
						continue
					}
					if q := a.D.mac[r]; markA[q] != pid {
						markA[q] = pid
						na++
					}
				}
			}
			for s := 0; s < b.S; s++ {
				if fb[s] == 0 {
					continue
				}
				for r := b.sStart[s]; r < b.sStart[s+1]; r++ {
					if !keep(b.D.flags[r]) {
						continue
					}
					if q := b.D.mac[r]; markB[q] != pid {
						markB[q] = pid
						nb++
						if markA[q] == pid {
							m++
						}
					}
				}
			}
			ka := 0.0
			for s := 1; s < a.S; s++ {
				if fa[s] != 0 && a.dt[s] < 60 {
					ka += a.ds[s]
				}
			}
			var jac, est any
			if na+nb-m != 0 {
				jac = Num(float64(m) / float64(na+nb-m) * 100)
			}
			if na != 0 && nb != 0 {
				est = Num(float64(na+1)*float64(nb+1)/float64(m+1) - 1)
			}
			pairs = append(pairs, map[string]any{"a": a.key, "b": b.key, "na": na, "nb": nb, "shared": m, "jaccard": jac, "km": Num(ka / 1000), "estimate": est})
		}
	}
	// leg matrix
	var legList []*legItem
	for i, d := range ds {
		for li, l := range d.legs {
			km := 0.0
			for s := l.a + 1; s <= l.b; s++ {
				if d.dt[s] < 60 {
					km += d.ds[s]
				}
			}
			if km >= .5 {
				legList = append(legList, &legItem{d: i, origin: d.origin, li: li, a: l.a, b: l.b, key: d.key, km: km / 1000, start: d.sT[l.a]})
			}
		}
	}
	var legPairs []*legPair
	var sameMean, crossMean any
	legsCapped := len(legList) > 80
	if !legsCapped && len(legList) > 1 {
		e.say(map[string]any{"type": "progress", "text": "Comparing legs", "frac": .75})
		for _, l := range legList {
			d := ds[l.d]
			var xs, ys []float64
			for p := range d.px {
				if s := int(d.ps[p]); s >= l.a && s <= l.b {
					xs, ys = append(xs, d.px[p]), append(ys, d.py[p])
				}
			}
			l.H = hashOf(xs, ys, math.Max(R, 20))
		}
		legPairs = []*legPair{}
		var same, cross []float64
		for x := 0; x < len(legList); x++ {
			for y := x + 1; y < len(legList); y++ {
				pid++
				A, Bb := legList[x], legList[y]
				da, db := ds[A.d], ds[Bb.d]
				na, nb, m, ra, rb := 0, 0, 0, 0, 0
				km, kb := 0.0, 0.0
				for s := A.a; s <= A.b; s++ {
					if !near(Bb.H, da.X[s], da.Y[s], R) {
						continue
					}
					if s > A.a && da.dt[s] < 60 {
						km += da.ds[s]
					}
					for r := da.sStart[s]; r < da.sStart[s+1]; r++ {
						if !keep(da.D.flags[r]) {
							continue
						}
						ra++
						if q := da.D.mac[r]; markA[q] != pid {
							markA[q] = pid
							na++
						}
					}
				}
				if km < 2000 {
					legPairs = append(legPairs, &legPair{x: x, y: y, km: km / 1000, da: A.d, db: Bb.d})
					continue
				}
				for s := Bb.a; s <= Bb.b; s++ {
					if !near(A.H, db.X[s], db.Y[s], R) {
						continue
					}
					if s > Bb.a && db.dt[s] < 60 {
						kb += db.ds[s]
					}
					for r := db.sStart[s]; r < db.sStart[s+1]; r++ {
						if !keep(db.D.flags[r]) {
							continue
						}
						rb++
						if q := db.D.mac[r]; markB[q] != pid {
							markB[q] = pid
							nb++
							if markA[q] == pid {
								m++
							}
						}
					}
				}
				lp := &legPair{x: x, y: y, km: km / 1000, da: A.d, db: Bb.d, na: na, nb: nb, ka: km / 1000, kb: kb / 1000, ra: ra, rb: rb}
				if na+nb-m != 0 {
					lp.hasJ, lp.jv = true, float64(m)/float64(na+nb-m)*100
					if A.origin == Bb.origin {
						same = append(same, lp.jv)
					} else {
						cross = append(cross, lp.jv)
					}
				}
				legPairs = append(legPairs, lp)
			}
		}
		sameMean, crossMean = mean(same), mean(cross)
	}
	// baseline detail
	bi := -1
	for i, d := range ds {
		if cfg.Baseline != nil && d.key == *cfg.Baseline {
			bi = i
			break
		}
	}
	if bi < 0 {
		bi = N - 1
	}
	var base *drive
	if bi >= 0 && bi < N {
		base = ds[bi]
	}
	detail := []any{}
	var profile any
	if N > 1 {
		e.say(map[string]any{"type": "progress", "text": "Comparing against the baseline drive", "frac": .85})
		detail, profile = e.baselineDetail(cfg, ds, bi, base, nearF, legList, legPairs, summaries, keep, R)
	}
	// AP history
	var hist []any
	cumAps := 0
	lat, lon := make([]float32, mC), make([]float32, mC)
	hasPos := make([]uint8, mC)
	for i, d := range ds {
		type wsum struct{ w, x, y float64 }
		m := map[uint32]*wsum{}
		var order []uint32
		for r := 0; r < d.n; r++ {
			if !keep(d.D.flags[r]) {
				continue
			}
			q := d.D.mac[r]
			ee := m[q]
			if ee == nil {
				ee = &wsum{}
				m[q] = ee
				order = append(order, q)
			}
			w := math.Pow(10, float64(d.D.rssi[r])/10)
			ee.w += w
			ee.x += w * d.D.lat[r]
			ee.y += w * d.D.lon[r]
		}
		nw, back, moved := 0, 0, 0
		for _, q := range order {
			ee := m[q]
			la, lo := ee.x/ee.w, ee.y/ee.w
			if hasPos[q] == 0 {
				nw++
				hasPos[q] = 1
			} else {
				back++
				if math.Hypot((la-float64(lat[q]))*110950, (lo-float64(lon[q]))*111320*e.cos) > 500 {
					moved++
				}
			}
			lat[q], lon[q] = float32(la), float32(lo)
		}
		cumAps += nw
		var notAgain any
		if i < N-1 {
			k := 0
			for q := range m {
				if lastDrive[q] == int32(i) {
					k++
				}
			}
			notAgain = k
		}
		cr := 0.0
		if i < len(cumRoad) {
			cr = cumRoad[i]
		}
		hist = append(hist, map[string]any{"key": d.key, "aps": len(m), "newAps": nw, "returning": back, "moved": moved, "notAgain": notAgain, "cumAps": cumAps, "cumRoad": Num(cr)})
	}
	quality := []any{}
	memRows := 0
	for _, f := range e.files {
		quality = append(quality, map[string]any{"file": f.Name, "device": f.Device, "raw": f.Raw, "kept": f.N, "noGps": f.NoGps, "corrupt": f.Corrupt, "badSignal": f.BadSignal,
			"other": f.Other, "outOfOrder": f.OutOfOrder, "start": Num(f.Tmin), "end": Num(f.Tmax), "hasNode": f.HasNode})
		memRows += f.N
	}
	ex := map[string]bool{}
	for _, k := range cfg.Excluded {
		ex[k] = true
	}
	allKeys := []any{}
	for _, d := range all {
		names := make([]string, len(d.files))
		for i, f := range d.files {
			names[i] = f.Name
		}
		km := 0.0
		for i, x := range d.ds {
			if d.dt[i] < 60 {
				km += x
			}
		}
		allKeys = append(allKeys, map[string]any{"partial": d.partial, "origin": d.origin, "points": pointsOf(d), "key": d.key, "start": Num(d.sT[0]), "end": Num(d.sT[d.S-1]),
			"device": d.files[0].Device, "solo": len(d.files) == 1 && d.files[0].Solo, "files": names, "included": !ex[d.key], "km": Num(km / 1000), "rows": d.n})
	}
	legsOut := []any{}
	for _, l := range legList {
		legsOut = append(legsOut, map[string]any{"key": l.key, "li": l.li, "km": Num(l.km), "start": Num(l.start)})
	}
	var lpOut any
	if legPairs != nil {
		l := make([]any, len(legPairs))
		for i, p := range legPairs {
			o := map[string]any{"x": p.x, "y": p.y, "km": Num(p.km), "da": p.da, "db": p.db}
			if p.kb != 0 || p.na != 0 || p.nb != 0 || p.hasJ || p.ra != 0 || p.rb != 0 || p.ka != 0 {
				o["j"], o["na"], o["nb"], o["ka"], o["kb"], o["ra"], o["rb"] = jOf(p), p.na, p.nb, Num(p.ka), Num(p.kb), p.ra, p.rb
			} else {
				o["j"] = nil
			}
			l[i] = o
		}
		lpOut = l
	}
	var baseKey any
	if base != nil {
		baseKey = base.key
	}
	if summaries == nil {
		summaries = []map[string]any{}
	}
	return map[string]any{"mode": mode, "drives": summaries, "allDrives": allKeys, "tracks": tracks, "roadKm": roadKm, "sharedShare": sharedShare,
		"pairs": pairs, "legs": legsOut, "legPairs": lpOut, "legsCapped": legsCapped, "sameMean": sameMean, "crossMean": crossMean, "baseline": baseKey,
		"detail": detail, "profile": profile, "hist": nonNilA(hist), "quality": quality, "n": N, "ms": time.Since(t0).Milliseconds(), "memRows": memRows, "macs": mC}
}

func jOf(p *legPair) any {
	if !p.hasJ {
		return nil
	}
	return Num(p.jv)
}

func pointsOf(d *drive) []any {
	pts := []any{}
	var gaps []int
	for s := 1; s < d.S; s++ {
		if d.dt[s] >= 60 {
			gaps = append(gaps, s)
		}
	}
	sort.SliceStable(gaps, func(a, b int) bool { return d.dt[gaps[a]] > d.dt[gaps[b]] })
	gaps = gaps[:min(8, len(gaps))]
	sort.SliceStable(gaps, func(a, b int) bool { return gaps[a] < gaps[b] })
	type pt struct {
		t    float64
		item map[string]any
	}
	var list []pt
	for _, s := range gaps {
		list = append(list, pt{d.sT[s], map[string]any{"t": Num(d.sT[s]), "kind": "gap", "gap": Num(d.dt[s])}})
	}
	far, fd := -1, 0.0
	for s := 0; s < d.S; s++ {
		if e := math.Hypot(d.X[s]-d.X[0], d.Y[s]-d.Y[0]); e > fd {
			fd, far = e, s
		}
	}
	if far > 0 && far < d.S-1 && fd > 500 {
		list = append(list, pt{d.sT[far], map[string]any{"t": Num(d.sT[far]), "kind": "far"}})
	}
	for _, f := range d.files[1:] {
		if f.Tmin > d.sT[0] {
			list = append(list, pt{f.Tmin, map[string]any{"t": Num(f.Tmin), "kind": "file"}})
		}
	}
	sort.SliceStable(list, func(a, b int) bool { return list[a].t < list[b].t })
	for _, p := range list {
		pts = append(pts, p.item)
	}
	return pts
}

func nonNil(a []Num) []Num {
	if a == nil {
		return []Num{}
	}
	return a
}
func nonNilI(a []int) []int {
	if a == nil {
		return []int{}
	}
	return a
}
func nonNilA(a []any) []any {
	if a == nil {
		return []any{}
	}
	return a
}
