package obs

import (
	"math/rand/v2"
	"reflect"
	"testing"
)

func assertIndexesEqual(t *testing.T, a, b *State) {
	t.Helper()
	for name, pair := range map[string][2]any{"radio": {a.ByRadio, b.ByRadio}, "ssid": {a.BySSID, b.BySSID}, "transmitter": {a.ByTrans, b.ByTrans}, "grid": {a.Grid, b.Grid}, "net_stats": {a.Net, b.Net}, "trans_stats": {a.Trans, b.Trans}, "segments": {a.start, b.start}, "rows": {a.N, b.N}, "max_id": {a.MaxID, b.MaxID}} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatal("incremental differs from full rebuild:", name, pair)
		}
	}
}
func TestAppendIndexesMatchFullRebuildAndPreserveOldSnapshots(t *testing.T) {
	strings := []string{"", "one", "two", "three", "2.4", "5", "6", "frequency", "manual assignment"}
	rng := rand.New(rand.NewPCG(1234, 5678))
	makeSegment := func(base int64, n int) *Segment {
		s := NewSegment(base, base, n)
		for i := 0; i < n; i++ {
			r := Row{SSID: uint32(rng.IntN(4)), MAC: uint32(rng.IntN(4)), Band: uint32(4 + rng.IntN(3)), BandSrc: uint32(7 + rng.IntN(2)), Radio: uint32(rng.IntN(8)), Lat: 48 + float64(rng.IntN(3))*.01, Lon: -123, RSSI: float64(-90 + rng.IntN(50))}
			s.Append(&r)
		}
		return s
	}
	state := &State{Segs: []*Segment{makeSegment(1, 2000)}, Str: strings}
	state.index()
	for round := 0; round < 30; round++ {
		// A full reference also detects accidental mutation of a previously captured
		// snapshot while another goroutine could still be reading it.
		oldReference := &State{Segs: state.Segs, Str: strings}
		oldReference.index()
		segs := append(append([]*Segment(nil), state.Segs...), makeSegment(state.MaxID+7, 1+rng.IntN(120)))
		next := &State{Segs: segs, Str: strings}
		if !next.appendIndex(state) {
			t.Fatal("append was not incremental")
		}
		full := &State{Segs: segs, Str: strings}
		full.index()
		assertIndexesEqual(t, next, full)
		assertIndexesEqual(t, state, oldReference)
		state = next
	}
}
func TestAppendCommitAndReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"", "SSID", "MAC", "2.4", "5", "frequency", "manual assignment", "radio-a", "radio-b"} {
		store.Dict.ID(s)
	}
	add := func(base int64, n int) {
		seg := NewSegment(base, base, n)
		for i := 0; i < n; i++ {
			r := Row{SSID: 1, MAC: 2, Band: 3 + uint32(i%2), BandSrc: 5 + uint32(i%2), Radio: 7 + uint32(i%2), Lat: 48, Lon: -123, RSSI: -60}
			seg.Append(&r)
		}
		if err := store.Commit(Change{Add: []*Segment{seg}}); err != nil {
			t.Fatal(err)
		}
	}
	add(1, 1000)
	old := store.State()
	add(1001, 10)
	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertIndexesEqual(t, store.State(), reopened.State())
	if old.N != 1000 || old.Net[1].Count != 1000 {
		t.Fatal("old snapshot changed")
	}
}
