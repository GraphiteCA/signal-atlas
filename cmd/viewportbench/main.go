// viewportbench times real local-location saves after panning during preparation.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/engine"
	"strings"
	"time"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func owned(path string) bool {
	for range 6 {
		b, e := os.ReadFile(filepath.Join(path, "owner.json"))
		var m map[string]any
		if e == nil && json.Unmarshal(b, &m) == nil && m["purpose"] == "signal-atlas-benchmark" && m["schema"] == float64(1) {
			return true
		}
		path = filepath.Dir(path)
	}
	return false
}
func main() {
	data := flag.String("data", "", "owned disposable data")
	build := flag.Bool("build", false, "create new synthetic fixture, without preparation")
	flag.Parse()
	runtime.GOMAXPROCS(8)
	root, e := filepath.Abs(*data)
	must(e)
	if !owned(root) {
		panic("owned data required")
	}
	if *build {
		if _, e := os.Stat(filepath.Join(root, "store")); !os.IsNotExist(e) {
			panic("new store required")
		}
		a, e := atlas.Open(filepath.Join(root, "store"))
		must(e)
		defer a.Close()
		var csv strings.Builder
		csv.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n")
		for n := 0; n < 10064; n++ {
			name := fmt.Sprintf("A Far %05d", n)
			lat, lon := 49.4, -124.4
			if n >= 10000 {
				name = fmt.Sprintf("Z Local %05d", n)
				lat, lon = 48.45, -123.45
			}
			for i := 0; i < 8; i++ {
				fmt.Fprintf(&csv, "02:72:00:00:%02X:%02X,%s,2026-11-01,2412,%d,%.7f,%.7f,3,WIFI\n", n/256, n%256, name, -50-i, lat+float64(i%3)*.00001, lon+float64(i%2)*.00001)
			}
		}
		file := filepath.Join(root, "fixture.csv")
		must(os.WriteFile(file, []byte(csv.String()), 0600))
		_, e = a.ImportCSV(file, false, nil, "", nil)
		must(e)
		json.NewEncoder(os.Stdout).Encode(map[string]any{"rows": a.Obs.State().N, "far_targets": 10000, "local_targets": 64})
		return
	}
	s, e := engine.New(engine.Options{Data: root, Version: "bench", Hosts: []string{"bench.local"}})
	must(e)
	defer s.Close()
	call := func(method, path string, body any) map[string]any {
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(method, "http://bench.local"+path, bytes.NewReader(b)))
		if w.Code != 200 {
			panic(w.Body.String())
		}
		var v map[string]any
		must(json.Unmarshal(w.Body.Bytes(), &v))
		return v
	}
	call("POST", "/api/app/preparation/settings", map[string]any{"enabled": true, "paused": false, "workers": 8, "memory_mb": 256, "grouping": "shared", "fit_mode": "sample"})
	call("POST", "/api/app/settings", map[string]any{"fast_fits": true, "gpu_fits": true})
	counts := func() (int64, int64) {
		c, put, e := s.Atlas.Conn(nil)
		must(e)
		defer put()
		r, e := db.One(c, `SELECT count(*) total,sum(CASE WHEN target_key LIKE 'n:Z Local %' THEN 1 ELSE 0 END) local FROM preparation_checkpoints`)
		must(e)
		return db.Int(r["total"]), db.Int(r["local"])
	}
	deadline := time.Now().Add(60 * time.Second)
	var before int64
	for {
		before, _ = counts()
		if before >= 100 {
			break
		}
		if time.Now().After(deadline) {
			panic("preparation did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	call("GET", "/api/app/map/features?bbox=-123.46,48.44,-123.44,48.46&zoom=14&clustering=1&priority_client=benchmark&priority_seq=1", nil)
	first := 0.0
	var total, local int64
	for {
		total, local = counts()
		if local > 0 && first == 0 {
			first = time.Since(start).Seconds()
		}
		if local == 64 {
			break
		}
		if time.Now().After(deadline) {
			panic("local preparation did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	all := time.Since(start).Seconds()
	call("POST", "/api/app/preparation/settings", map[string]any{"paused": true})
	page := call("GET", "/api/app/map/features?bbox=-123.46,48.44,-123.44,48.46&zoom=14&clustering=1", nil)
	if page["total"] != float64(64) {
		panic(fmt.Sprintf("local results not rendered: %v", page["total"]))
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"first_local_seconds": first, "all_64_local_seconds": all, "completed_at_pan": before, "completed_when_local_ready": total, "local_map_aps": page["total"], "progress_after_pan": total - before})
}
