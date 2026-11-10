// benchdrive times the Drive database against the target of ten million rows a minute.
//
//	go run ./cmd/benchdrive -csv big.csv -dir benchdrive
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"signal-atlas/internal/drivedb"
)

func main() {
	csv := flag.String("csv", "", "a big WiGLE CSV log")
	dir := flag.String("dir", "benchdrive", "folder to work in (emptied first)")
	csv2 := flag.String("csv2", "", "a second, different log, added to the first (how the rate holds as the database grows)")
	flag.Parse()
	os.RemoveAll(*dir)
	os.MkdirAll(*dir, 0o755)
	var peak uint64
	stop := make(chan bool)
	go func() {
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				peak = max(peak, ms.HeapInuse)
			}
		}
	}()
	rate := func(what string, rows int64, t time.Time) {
		el := time.Since(t).Seconds()
		fmt.Printf("%-44s %10d rows in %6.1fs = %5.1f million rows/minute\n", what, rows, el, float64(rows)/el*60/1e6)
	}
	a, err := drivedb.Create(filepath.Join(*dir, "a.sqlite3"))
	must(err)
	t := time.Now()
	r, err := a.Add(*csv, nil, nil)
	must(err)
	rate("add a CSV log to an empty database", r.RowsRead, t)
	if *csv2 != "" {
		t = time.Now()
		r, err := a.Add(*csv2, nil, nil)
		must(err)
		rate(fmt.Sprintf("add a second log (%d new, %d duplicates)", r.RowsAdded, r.Duplicates), r.RowsRead, t)
	}

	t = time.Now()
	n, err := a.ExportCSV(filepath.Join(*dir, "all.csv"), "bench", nil, nil)
	must(err)
	rate("export every sighting as one CSV", n, t)

	t = time.Now()
	r, err = a.Add(filepath.Join(*dir, "all.csv"), nil, nil)
	must(err)
	rate(fmt.Sprintf("add it back (%d duplicates)", r.Duplicates), r.RowsRead, t)

	t = time.Now()
	files, err := a.ExportDays(filepath.Join(*dir, "days"), "bench", nil, nil)
	must(err)
	rate(fmt.Sprintf("export one CSV per day (%d files)", len(files)), n, t)

	b, err := drivedb.Create(filepath.Join(*dir, "b.sqlite3"))
	must(err)
	a.Close()
	t = time.Now()
	r, err = b.Add(filepath.Join(*dir, "a.sqlite3"), nil, nil)
	must(err)
	rate("add that Drive database to another", r.RowsRead, t)
	b.Close()
	close(stop)
	st, _ := os.Stat(filepath.Join(*dir, "a.sqlite3"))
	fmt.Printf("database file: %.2f GB; peak Go memory %.2f GB\n", float64(st.Size())/1e9, float64(peak)/1e9)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
