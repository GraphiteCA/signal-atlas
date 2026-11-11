// A standalone candidate keeps DuckDB/CGO out of production builds until the
// evidence justifies replacing the observation store.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"signal-atlas/tools/duckbench/candidate"
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
	csv := flag.String("csv", "", "CSV to import; empty reopens existing candidate")
	path := flag.String("db", "", "candidate database path")
	memory := flag.String("memory", "1GB", "buffer manager limit (process RSS is measured separately)")
	requests := flag.Int("requests", 200, "samples per read trace")
	small := flag.String("small", "", "optional additional import")
	indexed := flag.Bool("index", false, "measure optional ART radio/SSID indexes")
	expected := flag.Int64("expected-rows", -1, "assert accepted count")
	flag.Parse()
	if *path == "" {
		panic("supply database path")
	}
	owned := false
	root := filepath.Dir(*path)
	for range 5 {
		b, e := os.ReadFile(filepath.Join(root, "owner.json"))
		var m struct {
			Purpose string `json:"purpose"`
			Schema  int    `json:"schema"`
		}
		if e == nil && json.Unmarshal(b, &m) == nil && m.Purpose == "signal-atlas-benchmark" && m.Schema == 1 {
			owned = true
			break
		}
		root = filepath.Dir(root)
	}
	if !owned {
		panic("owned benchmark directory required")
	}
	runtime.GOMAXPROCS(8)
	if *csv != "" {
		if _, err := os.Stat(*path); !os.IsNotExist(err) {
			panic("new candidate required")
		}
		must(os.MkdirAll(filepath.Dir(*path), 0755))
	}
	result := map[string]any{"go": runtime.Version(), "threads": 8, "memory_limit": *memory, "read_solver": "none; retrieval only", "database": *path}
	start := time.Now()
	s, err := candidate.Open(*path, 8, *memory)
	must(err)
	defer s.DB.Close()
	result["open_seconds"] = time.Since(start).Seconds()
	var version string
	must(s.DB.QueryRow("SELECT version()").Scan(&version))
	result["duckdb"] = version
	ctx := context.Background()
	for _, source := range []string{*csv, *small} {
		if source == "" {
			continue
		}
		start = time.Now()
		r, err := s.Import(ctx, source, "", false, nil)
		must(err)
		seconds := time.Since(start).Seconds()
		result["import_"+filepath.Base(source)] = map[string]any{"outcome": r, "wall_seconds": seconds, "rows_per_minute": float64(r["valid_rows"].(int64)) * 60 / seconds, "seconds": seconds}
	}
	if *indexed {
		start = time.Now()
		_, err = s.DB.Exec("CREATE INDEX IF NOT EXISTS observation_radio ON observations(radio_key); CREATE INDEX IF NOT EXISTS observation_ssid ON observations(ssid)")
		must(err)
		result["index_seconds"] = time.Since(start).Seconds()
	}
	var count int64
	must(s.DB.QueryRow("SELECT count(*) FROM observations").Scan(&count))
	result["rows"] = count
	if *expected >= 0 && count != *expected {
		panic(fmt.Sprintf("expected %d rows, got %d", *expected, count))
	}
	traces := map[string]any{}
	var radio, ssid string
	must(s.DB.QueryRow("SELECT min(radio_key),min(ssid) FROM radio_stats").Scan(&radio, &ssid))
	literal := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

	queries := map[string]string{
		"summary":         "SELECT count(*) observations,count(DISTINCT nullif(ssid,'')) ssids,max(rssi_dbm) peak FROM observations",
		"viewport_rows":   "SELECT id,latitude,longitude,rssi_dbm FROM observations WHERE longitude BETWEEN -123.401 AND -123.390 AND latitude BETWEEN 48.399 AND 48.410 LIMIT 1000",
		"radio_retrieval": "SELECT * FROM observations WHERE radio_key=" + literal(radio) + " ORDER BY id LIMIT 1000",
		"ssid_retrieval":  "SELECT * FROM observations WHERE ssid=" + literal(ssid) + " ORDER BY id LIMIT 1000",
		"catalog":         "SELECT ssid,count,radios FROM network_catalog ORDER BY count DESC,ssid LIMIT 40",
	}
	for name, q := range queries {
		samples := []float64{}
		n := *requests
		if name == "summary" || name == "ssid_retrieval" {
			n = min(n, 10)
		}
		var rows int
		for range n {
			start = time.Now()
			data, e := s.Query(ctx, q)
			must(e)
			rows = len(data)
			samples = append(samples, float64(time.Since(start).Microseconds())/1000)
		}
		ordered := append([]float64(nil), samples...)
		sort.Float64s(ordered)
		traces[name] = map[string]any{"runs_ms": samples, "p50_ms": ordered[(len(ordered)-1)/2], "p95_ms": ordered[(len(ordered)*95+99)/100-1], "rows": rows}
	}
	result["traces"] = traces
	_, err = s.DB.Exec("CHECKPOINT")
	must(err)
	info, err := os.Stat(*path)
	must(err)
	result["disk_bytes"] = info.Size()
	must(json.NewEncoder(os.Stdout).Encode(result))
	fmt.Fprintln(os.Stderr, "candidate complete")
}
