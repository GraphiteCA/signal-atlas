package obs

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestCompactPostingsAndAlignedReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := []uint32{store.Dict.ID(""), store.Dict.ID("A"), store.Dict.ID("B")}
	_ = ids
	segments := []*Segment{}
	for part := 0; part < 3; part++ {
		seg := NewSegment(int64(part+1), int64(part*20+1), 20)
		for i := 0; i < 20; i++ {
			r := Row{SSID: uint32(i % 3), MAC: uint32(i % 2), Radio: uint32(i % 3), Lat: 48 + float64(i%4)*.01, Lon: -123, RSSI: -float64(i), Acc: Null, Freq: Null, Line: int64(i + 1), RecVal: int64(i + 1)}
			seg.Append(&r)
		}
		segments = append(segments, seg)
	}
	if err = store.Commit(Change{Add: segments}); err != nil {
		t.Fatal(err)
	}
	st := store.State()
	for _, m := range []map[uint32][]uint32{st.BySSID, st.ByRadio} {
		for _, rows := range m {
			if len(rows) != cap(rows) {
				t.Fatal("spare posting capacity")
			}
			for i, g := range rows {
				if i > 0 && g <= rows[i-1] {
					t.Fatal("unordered postings")
				}
			}
		}
	}
	reopened, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(st.BySSID, reopened.State().BySSID) || !reflect.DeepEqual(st.Grid, reopened.State().Grid) {
		t.Fatal("reopen changed indexes")
	}
}
func TestReadSegmentRejectsMalformedHeadersWithoutPanic(t *testing.T) {
	for _, n := range []uint32{0, 4097, ^uint32(0)} {
		b := make([]byte, 20)
		copy(b, segMagic)
		binary.LittleEndian.PutUint32(b[8:], n)
		file := filepath.Join(t.TempDir(), "bad.bin")
		if err := os.WriteFile(file, b, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readSegment(file); err == nil {
			t.Fatal("accepted corrupt header", n)
		}
	}
}

func TestCompactDictionaryGrowthArenaAndConcurrentLookup(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	values := []string{"", "Hello", "日本語", strings.Repeat("x", (1<<20)+7)}
	for i := 0; i < 10000; i++ {
		values = append(values, fmt.Sprintf("network-%d", i))
	}
	for i, s := range values {
		if id := store.Dict.ID(s); id != uint32(i) {
			t.Fatal(i, id)
		}
	}
	snapshot := store.Dict.Snapshot()
	if err := store.Commit(Change{}); err != nil {
		t.Fatal(err)
	}
	re, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, s := range values {
				if id, ok := re.Dict.Lookup(s); !ok || id != uint32(i) {
					t.Errorf("lookup %d: %d %v", i, id, ok)
				}
			}
		}()
	}
	for i := 0; i < 10000; i++ {
		re.Dict.ID(fmt.Sprintf("new-%d", i))
	}
	wg.Wait()
	if !reflect.DeepEqual(snapshot, re.Dict.Snapshot()[:len(snapshot)]) {
		t.Fatal("growth changed prior IDs or arena bytes")
	}
	if _, ok := re.Dict.Lookup("absent"); ok {
		t.Fatal("unknown value found")
	}
	if err := re.Commit(Change{}); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(re.Dict.Snapshot(), again.Dict.Snapshot()) {
		t.Fatal("append/reopen changed dictionary")
	}
}
