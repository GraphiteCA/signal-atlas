package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
)

func waitJob(t *testing.T, j *Jobs) map[string]any {
	for i := 0; i < 600 && j.Running(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	s := j.Snapshot()
	if s["status"] != "complete" {
		t.Fatalf("job ended %v: %v", s["status"], s["error"])
	}
	return s
}

func observations(a *atlas.Atlas) []string {
	st := a.Obs.State()
	var out []string
	for _, s := range st.Segs {
		for i := 0; i < s.N; i++ {
			out = append(out, fmt.Sprintf("%s|%s|%s|%s|%g|%g|%g", st.Str[s.MAC[i]], st.Str[s.SSID[i]], st.Str[s.Band[i]], st.Str[s.First[i]], s.Lat[i], s.Lon[i], s.RSSI[i]))
		}
	}
	return out
}

// A logger's day database opened in the explorer gives the same observations as the same drive
// imported as a CSV survey; rows without a GPS fix and Bluetooth rows are left out.
func TestExplorerImportsLoggerDatabase(t *testing.T) {
	dir := t.TempDir()
	csv := "MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AltitudeMeters,AccuracyMeters,Type\n"
	dbPath := filepath.Join(dir, "wardrive_2026_05_01.sqlite3")
	c, err := db.Open(dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	db.Script(c, `CREATE TABLE "wardrive_2026_05_01"(id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, "MAC" TEXT, "SSID" TEXT, "AuthMode" TEXT,
		"FirstSeen" TEXT, "Channel" INTEGER, "Frequency" INTEGER, "RSSI" INTEGER, "CurrentLatitude" REAL, "CurrentLongitude" REAL,
		"AltitudeMeters" REAL, "AccuracyMeters" REAL, "RCOIs" TEXT, "MfgrId" TEXT, "Type" TEXT, "Node" TEXT, source INTEGER NOT NULL DEFAULT 0,
		line INTEGER NOT NULL DEFAULT 0, uploaded INTEGER);
		CREATE TABLE _nodes(session INTEGER, tag TEXT, mac TEXT);
		CREATE VIEW all_wardrives AS SELECT * FROM wardrive_2026_05_01;`)
	ins := func(mac, ssid, first string, freq, rssi int, lat, lon float64, typ string) {
		db.Exec(c, `INSERT INTO wardrive_2026_05_01(ts,MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AltitudeMeters,AccuracyMeters,Type,Node)
			VALUES(0,?,?,'[WPA2-PSK-CCMP][ESS]',?,6,?,?,?,?,70,4,?,'F158')`, mac, ssid, first, freq, rssi, lat, lon, typ)
	}
	for i := 0; i < 40; i++ {
		mac := fmt.Sprintf("02:5A:00:00:%02X:00", i%8)
		first := fmt.Sprintf("2026-05-01 10:%02d:%02d", i/60, i%60)
		lat, lon := 48.449+float64(i)*0.0001, 2.668+float64(i%5)*0.0001
		ins(mac, fmt.Sprintf("Net%d", i%8), first, 2437, -50-i%30, lat, lon, "WIFI")
		csv += fmt.Sprintf("%s,Net%d,[WPA2-PSK-CCMP][ESS],%s,6,2437,%d,%s,%s,70,4,WIFI\n", mac, i%8, first, -50-i%30,
			fmt.Sprint(lat), fmt.Sprint(lon))
	}
	ins("02:5A:00:00:EE:00", "NoFix", "2026-05-01 09:59:00", 2437, -40, 0, 0, "WIFI")
	ins("02:5A:00:00:EE:01", "Buds", "2026-05-01 10:00:30", 0, -60, 48.449, 2.668, "BLE")
	c.Close()
	csvPath := filepath.Join(dir, "drive.csv")
	os.WriteFile(csvPath, []byte(csv), 0o644)

	a, err := atlas.Open(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ja := NewJobs(a)
	if _, err := ja.ImportFile(csvPath, "drive.csv", false, false); err != nil {
		t.Fatal(err)
	}
	waitJob(t, ja)

	b, err := atlas.Open(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	jb := NewJobs(b)
	if _, err := jb.ImportLoggerDB(dbPath, filepath.Join(dir, "tmp"), false); err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, jb)
	r := s["result"].(map[string]any)
	if r["no_position_rows"] != int64(1) || r["non_wifi_rows"] != int64(1) || r["valid_rows"] != int64(40) {
		t.Fatalf("result %v", r)
	}
	if x, y := observations(a), observations(b); !reflect.DeepEqual(x, y) {
		t.Fatalf("observations differ:\ncsv: %v\ndb:  %v", x[:3], y[:3])
	}
	// the same database again is a duplicate
	if _, err := jb.ImportLoggerDB(dbPath, filepath.Join(dir, "tmp"), false); err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, jb); s["result"].(map[string]any)["duplicate"] != true {
		t.Fatalf("second import: %v", s["result"])
	}
}
