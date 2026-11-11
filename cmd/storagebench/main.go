// storagebench measures production reads and the SQL adapter on owned fixtures.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"signal-atlas/internal/atlas"
	"signal-atlas/internal/py"
	"sort"
	"strings"
	"time"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func main() {
	data := flag.String("data", "", "owned benchmark workspace (actual atlas directory)")
	n := flag.Int("requests", 200, "samples per selective trace")
	small := flag.String("import", "", "optional incremental CSV on an owned workspace")
	flag.Parse()
	if *data == "" {
		panic("supply data")
	}
	owned := false
	root := *data
	for range 5 {
		b, e := os.ReadFile(filepath.Join(root, "owner.json"))
		var marker struct {
			Purpose string `json:"purpose"`
			Schema  int    `json:"schema"`
		}
		if e == nil && json.Unmarshal(b, &marker) == nil && marker.Purpose == "signal-atlas-benchmark" && marker.Schema == 1 {
			owned = true
			break
		}
		root = filepath.Dir(root)
	}
	if !owned {
		panic("owned benchmark required")
	}
	runtime.GOMAXPROCS(8)
	start := time.Now()
	a, e := atlas.Open(*data)
	must(e)
	defer a.Close()
	out := map[string]any{"open_seconds": time.Since(start).Seconds(), "threads": 8, "go": runtime.Version(), "rows": a.Obs.State().N}
	if *small != "" {
		start = time.Now()
		result, err := a.ImportCSV(*small, false, nil, "", nil)
		must(err)
		out["incremental_import"] = map[string]any{"wall_seconds": time.Since(start).Seconds(), "outcome": result}
		out["rows"] = a.Obs.State().N
	}
	st := a.Obs.State()
	keys := []string{}
	ssids := []string{}
	for k := range st.ByRadio {
		keys = append(keys, st.Str[k])
	}
	for k := range st.BySSID {
		ssids = append(ssids, st.Str[k])
	}
	sort.Strings(keys)
	sort.Strings(ssids)
	literal := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	queries := map[string]string{
		"count":           "SELECT count(*) FROM observations",
		"viewport_rows":   "SELECT id,latitude,longitude,rssi_dbm FROM observations WHERE longitude BETWEEN -123.401 AND -123.390 AND latitude BETWEEN 48.399 AND 48.410 LIMIT 1000",
		"radio_retrieval": "SELECT * FROM observations WHERE radio_key=" + literal(keys[0]) + " ORDER BY id LIMIT 1000",
		"ssid_retrieval":  "SELECT * FROM observations WHERE ssid=" + literal(ssids[0]) + " ORDER BY id LIMIT 1000",
	}
	traces := map[string]any{}
	for name, q := range queries {
		samples := []float64{}
		requests := *n
		if name == "count" {
			requests = 3
		}
		rows := 0
		for range requests {
			start = time.Now()
			result, err := a.SQLQuery(context.Background(), atlas.SQLRequest{SQL: q, Limit: 1000, TimeoutSeconds: 120})
			must(err)
			rows = len(result.Rows)
			samples = append(samples, float64(time.Since(start).Microseconds())/1000)
		}
		traces[name] = distribution(samples)
		traces[name].(map[string]any)["rows"] = rows
	}
	for name, fn := range map[string]func() error{
		"production_summary": func() error {
			c, put, e := a.Conn(context.Background())
			if e != nil {
				return e
			}
			defer put()
			_, e = a.Summary(c, py.Params{})
			return e
		},
		"production_catalog": func() error { _, e := a.Networks(py.Params{}); return e },
	} {
		samples := []float64{}
		for range *n {
			start = time.Now()
			must(fn())
			samples = append(samples, float64(time.Since(start).Microseconds())/1000)
		}
		traces[name] = distribution(samples)
	}
	out["traces"] = traces
	var disk int64
	must(filepath.WalkDir(*data, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() {
			st, e := d.Info()
			if e != nil {
				return e
			}
			disk += st.Size()
		}
		return nil
	}))
	out["disk_bytes"] = disk
	must(json.NewEncoder(os.Stdout).Encode(out))
	fmt.Fprintln(os.Stderr, "storage probe complete")
}
func distribution(x []float64) map[string]any {
	sorted := append([]float64(nil), x...)
	sort.Float64s(sorted)
	return map[string]any{"runs_ms": x, "p50_ms": sorted[(len(sorted)-1)/2], "p95_ms": sorted[(len(sorted)*95+99)/100-1]}
}
