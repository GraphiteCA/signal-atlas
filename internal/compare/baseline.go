package compare

import (
	"math"
	"sort"
)

type apSum struct {
	w, x, y float64
	max     int8
	n       int
	ch      uint16
	bd      uint8
}

// ordered is a JS Map: insertion order kept.
type orderedSums struct {
	m     map[uint32]*apSum
	order []uint32
}

func (o *orderedSums) has(q uint32) bool { _, ok := o.m[q]; return ok }

type cell struct {
	a, b               map[uint32]bool
	hasA, hasB         bool
	va, vb             float64
	na, nb             int
	hxa, hya, hxb, hyb float64
}

type cellMap struct {
	m     map[int64]*cell
	order []int64
}

type obsItem struct {
	nb, no int
	va, vb float64
	dot    float64
	hasDir bool
}

func (e *Engine) baselineDetail(cfg *Config, ds []*drive, bi int, base *drive, nearF [][][]uint8, legList []*legItem, legPairs []*legPair, summaries []map[string]any, keep func(uint8) bool, R float64) ([]any, any) {
	N := len(ds)
	summ := func(d *drive, flags []uint8) *orderedSums {
		o := &orderedSums{m: map[uint32]*apSum{}}
		for s := 0; s < d.S; s++ {
			if flags[s] == 0 {
				continue
			}
			for r := d.sStart[s]; r < d.sStart[s+1]; r++ {
				if !keep(d.D.flags[r]) {
					continue
				}
				q := d.D.mac[r]
				x := o.m[q]
				if x == nil {
					x = &apSum{max: -128}
					o.m[q] = x
					o.order = append(o.order, q)
				}
				w := math.Pow(10, float64(d.D.rssi[r])/10)
				x.w += w
				x.x += w * d.X[s]
				x.y += w * d.Y[s]
				if d.D.rssi[r] > x.max {
					x.max, x.ch, x.bd = d.D.rssi[r], d.D.chn[r], d.D.band[r]
				}
				x.n++
			}
		}
		return o
	}
	const CELL = 400.0
	ckey := func(x, y float64) int64 { return hkey(x, y, CELL) }
	accCells := func(cells *cellMap, d *drive, s0, s1 int, isNear func(int) bool, sideA bool) {
		for s := s0; s <= s1; s++ {
			if !isNear(s) {
				continue
			}
			k := ckey(d.X[s], d.Y[s])
			c := cells.m[k]
			if c == nil {
				c = &cell{}
				cells.m[k] = c
				cells.order = append(cells.order, k)
			}
			set := c.b
			if sideA {
				set = c.a
			}
			if set == nil {
				set = map[uint32]bool{}
				if sideA {
					c.a, c.hasA = set, true
				} else {
					c.b, c.hasB = set, true
				}
			}
			for r := d.sStart[s]; r < d.sStart[s+1]; r++ {
				if keep(d.D.flags[r]) {
					set[d.D.mac[r]] = true
				}
			}
			if sideA {
				c.va += float64(d.v[s])
				c.na++
			} else {
				c.vb += float64(d.v[s])
				c.nb++
			}
			if s > 0 && d.dt[s] < 60 {
				if sideA {
					c.hxa += d.X[s] - d.X[s-1]
					c.hya += d.Y[s] - d.Y[s-1]
				} else {
					c.hxb += d.X[s] - d.X[s-1]
					c.hyb += d.Y[s] - d.Y[s-1]
				}
			}
		}
	}
	obsOf := func(cells *cellMap, out []obsItem) []obsItem {
		for _, k := range cells.order {
			c := cells.m[k]
			if !c.hasA || !c.hasB {
				continue
			}
			out = append(out, obsItem{nb: len(c.a), no: len(c.b), va: c.va / float64(c.na), vb: c.vb / float64(c.nb), dot: c.hxa*c.hxb + c.hya*c.hyb,
				hasDir: (c.hxa != 0 || c.hya != 0) && (c.hxb != 0 || c.hyb != 0)})
		}
		return out
	}
	var rs uint64 = 12345
	rnd := func() float64 { rs = (rs*1664525 + 1013904223) & 0xffffffff; return float64(rs) / 4294967296 }
	logC := func(n, k int) float64 {
		v := 0.0
		for i := 1; i <= k; i++ {
			v += math.Log(float64(n-k+i)) - math.Log(float64(i))
		}
		return v
	}
	signP := func(w, l int) any {
		n := w + l
		if n == 0 {
			return nil
		}
		k := min(w, l)
		p := 0.0
		for i := 0; i <= k; i++ {
			p += math.Exp(logC(n, i) - float64(n)*math.Ln2)
		}
		return Num(math.Min(1, 2*p))
	}
	pairStat := func(obs []obsItem, perLeg bool) map[string]any {
		n := len(obs)
		sB, sO, w, l, dir, nd := 0, 0, 0, 0, 0, 0
		for _, o := range obs {
			sB += o.nb
			sO += o.no
			if o.nb > o.no {
				w++
			} else if o.nb < o.no {
				l++
			}
			if o.hasDir {
				nd++
				if o.dot > 0 {
					dir++
				}
			}
		}
		var est, lo, hi any
		if sO != 0 {
			est = Num((float64(sB)/float64(sO) - 1) * 100)
		}
		if n >= 5 && sO != 0 {
			rs = 12345
			rr := make([]float64, 0, 1000)
			for b := 0; b < 1000; b++ {
				x, y := 0, 0
				for i := 0; i < n; i++ {
					o := obs[int(math.Floor(rnd()*float64(n)))]
					x += o.nb
					y += o.no
				}
				if y != 0 {
					rr = append(rr, (float64(x)/float64(y)-1)*100)
				} else {
					rr = append(rr, 0)
				}
			}
			sort.Float64s(rr)
			lo, hi = Num(rr[25]), Num(rr[974])
		}
		mB, mO, nm := 0, 0, 0
		for _, o := range obs {
			if math.Abs(o.va-o.vb) <= 15 {
				mB += o.nb
				mO += o.no
				nm++
			}
		}
		var estM, dirA any
		if nm >= 5 && mO != 0 {
			estM = Num((float64(mB)/float64(mO) - 1) * 100)
		}
		if nd != 0 {
			dirA = Num(float64(dir) / float64(nd) * 100)
		}
		return map[string]any{"n": n, "cell": CELL, "perLeg": perLeg, "est": est, "lo": lo, "hi": hi, "wins": w, "losses": l, "p": signP(w, l),
			"nMatched": nm, "estMatched": estM, "dirAgree": dirA}
	}
	off := cfg.TzOff * 60
	wk := func(t float64) bool {
		w := math.Mod(math.Floor((t-off)/86400+4), 7)
		return w == 0 || w == 6
	}
	gpsPct := func(x *drive) Num {
		r, n := 0, 0
		for _, f := range x.files {
			r += f.Raw
			n += f.NoGps
		}
		if r == 0 {
			return 0
		}
		return Num(float64(n) / float64(r) * 100)
	}
	detail := []any{}
	for j, d := range ds {
		if j == bi {
			continue
		}
		A, O := summ(base, nearF[bi][j]), summ(d, nearF[j][bi])
		var sh, dr []float64
		moved := 0
		for _, q := range A.order {
			ea := A.m[q]
			o := O.m[q]
			if o == nil {
				continue
			}
			dist := math.Hypot(ea.x/ea.w-o.x/o.w, ea.y/ea.w-o.y/o.w)
			sh = append(sh, dist)
			dr = append(dr, float64(int(ea.max)-int(o.max)))
			if dist > 500 {
				moved++
			}
		}
		km, kmb := 0.0, 0.0
		for s := 1; s < d.S; s++ {
			if nearF[j][bi][s] != 0 && d.dt[s] < 60 {
				km += d.ds[s]
			}
		}
		for s := 1; s < base.S; s++ {
			if nearF[bi][j][s] != 0 && base.dt[s] < 60 {
				kmb += base.ds[s]
			}
		}
		rB, rO := 0, 0
		for _, q := range A.order {
			rB += A.m[q].n
		}
		for _, q := range O.order {
			rO += O.m[q].n
		}
		var ret []any
		for _, c := range cuts {
			to, hit := 0, 0
			for _, q := range O.order {
				ee := O.m[q]
				if float64(ee.max) >= c.lo && float64(ee.max) <= c.hi {
					to++
					if A.has(q) {
						hit++
					}
				}
			}
			var pct any
			if to != 0 {
				pct = Num(float64(hit) / float64(to) * 100)
			}
			ret = append(ret, map[string]any{"bucket": c.name, "pct": pct, "n": to})
		}
		lA, oA, lR, oR, lp := 0, 0, 0, 0, 0
		lK, oK := 0.0, 0.0
		for _, p := range legPairs {
			if !p.hasJ {
				continue
			}
			if p.da == bi && p.db == j {
				lA += p.na
				lK += p.ka
				lR += p.ra
				oA += p.nb
				oK += p.kb
				oR += p.rb
				lp++
			} else if p.da == j && p.db == bi {
				lA += p.nb
				lK += p.kb
				lR += p.rb
				oA += p.na
				oK += p.ka
				oR += p.ra
				lp++
			}
		}
		var oneOnly []*apSum
		for _, q := range O.order {
			if !A.has(q) {
				oneOnly = append(oneOnly, O.m[q])
			}
		}
		var bandRow []any
		for _, b := range []uint8{1, 2, 3} {
			oT, oH, bT, bH := 0, 0, 0, 0
			for _, q := range O.order {
				if O.m[q].bd != b {
					continue
				}
				oT++
				if A.has(q) {
					oH++
				}
			}
			for _, q := range A.order {
				if A.m[q].bd != b {
					continue
				}
				bT++
				if O.has(q) {
					bH++
				}
			}
			var p1, p2 any
			if oT != 0 {
				p1 = Num(float64(oH) / float64(oT) * 100)
			}
			if bT != 0 {
				p2 = Num(float64(bH) / float64(bT) * 100)
			}
			bandRow = append(bandRow, map[string]any{"band": int(b), "otherAps": oT, "otherSeenByBase": p1, "baseAps": bT, "baseSeenByOther": p2})
		}
		type chc struct {
			ch                        uint16
			band                      uint8
			other, bothO, base, bothB int
		}
		chM := map[uint16]*chc{}
		chE := func(ch uint16, bd uint8) *chc {
			c := chM[ch]
			if c == nil {
				c = &chc{ch: ch, band: bd}
				chM[ch] = c
			}
			return c
		}
		for _, q := range O.order {
			ee := O.m[q]
			c := chE(ee.ch, ee.bd)
			c.other++
			if A.has(q) {
				c.bothO++
			}
		}
		for _, q := range A.order {
			ee := A.m[q]
			c := chE(ee.ch, ee.bd)
			c.base++
			if O.has(q) {
				c.bothB++
			}
		}
		chList := make([]*chc, 0, len(chM))
		for _, c := range chM {
			chList = append(chList, c)
		}
		sort.Slice(chList, func(a, b int) bool { return chList[a].ch < chList[b].ch })
		chOut := make([]any, len(chList))
		for i, c := range chList {
			chOut[i] = map[string]any{"ch": int(c.ch), "band": int(c.band), "other": c.other, "bothO": c.bothO, "base": c.base, "bothB": c.bothB}
		}
		var obs []obsItem
		perLeg := false
		for _, p := range legPairs {
			if !p.hasJ {
				continue
			}
			LA, LB := legList[p.x], legList[p.y]
			var L0, L1 *legItem
			if LA.d == bi && LB.d == j {
				L0, L1 = LA, LB
			} else if LA.d == j && LB.d == bi {
				L0, L1 = LB, LA
			} else {
				continue
			}
			cells := &cellMap{m: map[int64]*cell{}}
			accCells(cells, base, L0.a, L0.b, func(s int) bool { return near(L1.H, base.X[s], base.Y[s], R) }, true)
			accCells(cells, d, L1.a, L1.b, func(s int) bool { return near(L0.H, d.X[s], d.Y[s], R) }, false)
			obs = obsOf(cells, obs)
			perLeg = true
		}
		if len(obs) == 0 {
			cells := &cellMap{m: map[int64]*cell{}}
			fb0, fo0 := nearF[bi][j], nearF[j][bi]
			accCells(cells, base, 0, base.S-1, func(s int) bool { return fb0[s] != 0 }, true)
			accCells(cells, d, 0, d.S-1, func(s int) bool { return fo0[s] != 0 }, false)
			obs = obsOf(cells, obs)
		}
		stat := pairStat(obs, perLeg)
		fb, fo := nearF[bi][j], nearF[j][bi]
		tb, to := make([]float64, len(bands)), make([]float64, len(bands))
		kB, kO, tB, tO, allB, allO := 0.0, 0.0, 0.0, 0.0, 0.0, 0.0
		var hB, hO []float64
		for s := 1; s < base.S; s++ {
			if base.dt[s] >= 60 {
				continue
			}
			allB += base.ds[s]
			if fb[s] == 0 {
				continue
			}
			kB += base.ds[s]
			tB += base.dt[s]
			tb[bandOf(float64(base.v[s]))] += base.dt[s]
			hB = append(hB, math.Mod(base.sT[s], 86400))
		}
		for s := 1; s < d.S; s++ {
			if d.dt[s] >= 60 {
				continue
			}
			allO += d.ds[s]
			if fo[s] == 0 {
				continue
			}
			kO += d.ds[s]
			tO += d.dt[s]
			to[bandOf(float64(d.v[s]))] += d.dt[s]
			hO = append(hO, math.Mod(d.sT[s], 86400))
		}
		ov := 0.0
		if tB != 0 && tO != 0 {
			for b := range bands {
				ov += math.Abs(tb[b]/tB - to[b]/tO)
			}
		}
		var tod any
		hmB, okB := medv(hB)
		hmO, okO := medv(hO)
		if okB && okO {
			g := math.Abs(hmB - hmO)
			tod = Num(math.Min(g, 86400-g) / 3600)
		}
		rand := func(i int) any { return summaries[i]["mix"].(map[string]any)["rand"] }
		var speedOv, avgB, avgO any
		if tB != 0 && tO != 0 {
			speedOv = Num((1 - ov/2) * 100)
		}
		if tB != 0 {
			avgB = Num(kB / tB * 3.6)
		}
		if tO != 0 {
			avgO = Num(kO / tO * 3.6)
		}
		fair := map[string]any{"kmBase": Num(kB / 1000), "kmOther": Num(kO / 1000), "pctBase": zeroOr(allB != 0, kB/allB*100), "pctOther": zeroOr(allO != 0, kO/allO*100),
			"speedOverlap": speedOv, "avgSpeedBase": avgB, "avgSpeedOther": avgO, "todGapH": tod, "weekendBase": wk(base.sT[0]), "weekendOther": wk(d.sT[0]),
			"daysApart": Num(math.Abs(base.sT[0]-d.sT[0]) / 86400), "gpsBase": gpsPct(base), "gpsOther": gpsPct(d), "randBase": rand(bi), "randOther": rand(j)}
		var estimate, onlyOnce any
		aN, oN := len(A.order), len(O.order)
		if aN != 0 && oN != 0 {
			estimate = Num(float64(aN+1)*float64(oN+1)/float64(len(sh)+1) - 1)
		}
		if len(oneOnly) > 0 {
			k := 0
			for _, x := range oneOnly {
				if x.n == 1 {
					k++
				}
			}
			onlyOnce = Num(float64(k) / float64(len(oneOnly)) * 100)
		}
		pass := func(a, b float64) any {
			if b == 0 {
				return nil
			}
			return Num(a / b)
		}
		detail = append(detail, map[string]any{"key": d.key, "legPairs": lp, "passBase": pass(float64(lA), lK), "passOther": pass(float64(oA), oK),
			"passRowsBase": pass(float64(lR), lK), "passRowsOther": pass(float64(oR), oK), "baseAps": aN, "otherAps": oN, "shared": len(sh),
			"kmBase": Num(kmb / 1000), "kmOther": Num(km / 1000), "apsPerKmBase": zeroOr(kmb != 0, float64(aN)/(kmb/1000)), "apsPerKmOther": zeroOr(km != 0, float64(oN)/(km/1000)),
			"rowsPerKmBase": zeroOr(kmb != 0, float64(rB)/(kmb/1000)), "rowsPerKmOther": zeroOr(km != 0, float64(rO)/(km/1000)), "shiftMedian": med(sh), "shiftP90": qt(sh, .9),
			"moved": moved, "rssiDiff": med(dr), "retention": ret, "estimate": estimate, "onlyOnce": onlyOnce, "onlyCount": len(oneOnly), "bands": bandRow,
			"chans": chOut, "fair": fair, "stat": stat})
	}
	// profile along the baseline's longest leg
	lg := base.legs[0]
	best := -1.0
	for _, l := range base.legs {
		k := 0.0
		for s := l.a + 1; s <= l.b; s++ {
			if base.dt[s] < 60 {
				k += base.ds[s]
			}
		}
		if k > best {
			best, lg = k, l
		}
	}
	var ax, ay, al []float64
	ac := 0.0
	for p := range base.px {
		if s := int(base.ps[p]); s >= lg.a && s <= lg.b {
			ax, ay = append(ax, base.px[p]), append(ay, base.py[p])
			ac += float64(base.pl[p])
			al = append(al, ac)
		}
	}
	AX := hashOf(ax, ay, 60)
	toKm := func(x, y float64) int {
		cx, cy := int64(math.Floor(x/60)), int64(math.Floor(y/60))
		bd, b2 := 3600.0, -1
		for i := int64(-1); i <= 1; i++ {
			for j := int64(-1); j <= 1; j++ {
				for _, q := range AX.h[(cx+i)*4194304+(cy+j)] {
					dx, dy := ax[q]-x, ay[q]-y
					if e := dx*dx + dy*dy; e < bd {
						bd, b2 = e, q
					}
				}
			}
		}
		if b2 < 0 {
			return -1
		}
		return int(math.Floor(al[b2] / 1000))
	}
	kmN := int(math.Ceil(ac / 1000))
	series := map[string]any{}
	for i, d := range ds {
		sets := make([]map[uint32]bool, kmN)
		for k := range sets {
			sets[k] = map[uint32]bool{}
		}
		for s := 0; s < d.S; s++ {
			on := false
			for j := 0; j < N && !on; j++ {
				on = j != i && nearF[i][j][s] != 0
			}
			if !on {
				continue
			}
			b := toKm(d.X[s], d.Y[s])
			if b < 0 || b >= kmN {
				continue
			}
			for r := d.sStart[s]; r < d.sStart[s+1]; r++ {
				if keep(d.D.flags[r]) {
					sets[b][d.D.mac[r]] = true
				}
			}
		}
		sizes := make([]int, kmN)
		for k, x := range sets {
			sizes[k] = len(x)
		}
		series[d.key] = sizes
	}
	return detail, map[string]any{"km": kmN, "series": series, "legStart": Num(base.sT[lg.a])}
}
