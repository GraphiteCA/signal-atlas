package atlas

import (
	"signal-atlas/internal/obs"
	"sort"
)

type sampleRow struct {
	row  uint32
	id   int64
	rssi float64
}
type sampleHeap struct {
	rows   []sampleRow
	limit  int
	spread bool
}

func sampleHash(id int64) int64 { return (id*1103515245 + 12345) % 2147483647 }
func (h *sampleHeap) better(a, b sampleRow) bool {
	if h.spread {
		x, y := sampleHash(a.id), sampleHash(b.id)
		if x != y {
			return x < y
		}
	} else if a.rssi != b.rssi {
		return a.rssi > b.rssi
	}
	return a.id < b.id
}

// Keep the worst retained row at the root, so rejected rows cost one comparison.
func (h *sampleHeap) add(v sampleRow) {
	if len(h.rows) < h.limit {
		h.rows = append(h.rows, v)
		for i := len(h.rows) - 1; i > 0; {
			p := (i - 1) / 2
			if !h.better(h.rows[p], h.rows[i]) {
				break
			}
			h.rows[p], h.rows[i] = h.rows[i], h.rows[p]
			i = p
		}
		return
	}
	if !h.better(v, h.rows[0]) {
		return
	}
	h.rows[0] = v
	for i := 0; ; {
		c := 2*i + 1
		if c >= len(h.rows) {
			break
		}
		if c+1 < len(h.rows) && h.better(h.rows[c], h.rows[c+1]) {
			c++
		}
		if !h.better(h.rows[i], h.rows[c]) {
			break
		}
		h.rows[i], h.rows[c] = h.rows[c], h.rows[i]
		i = c
	}
}
func (h *sampleHeap) ordered() []sampleRow {
	sort.Slice(h.rows, func(i, j int) bool { return h.better(h.rows[i], h.rows[j]) })
	return h.rows
}

// selectSampleRows is the original sampler with bounded memory: keep the top
// 32 RSSI rows and lowest 224 ID hashes per radio instead of copying and sorting
// its entire history. Order and tie breakers are identical to the old sorts.
func selectSampleRows(st *obs.State, d *obs.Dict, f *Filter, keys []string, cancelled func() bool) (selected []uint32, count int64, first, last any, err error) {
	var raw int
	for _, k := range keys {
		if id, ok := d.Lookup(k); ok {
			raw += len(st.ByRadio[id])
		}
	}
	small := make([]uint32, 0, min(raw, 256))
	var seenRows int
	for _, k := range keys {
		id, ok := d.Lookup(k)
		if !ok {
			continue
		}
		strongest := sampleHeap{limit: 32}
		spread := sampleHeap{limit: 224, spread: true}
		if raw > 256 {
			strongest.rows = make([]sampleRow, 0, 32)
			spread.rows = make([]sampleRow, 0, 224)
		}
		for _, g := range st.ByRadio[id] {
			if seenRows&4095 == 0 && cancelled != nil && cancelled() {
				return nil, 0, nil, nil, ErrCancelled
			}
			seenRows++
			s, i := st.Loc(g)
			if !f.Match(s, i) {
				continue
			}
			count++
			date := st.Str[s.Date[i]]
			if first == nil || date < first.(string) {
				first = date
			}
			if last == nil || date > last.(string) {
				last = date
			}
			if len(small) < 256 {
				small = append(small, g)
			}
			if raw > 256 {
				x := sampleRow{g, s.ID(i), s.RSSI[i]}
				strongest.add(x)
				spread.add(x)
			}
		}
		if raw > 256 {
			seen := make(map[int64]bool, 256)
			for _, set := range [][]sampleRow{strongest.ordered(), spread.ordered()} {
				for _, x := range set {
					if !seen[x.id] {
						seen[x.id] = true
						selected = append(selected, x.row)
					}
				}
			}
		}
	}
	if count <= 256 {
		sort.Slice(small, func(i, j int) bool { return small[i] < small[j] })
		selected = small
	}
	if cancelled != nil && cancelled() {
		return nil, 0, nil, nil, ErrCancelled
	}
	return
}
