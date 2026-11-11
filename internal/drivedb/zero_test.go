package drivedb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestZeroPairsExcludedWithSeparateArchiveCounts(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "zero.csv")
	text := "MAC,SSID,FirstSeen,RSSI,CurrentLatitude,CurrentLongitude,Type,Node\n" +
		"02:00:00:00:00:01,Zero,2026-01-01 00:00:00,-60,0,-0,WIFI,node\n" +
		"02:00:00:00:00:02,Zero,2026-01-01 00:00:01,-60,0e5,0.0,BLE,node\n" +
		"02:00:00:00:00:03,Equator,2026-01-01 00:00:02,-60,0,10,WIFI,node\n" +
		"02:00:00:00:00:04,Meridian,2026-01-01 00:00:03,-60,10,0,BLE,node\n"
	if err := os.WriteFile(source, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "archive.sqlite3")
	d, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.Add(source, nil, nil)
	if err != nil || r.RowsRead != 4 || r.RowsAdded != 2 || r.ZeroExcluded != 2 || r.Skipped != 0 {
		t.Fatalf("archive result %+v %v", r, err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	r, err = d.Add(source, nil, nil)
	if err != nil || !r.Already {
		t.Fatal("archive whole-file dedup changed")
	}
	info, err := d.Info()
	if err != nil || info["sightings"] != int64(2) {
		t.Fatalf("archive reopen %v %v", info, err)
	}
}
