package obs

import (
	"maps"
	"math"
	"slices"
	"sort"
)

// appendIndex shares immutable, unaffected postings/catalogue entries with the
// previous State. Only a small append with identical old segment ordering is
// eligible; removals, band replacements and large imports use a full rebuild.
// No old map, statistic or backing array is mutated.
func (st *State) appendIndex(old *State) bool {
	if old.N == 0 || len(st.Segs) < len(old.Segs) {
		return false
	}
	for i, s := range old.Segs {
		if st.Segs[i] != s {
			return false
		}
	}
	added := st.Segs[len(old.Segs):]
	n := 0
	for _, s := range added {
		if s.Base <= old.MaxID {
			return false
		}
		n += s.N
	}
	if n == 0 || n > min(1_000_000, max(4096, old.N/4)) {
		return false
	}
	st.shape()
	delta := &State{Segs: added, Str: st.Str}
	delta.index()
	st.ByRadio = appendPostings(old.ByRadio, delta.ByRadio, uint32(old.N))
	st.BySSID = appendPostings(old.BySSID, delta.BySSID, uint32(old.N))
	st.ByTrans = appendPostings(old.ByTrans, delta.ByTrans, uint32(old.N))
	st.Grid = appendPostings(old.Grid, delta.Grid, uint32(old.N))
	st.Trans = maps.Clone(old.Trans)
	st.Net = maps.Clone(old.Net)
	changed := map[uint32]*NetStat{}
	for key, d := range delta.Trans {
		previous := old.Trans[key]
		merged := d
		oldBands := 0
		if previous != nil {
			value := *previous
			value.Count += d.Count
			value.Editable += d.Editable
			value.Peak = max(value.Peak, d.Peak)
			value.Bands = unionBands(previous.Bands, d.Bands, st.Str)
			merged = &value
			oldBands = len(previous.Bands)
		}
		st.Trans[key] = merged
		net := changed[key[0]]
		if net == nil {
			net = &NetStat{Peak: math.Inf(-1)}
			if previous := old.Net[key[0]]; previous != nil {
				*net = *previous
				net.Bands = slices.Clone(previous.Bands)
			}
			changed[key[0]] = net
			st.Net[key[0]] = net
		}
		net.Count += d.Count
		if previous == nil {
			net.Macs++
		}
		net.Radios += int64(len(merged.Bands) - oldBands)
		net.Peak = max(net.Peak, d.Peak)
		for _, band := range d.Bands {
			if !slices.Contains(net.Bands, band) {
				net.Bands = append(net.Bands, band)
			}
		}
	}
	for _, net := range changed {
		sort.Slice(net.Bands, func(i, j int) bool { return st.Str[net.Bands[i]] < st.Str[net.Bands[j]] })
	}
	return true
}

func appendPostings[K comparable](old, delta map[K][]uint32, offset uint32) map[K][]uint32 {
	out := maps.Clone(old)
	for key, rows := range delta {
		previous := old[key]
		merged := make([]uint32, len(previous)+len(rows))
		copy(merged, previous)
		for i, g := range rows {
			merged[len(previous)+i] = g + offset
		}
		out[key] = merged
	}
	return out
}
func unionBands(old, added []uint32, str []string) []uint32 {
	out := slices.Clone(old)
	for _, band := range added {
		if !slices.Contains(out, band) {
			out = append(out, band)
		}
	}
	sort.Slice(out, func(i, j int) bool { return str[out[i]] < str[out[j]] })
	return out
}
