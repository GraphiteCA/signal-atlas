package drivedb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"signal-atlas/internal/db"
	"signal-atlas/internal/pycsv"
)

func write(t *testing.T, path, text string) {
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func script(t *testing.T, path, sql string) {
	c, err := db.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := db.Script(c, sql); err != nil {
		t.Fatal(err)
	}
}

const csvLog = "WigleWifi-1.6,appRelease=test,model=x,release=1,device=x,display=,board=,brand=,star=Sol,body=3,subBody=0\n" +
	"MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AltitudeMeters,AccuracyMeters,RCOIs,MfgrId,Type\n" +
	"02:5a:00:00:01:00,Cafe,[WPA2-PSK-CCMP][ESS],2026-05-01 10:00:00,6,2437,-60,48.4490000,2.6680000,70,4,,,WIFI\n" +
	"02:5A:00:00:01:01,\"Comma, Net\",[ESS],2026-05-01 10:00:05,36,5180,-70,48.4491,2.6681,70,4,,,WIFI\n" +
	",NoMac,[ESS],2026-05-01 10:00:06,1,2412,-80,48.4491,2.6681,70,4,,,WIFI\n" +
	"02:5A:00:00:01:02,Buds,Misc,2026-05-02 09:00:00,,,-50,48.4492,2.6682,70,4,,,BLE\n"

func TestCombineAndExport(t *testing.T) {
	dir := t.TempDir()
	d, err := Create(filepath.Join(dir, "drives.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// a WiGLE CSV log
	logPath := filepath.Join(dir, "log1.csv")
	write(t, logPath, csvLog)
	r, err := d.Add(logPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindCSV || r.RowsRead != 4 || r.RowsAdded != 3 || r.Skipped != 1 {
		t.Fatalf("csv: %+v", r)
	}
	// the same file again is skipped whole
	if r, err := d.Add(logPath, nil, nil); err != nil || !r.Already {
		t.Fatalf("same file twice: %+v %v", r, err)
	}

	// a logger's day database: the same two Wi-Fi sightings with numbers written differently, one
	// new row, extra columns, and a view over the day tables that must not be read twice
	dayDB := filepath.Join(dir, "wardrive_2026_05_01.sqlite3")
	script(t, dayDB, `
CREATE TABLE wardrive_2026_05_01(id INTEGER PRIMARY KEY, MAC TEXT, SSID TEXT, AuthMode TEXT, FirstSeen TEXT, Channel INTEGER,
  Frequency INTEGER, RSSI INTEGER, CurrentLatitude REAL, CurrentLongitude REAL, AltitudeMeters REAL, AccuracyMeters REAL,
  RCOIs TEXT, MfgrId TEXT, Type TEXT, Node TEXT, ts INTEGER, hdop REAL);
INSERT INTO wardrive_2026_05_01(MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AltitudeMeters,AccuracyMeters,Type,Node) VALUES
  ('02:5A:00:00:01:00','Cafe','[WPA2-PSK-CCMP][ESS]','2026-05-01 10:00:00',6,2437,-60,48.449,2.668,70,4,'WIFI',''),
  ('02:5A:00:00:01:01','Comma, Net','[ESS]','2026-05-01 10:00:05',36,5180,-70,48.4491,2.6681,70,4,'WIFI',''),
  ('02:5A:00:00:02:00','Late','[ESS]','2026-05-03 23:59:59',11,2462,-75,48.45,2.67,70,4,'WIFI','F158');
CREATE VIEW all_wardrives AS SELECT *, 'wardrive_2026_05_01' AS day_table FROM wardrive_2026_05_01;`)
	r, err = d.Add(dayDB, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindTables || r.RowsRead != 3 || r.RowsAdded != 1 || r.Duplicates != 2 {
		t.Fatalf("day database: %+v", r)
	}

	// a WiGLE app backup: one Wi-Fi sighting, one Bluetooth, one before a GPS fix
	backup := filepath.Join(dir, "backup.sqlite")
	script(t, backup, `
CREATE TABLE network (bssid text, ssid text, frequency int, capabilities text, lasttime long, lastlat double, lastlon double, type text);
CREATE TABLE location (_id integer primary key autoincrement, bssid text, level integer, lat double, lon double, altitude double, accuracy float, time long);
INSERT INTO network VALUES ('02:5a:00:00:07:00','Shop',5745,'[WPA2]',0,0,0,'W'),('02:5a:00:00:07:01','Watch',0,'Misc',0,0,0,'B');
INSERT INTO location(bssid,level,lat,lon,altitude,accuracy,time) VALUES
  ('02:5a:00:00:07:00',-65,48.4495,2.6685,70,4,1777716000000),
  ('02:5a:00:00:07:01',-55,48.4495,2.6685,70,4,1777716001000),
  ('02:5a:00:00:07:00',-66,0,0,0,0,1777716002000);`)
	r, err = d.Add(backup, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindWiGLEBackup || r.RowsAdded != 3 {
		t.Fatalf("backup: %+v", r)
	}

	// a Signal Atlas database
	atlas := filepath.Join(dir, "atlas.sqlite3")
	script(t, atlas, `
CREATE TABLE imports(id INTEGER PRIMARY KEY, filename TEXT);
CREATE TABLE observations(id INTEGER PRIMARY KEY, import_id INTEGER, line INTEGER, record_id TEXT, ssid TEXT, mac TEXT, band TEXT,
  band_source TEXT, radio_key TEXT, latitude REAL, longitude REAL, rssi_dbm REAL, accuracy_m REAL, first_seen TEXT, date TEXT,
  channel TEXT, frequency_mhz REAL);
INSERT INTO observations VALUES (1,1,2,'2','Cafe','02:5A:00:00:01:00','2.4','frequency','x',48.449,2.668,-60.0,4.0,'2026-05-01 10:00:00','2026-05-01','6',2437.0),
  (2,1,3,'3','Far','02:5A:00:00:09:00','5','frequency','y',48.46,2.69,-80.0,NULL,'2026-05-04 08:00:00','2026-05-04','149',5745.0);`)
	r, err = d.Add(atlas, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindAtlas || r.RowsAdded != 1 || r.Duplicates != 1 {
		t.Fatalf("atlas: %+v", r)
	}

	info, err := d.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info["sightings"] != int64(8) || len(info["sources"].([]db.Row)) != 4 {
		t.Fatalf("info: %v", info)
	}

	// one CSV: the preamble, the header, every sighting; the commas in an SSID survive
	out := filepath.Join(dir, "all.csv")
	n, err := d.ExportCSV(out, "test", nil, nil)
	if err != nil || n != 8 {
		t.Fatalf("export: %d %v", n, err)
	}
	var recs [][]string
	if err := pycsv.ReadFile(out, func(r []string) error { recs = append(recs, append([]string(nil), r...)); return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(recs[0][0], "WigleWifi-1.6") || strings.Join(recs[1], ",") != strings.Join(Columns, ",") || len(recs) != 10 {
		t.Fatalf("csv shape: %d records, header %v", len(recs), recs[1])
	}
	if recs[3][1] != "Comma, Net" {
		t.Fatalf("quoted SSID came back as %q", recs[3][1])
	}
	// adding the export to a fresh database gives the same sightings; to this one, only duplicates
	d2, err := Create(filepath.Join(dir, "again.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if r, err := d2.Add(out, nil, nil); err != nil || r.RowsAdded != 8 {
		t.Fatalf("re-add export: %+v %v", r, err)
	}
	if r, err := d.Add(out, nil, nil); err != nil || r.RowsAdded != 0 || r.Duplicates != 8 {
		t.Fatalf("export back into its own database: %+v %v", r, err)
	}
	// and another Drive database adds like any other
	if r, err := d2.Add(d.Path, nil, nil); err != nil || r.Kind != KindDrive || r.Duplicates != 8 {
		t.Fatalf("a Drive database into another: %+v %v", r, err)
	}

	// one file per day
	days := filepath.Join(dir, "days")
	files, err := d.ExportDays(days, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	if len(files) != 4 || names[0] != "wigle_2026-05-01.csv" || names[3] != "wigle_2026-05-04.csv" {
		t.Fatalf("day files: %v", names)
	}
	b, _ := os.ReadFile(filepath.Join(days, "wigle_2026-05-01.csv"))
	if got := strings.Count(string(b), "\n"); got != 2+2 {
		t.Fatalf("2026-05-01 has %d lines:\n%s", got, b)
	}

	// a copy opens as a Drive database
	cp := filepath.Join(dir, "copy.sqlite3")
	if err := d.ExportCopy(cp); err != nil {
		t.Fatal(err)
	}
	d3, err := Open(cp)
	if err != nil {
		t.Fatal(err)
	}
	d3.Close()
	if k, _ := Detect(cp); k != KindDrive {
		t.Fatalf("copy detected as %q", k)
	}
	// a file that isn't a log or a database is refused
	junk := filepath.Join(dir, "notes.bin")
	write(t, junk, "hello")
	if _, err := d.Add(junk, nil, nil); err == nil {
		t.Fatal("a random file was accepted")
	}
}

// A cancelled add keeps nothing of that file.
func TestCancelKeepsNothing(t *testing.T) {
	dir := t.TempDir()
	d, err := Create(filepath.Join(dir, "drives.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var b strings.Builder
	b.WriteString("MAC,SSID,FirstSeen,RSSI,CurrentLatitude,CurrentLongitude,Type\n")
	for i := 0; i < 200000; i++ {
		b.WriteString("02:5A:00:")
		b.WriteString(strings.ToUpper(strings.Repeat("0", 0)))
		b.WriteString(string(rune('A'+i%26)) + "0:00:01,X,2026-05-01 10:00:00,")
		b.WriteString("-" + string(rune('0'+i%10)) + ",48.4,2.6,WIFI\n")
	}
	p := filepath.Join(dir, "big.csv")
	write(t, p, b.String())
	calls := 0
	_, err = d.Add(p, nil, func() bool { calls++; return calls > 1 })
	if err != ErrCancelled {
		t.Fatalf("want a cancel, got %v", err)
	}
	info, _ := d.Info()
	if info["sightings"] != int64(0) || len(info["sources"].([]db.Row)) != 0 {
		t.Fatalf("after a cancel: %v", info)
	}
	if v, _ := db.Scalar(d.c, "SELECT count(*) FROM sighting_keys"); db.Int(v) != 0 {
		t.Fatal("keys of a cancelled file were kept")
	}
}
