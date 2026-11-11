// memorybench profiles an explicitly owned snapshot without running preparation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/py"
	"slices"
	"time"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func main() {
	data := flag.String("data", "", "owned benchmark store")
	out := flag.String("out", "", "new output directory")
	flag.Parse()
	root, e := filepath.Abs(*data)
	must(e)
	owned := false
	for p, i := root, 0; i < 6; i, p = i+1, filepath.Dir(p) {
		b, e := os.ReadFile(filepath.Join(p, "owner.json"))
		var m map[string]any
		if e == nil && json.Unmarshal(b, &m) == nil && m["purpose"] == "signal-atlas-benchmark" && m["schema"] == float64(1) {
			owned = true
			break
		}
	}
	if !owned {
		panic("owned snapshot required")
	}
	must(os.Mkdir(*out, 0755))
	runtime.GOMAXPROCS(8)
	start := time.Now()
	a, e := atlas.Open(root)
	must(e)
	defer a.Close()
	report := map[string]any{}
	sample := func(name string) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		report[name] = map[string]any{"seconds": time.Since(start).Seconds(), "heap_alloc": m.HeapAlloc, "heap_inuse": m.HeapInuse, "heap_sys": m.HeapSys, "heap_objects": m.HeapObjects, "total_alloc": m.TotalAlloc, "gc": m.NumGC}
		f, e := os.Create(filepath.Join(*out, name+".heap"))
		must(e)
		must(pprof.WriteHeapProfile(f))
		must(f.Close())
	}
	sample("open")
	st := a.Obs.State()
	report["counts"] = map[string]any{"rows": st.N, "strings": len(st.Str), "radios": len(st.ByRadio), "networks": len(st.Net), "transmitters": len(st.Trans), "grid": len(st.Grid)}
	runtime.GC()
	sample("open_live")
	c, put, e := a.Conn(context.Background())
	must(e)
	summary, e := a.MapSummary(c)
	must(e)
	report["map_summary"] = summary
	sample("summary")
	page, e := a.MapFeatures(context.Background(), c, py.Params{"bbox": {"-180,-85,180,85"}, "zoom": {"8"}, "solver": {"opencl64-fast-v1"}})
	must(e)
	report["page"] = page
	var times []float64
	for range 12 {
		_, e = db.Exec(c, "UPDATE map_revision SET value=value+1 WHERE id=1")
		must(e)
		t := time.Now()
		_, e = a.MapSummary(c)
		must(e)
		times = append(times, time.Since(t).Seconds()*1000)
	}
	slices.Sort(times)
	report["uncached_summary_ms"] = map[string]any{"median": times[len(times)/2], "p95": times[len(times)-1], "trials": times}
	put()
	sample("map")
	runtime.GC()
	sample("map_live")
	must(json.NewEncoder(os.Stdout).Encode(report))
}
