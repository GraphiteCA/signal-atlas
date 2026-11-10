package compare

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"signal-atlas/internal/db"
)

// A logger's day database added to Compare gives the same analysis as the same drive as a CSV
// log: the database is read as the CSV it converts to, and its rows without a GPS fix are left out
// (Compare would ignore them anyway).
func TestDatabaseLogMatchesCSV(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	var cs struct {
		Cfgs []*Config `json:"cfgs"`
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "test", "compare", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	// one drive: 400 seconds east along a road, passing 30 access points
	type row struct {
		mac, ssid, first string
		ch, freq, rssi   int
		lat, lon         float64
	}
	var rows []row
	for s := 0; s < 400; s++ {
		lat, lon := 48.449, math.Round((2.660+float64(s)*10/73800)*1e7)/1e7 // as the CSV writes it
		for ap := 0; ap < 30; ap++ {
			apLon := 2.660 + float64(ap)*0.0018
			d := math.Abs(lon-apLon)*73800 + 20
			if d > 150 || (s+ap)%3 != 0 {
				continue
			}
			rows = append(rows, row{fmt.Sprintf("02:5A:00:00:%02X:00", ap), fmt.Sprintf("Net%d", ap),
				fmt.Sprintf("2026-05-01 10:%02d:%02d", s/60, s%60), 1 + ap%11, 2412 + 5*(ap%11), int(-35 - 25*math.Log10(d)), lat, lon})
		}
	}
	var csv strings.Builder
	csv.WriteString("WigleWifi-1.6,appRelease=test,model=Test,release=1,device=Test,display=,board=,brand=,star=Sol,body=3,subBody=0\n")
	csv.WriteString("MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AltitudeMeters,AccuracyMeters,RCOIs,MfgrId,Type,Node\n")
	for _, r := range rows {
		fmt.Fprintf(&csv, "%s,%s,[WPA2-PSK-CCMP][ESS],%s,%d,%d,%d,%.7f,%.7f,70,4,,,WIFI,\n", r.mac, r.ssid, r.first, r.ch, r.freq, r.rssi, r.lat, r.lon)
	}
	csvPath := filepath.Join(dir, "drive.csv")
	os.WriteFile(csvPath, []byte(csv.String()), 0o644)

	// the same drive as the Core writes it, plus sightings from before the GPS had a fix
	dbPath := filepath.Join(dir, "wardrive_2026_05_01.sqlite3")
	c, err := db.Open(dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Script(c, `CREATE TABLE "wardrive_2026_05_01"(id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, "MAC" TEXT, "SSID" TEXT,
		"AuthMode" TEXT, "FirstSeen" TEXT, "Channel" INTEGER, "Frequency" INTEGER, "RSSI" INTEGER, "CurrentLatitude" REAL,
		"CurrentLongitude" REAL, "AltitudeMeters" REAL, "AccuracyMeters" REAL, "RCOIs" TEXT, "MfgrId" TEXT, "Type" TEXT,
		"Node" TEXT, source INTEGER NOT NULL DEFAULT 0, line INTEGER NOT NULL DEFAULT 0, hdop REAL, uploaded INTEGER);
		CREATE TABLE _meta(key TEXT PRIMARY KEY, value TEXT);
		CREATE VIEW all_wardrives AS SELECT *, 'wardrive_2026_05_01' AS day_table FROM wardrive_2026_05_01;`)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(c, "BEGIN")
	for i := 0; i < 5; i++ {
		db.Exec(c, `INSERT INTO wardrive_2026_05_01(ts,MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,Type,Node)
			VALUES(0,?,'Early','[ESS]','2026-05-01 09:59:00',1,2412,-50,0,0,'WIFI','CORE')`, fmt.Sprintf("02:5A:00:00:FF:%02X", i))
	}
	for _, r := range rows {
		if _, err := db.Exec(c, `INSERT INTO wardrive_2026_05_01(ts,MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,
			CurrentLongitude,AltitudeMeters,AccuracyMeters,Type,Node) VALUES(0,?,?,'[WPA2-PSK-CCMP][ESS]',?,?,?,?,?,?,70,4,'WIFI','')`,
			r.mac, r.ssid, r.first, r.ch, r.freq, r.rssi, r.lat, r.lon); err != nil {
			t.Fatal(err)
		}
	}
	db.Exec(c, "COMMIT")
	c.Close()

	run := func(path string) map[string]any {
		var last map[string]any
		e := New(func(m map[string]any) {
			if m["type"] != "progress" {
				last = m
			}
		}, filepath.Join(dir, "tmp-"+filepath.Base(path)))
		defer e.Close()
		st, _ := os.Stat(path)
		e.Handle(&Message{Type: "add", Files: []FileIn{{Name: filepath.Base(path), Size: st.Size(), Path: path}}, Cfg: cs.Cfgs[0]})
		if errs, ok := last["errors"].([]any); ok && len(errs) > 0 {
			t.Fatalf("%s: %v", filepath.Base(path), errs)
		}
		e.Handle(&Message{Type: "analyse", Cfg: cs.Cfgs[0]})
		out := roundTrip(last).(map[string]any)
		r, _ := out["result"].(map[string]any)
		if r == nil {
			t.Fatalf("%s: no result: %v", filepath.Base(path), out)
		}
		return r
	}
	a, b2 := run(csvPath), run(dbPath)
	// names, sizes, timing and the logger's description are about the file, not the drive
	var scrub func(v any) any
	scrub = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, y := range x {
				switch k {
				case "ms", "name", "file", "files", "size", "bytes", "device", "app", "label", "title", "key", "keys", "id", "fileName":
					continue
				}
				out[k] = scrub(y)
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, y := range x {
				out[i] = scrub(y)
			}
			return out
		}
		return v
	}
	if drives := a["drives"].([]any); len(drives) == 0 {
		t.Fatal("the CSV drive gave no drives")
	}
	if !reflect.DeepEqual(scrub(a), scrub(b2)) {
		ja, _ := json.Marshal(scrub(a))
		jb, _ := json.Marshal(scrub(b2))
		n := 0
		for n < len(ja) && n < len(jb) && ja[n] == jb[n] {
			n++
		}
		lo := max(0, n-200)
		t.Fatalf("the database's analysis differs from the CSV's at byte %d:\ncsv: %s\ndb:  %s", n, ja[lo:min(len(ja), n+200)], jb[lo:min(len(jb), n+200)])
	}
}
