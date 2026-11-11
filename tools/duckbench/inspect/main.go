// inspect records query plans and real target-aware point lookups, read-only.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	_ "github.com/duckdb/duckdb-go/v2"
	"os"
	"path/filepath"
	"signal-atlas/internal/db"
	"slices"
	"strings"
	"time"
	"zombiezen.com/go/sqlite"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func owned(p string) bool {
	for range 6 {
		b, e := os.ReadFile(filepath.Join(p, "owner.json"))
		var m map[string]any
		if e == nil && json.Unmarshal(b, &m) == nil && m["purpose"] == "signal-atlas-benchmark" {
			return true
		}
		p = filepath.Dir(p)
	}
	return false
}
func main() {
	path := flag.String("db", "", "owned ordered observations")
	meta := flag.String("metadata", "", "owned catalogue")
	canonical := flag.String("canonical", "", "optional canonical rows to fingerprint")
	flag.Parse()
	if !owned(filepath.Dir(*path)) || !owned(filepath.Dir(*meta)) {
		panic("owned fixtures required")
	}
	q, e := sql.Open("duckdb", *path+"?access_mode=read_only")
	must(e)
	defer q.Close()
	q.SetMaxOpenConns(1)
	_, e = q.Exec("SET threads=2;SET memory_limit='512MB'")
	must(e)
	c, e := db.OpenReadOnly(*meta)
	must(e)
	defer c.Close()
	var targets []string
	must(db.Each(c, "SELECT target_key FROM preparation_targets WHERE rowid%7303=0 ORDER BY target_key LIMIT 200", nil, func(s *sqlite.Stmt) error { targets = append(targets, s.ColumnText(0)); return nil }))
	if len(targets) < 20 {
		panic("full representative catalogue required")
	}
	radioQuery, e := q.Prepare("SELECT radio_key FROM observations WHERE target_key=? ORDER BY id LIMIT 1")
	must(e)
	defer radioQuery.Close()
	pointSQL := "SELECT id,import_id,line,record_id,ssid,mac,band,band_source,radio_key,latitude,longitude,rssi_dbm,accuracy_m,first_seen,date,channel,frequency_mhz FROM observations WHERE target_key=? AND radio_key=? ORDER BY id LIMIT 1000"
	point, e := q.Prepare(pointSQL)
	must(e)
	defer point.Close()
	var times []float64
	total := 0
	for _, t := range targets {
		var radio string
		must(radioQuery.QueryRow(t).Scan(&radio))
		start := time.Now()
		rows, e := point.Query(t, radio)
		must(e)
		for rows.Next() {
			values := make([]any, 17)
			ptrs := make([]any, 17)
			for i := range values {
				ptrs[i] = &values[i]
			}
			must(rows.Scan(ptrs...))
			total++
		}
		must(rows.Err())
		must(rows.Close())
		times = append(times, time.Since(start).Seconds()*1000)
	}
	slices.Sort(times)
	report := map[string]any{"schema": 1, "requests": len(times), "rows_returned": total, "point_ms": map[string]any{"p50": times[len(times)/2], "p95": times[len(times)*95/100], "max": times[len(times)-1]}, "point_sql": pointSQL}
	plan := func(query string, args ...any) string {
		rows, e := q.Query(query, args...)
		must(e)
		defer rows.Close()
		var parts []string
		for rows.Next() {
			var key, value string
			must(rows.Scan(&key, &value))
			parts = append(parts, value)
		}
		must(rows.Err())
		return strings.Join(parts, "\n")
	}
	var radio string
	must(radioQuery.QueryRow(targets[0]).Scan(&radio))
	report["point_plan"] = plan("EXPLAIN ANALYZE "+pointSQL, targets[0], radio)
	_, e = q.Exec("CREATE TEMP TABLE wanted AS SELECT DISTINCT target_key,ssid,mac FROM observations WHERE target_key>=? ORDER BY target_key LIMIT 256", targets[0])
	must(e)
	report["ordered_plan"] = plan("EXPLAIN ANALYZE SELECT o.id,o.ssid,o.mac,o.band,o.latitude,o.longitude,o.rssi_dbm FROM observations o JOIN wanted w ON o.target_key=w.target_key WHERE o.target_key>=? AND o.target_key<=? ORDER BY o.id", targets[0], targets[1])
	report["unordered_plan"] = plan("EXPLAIN SELECT o.id,o.ssid,o.mac,o.band,o.latitude,o.longitude,o.rssi_dbm FROM observations o JOIN wanted w ON o.ssid=w.ssid AND(o.ssid<>'' OR o.mac=w.mac) ORDER BY o.id")
	cols := "id,import_id,line,record_id,ssid,mac,band,band_source,radio_key,latitude,longitude,rssi_dbm,accuracy_m,first_seen,date,channel,frequency_mhz"
	fingerprint := func(table string) map[string]any {
		var count, sum int64
		var hash string
		must(q.QueryRow("SELECT count(*),cast(sum(id) AS BIGINT),cast(bit_xor(hash("+cols+")) AS VARCHAR) FROM "+table).Scan(&count, &sum, &hash))
		return map[string]any{"rows": count, "sum_ids": sum, "all_columns_xor_hash": hash}
	}
	report["ordered_fingerprint"] = fingerprint("observations")
	if *canonical != "" {
		if !owned(filepath.Dir(*canonical)) {
			panic("owned canonical required")
		}
		_, e = q.ExecContext(context.Background(), "ATTACH '"+strings.ReplaceAll(*canonical, "'", "''")+"' AS source (READ_ONLY)")
		must(e)
		report["canonical_fingerprint"] = fingerprint("source.observations")
	}
	must(json.NewEncoder(os.Stdout).Encode(report))
	fmt.Fprintln(os.Stderr, "Point plans and row fingerprints recorded; no file data changed.")
}
