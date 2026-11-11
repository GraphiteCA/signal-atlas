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
	s, err := New(Options{Data: filepath.Join(dir, "data"), Version: "dev", UI: os.DirFS("../../ui"), Explorer: os.DirFS("../../python/templates")})
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
	for _, path := range []string{"/map", "/app/map/map.js", "/app/map/display.js", "/app/vendor/maplibre/maplibre-gl.js", "/app/vendor/deck/deck.gl.js", "/app/vendor/leaflet/leaflet.js", "/atlas/"} {
		if r := request("GET", path, ""); r.Code != 200 {
			t.Fatalf("asset %s: %d", path, r.Code)
		}
	}
	if r := request("GET", "/atlas/", ""); bytes.Contains(r.Body.Bytes(), []byte("https://unpkg.com/leaflet")) {
		t.Fatal("reference fallback still needs CDN")
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
