// bench times the engine's big pipelines against the target of ten million rows a minute.
//
//	go run ./cmd/bench -csv big.csv -store benchstore
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/engine"
	"signal-atlas/internal/py"
)

func main() {
	csv := flag.String("csv", "", "WiGLE CSV to import into a fresh Signal Atlas store")
	store := flag.String("store", "benchstore", "store folder to create")
	fits := flag.Int("fits", 200, "radios to fit in the fit test")
	export := flag.String("export", "", "also time writing the store as a Graphite-format SQLite file here")
	fitAll := flag.Duration("fitall", 0, "run \"Fit every AP\" for this long and report its rate and peak memory")
	flag.Parse()
	os.RemoveAll(*store)
	a, err := atlas.Open(*store)
	if err != nil {
		panic(err)
	}
	t0 := time.Now()
	r, err := a.ImportCSV(*csv, true, nil, "", func(s string) { fmt.Printf("  %-22s at %6.1fs\n", s, time.Since(t0).Seconds()) })
	if err != nil {
		panic(err)
	}
	el := time.Since(t0).Seconds()
	rows := db.Int(r["valid_rows"])
	fmt.Printf("import CSV: %d rows in %.1fs = %.1f million rows/minute\n", rows, el, float64(rows)/el*60/1e6)

	t0 = time.Now()
	a.Close()
	if a, err = atlas.Open(*store); err != nil {
		panic(err)
	}
	fmt.Printf("reopen (load + index): %.1fs\n", time.Since(t0).Seconds())
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("memory in use with the store open: %.1f GB\n", float64(ms.HeapInuse)/1e9)
	if *fitAll > 0 {
		benchFitAll(a, *fitAll, ms.HeapInuse)
		return
	}

	c, put, _ := a.Conn(nil)
	defer put()
	timeIt := func(name string, fn func() error) {
		t := time.Now()
		if err := fn(); err != nil {
			panic(err)
		}
		fmt.Printf("  %-34s %7.0f ms\n", name, float64(time.Since(t).Microseconds())/1000)
	}
	fmt.Println("explorer queries over the whole store:")
	timeIt("summary", func() error { _, err := a.Summary(c, py.Params{}); return err })
	timeIt("points (map, aggregated)", func() error { _, err := a.Points(py.Params{}); return err })
	timeIt("points in a 2 km box", func() error {
		_, err := a.Points(py.Params{"bbox": {"-87.60,41.95,-87.58,41.97"}})
		return err
	})
	timeIt("radios page", func() error { _, err := a.Radios(py.Params{}); return err })
	timeIt("networks page", func() error { _, err := a.Networks(py.Params{}); return err })
	timeIt("dates", func() error { _, err := a.AvailableDates(py.Params{}); return err })
	timeIt("summary, SSID search 'net-1'", func() error { _, err := a.Summary(c, py.Params{"ssid_query": {"net-1"}}); return err })
	var groups []atlas.Group
	timeIt("selected groups (all radios)", func() error { var err error; groups, err = a.SelectedGroups(py.Params{}); return err })
	fmt.Printf("  %d radios in the store\n", len(groups))
	p := py.Params{"fit_mode": {"sample"}}
	_, key, _ := atlas.EstimateScope(p)
	n := min(*fits, len(groups))
	t := time.Now()
	done := make(chan error, n)
	sem := make(chan struct{}, 14)
	for _, g := range groups[:n] {
		sem <- struct{}{}
		go func(g atlas.Group) {
			defer func() { <-sem }()
			cc, put2, err := a.Conn(nil)
			if err == nil {
				_, err = a.FitAndStore(cc, g, p, key, nil)
				put2()
			}
			done <- err
		}(g)
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			panic(err)
		}
	}
	s := time.Since(t).Seconds()
	fmt.Printf("fits (sample mode, 14 at once): %d in %.1fs = %.0f radios/minute\n", n, s, float64(n)/s*60)
	if *export != "" {
		os.Remove(*export)
		t = time.Now()
		if err := a.ExportSQLite(*export, func(st string) { fmt.Printf("  %s at %.1fs\n", st, time.Since(t).Seconds()) }); err != nil {
			panic(err)
		}
		fmt.Printf("export to SQLite: %.1fs\n", time.Since(t).Seconds())
		// and opening that file again in another store, as "Open database" does
		os.RemoveAll(*store + "-opened")
		b, err := atlas.Open(*store + "-opened")
		if err != nil {
			panic(err)
		}
		t = time.Now()
		r, err := b.OpenDatabase(*export, nil, nil)
		if err != nil {
			panic(err)
		}
		el := time.Since(t).Seconds()
		n := r["valid_rows"].(int64)
		fmt.Printf("open SQLite database: %d rows in %.1fs = %.1f million rows/minute\n", n, el, float64(n)/el*60/1e6)
		b.Close()
	}
}

// benchFitAll runs the "Fit every AP" job for a while, sampling memory, then cancels it.
func benchFitAll(a *atlas.Atlas, d time.Duration, base uint64) {
	j := engine.NewJobs(a)
	t := time.Now()
	if _, err := j.FitAll(py.Params{"min_rssi": {"-100"}}); err != nil {
		panic(err)
	}
	fmt.Printf("fit every AP: listed in %.1fs\n", time.Since(t).Seconds())
	var peak uint64
	var ms runtime.MemStats
	for time.Since(t) < d && j.Running() {
		time.Sleep(500 * time.Millisecond)
		runtime.ReadMemStats(&ms)
		peak = max(peak, ms.HeapInuse)
	}
	s := j.Snapshot()
	el := time.Since(t).Seconds()
	j.Cancel()
	for j.Running() {
		time.Sleep(100 * time.Millisecond)
	}
	done, total := int64(s["completed"].(int)), int64(s["total"].(int))
	fmt.Printf("fit every AP: %d of %d networks in %.0fs = %.0f networks/minute (%v fitted, %v too few), %.0f minutes for all\n",
		done, total, el, float64(done)/el*60, s["fitted"], s["skipped"], float64(total)/(float64(done)/el)/60)
	fmt.Printf("fit every AP: peak memory %.2f GB, %.2f GB above the open store\n", float64(peak)/1e9, float64(peak-min(peak, base))/1e9)
}
