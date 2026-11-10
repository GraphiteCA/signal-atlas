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
	"signal-atlas/internal/py"
)

func main() {
	csv := flag.String("csv", "", "WiGLE CSV to import into a fresh Signal Atlas store")
	store := flag.String("store", "benchstore", "store folder to create")
	fits := flag.Int("fits", 200, "radios to fit in the fit test")
	export := flag.String("export", "", "also time writing the store as a Graphite-format SQLite file here")
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
	}
}
