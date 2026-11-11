package atlas

import (
	"context"
	"os"
	"path/filepath"
	"signal-atlas/internal/db"
	"strings"
	"testing"
	"time"
)

func sqlFixture(t *testing.T) *Atlas {
	t.Helper()
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "survey"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	file := filepath.Join(dir, "input.csv")
	err = os.WriteFile(file, []byte("id,MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n"+
		"9223372036854775807,02:00:00:00:00:01,Alpha,2026-11-01,2412,-60,48,-123,3,WIFI\n"+
		"text-id,02:00:00:00:00:02,Alpha,,,,48.1,-123.1,,BLE\n"+
		"text-id,02:00:00:00:00:02,Alpha,2026-11-02,5180,-50,48.1,-123.1,,WIFI\n"+
		"4,02:00:00:00:00:03,,2026-11-03,,-70,48.2,-123.2,,WIFI\n"+
		"5,02:00:00:00:00:04,Zero,,2412,-60,0,0,3,WIFI\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	return a
}
func TestSQLBrowserRealObservationsAndSourceFields(t *testing.T) {
	a := sqlFixture(t)
	ctx := context.Background()
	schema, err := a.SQLSchema(ctx)
	if err != nil || schema["observations"] != 3 {
		t.Fatal(schema, err)
	}
	r, err := a.SQLQuery(ctx, SQLRequest{SQL: "SELECT count(*),count(DISTINCT ssid) FROM observations"})
	if err != nil || r.Rows[0][0] != "3" || r.Rows[0][1] != "2" {
		t.Fatal(r, err)
	}
	r, err = a.SQLQuery(ctx, SQLRequest{SQL: "WITH chosen AS (SELECT id,record_id,ssid,frequency_mhz,accuracy_m,line FROM observations) SELECT * FROM chosen ORDER BY id; -- trailing comment", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 2 || !r.HasMore || r.Rows[0][1] != "9223372036854775807" || r.Rows[1][1] != "text-id" || r.Rows[1][4] != nil || r.Rows[1][5] != "4" {
		t.Fatal(r)
	}
	next, err := a.SQLQuery(ctx, SQLRequest{SQL: "SELECT id,ssid,frequency_mhz FROM observations ORDER BY id", Limit: 2, Offset: 2, Revision: r.Revision})
	if err != nil || len(next.Rows) != 1 || next.HasMore || next.Rows[0][1] != "" || next.Rows[0][2] != nil {
		t.Fatal(next, err)
	}
	for _, q := range []string{"SELECT count(*) FROM observations WHERE ssid='Alpha'", "SELECT count(*) FROM observations WHERE ssid='alpha' COLLATE NOCASE", "SELECT count(*) FROM observations WHERE ssid IN ('Alpha')"} {
		r, err = a.SQLQuery(ctx, SQLRequest{SQL: q})
		if err != nil || r.Rows[0][0] != "2" {
			t.Fatal(q, r, err)
		}
	}
	r, err = a.SQLQuery(ctx, SQLRequest{SQL: "SELECT count(*) FROM network_catalog"})
	if err != nil || r.Rows[0][0] != "2" {
		t.Fatal(r, err)
	}
	r, err = a.SQLQuery(ctx, SQLRequest{SQL: "SELECT count(*) FROM observations", Explain: true})
	if err != nil || len(r.Rows) == 0 {
		t.Fatal(r, err)
	}
}
func TestSQLBrowserRejectsWritesAndScripts(t *testing.T) {
	a := sqlFixture(t)
	for _, q := range []string{"DELETE FROM observations", "UPDATE imports SET filename='bad'", "DROP TABLE estimates", "CREATE TEMP TABLE bad(a)", "ATTACH ':memory:' AS bad", "PRAGMA query_only=OFF", "VACUUM", "BEGIN", "SELECT 1;SELECT 2", "SELECT 1; VACUUM", "SELECT 1; EXPLAIN DELETE FROM imports", "SELECT 1;DELETE FROM imports", "SELECT * FROM main.observations", "SELECT load_extension('bad')", "SELECT writefile('bad','x')"} {
		if r, err := a.SQLQuery(context.Background(), SQLRequest{SQL: q}); err == nil {
			t.Fatalf("accepted %s: %+v", q, r)
		}
	}
	r, err := a.SQLQuery(context.Background(), SQLRequest{SQL: "SELECT count(*) FROM observations"})
	if err != nil || r.Rows[0][0] != "3" {
		t.Fatal(r, err)
	}
}
func TestSQLBrowserLimitsValuesAndCancellation(t *testing.T) {
	a := sqlFixture(t)
	ctx := context.Background()
	r, err := a.SQLQuery(ctx, SQLRequest{SQL: "SELECT 9223372036854775807 exact,NULL missing,'' empty,'<script>bad</script>' markup,hex(zeroblob(3000)) long"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Rows[0][0] != "9223372036854775807" || r.Rows[0][1] != nil || r.Rows[0][2] != "" || r.TruncatedCells != 1 {
		t.Fatal(r)
	}
	for _, req := range []SQLRequest{{SQL: "SELECT 1", Limit: 1001}, {SQL: "SELECT 1", Offset: -1}, {SQL: "SELECT 1", TimeoutSeconds: 121}, {SQL: "SELECT 1", Revision: "stale"}, {SQL: "SELECT zeroblob(1000000000)"}, {SQL: "SELECT " + strings.Repeat("1,", 256) + "1"}} {
		if _, err = a.SQLQuery(ctx, req); err == nil {
			t.Fatal("accepted", req)
		}
	}
	cancelCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = a.SQLQuery(cancelCtx, SQLRequest{SQL: "WITH RECURSIVE n(v) AS (VALUES(1) UNION ALL SELECT v+1 FROM n) SELECT sum(v) FROM n"})
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("did not cancel", err, time.Since(start))
	}
	r, err = a.SQLQuery(ctx, SQLRequest{SQL: "SELECT 1; /* comment */"})
	if err != nil || r.Rows[0][0] != "1" {
		t.Fatal(r, err)
	}
}

func TestSQLBrowserCellAndBytePageBudgets(t *testing.T) {
	a := sqlFixture(t)
	columns := strings.TrimSuffix(strings.Repeat("'x',", 256), ",")
	r, err := a.SQLQuery(context.Background(), SQLRequest{SQL: "WITH RECURSIVE n(v) AS (VALUES(1) UNION ALL SELECT v+1 FROM n WHERE v<1000) SELECT " + columns + " FROM n", Limit: 1000})
	if err != nil || !r.HasMore || len(r.Rows) != 78 || len(r.Rows)*len(r.Columns) > 20_000 {
		t.Fatal(r, err)
	}
	columns = strings.TrimSuffix(strings.Repeat("hex(zeroblob(3000)),", 256), ",")
	r, err = a.SQLQuery(context.Background(), SQLRequest{SQL: "WITH n(v) AS (VALUES(1),(2),(3)) SELECT " + columns + " FROM n", Limit: 1000})
	if err != nil || !r.HasMore || len(r.Rows) != 1 || r.TruncatedCells != 256 {
		t.Fatal(r, err)
	}
	var bytes int
	for _, row := range r.Rows {
		for _, cell := range row {
			if cell != nil {
				bytes += len(cell.(string))
			}
		}
	}
	if bytes > 2_000_000 {
		t.Fatal("cell byte budget exceeded", bytes)
	}
}

func TestSQLBrowserRevisionCoversNonFitMetadata(t *testing.T) {
	a := sqlFixture(t)
	ctx := context.Background()
	r, err := a.SQLQuery(ctx, SQLRequest{SQL: "SELECT * FROM comparisons"})
	if err != nil {
		t.Fatal(err)
	}
	c, put, err := a.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(c, "INSERT INTO comparisons VALUES('sql-test','name','2026-11-10','{}')")
	put()
	if err != nil {
		t.Fatal(err)
	}
	newer, err := a.SQLQuery(ctx, SQLRequest{SQL: "SELECT * FROM comparisons"})
	if err != nil || newer.Revision == r.Revision || len(newer.Rows) != 1 {
		t.Fatal(newer, err)
	}
	if _, err = a.SQLQuery(ctx, SQLRequest{SQL: "SELECT * FROM comparisons", Revision: r.Revision}); err == nil {
		t.Fatal("accepted stale metadata page")
	}
}
