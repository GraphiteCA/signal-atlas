// Complete preparation using bounded DuckDB observation batches and the exact
// production Go grouping, sampler, solver and SQLite checkpoint/result writer.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"signal-atlas/tools/duckbench/candidate"
	"zombiezen.com/go/sqlite"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func owned(path string) bool {
	for range 6 {
		b, e := os.ReadFile(filepath.Join(path, "owner.json"))
		var m struct {
			Purpose string
			Schema  int
		}
		if e == nil && json.Unmarshal(b, &m) == nil && m.Purpose == "signal-atlas-benchmark" && m.Schema == 1 {
			return true
		}
		path = filepath.Dir(path)
	}
	return false
}
func main() {
	path := flag.String("db", "", "owned DuckDB observations")
	metadata := flag.String("metadata", "", "independent preparation.sqlite3 copied from the unfitted input")
	build := flag.String("build-from", "", "build canonical DuckDB observations from an owned native copy, then exit")
	workers := flag.Int("workers", 8, "Go fitting workers")
	solver := flag.String("solver", "fast", "fast or reference, matched to native run")
	orderedFrom := flag.String("order-from", "", "create new target-ordered DuckDB from owned canonical observations")
	ordered := flag.Bool("ordered", false, "read target-ordered observations with zonemap range pruning")
	duckThreads := flag.Int("duck-threads", 2, "DuckDB query threads")
	memory := flag.String("memory", "512MB", "DuckDB buffer budget; process RSS is measured separately")
	threads := flag.Int("threads", 8, "Go CPU parallelism")
	gpu := flag.Bool("gpu", false, "same versioned float64 GPU solver as native")
	rowGroup := flag.Int("row-group", 16384, "row group size for new ordered file")
	batchSize := flag.Int("batch", 256, "targets per bounded observation read")
	flag.Parse()
	runtime.GOMAXPROCS(*threads)
	ctx := context.Background()
	absolute, e := filepath.Abs(*path)
	must(e)
	if !owned(filepath.Dir(absolute)) {
		panic("owned benchmark database required")
	}
	if *build != "" || *orderedFrom != "" {
		if _, e := os.Stat(absolute); !os.IsNotExist(e) {
			panic("new DuckDB file required")
		}
		must(os.MkdirAll(filepath.Dir(absolute), 0755))
		if *build != "" && !owned(*build) {
			panic("owned canonical source required")
		}
	}
	if *orderedFrom != "" {
		buildOrdered(ctx, absolute, *orderedFrom, *rowGroup, *duckThreads, *memory)
		return
	}
	store, e := candidate.Open(absolute, *duckThreads, *memory)
	must(e)
	defer store.DB.Close()
	if *build != "" {
		buildCanonical(ctx, store, *build)
		return
	}
	if !owned(filepath.Dir(*metadata)) || *workers < 1 || *workers > 16 || *batchSize < 1 || *batchSize > 1024 {
		panic("owned metadata and bounded workers/batch required")
	}
	c, e := db.Open(*metadata, true)
	must(e)
	defer c.Close()
	must(db.Script(c, "PRAGMA journal_mode=WAL;"))
	n, e := db.Scalar(c, "SELECT count(*) FROM preparation_checkpoints")
	must(e)
	saved, e := db.Scalar(c, "SELECT count(*) FROM estimates")
	must(e)
	if db.Int(n) != 0 || db.Int(saved) != 0 {
		panic("fresh unfitted preparation metadata required")
	}
	must(atlas.EnsureLocationProjection(c))
	var observationCount int64
	must(store.DB.QueryRow("SELECT count(*) FROM observations").Scan(&observationCount))
	expected, e := db.Scalar(c, "SELECT sum(active_rows) FROM imports")
	must(e)
	if observationCount != db.Int(expected) {
		panic("canonical observation count mismatch")
	}
	settings := atlas.DefaultPreparationSettings()
	settings.Workers = *workers
	ns, e := atlas.NewPreparationNamespace(settings, *solver == "fast", *gpu)
	must(e)
	start := time.Now()
	must(db.WriteTx(c, func() error {
		if _, e := db.Exec(c, "INSERT INTO preparation_namespaces(namespace,scope_key,grouping,total) SELECT ?,?,?,count(*) FROM preparation_targets", ns.ID, ns.ScopeKey, ns.Grouping); e != nil {
			return e
		}
		_, e := db.Exec(c, "INSERT INTO preparation_pending(namespace,target_key,version) SELECT ?,target_key,version FROM preparation_targets ORDER BY target_key", ns.ID)
		return e
	}))
	total, e := db.Scalar(c, "SELECT count(*) FROM preparation_targets")
	must(e)
	targetCount := db.Int(total)
	_, e = db.Exec(c, "PRAGMA synchronous=NORMAL")
	must(e)
	catalog, e := db.OpenReadOnly(*metadata)
	must(e)
	defer catalog.Close()
	readConn, e := store.DB.Conn(ctx)
	must(e)
	defer readConn.Close()
	readSQL := `SELECT o.id,o.import_id,o.line,o.ssid,o.mac,o.band,o.band_source,o.radio_key,o.latitude,o.longitude,o.rssi_dbm,o.date FROM observations o JOIN wanted w ON o.ssid=w.ssid AND (o.ssid<>'' OR o.mac=w.mac) ORDER BY o.id`
	if *ordered {
		readSQL = `SELECT o.id,o.import_id,o.line,o.ssid,o.mac,o.band,o.band_source,o.radio_key,o.latitude,o.longitude,o.rssi_dbm,o.date FROM observations o JOIN wanted w ON o.target_key=w.target_key WHERE o.target_key>=? AND o.target_key<=? ORDER BY o.id`
	}

	_, e = readConn.ExecContext(ctx, "CREATE TEMP TABLE wanted(target_key VARCHAR,ssid VARCHAR,mac VARCHAR)")
	must(e)
	readQuery, e := readConn.PrepareContext(ctx, readSQL)
	must(e)
	defer readQuery.Close()
	type job struct {
		view *atlas.Atlas
		t    atlas.PreparationTarget
	}
	type output struct {
		prepared atlas.PreparedTarget
		seconds  float64
		err      error
	}
	jobs := make(chan job)
	results := make(chan output, *workers*2)
	readErr := make(chan error, 1)
	var readSeconds float64
	var batches int
	go func() {
		defer close(jobs)
		after := ""
		for {
			selected := []atlas.PreparationTarget{}
			e := db.Each(catalog, "SELECT target_key,ssid,mac,version FROM preparation_targets WHERE target_key>? ORDER BY target_key LIMIT ?", []any{after, *batchSize}, func(s *sqlite.Stmt) error {
				selected = append(selected, atlas.PreparationTarget{Key: s.ColumnText(0), SSID: s.ColumnText(1), MAC: s.ColumnText(2), Version: s.ColumnInt64(3)})
				return nil
			})
			if e != nil {
				readErr <- e
				return
			}
			if len(selected) == 0 {
				break
			}
			after = selected[len(selected)-1].Key
			t := time.Now()
			view, e := readBatch(ctx, readConn, readQuery, selected, *ordered)
			readSeconds += time.Since(t).Seconds()
			batches++
			if e != nil {
				readErr <- e
				return
			}
			for _, target := range selected {
				jobs <- job{view, target}
			}
		}
		readErr <- nil
	}()
	var wg sync.WaitGroup
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				t := time.Now()
				prepared, e := job.view.ComputeUncachedPreparationTarget(ctx, ns, job.t, (256<<20)/int64(*workers))
				results <- output{prepared, time.Since(t).Seconds(), e}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	pending := []atlas.PreparedTarget{}
	pendingBytes := 0
	commits := 0
	computeSeconds, commitSeconds := 0.0, 0.0
	completed := 0
	flush := func() {
		if len(pending) == 0 {
			return
		}
		t := time.Now()
		accepted, e := atlas.CommitPreparationResults(ctx, c, ns, pending)
		must(e)
		commitSeconds += time.Since(t).Seconds()
		completed += len(accepted)
		commits++
		pending = nil
		pendingBytes = 0
		if commits%100 == 0 {
			fmt.Fprintf(os.Stderr, "completed=%d seconds=%.2f\n", completed, time.Since(start).Seconds())
		}
	}
	for results != nil {
		select {
		case value, ok := <-results:
			if !ok {
				results = nil
				break
			}
			must(value.err)
			computeSeconds += value.seconds
			pending = append(pending, value.prepared)
			for _, write := range value.prepared.Writes {
				pendingBytes += len(write.Key) + len(write.Text)
			}
			if len(pending) >= 512 || pendingBytes >= 2<<20 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
	flush()
	must(<-readErr)
	seconds := time.Since(start).Seconds()
	stats, e := db.One(c, "SELECT count(*) completed,sum(fitted) fitted,sum(unavailable) unavailable FROM preparation_checkpoints")
	must(e)
	remaining, e := db.Scalar(c, "SELECT count(*) FROM preparation_pending")
	must(e)
	if db.Int(remaining) != 0 || db.Int(stats["completed"]) != targetCount {
		panic("incomplete sweep")
	}
	sum := sha256.New()
	must(db.Each(c, "SELECT radio_key,scope_key,result FROM estimates ORDER BY radio_key,scope_key", nil, func(s *sqlite.Stmt) error {
		for i := 0; i < 3; i++ {
			sum.Write([]byte(s.ColumnText(i)))
			sum.Write([]byte{0})
		}
		return nil
	}))
	must(json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "backend": "duckdb-batched", "streamed_target_catalog": true, "cache_durability": "WAL NORMAL; namespace seed FULL", "solver": *solver, "threads": *threads, "duckdb_threads": *duckThreads, "ordered": *ordered, "memory_limit": *memory, "gpu_requested": *gpu, "workers": *workers, "read_batch_targets": *batchSize, "rows": observationCount, "seconds": seconds, "targets_per_second": float64(targetCount) / seconds, "fitted_per_second": float64(db.Int(stats["fitted"])) / seconds, "stats": stats, "read_seconds": readSeconds, "read_batches": batches, "compute_seconds": computeSeconds, "commit_seconds": commitSeconds, "commit_batches": commits, "estimates_sha256": hex.EncodeToString(sum.Sum(nil))}))
}

