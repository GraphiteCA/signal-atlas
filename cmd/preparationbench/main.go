// preparationbench measures bounded background work on disposable owned surveys.
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
	"sort"
	"sync"
	"time"

	"signal-atlas/internal/db"
	"signal-atlas/internal/engine"
)

type row = map[string]any

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func owned(path string) bool {
	for range 4 {
		b, e := os.ReadFile(filepath.Join(path, "owner.json"))
		var m struct {
			Purpose string
			Schema  int
		}
		if e == nil && json.Unmarshal(b, &m) == nil && m.Purpose == "signal-atlas-benchmark" && m.Schema == 1 {
			return true
		}
		path = filepath.Dir(path)
	}
	return false
}
func distribution(xs []float64) row {
	sort.Float64s(xs)
	if len(xs) == 0 {
		return row{"count": 0}
	}
	return row{"count": len(xs), "p50_ms": xs[len(xs)/2], "p95_ms": xs[(len(xs)*95-1)/100], "max_ms": xs[len(xs)-1]}
}
func main() {
	data := flag.String("data", "", "Owned disposable engine data directory")
	requests := flag.Int("requests", 60, "Map traces during preparation")
	flag.Parse()
	root, err := filepath.EvalSymlinks(*data)
	must(err)
	root, err = filepath.Abs(root)
	must(err)
	if !owned(root) {
		panic("owned fixture required")
	}
	runtime.GOMAXPROCS(8)
	start := time.Now()
	s, err := engine.New(engine.Options{Data: root, Version: "dev"})
	must(err)
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	s.AllowHost("bench.local")
	report := row{"schema": 1, "open_seconds": time.Since(start).Seconds(), "data": root, "notes": "Eight scheduler slots, default reference/sample/shared preparation and two workers. Map requests every 150 ms; background yields to map work. Filesystem cache uncontrolled. Native delivery measured separately."}
	call := func(method, path string, body any) row {
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(method, "http://bench.local"+path, bytes.NewReader(b)))
		if w.Code != 200 && w.Code != 202 {
			panic(fmt.Sprintf("%s: %d %s", path, w.Code, w.Body.String()))
		}
		var v row
		must(json.Unmarshal(w.Body.Bytes(), &v))
		return v
	}
	settings := func(paused bool) { call("POST", "/api/app/preparation/settings", row{"paused": paused}) }
	stats := func() row {
		c, put, e := s.Atlas.Conn(nil)
		must(e)
		defer put()
		v, e := db.One(c, "SELECT count(*) completed,coalesce(sum(fitted),0) fitted,coalesce(sum(unavailable),0) unavailable FROM preparation_checkpoints")
		must(e)
		return v
	}
	settings(true)
	path := "/api/app/map/features?bbox=-123.401,48.399,-123.390,48.410&zoom=15&min_rssi=-150"
	t := time.Now()
	call("GET", path, nil)
	report["cold_map_seconds"] = time.Since(t).Seconds()
	call("GET", "/api/app/map/summary", nil)
	var mu sync.Mutex
	var deliveries []float64
	s.SetJobEventSink(engine.JobEventSinkFunc(func(e engine.JobEvent) {
		if e.CommittedAtMS > 0 {
			mu.Lock()
			if len(deliveries) < 4096 {
				deliveries = append(deliveries, float64(time.Now().UnixMilli()-e.CommittedAtMS))
			}
			mu.Unlock()
		}
	}))
	settings(false)
	time.Sleep(2 * time.Second)
	report["before_map_trace"] = stats()
	var traces []float64
	for i := 0; i < *requests; i++ {
		t = time.Now()
		call("GET", path, nil)
		traces = append(traces, float64(time.Since(t))/float64(time.Millisecond))
		time.Sleep(150 * time.Millisecond)
	}
	report["active_map_trace"] = distribution(traces)
	report["after_map_trace"] = stats()
	t = time.Now()
	settings(true)
	report["pause_join_ms"] = float64(time.Since(t)) / float64(time.Millisecond)
	before := stats()
	time.Sleep(300 * time.Millisecond)
	after := stats()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		panic("work committed after pause joined")
	}
	report["paused_checkpoints"] = after
	// Force committed changes to a saved synthetic location before each read.
	// This separates incremental-update cost from unchanged warm viewport hits.
	c, put, err := s.Atlas.Conn(nil)
	must(err)
	fit, err := db.One(c, `SELECT radio_key,scope_key,result FROM estimates WHERE json_extract(result,'$.status')='fitted' AND scope_key LIKE '%sample%' AND scope_key NOT LIKE '%solver%' LIMIT 1`)
	must(err)
	if fit == nil {
		panic("synthetic saved fit required")
	}
	var churn, counts []float64
	for i := 0; i < 60; i++ {
		_, err = db.Exec(c, "UPDATE estimates SET result=json_set(result,'$.latitude',48.4001+?) WHERE radio_key=? AND scope_key=?", float64(i%2)*0.000001, fit["radio_key"], fit["scope_key"])
		must(err)
		t = time.Now()
		call("GET", path, nil)
		churn = append(churn, float64(time.Since(t))/float64(time.Millisecond))
		t = time.Now()
		call("GET", "/api/app/map/summary", nil)
		counts = append(counts, float64(time.Since(t))/float64(time.Millisecond))
	}
	_, err = db.Exec(c, "UPDATE estimates SET result=? WHERE radio_key=? AND scope_key=?", fit["result"], fit["radio_key"], fit["scope_key"])
	must(err)
	put()
	report["committed_fit_update_map"] = distribution(churn)
	report["committed_fit_update_summary"] = distribution(counts)
	mu.Lock()
	report["in_process_sink_delivery"] = distribution(deliveries)
	mu.Unlock()
	snapshot := filepath.Join(root, "prepared-snapshot.atlas")
	t = time.Now()
	op := call("POST", "/api/app/admin/snapshot", row{"path": snapshot, "format": "workspace"})
	for op["status"] == "running" {
		time.Sleep(10 * time.Millisecond)
		op = call("GET", "/api/app/admin/operation", nil)
	}
	if op["status"] != "complete" {
		panic(fmt.Sprint(op))
	}
	report["checkpoint_snapshot_seconds"] = time.Since(t).Seconds()
	s.Close()
	s = nil
	t = time.Now()
	s, err = engine.New(engine.Options{Data: root, Version: "dev"})
	must(err)
	s.AllowHost("bench.local")
	report["partial_reopen_seconds"] = time.Since(t).Seconds()
	if fmt.Sprint(stats()) != fmt.Sprint(after) {
		panic("restart changed checkpoints")
	}
	state := call("GET", "/api/app/preparation/status", nil)
	if !state["settings"].(map[string]any)["paused"].(bool) {
		panic("restart lost pause preference")
	}
	settings(false)
	time.Sleep(2 * time.Second)
	settings(true)
	report["after_resume"] = stats()
	report["final_preparation"] = call("GET", "/api/app/preparation/status", nil)
	must(json.NewEncoder(os.Stdout).Encode(report))
}
