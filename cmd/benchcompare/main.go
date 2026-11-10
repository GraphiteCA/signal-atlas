// benchcompare times Wardrive Compare's engine: add the logs, analyse, report rows per minute.
//
//	go run ./cmd/benchcompare a.csv b.csv ...
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"signal-atlas/internal/compare"
)

func main() {
	var files []compare.FileIn
	for _, p := range os.Args[1:] {
		st, err := os.Stat(p)
		if err != nil {
			panic(err)
		}
		files = append(files, compare.FileIn{Name: filepath.Base(p), Size: st.Size(), Path: p})
	}
	cfg := &compare.Config{Mode: "drive", DriveGapH: 3, LegGapMin: 10, Radius: 50, Typ: "wifi"}
	var rows int
	e := compare.New(func(m map[string]any) {
		if m["type"] == "result" && m["result"] != nil {
			rows = m["result"].(map[string]any)["memRows"].(int)
			fmt.Println("drives:", m["result"].(map[string]any)["n"])
		}
		if m["type"] == "error" || m["type"] == "added" {
			fmt.Println(m)
		}
	}, os.TempDir())
	t0 := time.Now()
	e.Handle(&compare.Message{Type: "add", Files: files, Cfg: cfg})
	el := time.Since(t0).Seconds()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("add + analyse: %d rows in %.1fs = %.1f million rows/minute (live after the analysis %.0f MB)\n", rows, el, float64(rows)/el*60/1e6, float64(ms.HeapInuse)/1e6)
	t0 = time.Now()
	e.Handle(&compare.Message{Type: "analyse", Cfg: &compare.Config{Mode: "leg", DriveGapH: 3, LegGapMin: 10, Radius: 50, Typ: "wifi"}})
	fmt.Printf("re-analyse by leg: %.1fs\n", time.Since(t0).Seconds())
}
