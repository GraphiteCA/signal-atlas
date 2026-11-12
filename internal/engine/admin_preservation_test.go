package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"signal-atlas/internal/db"
)

func TestAdminOriginalSourceExport(t *testing.T) {
	s := adminServer(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "input.csv")
	body := "MAC,SSID,RSSI,CurrentLatitude,CurrentLongitude,AuthMode,Node\r\nAA,test,-60,49,-123,WPA3,logger\r\n"
	if err := os.WriteFile(source, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	requireComplete(t, adminAction(t, s, "import", map[string]any{"paths": []string{source}}))
	history := adminRequest(t, s, "GET", "/api/app/admin/imports", nil, "")
	if history.Code != 200 || !strings.Contains(history.Body.String(), "source_sha256") || !strings.Contains(history.Body.String(), "source_bytes") {
		t.Fatal(history.Body.String())
	}
	dest := filepath.Join(dir, "restored.csv")
	requireComplete(t, adminAction(t, s, "export-source", map[string]any{"import_id": 1, "path": dest}))
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != body {
		t.Fatalf("%q %v", got, err)
	}
	for _, path := range []string{dest, filepath.Join(s.Atlas.Root, "bad.csv")} {
		if r := adminAction(t, s, "export-source", map[string]any{"import_id": 1, "path": path}); r["status"] != "failed" {
			t.Fatal(r)
		}
	}
	got, _ = os.ReadFile(dest)
	if string(got) != body {
		t.Fatal("existing source changed")
	}
}

func TestWiGLEPreservesDownloadedContainer(t *testing.T) {
	s := adminServer(t)
	var compressed bytes.Buffer
	z := gzip.NewWriter(&compressed)
	z.Write([]byte("MAC,SSID,RSSI,CurrentLatitude,CurrentLongitude,Node\nAA,test,-60,49,-123,rig\n"))
	z.Close()
	c := testWiGLEClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/file/csv/") {
			w.Write(compressed.Bytes())
			return
		}
		if r.URL.Query().Get("pagestart") == "0" {
			fmt.Fprint(w, `{"success":true,"results":[{"transid":"a","fileName":"drive.csv.gz"}]}`)
		} else {
			fmt.Fprint(w, `{"success":true,"results":[]}`)
		}
	})
	if _, err := s.admin.start("wigle", func(ctx context.Context) (any, error) { return s.adminWiGLE(ctx, "secret", false, c) }); err != nil {
		t.Fatal(err)
	}
	s.admin.mu.Lock()
	done := s.admin.done
	s.admin.mu.Unlock()
	<-done
	requireComplete(t, s.admin.snapshot())
	dest := filepath.Join(t.TempDir(), "original.gz")
	if err := s.Atlas.ExportOriginalSource(context.Background(), 1, dest); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, compressed.Bytes()) {
		t.Fatal("download container not preserved")
	}
}

func TestLoggerAdapterRetainsVendorAndNode(t *testing.T) {
	s := adminServer(t)
	path := filepath.Join(t.TempDir(), "logger.sqlite")
	c, err := db.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Script(c, `CREATE TABLE scans(id INTEGER,MAC TEXT,SSID TEXT,RSSI REAL,CurrentLatitude REAL,CurrentLongitude REAL,Node TEXT,AuthMode TEXT,Vendor TEXT);
INSERT INTO scans VALUES(1,'AA','test',-60,49,-123,'one','WPA3','custom'),(2,'AA','test',-60,49,-123,'two','WPA3','custom'),(3,'AA','test',-60,49,-123,'one','WPA3','custom');`)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	requireComplete(t, adminAction(t, s, "import", map[string]any{"paths": []string{path}}))
	if s.Atlas.Obs.State().N != 2 {
		t.Fatal("node identities collapsed")
	}
	c, put, err := s.Atlas.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.All(c, "SELECT source_table,record_id FROM observation_provenance ORDER BY line")
	put()
	if err != nil || len(rows) != 3 || rows[0]["source_table"] != "scans" || rows[2]["record_id"] != "3" {
		t.Fatalf("%v %v", rows, err)
	}
	out := filepath.Join(t.TempDir(), "logger.sqlite")
	if err := s.Atlas.ExportOriginalSource(context.Background(), 1, out); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(path)
	got, _ := os.ReadFile(out)
	if !bytes.Equal(want, got) {
		t.Fatal("archived adapter CSV instead of original database")
	}
}