func readBatch(ctx context.Context, connection *sql.Conn, query *sql.Stmt, targets []atlas.PreparationTarget, ordered bool) (*atlas.Atlas, error) {
	if _, e := connection.ExecContext(ctx, "DELETE FROM wanted"); e != nil {
		return nil, e
	}
	marks := make([]string, len(targets))
	args := make([]any, 0, len(targets)*3)
	for i, t := range targets {
		marks[i] = "(?,?,?)"
		args = append(args, t.Key, t.SSID, t.MAC)
	}
	if _, e := connection.ExecContext(ctx, "INSERT INTO wanted VALUES "+strings.Join(marks, ","), args...); e != nil {
		return nil, e
	}
	var argsRead []any
	if ordered {
		argsRead = []any{targets[0].Key, targets[len(targets)-1].Key}
	}
	rows, e := query.QueryContext(ctx, argsRead...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	dict := obs.NewDictionary()
	seg := obs.NewSegment(1, 1, len(targets)*64)
	for rows.Next() {
		if seg.N >= 1_000_000 {
			return nil, fmt.Errorf("observation batch exceeds the benchmark read budget")
		}
		var id, importID, line int64
		var ssid, mac, band, source, radio, date string
		var lat, lon, rssi float64
		if e = rows.Scan(&id, &importID, &line, &ssid, &mac, &band, &source, &radio, &lat, &lon, &rssi, &date); e != nil {
			return nil, e
		}
		row := obs.Row{SSID: dict.ID(ssid), MAC: dict.ID(mac), Band: dict.ID(band), BandSrc: dict.ID(source), Radio: dict.ID(radio), Date: dict.ID(date), Channel: dict.ID(""), First: dict.ID(""), Lat: lat, Lon: lon, RSSI: rssi, Acc: obs.Null, Freq: obs.Null, Line: line}
		seg.Append(&row)
		seg.OriginalIDs = append(seg.OriginalIDs, id)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	return &atlas.Atlas{Obs: obs.NewReadView([]*obs.Segment{seg}, dict)}, nil
}

func buildCanonical(ctx context.Context, s *candidate.Store, source string) {
	started := time.Now()
	a, e := atlas.Open(source)
	must(e)
	defer a.Close()
	st := a.Obs.State()
	conn, e := s.DB.Conn(ctx)
	must(e)
	defer conn.Close()
	must(conn.Raw(func(raw any) error {
		appender, e := duckdb.NewAppenderFromConn(raw.(driver.Conn), "main", "observations")
		if e != nil {
			return e
		}
		defer appender.Close()
		nullable := func(v float64) driver.Value {
			if obs.IsNull(v) {
				return nil
			}
			return v
		}
		for _, seg := range st.Segs {
			for i := 0; i < seg.N; i++ {
				record := strconv.FormatInt(seg.Line[i], 10)
				if seg.RecKind[i] == obs.RecInt {
					record = strconv.FormatInt(seg.RecVal[i], 10)
				} else if seg.RecKind[i] == obs.RecStr {
					record = st.Str[seg.RecVal[i]]
				}
				if e = appender.AppendRow(seg.ID(i), seg.ImportID, seg.Line[i], record, st.Str[seg.SSID[i]], st.Str[seg.MAC[i]], st.Str[seg.Band[i]], st.Str[seg.BandSrc[i]], st.Str[seg.Radio[i]], seg.Lat[i], seg.Lon[i], seg.RSSI[i], nullable(seg.Acc[i]), st.Str[seg.First[i]], st.Str[seg.Date[i]], st.Str[seg.Channel[i]], nullable(seg.Freq[i])); e != nil {
					return e
				}
			}
		}
		return appender.Close()
	}))
	_, e = s.DB.Exec("CHECKPOINT")
	must(e)
	var count int64
	must(s.DB.QueryRow("SELECT count(*) FROM observations").Scan(&count))
	if count != int64(st.N) {
		panic("canonical conversion count mismatch")
	}
	must(json.NewEncoder(os.Stdout).Encode(map[string]any{"rows": count, "conversion_seconds": time.Since(started).Seconds(), "note": "Unmeasured fixture setup; canonical source IDs, field values and row order are preserved."}))
}

// The only source connection is read-only. The fresh file keeps every canonical
// column and original ID; target ordering is a physical layout, not sampling.
func buildOrdered(ctx context.Context, path, source string, rowGroup, threads int, memory string) {
	src, e := filepath.Abs(source)
	must(e)
	if !owned(filepath.Dir(src)) {
		panic("owned canonical source required")
	}
	if rowGroup < 2048 || rowGroup%2048 != 0 {
		panic("row group must be a multiple of 2048")
	}
	started := time.Now()
	q, e := sql.Open("duckdb", "")
	must(e)
	defer q.Close()
	q.SetMaxOpenConns(1)
	literal := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	for _, sql := range []string{fmt.Sprintf("SET threads=%d", threads), "SET memory_limit=" + literal(memory), "SET preserve_insertion_order=false", "SET temp_directory=" + literal(path+".tmp"), "ATTACH " + literal(src) + " AS source (READ_ONLY)", fmt.Sprintf("ATTACH %s AS ordered (ROW_GROUP_SIZE %d)", literal(path), rowGroup), "CREATE TABLE ordered.atlas_meta AS SELECT 1 AS version", "CREATE TABLE ordered.observations AS SELECT *, CASE WHEN ssid<>'' THEN 'n:'||ssid ELSE 'h:'||upper(mac) END AS target_key FROM source.observations ORDER BY target_key,id", "CHECKPOINT ordered"} {
		_, e = q.ExecContext(ctx, sql)
		must(e)
	}
	var count int64
	must(q.QueryRowContext(ctx, "SELECT count(*) FROM ordered.observations").Scan(&count))
	must(json.NewEncoder(os.Stdout).Encode(map[string]any{"rows": count, "ordering_seconds": time.Since(started).Seconds(), "row_group_size": rowGroup, "source": src}))
}
