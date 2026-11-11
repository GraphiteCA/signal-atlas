package atlas

import (
	"context"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"

	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"
)

// Filter is survey_data.filters(params, spatial), compiled to a test on a stored row. Validation,
// and the order errors are found in, are the Python's.
type Filter struct {
	none                      bool
	like                      string // ASCII-lowercased needle of o.ssid LIKE '%…%' (case folds ASCII only, as SQLite's LIKE)
	hasLike                   bool
	ssidIn, ssidOut, macIn    *strSet
	radioIn, radioOut, bandIn *strSet
	sel                       []macChoice
	dateFrom, dateTo          string
	dateIn                    map[string]bool
	minRSSI                   float64
	bbox                      *[4]float64
	// compiled against a State
	st     *obs.State
	ids    map[*strSet]map[uint32]bool
	selC   []selCompiled
	dateOK map[uint32]bool
	likeOK map[uint32]bool
}

type strSet struct{ vals []string }

type selCompiled struct {
	ssid    uint32
	known   bool
	include bool
	macs    map[uint32]bool
}

// ParseFilter validates params as filters() does.
func ParseFilter(p py.Params, spatial bool) (*Filter, error) {
	f := &Filter{}
	if sq := p.First("ssid_query", ""); sq != "" {
		f.like, f.hasLike = asciiLower(sq), true
	}
	f.none = p.Is("none", "1")
	for _, k := range []struct {
		key string
		dst **strSet
		mac bool
	}{{"ssid", &f.ssidIn, false}, {"exclude_ssid", &f.ssidOut, false}, {"mac", &f.macIn, true}, {"radio", &f.radioIn, false}, {"exclude_radio", &f.radioOut, false}, {"band", &f.bandIn, false}} {
		terms := p.Get(k.key, nil)
		if len(terms) > 200 {
			return nil, py.Errorf("Select at most 200 individual filter values, or use All")
		}
		if len(terms) > 0 {
			s := &strSet{}
			for _, t := range terms {
				if k.mac {
					t = strings.ToUpper(py.Strip(t))
				}
				s.vals = append(s.vals, t)
			}
			*k.dst = s
		}
	}
	if p.Has("mac_selection") {
		ch, err := parseMacSelection(p["mac_selection"][0])
		if err != nil {
			return nil, err
		}
		total := 0
		for i := range ch {
			total += len(ch[i].macs)
			if total > 500 {
				return nil, py.Errorf("Select at most 500 individual MACs, or select the whole SSID")
			}
			for j := range ch[i].macs {
				ch[i].macs[j] = strings.ToUpper(py.Strip(ch[i].macs[j]))
			}
		}
		f.sel = ch
	}
	d, err := ObservationDates(p)
	if err != nil {
		return nil, err
	}
	f.dateFrom, f.dateTo = d.From, d.To
	if len(d.Dates) > 0 {
		f.dateIn = map[string]bool{}
		for _, x := range d.Dates {
			f.dateIn[x] = true
		}
	}
	if f.minRSSI, err = minRSSI(p); err != nil {
		return nil, err
	}
	if spatial && p.Has("bbox") {
		b, err := Bbox(p["bbox"][0])
		if err != nil {
			return nil, err
		}
		f.bbox = &b
	}
	return f, nil
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// Compile binds the filter to a State's string ids.
func (f *Filter) Compile(st *obs.State, d *obs.Dict) *Filter {
	f.st = st
	f.ids = map[*strSet]map[uint32]bool{}
	for _, s := range []*strSet{f.ssidIn, f.ssidOut, f.macIn, f.radioIn, f.radioOut, f.bandIn} {
		if s == nil {
			continue
		}
		m := map[uint32]bool{}
		for _, v := range s.vals {
			if id, ok := d.Lookup(v); ok && int(id) < len(st.Str) {
				m[id] = true
			}
		}
		f.ids[s] = m
	}
	f.selC = nil
	for _, c := range f.sel {
		sc := selCompiled{include: c.mode == "include", macs: map[uint32]bool{}}
		if id, ok := d.Lookup(c.ssid); ok && int(id) < len(st.Str) {
			sc.ssid, sc.known = id, true
		}
		for _, m := range c.macs {
			if id, ok := d.Lookup(m); ok && int(id) < len(st.Str) {
				sc.macs[id] = true
			}
		}
		if len(c.macs) == 0 {
			if !sc.include {
				continue // an exclude with no MACs adds no clause
			}
			sc.macs = nil // clause: o.ssid <> X
		}
		f.selC = append(f.selC, sc)
	}
	f.dateOK = map[uint32]bool{}
	f.likeOK = map[uint32]bool{}
	return f
}

func (f *Filter) dateAccept(id uint32) bool {
	ok, seen := f.dateOK[id]
	if seen {
		return ok
	}
	d := f.st.Str[id]
	ok = true
	if f.dateFrom != "" && !(d >= f.dateFrom) {
		ok = false
	}
	if f.dateTo != "" && !(d <= f.dateTo) {
		ok = false
	}
	if f.dateIn != nil && !f.dateIn[d] {
		ok = false
	}
	if (f.dateFrom != "" || f.dateTo != "") && d == "" {
		ok = false
	}
	return ok
}

// Match is the WHERE clause on row i of segment s. Safe for one goroutine at a time per Filter
// (Clone it for others): it caches per-date answers.
func (f *Filter) Match(s *obs.Segment, i int) bool {
	if f.none {
		return false
	}
	if !(s.RSSI[i] >= f.minRSSI) {
		return false
	}
	if f.bbox != nil {
		la, lo := s.Lat[i], s.Lon[i]
		b := f.bbox
		if !(la >= b[1] && la <= b[3] && lo >= b[0] && lo <= b[2]) {
			return false
		}
	}
	if f.hasLike {
		id := s.SSID[i]
		ok, seen := f.likeOK[id]
		if !seen {
			ok = strings.Contains(asciiLower(f.st.Str[id]), f.like)
			f.likeOK[id] = ok
		}
		if !ok {
			return false
		}
	}
	if f.ssidIn != nil && !f.ids[f.ssidIn][s.SSID[i]] {
		return false
	}
	if f.ssidOut != nil && f.ids[f.ssidOut][s.SSID[i]] {
		return false
	}
	if f.macIn != nil && !f.ids[f.macIn][s.MAC[i]] {
		return false
	}
	if f.radioIn != nil && !f.ids[f.radioIn][s.Radio[i]] {
		return false
	}
	if f.radioOut != nil && f.ids[f.radioOut][s.Radio[i]] {
		return false
	}
	if f.bandIn != nil && !f.ids[f.bandIn][s.Band[i]] {
		return false
	}
	for _, c := range f.selC {
		if !c.known {
			continue // o.ssid <> X holds for every row
		}
		if s.SSID[i] != c.ssid {
			continue
		}
		if c.macs == nil { // include, no MACs: o.ssid <> X
			return false
		}
		if c.include != c.macs[s.MAC[i]] {
			return false
		}
	}
	if (f.dateFrom != "" || f.dateTo != "" || f.dateIn != nil) && !f.dateAccept(s.Date[i]) {
		return false
	}
	return true
}

// Clone copies the filter for another goroutine.
func (f *Filter) Clone() *Filter {
	c := *f
	c.dateOK = map[uint32]bool{}
	c.likeOK = map[uint32]bool{}
	return &c
}

// candidates returns the global rows an index narrows the filter to (ascending), or nil for a
// full scan. Every row that can match is included; Match still decides.
func (f *Filter) candidates() []uint32 {
	st := f.st
	union := func(lists [][]uint32) []uint32 {
		n := 0
		for _, l := range lists {
			n += len(l)
		}
		out := make([]uint32, 0, n)
		for _, l := range lists {
			out = append(out, l...)
		}
		if len(lists) > 1 {
			sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
		}
		return out
	}
	if f.radioIn != nil {
		var lists [][]uint32
		for id := range f.ids[f.radioIn] {
			lists = append(lists, st.ByRadio[id])
		}
		return union(lists)
	}
	if f.ssidIn != nil {
		var lists [][]uint32
		for id := range f.ids[f.ssidIn] {
			lists = append(lists, st.BySSID[id])
		}
		return union(lists)
	}
	if f.bbox != nil {
		b := f.bbox
		la0, la1 := int64(math.Floor(b[1]/obs.GridDeg)), int64(math.Floor(b[3]/obs.GridDeg))
		lo0, lo1 := int64(math.Floor(b[0]/obs.GridDeg)), int64(math.Floor(b[2]/obs.GridDeg))
		cells := (la1 - la0 + 1) * (lo1 - lo0 + 1)
		if cells > 0 && cells <= int64(len(st.Grid))*2+64 {
			var lists [][]uint32
			for la := la0; la <= la1; la++ {
				for lo := lo0; lo <= lo1; lo++ {
					if l := st.Grid[la<<32|int64(uint32(int32(lo)))]; l != nil {
						lists = append(lists, l)
					}
				}
			}
			return union(lists)
		}
	}
	return nil
}

// Scan visits every matching row in id order, split over the cores: each worker gets a
// contiguous run of rows and its own accumulator, and the accumulators come back in row order.
func Scan[A any](f *Filter, newAcc func() A, visit func(a A, g uint32, s *obs.Segment, i int)) []A {
	return ScanContext(context.Background(), f, newAcc, visit)
}

// ScanContext joins all workers before returning, including on cancellation.
func ScanContext[A any](ctx context.Context, f *Filter, newAcc func() A, visit func(a A, g uint32, s *obs.Segment, i int)) []A {
	st := f.st
	if f.none {
		return nil
	}
	cand := f.candidates()
	workers := runtime.NumCPU()
	var n int
	if cand != nil {
		n = len(cand)
	} else {
		n = st.N
	}
	if n == 0 {
		return nil
	}
	if n < 20000 {
		workers = 1
	}
	chunk := (n + workers - 1) / workers
	accs := make([]A, 0, workers)
	type part struct {
		lo, hi int
		acc    A
	}
	var parts []*part
	for lo := 0; lo < n; lo += chunk {
		parts = append(parts, &part{lo: lo, hi: min(n, lo+chunk), acc: newAcc()})
	}
	var wg sync.WaitGroup
	for _, pt := range parts {
		wg.Add(1)
		go func(pt *part) {
			defer wg.Done()
			ff := f.Clone()
			if cand != nil {
				for k := pt.lo; k < pt.hi; k++ {
					if k&4095 == 0 && ctx.Err() != nil {
						return
					}
					g := cand[k]
					s, i := st.Loc(g)
					if ff.Match(s, i) {
						visit(pt.acc, g, s, i)
					}
				}
				return
			}
			// full scan: walk segments from global row lo
			g := uint32(pt.lo)
			s, i := st.Loc(g)
			si := segIndex(st, s)
			for k := pt.lo; k < pt.hi; k++ {
				if k&4095 == 0 && ctx.Err() != nil {
					return
				}
				for i >= s.N {
					si++
					s, i = st.Segs[si], 0
				}
				if ff.Match(s, i) {
					visit(pt.acc, g, s, i)
				}
				i++
				g++
			}
		}(pt)
	}
	wg.Wait()
	for _, pt := range parts {
		accs = append(accs, pt.acc)
	}
	return accs
}

func segIndex(st *obs.State, s *obs.Segment) int {
	for i, x := range st.Segs {
		if x == s {
			return i
		}
	}
	return -1
}
