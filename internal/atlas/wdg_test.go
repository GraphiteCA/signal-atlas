package atlas

import (
	"path/filepath"
	"strings"
	"testing"
)

const wdgHeader = "mac,ssid,auth_mode,channel,frequency,rssi,latitude,longitude,first_seen,captured_at,last_scan_at,scan_count\n"

func TestWDGImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wdg.csv")
	writeFile(t, path, wdgHeader+"02:00:00:00:00:01,\"Office, West\",[WPA2_PSK],1,,-83,28.6,-80.8,2026-04-28 12:51:02,2026-04-28 15:19:13+00,,8\n"+
		"02:00:00:00:00:02,,[WPA2_PSK],36,5180,-65,28.7,-80.9,2026-04-28 12:52:02,,,1\n")
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var messages []string
	r, err := a.ImportCSV(path, false, nil, "", func(s string) { messages = append(messages, s) })
	if err != nil {
		t.Fatal(err)
	}
	if r["valid_rows"] != int64(2) || r["format_message"] != "WDG formatting recognized" || !strings.Contains(strings.Join(messages, "\n"), "WDG formatting recognized") {
		t.Fatalf("unexpected result: %v, messages: %v", r, messages)
	}
	var h csvHeader
	if _, err := h.take(strings.Split(strings.TrimSpace(wdgHeader), ",")); err != nil {
		t.Fatal(err)
	}
	o, err := ParseObservation(h.fields([]string{"02:00:00:00:00:01", "Office, West", "[WPA2_PSK]", "1", "", "-83", "28.6", "-80.8", "2026-04-28 12:51:02"}), 2)
	if err != nil {
		t.Fatal(err)
	}
	if o.SSID != "Office, West" || o.Latitude != 28.6 || o.Longitude != -80.8 || o.FirstSeen != "2026-04-28 12:51:02" || o.Channel != "1" || o.Band != "unknown" {
		t.Fatalf("incorrect WDG mapping: %+v", o)
	}
	var ordinary csvHeader
	if _, err := ordinary.take([]string{"mac", "ssid", "rssi", "latitude", "longitude"}); err == nil || ordinary.wdg {
		t.Fatal("unrecognized headers should retain normal validation")
	}
}
