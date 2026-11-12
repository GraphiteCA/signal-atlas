package engine

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"signal-atlas/internal/db"
)

func TestMapAPIRoutingPreferencesAndPointCache(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Options{DisablePreparation: true, Data: filepath.Join(dir, "data"), Version: "dev", UI: os.DirFS("../../ui")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AllowHost("test.local")
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://test.local"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/map", "/app/map/map.js", "/app/map/display.js", "/app/vendor/maplibre/maplibre-gl.js", "/app/vendor/deck/deck.gl.js", "/app/vendor/leaflet/leaflet.js"} {
		if r := request("GET", path, ""); r.Code != 200 {
			t.Fatalf("asset %s: %d", path, r.Code)
		}
	}
	for _, path := range []string{"/atlas", "/atlas/", "/explorer.js", "/explorer.css", "/satellite.js", "/network_tree.js", "/network_tree.css", "/observation_scope.js", "/estimate_comparison.js", "/online_map.js", "/follow_estimate.js", "/area_estimates.js"} {
		if r := request("GET", path, ""); r.Code != 404 {
			t.Fatalf("retired observation analysis asset %s: %d", path, r.Code)
		}
	}
	for _, body := range []string{`{"radius_m":51}`, `{"radius_m":4}`, `{"label_min_zoom":23}`, `{"label_min_zoom":1}`, `{"radius_m":10.5}`} {
		if r := request("POST", "/api/app/map/display", body); r.Code != 400 {
			t.Fatalf("invalid preference %s: %d", body, r.Code)
		}
	}
	body := `{"clustering":false,"label_min_zoom":2,"radius_m":50,"rings":false}`
	if r := request("POST", "/api/app/map/display", body); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	var saved map[string]any
	json.Unmarshal(request("GET", "/api/app/map/display", "").Body.Bytes(), &saved)
	if saved["radius_m"] != float64(50) || saved["clustering"] != false {
		t.Fatal(saved)
	}
	importCSV := func(name, ssid string) {
		t.Helper()
		file := filepath.Join(dir, name)
		text := "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n02:51:00:00:00:01," + ssid + ",2026-05-01 10:00:00,2437,-65,48.4,-123.4,4,WIFI\n"
		if err := os.WriteFile(file, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Atlas.ImportCSV(file, false, nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	importCSV("first.csv", "First")
	path := "/api/app/map/points?bbox=-124,48,-123,49&max=500000"
	first := request("GET", path, "")
	if first.Code != 200 || first.Header().Get("X-Total") != "1" || len(first.Body.Bytes()) != 12 {
		t.Fatalf("packed points: %d %v", first.Code, first.Header())
	}
	cache := &s.mapPoints.data[0]
	second := request("GET", path, "")
	if &s.mapPoints.data[0] != cache || !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("packed buffers not reused")
	}
	importCSV("second.csv", "Second")
	third := request("GET", path, "")
	if third.Code != 200 || third.Header().Get("X-Total") != "2" || len(third.Body.Bytes()) != 24 {
		t.Fatal("stale observation cache")
	}
	importCSV("hidden.csv", "")
	for _, tc := range []struct{ query, total string }{
		{"", "3"},
		{"&ssid_query=first", "1"},
		{"&ssid_query=missing", "0"},
		{"&hide_hidden=1", "2"},
		{"&hide_hidden=1&ssid_query=SECOND", "1"},
		{"&hide_hidden=0", "3"},
	} {
		r := request("GET", path+tc.query, "")
		if r.Code != 200 || r.Header().Get("X-Total") != tc.total {
			t.Fatalf("filtered sightings %s: %d %v", tc.query, r.Code, r.Header())
		}
	}
	if r := request("GET", "/api/app/map/features?zoom=NaN", ""); r.Code != 400 {
		t.Fatal("invalid zoom accepted")
	}
	c, put, _ := s.Atlas.Conn(nil)
	before, _ := db.Scalar(c, "SELECT value FROM map_revision")
	request("POST", "/api/app/map/display", `{"radius_m":10}`)
	after, _ := db.Scalar(c, "SELECT value FROM map_revision")
	put()
	if before != after {
		t.Fatal("preference changed fit revision")
	}
	r := httptest.NewRequest("POST", "http://untrusted.local/api/app/map/display", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("map preferences bypassed host guard")
	}
}
