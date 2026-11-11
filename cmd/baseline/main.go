// baseline measures the unchanged engine. It never deletes or reuses a survey
// for import; -reopen-existing measures an explicitly owned benchmark store.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/drivedb"
	"signal-atlas/internal/engine"
	"signal-atlas/internal/py"
)

type row = map[string]any

var report = row{"schema": 1, "engine": "upstream"}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func ownedBenchmarkData(path string) bool {
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	for range 4 {
		b, err := os.ReadFile(filepath.Join(path, "owner.json"))
		if err == nil {
			var marker struct {
				Purpose string `json:"purpose"`
				Schema  int    `json:"schema"`
			}
			if json.Unmarshal(b, &marker) == nil && marker.Purpose == "signal-atlas-benchmark" && marker.Schema == 1 {
				return true
			}
		}
		path = filepath.Dir(path)
	}
	return false
}
func timed(name string, fn func() (any, error)) any {
	fmt.Fprintln(os.Stderr, name)
	t := time.Now()
	v, err := fn()
	must(err)
	report[name] = row{"wall_seconds": time.Since(t).Seconds(), "result": v}
	return v
}
func distribution(values []float64) row {
	x := append([]float64(nil), values...)
	sort.Float64s(x)
	if len(x) == 0 {
		return row{"count": 0}
	}
	return row{"count": len(x), "p50_ms": x[int(math.Ceil(float64(len(x))*.5))-1], "p95_ms": x[int(math.Ceil(float64(len(x))*.95))-1], "max_ms": x[len(x)-1], "runs_ms": values}
}
func query(s *engine.Server, path string) (int, int) {
	r := httptest.NewRequest("GET", "http://benchmark.local"+path, nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		panic(fmt.Sprintf("%s: %d %s", path, w.Code, w.Body.String()))
	}
	return w.Code, w.Body.Len()
}
func measurements(s *engine.Server, n int, bbox string, mapOnly bool) {
	paths := []string{
		"/api/points?bbox=" + bbox,
		"/api/area-estimates?zoom=14&bbox=" + bbox,
		"/api/app/map/fits?bbox=" + bbox,
		"/api/points?band=5&bbox=" + bbox,
	}
	traces := row{}
	for _, path := range paths {
		if mapOnly && strings.HasPrefix(path, "/api/points?") {
			continue
		}
		values := []float64{}
		maxBytes := 0
		for range n {
			t := time.Now()
			_, size := query(s, path)
			values = append(values, float64(time.Since(t).Microseconds())/1000)
			maxBytes = max(maxBytes, size)
		}
		r := distribution(values)
		r["response_bytes_max"] = maxBytes
		traces[path] = r
	}
	report["viewport_traces"] = traces
	if mapOnly {
		return
	}
	packed := row{}
	for _, limit := range []int{500000, 2000000, 8000000} {
		path := fmt.Sprintf("/api/app/map/points?bbox=%s&max=%d", bbox, limit)
		vals := []float64{}
		size := 0
		for range 3 {
			t := time.Now()
			_, size = query(s, path)
			vals = append(vals, float64(time.Since(t).Microseconds())/1000)
		}
		r := distribution(vals)
		r["response_bytes"] = size
		packed[fmt.Sprint(limit)] = r
	}
	report["packed_gpu_points"] = packed
}
func fitBatch(a *atlas.Atlas, groups []atlas.Group, p py.Params, workers int) row {
	_, key, err := atlas.EstimateScope(p)
	must(err)
	results := make(chan row, len(groups))
	queue := make(chan atlas.Group, len(groups))
	for _, g := range groups {
		queue <- g
	}
	close(queue)
	t := time.Now()
	for range min(workers, len(groups)) {
		go func() {
			c, put, err := a.Conn(nil)
			must(err)
			defer put()
			for g := range queue {
				start := time.Now()
				status, err := a.FitAndStore(c, g, p, key, nil)
				must(err)
				r, err := db.One(c, "SELECT result FROM estimates WHERE radio_key=? AND scope_key=?", g["radio_key"], key)
				must(err)
				var fit row
				must(json.Unmarshal([]byte(db.Str(r["result"])), &fit))
				results <- row{"radio_key": g["radio_key"], "status": status, "wall_ms": float64(time.Since(start).Microseconds()) / 1000, "latitude": fit["latitude"], "longitude": fit["longitude"], "residual_rms_db": fit["residual_rms_db"], "count": fit["count"], "used_observations": fit["used_observations"], "coordinate_groups": fit["coordinate_groups"], "reason": fit["reason"]}
			}
		}()
	}
	out := []row{}
	fitted := 0
	for range groups {
		r := <-results
		out = append(out, r)
		if r["status"] == "fitted" {
			fitted++
		}
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]["radio_key"]) < fmt.Sprint(out[j]["radio_key"]) })
	seconds := time.Since(t).Seconds()
	return row{"wall_seconds": seconds, "attempted": len(groups), "fitted": fitted, "unavailable": len(groups) - fitted, "computed_groups_per_minute": float64(len(groups)) / seconds * 60, "fitted_groups_per_minute": float64(fitted) / seconds * 60, "results": out}
}
func main() {
	defer func() {
		if e := recover(); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
	}()
	csv := flag.String("csv", "", "CSV path(s), separated by semicolon")
	store := flag.String("store", "", "new benchmark data directory")
	threads := flag.Int("threads", runtime.NumCPU(), "GOMAXPROCS and fit workers")
	requests := flag.Int("requests", 200, "requests per trace")
	fits := flag.Int("fits", 8, "radio groups per solver/mode")
	export := flag.Bool("export", false, "export and reopen complete SQLite snapshot")
	archive := flag.Bool("archive", false, "measure independent Drive archive")
	archiveOnly := flag.Bool("archive-only", false, "measure only the independent archive")
	cpuProfile := flag.String("cpuprofile", "", "separate diagnostic run CPU profile")
	memProfile := flag.String("memprofile", "", "separate diagnostic run heap profile")
	mapOnly := flag.Bool("map-only", false, "only AP viewport traces, no observation scans")
	reopen := flag.Bool("reopen-existing", false, "open an existing benchmark store without import")
	expected := flag.Int64("expected-rows", -1, "assert imported observation count")
	bbox := flag.String("bbox", "-123.401,48.399,-123.390,48.410", "viewport w,s,e,n")
	flag.Parse()
	if *cpuProfile != "" {
		f, e := os.OpenFile(*cpuProfile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		must(e)
		must(pprof.StartCPUProfile(f))
		defer func() { pprof.StopCPUProfile(); f.Close() }()
		report["profiled_run"] = true
	}
	if *store == "" || *threads < 1 || *requests < 1 {
		panic("supply a store and positive worker/request counts")
	}
	if !*reopen {
		if _, err := os.Stat(*store); !os.IsNotExist(err) {
			panic("import benchmark requires a new store directory")
		}
	} else if !ownedBenchmarkData(*store) {
		panic("reopen requires an owned benchmark data directory")
	}
	runtime.GOMAXPROCS(*threads)
	report["utc"] = time.Now().UTC().Format(time.RFC3339)
	report["go"] = runtime.Version()
	report["threads"] = *threads
	report["logical_cpus"] = runtime.NumCPU()
	report["sqlite"] = atlas.SQLiteVersion
	if b, ok := debug.ReadBuildInfo(); ok {
		report["build_info"] = b
	}
	if *archiveOnly {
		if *csv == "" || *reopen {
			panic("archive-only requires CSV and new directory")
		}
		must(os.MkdirAll(*store, 0755))
		archiveWork(*store, strings.Split(*csv, ";"))
		must(json.NewEncoder(os.Stdout).Encode(report))
		return
	}
	t := time.Now()
	s, err := engine.New(engine.Options{DisablePreparation: true, Data: *store, Version: "dev", Hosts: []string{"benchmark.local"}})
	must(err)
	defer s.Close()
	report["open_seconds"] = time.Since(t).Seconds()
	s.Jobs.Threads = *threads
	a := s.Atlas
	if !*reopen {
		if *csv == "" {
			panic("supply -csv")
		}
		files := strings.Split(*csv, ";")
		imports := []row{}
		total := int64(0)
		for _, file := range files {
			t = time.Now()
			r, err := a.ImportCSV(file, true, nil, "", func(stage string) { fmt.Fprintln(os.Stderr, stage) })
			must(err)
			sec := time.Since(t).Seconds()
			total += db.Int(r["valid_rows"])
			imports = append(imports, row{"file": filepath.Base(file), "wall_seconds": sec, "rows_per_minute": float64(db.Int(r["valid_rows"])) / sec * 60, "outcome": r})
		}
		report["imports"] = imports
		if *expected >= 0 && int64(a.Obs.State().N) != *expected {
			panic(fmt.Sprintf("expected %d rows, got %d", *expected, a.Obs.State().N))
		}
		timed("duplicate_import", func() (any, error) { return a.ImportCSV(files[0], true, nil, "", nil) })
		if a.Obs.State().N != int(total) {
			panic("duplicate import changed observation count")
		}
		if *archive {
			archiveWork(*store, files)
		}
	}
	report["observation_count"] = a.Obs.State().N
	c, put, err := a.Conn(nil)
	must(err)
	timed("summary", func() (any, error) { return a.Summary(c, py.Params{}) })
	put()
	if *fits > 0 && !*reopen {
		gs, err := a.SelectedGroups(py.Params{})
		must(err)
		// Match the nearby map's group identities, rather than storing raw
		// radio keys that the explorer's eligibility check would discard.
		gs = atlas.EstimationGroups(gs, py.Params{"shared": {"1"}, "individual": {"0"}})
		gs = gs[:min(*fits, len(gs))]
		all := row{}
		for _, mode := range []string{"sample", "all"} {
			for _, solver := range []string{"reference", "fast"} {
				fmt.Fprintln(os.Stderr, "fits", mode, solver)
				p := py.Params{"fit_mode": {mode}}
				if solver == "fast" {
					p["solver"] = []string{"fast"}
				}
				all[mode+"/"+solver] = fitBatch(a, gs, p, *threads)
			}
		}
		report["fits"] = all
	}
	measurements(s, *requests, url.QueryEscape(*bbox), *mapOnly)
	if *export {
		path := filepath.Join(*store, "snapshot.sqlite3")
		timed("export_sqlite", func() (any, error) {
			err := a.ExportSQLite(path, func(stage string) { fmt.Fprintln(os.Stderr, stage) })
			return nil, err
		})
		b, err := atlas.Open(filepath.Join(*store, "snapshot-reopened"))
		must(err)
		timed("open_sqlite_snapshot", func() (any, error) { return b.OpenDatabase(path, nil, nil) })
		if b.Obs.State().N != a.Obs.State().N {
			panic("snapshot row count mismatch")
		}
		must(b.Close())
	}
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	report["go_memory"] = row{"heap_alloc": mem.HeapAlloc, "heap_inuse": mem.HeapInuse, "sys": mem.Sys}
	if *memProfile != "" {
		f, e := os.OpenFile(*memProfile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		must(e)
		must(pprof.WriteHeapProfile(f))
		must(f.Close())
	}
	bytes := int64(0)
	must(filepath.Walk(*store, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			bytes += info.Size()
		}
		return nil
	}))
	report["store_directory_bytes"] = bytes
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	must(enc.Encode(report))
}

func archiveWork(dir string, files []string) {
	d, err := drivedb.Create(filepath.Join(dir, "drive.sqlite3"))
	must(err)
	defer d.Close()
	for i, file := range files {
		timed(fmt.Sprintf("archive_import_%d", i), func() (any, error) { return d.Add(file, nil, nil) })
	}
	timed("archive_duplicate_file", func() (any, error) { return d.Add(files[0], nil, nil) })
	csv := filepath.Join(dir, "archive-export.csv")
	n := timed("archive_export_csv", func() (any, error) { return d.ExportCSV(csv, "baseline", nil, nil) }).(int64)
	r := timed("archive_reimport_export", func() (any, error) { return d.Add(csv, nil, nil) }).(drivedb.Result)
	if r.RowsAdded != 0 || r.Duplicates != n {
		panic(fmt.Sprintf("archive dedup mismatch: exported %d, reimport %+v", n, r))
	}
	timed("archive_export_days", func() (any, error) { return d.ExportDays(filepath.Join(dir, "days"), "baseline", nil, nil) })
	timed("archive_info", func() (any, error) { return d.Info() })
}
