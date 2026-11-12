package atlas

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
)

func TestPreservationRoundTrips(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	header := "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,Type,AuthMode,AltitudeMeters,RCOIs,MfgrId,Node,Vendor\r\n"
	row := "AA,\"Cafe, north\",2026-01-01 12:00:00.123,2412,-60,49,-123,WIFI,WPA3,12.50,abc,42,node-a,\"line one\nline two\"\r\n"
	body := header + row + row + strings.Replace(row, "node-a", "node-b", 1) + strings.Replace(row, "WPA3", "WPA2", 1) + strings.Replace(row, "49,-123", "0,0", 1) + strings.Replace(row, "WIFI", "BLE", 1)
	source := filepath.Join(dir, "original.csv")
	writeFile(t, source, body)
	r, err := a.ImportCSV(source, false, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r["valid_rows"] != int64(3) || r["duplicate_rows"] != int64(1) || r["zero_excluded_rows"] != int64(1) || r["non_wifi_rows"] != int64(1) {
		t.Fatal(r)
	}
	if len(table(t, a, "SELECT * FROM observation_provenance")) != 4 {
		t.Fatal("duplicate provenance missing")
	}
	if p, err := a.DuplicatePreview(ctx); err != nil || p.Rows != 0 {
		t.Fatalf("distinct extras deduplicated: %v %v", p, err)
	}
	// Original bytes remain available after the user's input disappears.
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	verify := func(store *Atlas, name string) {
		t.Helper()
		dest := filepath.Join(dir, name)
		if err := store.ExportOriginalSource(ctx, 1, dest); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(dest)
		if string(b) != body {
			t.Fatal("original bytes changed")
		}
	}
	verify(a, "restored.csv")
	workspace := filepath.Join(dir, "snapshot")
	if err := a.CopyWorkspace(ctx, workspace, nil); err != nil {
		t.Fatal(err)
	}
	b, err := Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	verify(b, "workspace-original.csv")
	b.Close()
	portable := filepath.Join(dir, "portable.sqlite3")
	if err := a.ExportSQLite(portable, nil); err != nil {
		t.Fatal(err)
	}
	b, err = Open(filepath.Join(dir, "converted"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.OpenDatabase(portable, nil, nil); err != nil {
		t.Fatal(err)
	}
	verify(b, "sqlite-original.csv")
	for _, tableName := range preservationTables {
		if !reflect.DeepEqual(table(t, a, "SELECT * FROM "+tableName+" ORDER BY 1,2"), table(t, b, "SELECT * FROM "+tableName+" ORDER BY 1,2")) {
			t.Fatalf("%s did not round trip", tableName)
		}
	}
	out := filepath.Join(dir, "survey.csv")
	if _, _, err := a.ExportSurveyCSV(ctx, out, "test", false, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 5 {
		t.Fatalf("expected preamble/header/three sightings: %d", len(records))
	}
	c, err := Open(filepath.Join(dir, "csv-roundtrip"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ImportCSV(out, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(table(t, a, "SELECT fields_json FROM observation_details ORDER BY observation_id"), table(t, c, "SELECT fields_json FROM observation_details ORDER BY observation_id")) {
		t.Fatal("CSV lost extra fields")
	}
	// Import scans from portable SQLite must retain structured fields as well.
	adapter := filepath.Join(dir, "adapted.csv")
	if err := DatabaseImportCSV(ctx, portable, adapter); err != nil {
		t.Fatal(err)
	}
	d, err := Open(filepath.Join(dir, "scans"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ImportCSVWithSource(adapter, portable, false, nil, "portable.sqlite3", nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(table(t, a, "SELECT fields_json FROM observation_details ORDER BY observation_id"), table(t, d, "SELECT fields_json FROM observation_details ORDER BY observation_id")) {
		t.Fatal("SQLite scan adapter lost extra fields")
	}
}

func TestPreservationMigrationAndArchiveCancellation(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "survey")
	a, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	source := filepath.Join(dir, "input.csv")
	writeFile(t, source, "MAC,SSID,RSSI,CurrentLatitude,CurrentLongitude,AuthMode\nAA,test,-60,49,-123,WPA3\n")
	completed := 0
	_, err = a.ImportCSV(source, false, func(v, i, read, total int64) error {
		if read == total {
			completed++
			if completed == 2 {
				return context.Canceled
			}
		}
		return nil
	}, "", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected archive-stage cancellation: %v", err)
	}
	if a.Obs.State().N != 0 || len(table(t, a, "SELECT * FROM imports")) != 0 || len(table(t, a, "SELECT * FROM source_files")) != 0 || len(table(t, a, "SELECT * FROM observation_provenance")) != 0 {
		t.Fatal("cancelled archive left committed data")
	}
	if _, err := a.ImportCSV(source, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-preservation workspace without fabricating lost content.
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range preservationTables {
		if _, err := db.Exec(c, "DROP TABLE "+name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(c, "PRAGMA user_version=5"); err != nil {
		t.Fatal(err)
	}
	put()
	a.Close()
	a, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(table(t, a, "SELECT * FROM import_sources")) != 0 {
		t.Fatal("migration invented an original")
	}
	if _, err := a.ImportCSV(source, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if a.Obs.State().N != 1 || len(table(t, a, "SELECT * FROM import_sources")) != 1 {
		t.Fatal("legacy reimport failed to archive original")
	}
	if len(table(t, a, "SELECT * FROM observation_details")) != 0 {
		t.Fatal("legacy details unexpectedly fabricated")
	}
}

func TestMaintenanceTransfersDuplicateProvenance(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	source := filepath.Join(dir, "input.csv")
	writeFile(t, source, "MAC,SSID,RSSI,CurrentLatitude,CurrentLongitude,Node\nAA,test,-60,49,-123,rig\n")
	if _, err := a.ImportCSV(source, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	st := a.Obs.State()
	legacy := st.Segs[0].Range(0, 1)
	legacy.Base = st.MaxID + 1
	if err := a.Obs.Commit(obs.Change{Add: []*obs.Segment{legacy}}); err != nil {
		t.Fatal(err)
	}
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"UPDATE imports SET active_rows=2,valid_rows=2", "INSERT INTO observation_details SELECT 2,fields_json FROM observation_details WHERE observation_id=1", "INSERT INTO observation_provenance VALUES(1,3,'legacy',2,'scans','device')"} {
		if _, err := db.Exec(c, q); err != nil {
			t.Fatal(err)
		}
	}
	put()
	b, _, err := a.PrepareDeduplication(context.Background(), filepath.Join(dir, "cleaned"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Obs.State().N != 1 {
		t.Fatal("duplicate not removed")
	}
	r := table(t, b, "SELECT observation_id,source_table,source_record_source FROM observation_provenance WHERE line=3")
	if len(r) != 1 || db.Int(r[0]["observation_id"]) != 1 || r[0]["source_table"] != "scans" || r[0]["source_record_source"] != "device" {
		t.Fatal(r)
	}
	if len(table(t, b, "SELECT * FROM observation_details")) != 1 {
		t.Fatal("removed ID still has details")
	}
}

func TestPreservationFailureCancellationAndChunks(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	header := "MAC,SSID,RSSI,CurrentLatitude,CurrentLongitude,Type,Vendor\n"
	path := filepath.Join(dir, "source.csv")
	body := header + strings.Repeat("AA,test,-60,49,-123,WIFI,"+strings.Repeat("x", 100000)+"\n", 11)
	writeFile(t, path, body)
	if _, err := a.ImportCSV(path, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if len(table(t, a, "SELECT chunk FROM source_chunks")) != 2 {
		t.Fatal("source not chunked")
	}
	bad := filepath.Join(dir, "bad.csv")
	writeFile(t, bad, header+"BB,test,-60,49,-123,WIFI,ok\nBB,test,bad,49,-123,WIFI,no\n")
	if _, err := a.ImportCSV(bad, false, nil, "", nil); err == nil {
		t.Fatal("expected validation failure")
	}
	if len(table(t, a, "SELECT * FROM import_sources")) != 1 || len(table(t, a, "SELECT * FROM observation_provenance")) != 11 {
		t.Fatal("failed import published preservation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := filepath.Join(dir, "cancel.csv")
	if err := a.ExportOriginalSource(ctx, 1, dest); err == nil {
		t.Fatal("cancellation ignored")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("partial source left behind")
	}
	dest = filepath.Join(dir, "existing.csv")
	writeFile(t, dest, "keep")
	if err := a.ExportOriginalSource(context.Background(), 1, dest); err == nil {
		t.Fatal("existing file overwritten")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "keep" {
		t.Fatal("existing destination changed")
	}
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(c, "UPDATE source_chunks SET payload=x'00' WHERE chunk=0")
	put()
	if err != nil {
		t.Fatal(err)
	}
	dest = filepath.Join(dir, "corrupt.csv")
	if err := a.ExportOriginalSource(context.Background(), 1, dest); err == nil {
		t.Fatal("corrupt source exported")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("corrupt output left behind")
	}
}

func TestWiGLEMillisecondsAndOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wigle.sqlite")
	c, err := db.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Script(c, `CREATE TABLE network(bssid TEXT,ssid TEXT,capabilities TEXT,frequency INTEGER,type TEXT);
CREATE TABLE location(_id INTEGER PRIMARY KEY,bssid TEXT,level INTEGER,lat REAL,lon REAL,altitude REAL,accuracy REAL,time INTEGER);
INSERT INTO network VALUES('AA','test','WPA3',2412,'W');
INSERT INTO location VALUES(1,'AA',-60,49,-123,12.5,3,1767355200123),(2,'AA',-60,49,-123,12.5,3,1767355200456);`)
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
	if err != nil {
		t.Fatal(err)
	}
	if r["valid_rows"] != int64(2) {
		t.Fatal("subsecond sightings collapsed", r)
	}
	st := a.Obs.State()
	if !strings.HasSuffix(st.Str[st.Segs[0].First[0]], ".123") {
		t.Fatal("milliseconds lost")
	}
	out := filepath.Join(dir, "restored.sqlite")
	if err := a.ExportOriginalSource(context.Background(), 1, out); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(path)
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, want) {
		t.Fatal("WiGLE backup not exact")
	}
}
