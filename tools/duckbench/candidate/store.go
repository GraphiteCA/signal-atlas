// Benchmark-only donor candidate; see README.md for origin and deliberate differences.
package candidate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	_ "github.com/duckdb/duckdb-go/v2"
)

type Row = map[string]any

type Store struct {
	DB    *sql.DB
	Path  string
	Write sync.Mutex
}

func Open(path string, threads int, memory string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		// Do not let DuckDB attempt to open or change an existing SQLite file.
		if f, err := os.Open(path); err == nil {
			var h [16]byte
			_, _ = f.Read(h[:])
			f.Close()
			if strings.HasPrefix(string(h[:]), "SQLite format 3") {
				return nil, fmt.Errorf("this is a legacy SQLite survey; choose a new .duckdb file and import the source CSVs (the SQLite file is unchanged)")
			}
		}
	}
	if threads < 1 {
		threads = min(runtime.NumCPU(), 16)
	}
	if memory == "" {
		memory = "2GB"
	}
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(6)
	s := &Store{DB: db, Path: path}
	fail := func(e error) (*Store, error) { db.Close(); return nil, e }
	for _, q := range []string{
		fmt.Sprintf("SET threads=%d", threads), "SET memory_limit=" + literal(memory),
		"SET preserve_insertion_order=false",
	} {
		if _, err = db.Exec(q); err != nil {
			return fail(err)
		}
	}
	var tables int
	if err = db.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema='main'").Scan(&tables); err != nil {
		return fail(err)
	}
	if tables > 0 {
		var version int
		if err = db.QueryRow("SELECT version FROM atlas_meta").Scan(&version); err != nil || version != 1 {
			return fail(fmt.Errorf("not a supported Signal Atlas desktop database"))
		}
	}
	_, err = db.Exec(schema)
	if err != nil {
		return fail(err)
	}
	// Adopt the existing optional local excerpt once, matching the explorer's
	// sidecar convention. Invalid optional context must not prevent startup.
	if path != ":memory:" {
		sidecar := filepath.Join(filepath.Dir(path), "basemap.geojson")
		if info, e := os.Stat(sidecar); e == nil && info.Size() <= 20_000_000 {
			if b, e := os.ReadFile(sidecar); e == nil {
				var context struct {
					Type     string            `json:"type"`
					Features []json.RawMessage `json:"features"`
				}
				if json.Unmarshal(b, &context) == nil && context.Type == "FeatureCollection" && context.Features != nil {
					if _, e = db.Exec("INSERT INTO map_assets SELECT 'basemap',? WHERE NOT EXISTS(SELECT 1 FROM map_assets WHERE name='basemap')", string(b)); e != nil {
						return fail(e)
					}
				}
			}
		}
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS atlas_meta(version INTEGER);
INSERT INTO atlas_meta SELECT 1 WHERE NOT EXISTS(SELECT 1 FROM atlas_meta);
CREATE TABLE IF NOT EXISTS imports(id BIGINT PRIMARY KEY, filename VARCHAR, sha256 VARCHAR UNIQUE,
 imported_utc VARCHAR, valid_rows BIGINT, invalid_rows BIGINT, non_wifi_rows BIGINT, elapsed_seconds DOUBLE);
CREATE TABLE IF NOT EXISTS observations(id BIGINT,import_id BIGINT,line BIGINT,record_id VARCHAR,
 ssid VARCHAR,mac VARCHAR,band VARCHAR,band_source VARCHAR,radio_key VARCHAR,
 latitude DOUBLE,longitude DOUBLE,rssi_dbm DOUBLE,accuracy_m DOUBLE,first_seen VARCHAR,date VARCHAR,
 channel VARCHAR,frequency_mhz DOUBLE);
CREATE TABLE IF NOT EXISTS radio_stats(radio_key VARCHAR PRIMARY KEY,ssid VARCHAR,mac VARCHAR,band VARCHAR,
 count BIGINT,peak DOUBLE,south DOUBLE,north DOUBLE,west DOUBLE,east DOUBLE,first_date VARCHAR,last_date VARCHAR);
CREATE OR REPLACE VIEW network_catalog AS SELECT ssid,sum(count)::BIGINT count,count(DISTINCT mac) macs,
 count(*) radios,max(peak) peak,string_agg(DISTINCT band,',') bands FROM radio_stats GROUP BY ssid;
CREATE OR REPLACE VIEW transmitter_catalog AS SELECT ssid,mac,sum(count)::BIGINT count,count(*) radio_count,
 string_agg(DISTINCT band,',') bands,max(peak) peak FROM radio_stats GROUP BY ssid,mac;
CREATE TABLE IF NOT EXISTS estimates(cache_key VARCHAR PRIMARY KEY,radio_key VARCHAR,scope_key VARCHAR,
 latitude DOUBLE,longitude DOUBLE,result VARCHAR);
CREATE TABLE IF NOT EXISTS comparisons(id VARCHAR PRIMARY KEY,payload VARCHAR);
CREATE TABLE IF NOT EXISTS map_assets(name VARCHAR PRIMARY KEY,payload VARCHAR);
CREATE TABLE IF NOT EXISTS band_assignments(ssid VARCHAR,mac VARCHAR,band VARCHAR,PRIMARY KEY(ssid,mac));
CREATE TABLE IF NOT EXISTS app_settings(name VARCHAR PRIMARY KEY,payload VARCHAR);
`

const statsSQL = `SELECT radio_key,ssid,mac,band,count(*)::BIGINT count,max(rssi_dbm) peak,
 min(latitude) south,max(latitude) north,min(longitude) west,max(longitude) east,
 min(date) first_date,max(date) last_date FROM observations GROUP BY radio_key,ssid,mac,band`

const clearEstimatesSQL = `DELETE FROM estimates; DELETE FROM app_settings WHERE name LIKE 'precompute_complete_%'`

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func ident(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func JSON(v any) string       { b, _ := json.Marshal(v); return string(b) }

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func query(ctx context.Context, db querier, q string, args ...any) ([]Row, error) {
	r, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	cols, err := r.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]Row, 0)
	for r.Next() {
		v := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range v {
			ptr[i] = &v[i]
		}
		if err = r.Scan(ptr...); err != nil {
			return nil, err
		}
		row := Row{}
		for i, k := range cols {
			row[k] = v[i]
		}
		out = append(out, row)
	}
	return out, r.Err()
}
func (s *Store) Query(ctx context.Context, q string, args ...any) ([]Row, error) {
	return query(ctx, s.DB, q, args...)
}
func (s *Store) One(ctx context.Context, q string, args ...any) (Row, error) {
	r, e := s.Query(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	if len(r) == 0 {
		return Row{}, nil
	}
	return r[0], nil
}
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
