package atlas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"signal-atlas/internal/db"
)

func TestAutomaticImportDeduplication(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	header := "MAC,SSID,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n"
	row := "02:00:00:00:00:01,Test,2026-01-02 12:00:00,1,2412,-60,49,-123,3,WIFI\n"
	importFile := func(name, body string) map[string]any {
		t.Helper()
		path := filepath.Join(dir, name)
		writeFile(t, path, header+body)
		r, err := a.ImportCSV(path, false, nil, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := importFile("first.csv", row+row)
	if r["valid_rows"] != int64(1) || r["duplicate_rows"] != int64(1) {
		t.Fatal(r)
	}
	r = importFile("overlap.csv", row+strings.Replace(row, "-60", "-61", 1)+row)
	if r["valid_rows"] != int64(1) || r["duplicate_rows"] != int64(2) {
		t.Fatal(r)
	}
	r = importFile("duplicates.csv", row+row+row)
	if r["valid_rows"] != int64(0) || r["duplicate_rows"] != int64(3) || r["empty"] != true {
		t.Fatal(r)
	}
	// Failure after a unique row must not poison a subsequent import.
	newRow := strings.Replace(row, "12:00:00", "12:00:01", 1)
	failed := filepath.Join(dir, "failed.csv")
	writeFile(t, failed, header+newRow+strings.Replace(row, "49,-123", "bad,-123", 1))
	if _, err = a.ImportCSV(failed, false, nil, "", nil); err == nil {
		t.Fatal("invalid file accepted")
	}
	if a.Obs.State().N != 2 {
		t.Fatal("failed import changed observations")
	}
	cancelled := filepath.Join(dir, "cancel.csv")
	writeFile(t, cancelled, header+newRow)
	if _, err = a.ImportCSV(cancelled, false, func(v, i, read, total int64) error {
		if read == total {
			return context.Canceled
		}
		return nil
	}, "", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r = importFile("retry.csv", newRow)
	if r["valid_rows"] != int64(1) || r["duplicate_rows"] != int64(0) {
		t.Fatal(r)
	}
	root := a.Root
	a.Close()
	a, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	r = importFile("after-restart.csv", newRow+row)
	if r["valid_rows"] != int64(0) || r["duplicate_rows"] != int64(2) {
		t.Fatal(r)
	}
	if err = a.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	history := table(t, a, "SELECT * FROM imports ORDER BY id")
	if len(history) != 5 || db.Int(history[2]["duplicate_rows"]) != 3 || db.Int(history[2]["active_rows"]) != 0 {
		t.Fatal(history)
	}
	if a.Obs.State().N != 3 {
		t.Fatal("unexpected observations")
	}
}

func TestWiGLEAutomaticDeduplication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.sqlite")
	c, err := db.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Script(c, `CREATE TABLE network(bssid TEXT,ssid TEXT,capabilities TEXT,frequency INTEGER,type TEXT);
 CREATE TABLE location(_id INTEGER PRIMARY KEY,bssid TEXT,level INTEGER,lat REAL,lon REAL,altitude REAL,accuracy REAL,time INTEGER);
 INSERT INTO network VALUES('02:00:00:00:00:01','Test','',2412,'W');
 INSERT INTO location VALUES(1,'02:00:00:00:00:01',-60,49,-123,0,3,1767355200000),(2,'02:00:00:00:00:01',-60,49,-123,0,3,1767355200000);`)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r, err := a.ImportWiGLE(path, false, nil, nil)
	if err != nil || r["valid_rows"] != int64(1) || r["duplicate_rows"] != int64(1) {
		t.Fatalf("%v %v", r, err)
	}
	c, err = db.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(c, "INSERT INTO location VALUES(3,'02:00:00:00:00:01',-61,49,-123,0,3,1767355200000)")
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	r, err = a.ImportWiGLE(path, false, nil, nil)
	if err != nil || r["valid_rows"] != int64(1) || r["duplicate_rows"] != int64(2) {
		t.Fatalf("%v %v", r, err)
	}
	// A CSV export overlaps the same native-backup observations.
	out := filepath.Join(dir, "export.csv")
	if _, _, err = a.ExportSurveyCSV(context.Background(), out, "test", false, nil); err != nil {
		t.Fatal(err)
	}
	r, err = a.ImportCSV(out, false, nil, "", nil)
	if err != nil || r["valid_rows"] != int64(0) || r["duplicate_rows"] != int64(2) {
		t.Fatalf("%v %v", r, err)
	}
	if _, err = os.Stat(out); err != nil {
		t.Fatal(err)
	}
}
