// mapbench measures Phase 1 against a previously owned Phase 0 rendering fixture.
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
	"time"

	"signal-atlas/internal/engine"
)

func owned(path string) bool {
	for range 4 {
		b, err := os.ReadFile(filepath.Join(path, "owner.json"))
		var v struct {
			Purpose string `json:"purpose"`
			Schema  int    `json:"schema"`
		}
		if err == nil && json.Unmarshal(b, &v) == nil && v.Purpose == "signal-atlas-benchmark" && v.Schema == 1 {
			return true
		}
		path = filepath.Dir(path)
	}
	return false
}
func main() {
	data := flag.String("data", "", "Owned Phase 0 fixture data directory")
	count := flag.Int("requests", 200, "Warm requests per trace")
	flag.Parse()
	path, err := filepath.EvalSymlinks(*data)
	must(err)
	path, err = filepath.Abs(path)
	must(err)
	if !owned(path) {
		panic("mapbench requires an owned benchmark fixture")
	}
	runtime.GOMAXPROCS(8)
	s, err := engine.New(engine.Options{Data: path, Version: "dev"})
	must(err)
	defer s.Close()
	s.AllowHost("bench.local")
	request := func(url string) ([]byte, float64) {
		r := httptest.NewRequest("GET", "http://bench.local"+url, nil)
		w := httptest.NewRecorder()
		start := time.Now()
		s.ServeHTTP(w, r)
		ms := float64(time.Since(start)) / float64(time.Millisecond)
		if w.Code != 200 {
			panic(fmt.Sprintf("%s: %d %s", url, w.Code, w.Body.String()))
		}
		return w.Body.Bytes(), ms
	}
	base := "bbox=-123.401,48.399,-123.390,48.410&zoom=15&min_rssi=-150"
	result := map[string]any{"schema": 1, "fixture": path, "requests": *count, "threads": 8, "notes": "Fresh process; filesystem cache uncontrolled. Rendering locations are synthetic Phase 0 fixtures. Cold requests excluded from warm traces."}
	for _, tc := range []struct{ name, path string }{{"clusters", "/api/app/map/features?" + base}, {"individuals", "/api/app/map/features?" + base + "&clustering=0"}, {"columns_compatibility", "/api/app/map/fits?" + base}, {"summary", "/api/app/map/summary"}, {"points_500k", "/api/app/map/points?" + base + "&max=500000"}} {
		b, cold := request(tc.path)
		var values []float64
		var payload any
		if tc.name != "points_500k" {
			var p map[string]any
			must(json.Unmarshal(b, &p))
			for _, k := range []string{"total", "feature_total", "count", "located_ssids", "observations", "next"} {
				if p[k] != nil {
					if payload == nil {
						payload = map[string]any{}
					}
					payload.(map[string]any)[k] = p[k]
				}
			}
		}
		for i := 0; i < *count; i++ {
			_, ms := request(tc.path)
			values = append(values, ms)
		}
		sort.Float64s(values)
		result[tc.name] = map[string]any{"cold_ms": cold, "bytes": len(b), "warm_p50_ms": values[(len(values)-1)/2], "warm_p95_ms": values[(len(values)*95+99)/100-1], "payload": payload}
	}
	// Unlike identical response-cache hits, this changes the bbox on every pan.
	values := []float64{}
	for i := 0; i < *count; i++ {
		_, ms := request(fmt.Sprintf("/api/app/map/features?bbox=%.6f,48.399,-123.390,48.410&zoom=15&min_rssi=-150", -123.401+float64(i%30)*.000001))
		values = append(values, ms)
	}
	sort.Float64s(values)
	result["panning_clusters"] = map[string]any{"warm_p50_ms": values[(len(values)-1)/2], "warm_p95_ms": values[(len(values)*95+99)/100-1]}
	b, err := json.MarshalIndent(result, "", "  ")
	must(err)
	fmt.Println(string(b))
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
