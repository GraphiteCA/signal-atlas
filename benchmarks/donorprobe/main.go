//go:build donorbaseline

// Copied into the frozen donor module for matched engine measurements.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"signalatlas/internal/atlas"
)

type row = map[string]any

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func numeric(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case float64:
		return x
	}
	return 0
}
func main() {
	defer func() {
		if e := recover(); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
	}()
	csv := flag.String("csv", "", "CSV paths separated by semicolon")
	dir := flag.String("store", "", "new data directory")
	threads := flag.Int("threads", 8, "Go and DuckDB workers")
	requests := flag.Int("requests", 200, "requests per trace")
	fits := flag.Int("fits", 8, "fit groups")
	reopen := flag.Bool("reopen-existing", false, "open existing benchmark data")
	expected := flag.Int64("expected-rows", -1, "expected accepted rows")
	bbox := flag.String("bbox", "-123.401,48.399,-123.390,48.410", "w,s,e,n")
	export := flag.Bool("export", false, "complete database snapshot")
	flag.Bool("archive", false, "not supported by donor")
	flag.Parse()
	if *dir == "" || *threads < 1 {
		panic("directory and positive workers required")
	}
	if !*reopen {
		if _, err := os.Stat(*dir); !os.IsNotExist(err) {
			panic("requires fresh directory")
		}
	}
	must(os.MkdirAll(*dir, 0755))
	runtime.GOMAXPROCS(*threads)
	r := row{"schema": 1, "engine": "donor-duckdb", "go": runtime.Version(), "threads": *threads, "memory_limit": "2GB", "utc": time.Now().UTC().Format(time.RFC3339)}
	if info, ok := debug.ReadBuildInfo(); ok {
		r["build_info"] = info
	}
	t := time.Now()
	s, err := atlas.Open(filepath.Join(*dir, "survey.duckdb"), *threads, "2GB")
	must(err)
	defer s.DB.Close()
	r["open_seconds"] = time.Since(t).Seconds()
	ctx := context.Background()
	var version string
	must(s.DB.QueryRow("SELECT version()").Scan(&version))
	r["duckdb_version"] = version
	if !*reopen {
		imports := []row{}
		for _, file := range strings.Split(*csv, ";") {
			t = time.Now()
			result, err := s.Import(ctx, file, "", true, nil)
			must(err)
			sec := time.Since(t).Seconds()
			imports = append(imports, row{"file": filepath.Base(file), "outcome": result, "wall_seconds": sec, "rows_per_minute": numeric(result["valid_rows"]) / sec * 60})
		}
		r["imports"] = imports
		t = time.Now()
		v, err := s.Import(ctx, strings.Split(*csv, ";")[0], "", true, nil)
		must(err)
		r["duplicate_import"] = row{"result": v, "wall_seconds": time.Since(t).Seconds()}
	}
	t = time.Now()
	info, err := s.Summary(ctx, url.Values{})
	must(err)
	r["first_summary_seconds"] = time.Since(t).Seconds()
	r["summary"] = info
	r["observation_count"] = info["count"]
	if *expected >= 0 && numeric(info["count"]) != float64(*expected) {
		panic(fmt.Sprintf("expected %d, got %v", *expected, info["count"]))
	}
	if *fits > 0 && !*reopen {
		groups, _, err := s.Groups(ctx, url.Values{"individual": {"1"}, "shared": {"0"}})
		must(err)
		sort.Slice(groups, func(i, j int) bool { return groups[i].Key < groups[j].Key })
		groups = groups[:min(*fits, len(groups))]
		fitResults := row{}
		for _, mode := range []string{"sample", "all"} {
			fmt.Fprintln(os.Stderr, "fit", mode)
			start := time.Now()
			out := []row{}
			fitted := 0
			for _, g := range groups {
				t = time.Now()
				fit, err := s.Fit(ctx, g, url.Values{"fit_mode": {mode}})
				must(err)
				fit["wall_ms"] = float64(time.Since(t).Microseconds()) / 1000
				out = append(out, fit)
				if fit["status"] == "fitted" {
					fitted++
				}
			}
			sec := time.Since(start).Seconds()
			fitResults[mode] = row{"results": out, "wall_seconds": sec, "attempted": len(groups), "fitted": fitted, "computed_groups_per_minute": float64(len(groups)) / sec * 60, "workers": 1}
		}
		r["fits"] = fitResults
	}
	server := atlas.NewServer(s, nil)
	defer server.Close()
	paths := []string{"/api/points?bbox=" + url.QueryEscape(*bbox), "/api/area-estimates?zoom=14&bbox=" + url.QueryEscape(*bbox), "/api/points?band=5&bbox=" + url.QueryEscape(*bbox)}
	traces := row{}
	for _, path := range paths {
		fmt.Fprintln(os.Stderr, "query trace", path, "requests", *requests)
		vals := []float64{}
		size := 0
		for range *requests {
			req := httptest.NewRequest("GET", "http://benchmark.local"+path, nil)
			req.AddCookie(&http.Cookie{Name: "atlas_session", Value: server.Token})
			w := httptest.NewRecorder()
			t = time.Now()
			server.ServeHTTP(w, req)
			vals = append(vals, float64(time.Since(t).Microseconds())/1000)
			if w.Code != 200 {
				panic(w.Body.String())
			}
			size = max(size, w.Body.Len())
		}
		sorted := append([]float64(nil), vals...)
		sort.Float64s(sorted)
		traces[path] = row{"runs_ms": vals, "count": len(vals), "p50_ms": sorted[int(math.Ceil(float64(len(vals))*.5))-1], "p95_ms": sorted[int(math.Ceil(float64(len(vals))*.95))-1], "max_ms": sorted[len(sorted)-1], "response_bytes_max": size}
	}
	r["viewport_traces"] = traces
	if *export && !*reopen {
		t = time.Now()
		req := httptest.NewRequest("GET", "http://benchmark.local/api/database/download", nil)
		req.AddCookie(&http.Cookie{Name: "atlas_session", Value: server.Token})
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != 200 {
			panic(w.Body.String())
		}
		r["snapshot"] = row{"wall_seconds": time.Since(t).Seconds(), "bytes": w.Body.Len()}
	}
	_, err = s.DB.Exec("CHECKPOINT")
	must(err)
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	r["go_memory"] = row{"heap_alloc": mem.HeapAlloc, "heap_inuse": mem.HeapInuse, "sys": mem.Sys}
	bytes := int64(0)
	must(filepath.Walk(*dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			bytes += info.Size()
		}
		return nil
	}))
	r["store_directory_bytes"] = bytes
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	must(enc.Encode(r))
}
