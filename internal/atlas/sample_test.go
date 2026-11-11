package atlas

import (
	"fmt"
	"reflect"
	"signal-atlas/internal/obs"
	"sort"
	"testing"
)

func sampleFixture(n, radios int) (*obs.State, *obs.Dict, *Filter, []string) {
	d := obs.NewDictionary()
	empty := d.ID("")
	date := d.ID("2026-11-01")
	sid := d.ID("Survey")
	band := d.ID("5")
	keys := make([]string, radios)
	ids := make([]uint32, radios)
	for i := range keys {
		keys[i] = fmt.Sprintf("radio-%d", i)
		ids[i] = d.ID(keys[i])
	}
	s := obs.NewSegment(1, 1, n)
	for i := 0; i < n; i++ {
		r := obs.Row{SSID: sid, MAC: ids[i%radios], Band: band, BandSrc: empty, Radio: ids[i%radios], Date: date, Channel: empty, First: date, Lat: 48.4, Lon: -123.4, RSSI: float64(-30 - (i*17)%80), Acc: obs.Null, Freq: obs.Null}
		s.Append(&r)
		s.OriginalIDs = append(s.OriginalIDs, int64(i)*13+(1<<50))
	}
	st := obs.NewReadView([]*obs.Segment{s}, d).State()
	f := (&Filter{minRSSI: -90}).Compile(st, d)
	return st, d, f, keys
}

// Independent oracle: the prior production full stable sorts and tie breakers.
func sampleSortReference(st *obs.State, d *obs.Dict, f *Filter, keys []string) (selected []uint32, count int64) {
	var rows, match []uint32
	for _, k := range keys {
		id, _ := d.Lookup(k)
		rows = append(rows, st.ByRadio[id]...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i] < rows[j] })
	for _, g := range rows {
		s, i := st.Loc(g)
		if f.Match(s, i) {
			match = append(match, g)
		}
	}
	count = int64(len(match))
	if count <= 256 {
		return match, count
	}
	for _, k := range keys {
		kid, _ := d.Lookup(k)
		var mine []uint32
		for _, g := range match {
			s, i := st.Loc(g)
			if s.Radio[i] == kid {
				mine = append(mine, g)
			}
		}
		id := func(g uint32) int64 { s, i := st.Loc(g); return s.ID(i) }
		strong := append([]uint32(nil), mine...)
		sort.SliceStable(strong, func(i, j int) bool {
			a, ai := st.Loc(strong[i])
			b, bi := st.Loc(strong[j])
			if a.RSSI[ai] != b.RSSI[bi] {
				return a.RSSI[ai] > b.RSSI[bi]
			}
			return id(strong[i]) < id(strong[j])
		})
		spread := append([]uint32(nil), mine...)
		sort.SliceStable(spread, func(i, j int) bool {
			a, b := sampleHash(id(spread[i])), sampleHash(id(spread[j]))
			if a != b {
				return a < b
			}
			return id(spread[i]) < id(spread[j])
		})
		seen := map[int64]bool{}
		for _, g := range append(strong[:min(32, len(strong))], spread[:min(224, len(spread))]...) {
			if !seen[id(g)] {
				seen[id(g)] = true
				selected = append(selected, g)
			}
		}
	}
	return
}
func TestBoundedSamplerMatchesFullSortWithTiesAndOriginalIDs(t *testing.T) {
	for _, n := range []int{0, 3, 256, 257, 500, 5000} {
		for _, radios := range []int{1, 3, 20} {
			st, d, f, keys := sampleFixture(n, radios)
			want, count := sampleSortReference(st, d, f, keys)
			got, n, first, last, err := selectSampleRows(st, d, f, keys, nil)
			if err != nil || n != count || !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
				t.Fatal(n, radios, count, len(got), len(want), err)
			}
			if n > 0 && (first != "2026-11-01" || last != "2026-11-01") {
				t.Fatal(first, last)
			}
			if _, _, _, _, err := selectSampleRows(st, d, f, keys, func() bool { return true }); err != ErrCancelled {
				t.Fatal("cancellation", err)
			}
		}
	}
}
func BenchmarkObservationSamplerRowIndexOracle(b *testing.B) {
	st, d, f, keys := sampleFixture(200000, 3)
	b.Run("full-sort", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sampleSortReference(st, d, f, keys)
		}
	})
	b.Run("bounded", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			selectSampleRows(st, d, f, keys, nil)
		}
	})
}
