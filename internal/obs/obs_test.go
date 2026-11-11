package obs

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
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
