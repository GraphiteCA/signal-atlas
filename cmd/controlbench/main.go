// controlbench measures incremental imports, concurrent maps and cancellation
// on a fresh benchmark-only copy of an input CSV.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/engine"
	"signal-atlas/internal/py"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func main() {
	runtime.GOMAXPROCS(8)
	csv := flag.String("csv", "", "initial CSV")
	dir := flag.String("data", "", "fresh benchmark directory")
	flag.Parse()
	if *csv == "" || *dir == "" {
		panic("CSV and data required")
	}
	if _, e := os.Stat(*dir); !os.IsNotExist(e) {
		panic("directory must be new")
	}
	s, e := engine.New(engine.Options{Data: *dir, Version: "dev", Hosts: []string{"benchmark.local"}})
	must(e)
	defer s.Close()
	s.Jobs.Threads = 8
	a := s.Atlas
	initial, e := a.ImportCSV(*csv, true, nil, "", nil)
	must(e)
	groups, e := a.SelectedGroups(py.Params{})
	must(e)
	selected := []atlas.Group{}
	for _, g := range groups {
		if g["ssid"] != "" && db.Int(g["count"]) >= 4 {
			selected = append(selected, g)
			if len(selected) == 32 {
				break
			}
		}
	}
	p := py.Params{"solver": {"fast"}, "individual": {"1"}, "shared": {"0"}, "fit_mode": {"sample"}}
	selected = atlas.EstimationGroups(selected, p)
	_, scope, e := atlas.EstimateScope(p)
	must(e)
	c, put, e := a.Conn(nil)
	must(e)
	for _, g := range selected {
		_, e = a.FitAndStore(c, g, p, scope, nil)
		must(e)
	}
	before, e := db.All(c, "SELECT radio_key,result FROM estimates ORDER BY radio_key")
	must(e)
	put()
	changed := selected[0]
	impMS := []float64{}
	for i := range 25 {
		file := filepath.Join(*dir, fmt.Sprintf("small-%02d.csv", i))
		var text strings.Builder
		text.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,Type\n")
		for k := range 8 {
			fmt.Fprintf(&text, "%s,%s,2026-11-10 12:%02d:%02d,2412,%d,%.6f,%.6f,WIFI\n", changed["mac"], changed["ssid"], i, k, -35-k, 48.4+float64(k)*.00001, -123.4+float64(i)*.00001)
		}
		must(os.WriteFile(file, []byte(text.String()), 0644))
		t := time.Now()
		_, e = a.ImportCSV(file, true, nil, "", nil)
		must(e)
		impMS = append(impMS, float64(time.Since(t).Microseconds())/1000)
	}
	c, put, e = a.Conn(nil)
	must(e)
	after, e := db.All(c, "SELECT radio_key,result FROM estimates ORDER BY radio_key")
	must(e)
	put()
	retained := map[string]string{}
	for _, r := range after {
		retained[db.Str(r["radio_key"])] = db.Str(r["result"])
	}
	for _, r := range before {
		if r["radio_key"] == changed["radio_key"] {
			if _, ok := retained[db.Str(r["radio_key"])]; ok {
				panic("changed fit was not invalidated")
			}
		} else if retained[db.Str(r["radio_key"])] != r["result"] {
			panic("untouched fit changed")
		}
	}
	if len(after) != len(before)-1 {
		panic("selective invalidation count mismatch")
	}
	_, e = s.Jobs.FitAll(p)
	must(e)
	traces := []float64{}
	for i := range 200 {
		path := "/api/app/map/fits?solver=fast&bbox=-123.401,48.399,-123.390,48.410"
		if i%2 == 0 {
			path = "/api/points?bbox=-123.401,48.399,-123.390,48.410"
		}
		r := httptest.NewRequest("GET", "http://benchmark.local"+path, nil)
		w := httptest.NewRecorder()
		t := time.Now()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			panic(w.Body.String())
		}
		traces = append(traces, float64(time.Since(t).Microseconds())/1000)
	}
	// Ensure cancellation measures active work, rather than an already-finished
	// short fixture job. A new scope requires work without destroying old fits.
	if !s.Jobs.Running() {
		p = p.With("min_rssi", []string{"-149"})
		_, e = s.Jobs.FitAll(p)
		must(e)
		started := time.Now()
		for s.Jobs.Running() && db.Int(s.Jobs.Snapshot()["fitted"]) == 0 && time.Since(started) < 5*time.Second {
			time.Sleep(5 * time.Millisecond)
		}
	}
	beforeCancel := s.Jobs.Snapshot()
	if beforeCancel["status"] != "running" {
		panic("cancellation fixture finished before measurement")
	}
	t := time.Now()
	s.Jobs.Cancel()
	ack := time.Since(t).Seconds() * 1000
	for s.Jobs.Running() {
		if time.Since(t) > 10*time.Second {
			panic("cancel did not join within 10 s")
		}
		time.Sleep(time.Millisecond)
	}
	joined := time.Since(t).Seconds() * 1000
	cancelled := s.Jobs.Snapshot()
	c, put, e = a.Conn(nil)
	must(e)
	cached, e := db.Scalar(c, "SELECT count(*) FROM estimates")
	must(e)
	put()
	// A restarted job must reuse the committed outcomes rather than erase them.
	_, e = s.Jobs.FitAll(p)
	must(e)
	resumeStart := time.Now()
	for s.Jobs.Running() && time.Since(resumeStart) < 3*time.Second {
		if db.Int(s.Jobs.Snapshot()["cached"]) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.Jobs.Cancel()
	for s.Jobs.Running() {
		time.Sleep(time.Millisecond)
	}
	resume := s.Jobs.Snapshot()
	if db.Int(resume["cached"]) < 1 {
		panic("resumed job did not report reusing any committed fits")
	}
	sort.Float64s(impMS)
	sorted := append([]float64(nil), traces...)
	sort.Float64s(sorted)
	out := map[string]any{"schema": 1, "initial_import": initial, "small_imports": 25, "new_rows": 200, "small_import_p50_ms": impMS[len(impMS)/2], "small_import_p95_ms": impMS[len(impMS)*95/100], "cached_before": len(before), "cached_after_import": len(after), "untouched_fits_identical": true, "concurrent_viewport_ms": traces, "concurrent_viewport_p95_ms": sorted[len(sorted)*95/100], "cancel_ack_ms": ack, "cancel_join_ms": joined, "before_cancel": beforeCancel, "cancelled": cancelled, "committed_cache_after_cancel": cached, "resume": resume}
	must(json.NewEncoder(os.Stdout).Encode(out))
}
