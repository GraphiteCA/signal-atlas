package atlas

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"signal-atlas/internal/db"
	"signal-atlas/internal/py"
)

func demoCSV(t *testing.T) string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..", "python", "demo-survey.csv")
}

func allRows(a *Atlas) []Row {
	st := a.Obs.State()
	var out []Row
	for _, s := range st.Segs {
		for i := 0; i < s.N; i++ {
			out = append(out, fullRow(st, s, i))
		}
	}
	return out
}

func table(t *testing.T, a *Atlas, q string, args ...any) []db.Row {
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer put()
	rows, err := db.All(c, q, args...)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A database the explorer saves opens again with the same observations, imports and estimates.
func TestSaveAndOpenDatabase(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.ImportCSV(demoCSV(t), false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	// a second import, so ids span two imports
	gen := filepath.Join(dir, "second.csv")
	writeFile(t, gen, "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n"+
		"02:5A:00:00:09:00,Other,2026-01-02 10:00:00,5180,-70,48.4490,2.6680,4,WIFI\n"+
		"02:5A:00:00:09:00,Other,2026-01-02 10:00:05,5180,-72,48.4491,2.6682,,WIFI\n")
	if _, err := a.ImportCSV(gen, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	c, put, _ := a.Conn(nil)
	groups, err := a.SelectedGroups(py.Params{"ssid": {"ExampleNet"}})
	if err != nil {
		t.Fatal(err)
	}
	p := py.Params{"ssid": {"ExampleNet"}}
	_, scopeKey, _ := EstimateScope(p)
	for _, g := range EstimationGroups(groups, p) {
		if _, err := a.FitAndStore(c, g, p, scopeKey, func() bool { return false }); err != nil {
			t.Fatal(err)
		}
	}
	put()
	saved := filepath.Join(dir, "saved.sqlite3")
	if err := a.ExportSQLite(saved, nil); err != nil {
		t.Fatal(err)
	}
	if k, err := DatabaseKind(saved); err != nil || k != KindAtlas {
		t.Fatalf("kind %q, %v", k, err)
	}

	b, err := Open(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	// something already in b, which opening replaces
	if _, err := b.ImportCSV(gen, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	r, err := b.OpenDatabase(saved, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r["valid_rows"], int64(len(allRows(a))); got != want {
		t.Fatalf("opened %v rows, want %v", got, want)
	}
	if !reflect.DeepEqual(allRows(a), allRows(b)) {
		t.Fatal("observations differ after save and open")
	}
	for _, q := range []string{"SELECT * FROM imports ORDER BY id", "SELECT * FROM estimates ORDER BY radio_key,scope_key", "SELECT * FROM band_assignments"} {
		if x, y := table(t, a, q), table(t, b, q); !reflect.DeepEqual(x, y) {
			t.Fatalf("%s differs:\n%v\n%v", q, x, y)
		}
	}
	if n := len(table(t, b, "SELECT * FROM estimates")); n == 0 {
		t.Fatal("no estimates came across")
	}
	// it survives closing and reopening the store
	b.Close()
	b2, err := Open(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if !reflect.DeepEqual(allRows(a), allRows(b2)) {
		t.Fatal("observations differ after reopening the store")
	}
}

// A WiGLE Android backup's Wi-Fi sightings are imported as a CSV's would be.
func TestImportWiGLE(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.sqlite")
	c, err := db.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Script(c, `
CREATE TABLE network (bssid text not null, ssid text not null, frequency int not null, capabilities text not null,
  lasttime long not null, lastlat double not null, lastlon double not null, type text not null default 'W',
  bestlevel integer not null default 0, bestlat double not null default 0, bestlon double not null default 0,
  rcois text not null default '', mfgrid integer not null default 0, service text not null default '', primary key (bssid));
CREATE TABLE location (_id integer primary key autoincrement, bssid text not null, level integer not null,
  lat double not null, lon double not null, altitude double not null, accuracy float not null, time long not null,
  external integer not null default 0, mfgrid integer not null default 0);
INSERT INTO network(bssid,ssid,frequency,capabilities,lasttime,lastlat,lastlon,type) VALUES
  ('02:5a:00:00:07:00','Cafe',2437,'[WPA2-PSK-CCMP][ESS]',0,0,0,'W'),
  ('02:5a:00:00:07:01','Cafe',5745,'[WPA2-PSK-CCMP][ESS]',0,0,0,'W'),
  ('02:5a:00:00:07:02','Headphones',0,'Misc',0,0,0,'B');
INSERT INTO location(bssid,level,lat,lon,altitude,accuracy,time) VALUES
  ('02:5a:00:00:07:00',-60,48.4490,2.6680,70,4,1773512447000),
  ('02:5a:00:00:07:01',-75,48.4491,2.6681,70,4,1773512450000),
  ('02:5a:00:00:07:02',-50,48.4491,2.6681,70,4,1773512451000),
  ('02:5a:00:00:07:00',-62,0,0,0,0,1773512452000);`)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	if k, err := DatabaseKind(path); err != nil || k != KindWiGLE {
		t.Fatalf("kind %q, %v", k, err)
	}
	a, err := Open(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r, err := a.ImportWiGLE(path, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r["valid_rows"] != int64(2) || r["invalid_rows"] != int64(0) || r["non_wifi_rows"] != int64(1) || r["zero_excluded_rows"] != int64(1) {
		t.Fatalf("result %v", r)
	}
	rows := allRows(a)
	if rows[0]["mac"] != "02:5A:00:00:07:00" || rows[0]["band"] != "2.4" || rows[0]["channel"] != "6" || rows[0]["first_seen"] != "2026-03-14 18:20:47" {
		t.Fatalf("first row %v", rows[0])
	}
	if rows[1]["band"] != "5" || rows[1]["channel"] != "149" {
		t.Fatalf("second row %v", rows[1])
	}
	if r, _ := a.ImportWiGLE(path, true, nil, nil); r["duplicate"] != true {
		t.Fatalf("the same backup twice should be a duplicate: %v", r)
	}
}

func writeFile(t *testing.T, path, text string) {
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
