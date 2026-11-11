package atlas

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"zombiezen.com/go/sqlite"
)

type SQLRequest struct {
	SQL            string `json:"sql"`
	Limit          int    `json:"limit"`
	Offset         int64  `json:"offset"`
	Explain        bool   `json:"explain"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Revision       string `json:"revision"`
	ID             string `json:"id"`
}
type SQLColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}
type SQLResult struct {
	Columns        []SQLColumn `json:"columns"`
	Rows           [][]any     `json:"rows"`
	Offset         int64       `json:"offset"`
	NextOffset     int64       `json:"next_offset"`
	HasMore        bool        `json:"has_more"`
	TruncatedCells int         `json:"truncated_cells"`
	ElapsedSeconds float64     `json:"elapsed_seconds"`
	Revision       string      `json:"revision"`
	Observations   int         `json:"observations"`
}

const observationDeclaration = `CREATE TABLE observations(id INTEGER,import_id INTEGER,line INTEGER,record_id TEXT,
 ssid TEXT,mac TEXT,band TEXT,band_source TEXT,radio_key TEXT,latitude REAL,longitude REAL,rssi_dbm REAL,
 accuracy_m REAL,first_seen TEXT,date TEXT,channel TEXT,frequency_mhz REAL)`

// A private read-only metadata connection and immutable observation State form
// one snapshot. No observations are copied, exported, or inserted into metadata.
// The writer lock is held only while capturing both halves, never during SQL.
func (a *Atlas) sqlSnapshot(ctx context.Context) (*sqlite.Conn, *obs.State, string, error) {
	for !a.WriteMu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, nil, "", ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	defer a.WriteMu.Unlock()
	a.sqlMu.Lock()
	defer a.sqlMu.Unlock()
	if a.sqlClosed {
		return nil, nil, "", fmt.Errorf("survey is closed")
	}
	if a.sqlVersion == nil {
		var e error
		a.sqlVersion, e = db.OpenReadOnly(a.MetaPath)
		if e != nil {
			return nil, nil, "", e
		}
	}
	a.sqlVersion.SetInterrupt(ctx.Done())
	defer a.sqlVersion.SetInterrupt(nil)
	c, err := db.OpenReadOnly(a.MetaPath)
	if err != nil {
		return nil, nil, "", err
	}
	fail := func(e error) (*sqlite.Conn, *obs.State, string, error) { c.Close(); return nil, nil, "", e }
	c.SetInterrupt(ctx.Done())
	for _, q := range []string{"PRAGMA cache_size=-2048", "PRAGMA temp_store=FILE"} {
		if _, err = db.Exec(c, q); err != nil {
			return fail(err)
		}
	}
	// data_version must be read from one persistent observer connection. A
	// newly opened connection starts its counter afresh and cannot detect changes
	// to comparisons/preferences or unavailable preparation results between pages.
	var revision int64
	for {
		before, e := db.Scalar(a.sqlVersion, "PRAGMA data_version")
		if e != nil {
			return fail(e)
		}
		if _, e = db.Exec(c, "BEGIN"); e != nil {
			return fail(e)
		}
		if _, e = db.Scalar(c, "SELECT value FROM map_revision WHERE id=1"); e != nil {
			return fail(e)
		}
		after, e := db.Scalar(a.sqlVersion, "PRAGMA data_version")
		if e != nil {
			return fail(e)
		}
		if db.Int(before) == db.Int(after) {
			revision = db.Int(after)
			break
		}
		if _, e = db.Exec(c, "ROLLBACK"); e != nil {
			return fail(e)
		}
		if e = ctx.Err(); e != nil {
			return fail(e)
		}
	}

	st := a.Obs.State()
	if err = c.SetModule("observation_source", &sqlite.Module{Connect: func(*sqlite.Conn, *sqlite.VTableConnectOptions) (sqlite.VTable, *sqlite.VTableConfig, error) {
		ordered := true
		var end int64
		for _, seg := range st.Segs {
			if seg.Base < end {
				ordered = false
			}
			end = seg.Base + int64(seg.N)
		}
		return &observationTable{st: st, ctx: ctx, ordered: ordered}, &sqlite.VTableConfig{Declaration: observationDeclaration, AllowIndirect: true}, nil
	}, UseConnectAsCreate: true}); err != nil {
		return fail(err)
	}
	// TEMP shadows the deliberately empty compatibility table in metadata. The
	// authorizer also rejects explicitly qualifying that empty main table.
	if err = db.Script(c, `CREATE VIRTUAL TABLE temp.observations USING observation_source;
 CREATE TEMP VIEW network_catalog AS SELECT ssid,count(*) count,count(DISTINCT mac) macs,count(DISTINCT radio_key) radios,max(rssi_dbm) peak,group_concat(DISTINCT band) bands FROM observations GROUP BY ssid;
 CREATE TEMP VIEW transmitter_catalog AS SELECT ssid,mac,count(*) count,count(DISTINCT band) radio_count,group_concat(DISTINCT band) bands,max(rssi_dbm) peak FROM observations GROUP BY ssid,mac;
 CREATE TEMP VIEW radio_catalog AS SELECT radio_key,ssid,mac,band,count(*) count,max(rssi_dbm) peak FROM observations GROUP BY radio_key;
 PRAGMA query_only=ON;`); err != nil {
		return fail(err)
	}
	c.Limit(sqlite.LimitLength, 2_000_000)
	c.Limit(sqlite.LimitSQLLength, 101_000)
	c.Limit(sqlite.LimitColumn, 256)
	c.Limit(sqlite.LimitAttached, 0)
	c.Limit(sqlite.LimitExprDepth, 100)
	c.Limit(sqlite.LimitVDBEOp, 100_000)
	c.Limit(sqlite.LimitWorkerThreads, 0)
	return c, st, digest(fmt.Sprintf("%p:%p:%d", st, a.sqlVersion, revision)), nil
}

func browserAuthorizer(a sqlite.Action) sqlite.AuthResult {
	switch a.Type() {
	case sqlite.OpSelect, sqlite.OpRecursive:
		return sqlite.AuthResultOK
	case sqlite.OpRead:
		if a.Database() == "main" && (a.Table() == "observations" || a.Table() == "spatial" || strings.HasPrefix(a.Table(), "spatial_")) {
			return sqlite.AuthResultDeny
		}
		return sqlite.AuthResultOK
	case sqlite.OpFunction:

		return sqlite.AuthResultOK
	default:
		return sqlite.AuthResultDeny
	}
}

// SQLQuery accepts exactly one engine-authorized read. Limits apply before JSON
// transfer; SQLite interruption and the virtual cursor both observe cancellation.
func (a *Atlas) SQLQuery(ctx context.Context, r SQLRequest) (*SQLResult, error) {
	if strings.TrimSpace(r.SQL) == "" || len(r.SQL) > 100_000 || strings.ContainsRune(r.SQL, 0) {
		return nil, fmt.Errorf("enter one read-only SQL query, at most 100 KB")
	}
	if r.Limit == 0 {
		r.Limit = 200
	}
	if r.Limit < 1 || r.Limit > 1000 || r.Offset < 0 || r.Offset > 1_000_000_000 {
		return nil, fmt.Errorf("page size must be 1–1,000 and offset 0–1,000,000,000")
	}
	if r.TimeoutSeconds == 0 {
		r.TimeoutSeconds = 30
	}
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > 120 {
		return nil, fmt.Errorf("timeout must be 1–120 seconds")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.TimeoutSeconds)*time.Second)
	defer cancel()
	start := time.Now()
	c, st, revision, err := a.sqlSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if r.Revision != "" && r.Revision != revision {
		return nil, fmt.Errorf("survey revision changed; run the query again before paging")
	}
	if err = c.SetAuthorizer(sqlite.AuthorizeFunc(browserAuthorizer)); err != nil {
		return nil, err
	}
	// The pinned SQLite wrapper converts generic function errors to text. Block
	// external helpers by overriding all relevant arities and checking a Go-side
	// denial flag after every native step, without invoking any external work.
	denied := false
	for _, name := range []string{"load_extension", "writefile", "readfile", "eval"} {
		for _, arity := range []int{-1, 1, 2, 3} {
			if err = c.CreateFunction(name, &sqlite.FunctionImpl{NArgs: arity, Scalar: func(sqlite.Context, []sqlite.Value) (sqlite.Value, error) { denied = true; return sqlite.Value{}, nil }}); err != nil {
				return nil, err
			}
		}
	}
	query := r.SQL
	if r.Explain {
		query = "EXPLAIN QUERY PLAN " + query
	}
	stmt, tail, err := c.PrepareTransient(query)
	if err != nil {
		return nil, fmt.Errorf("read-only SQL: %w", err)
	}
	if stmt != nil {
		defer stmt.Finalize()
	}
	if stmt == nil || stmt.ColumnCount() == 0 {
		return nil, fmt.Errorf("enter one SELECT or WITH query")
	}
	// SQLite parses the first statement. Only whitespace, empty semicolons
	// and comments may follow its native tail; a second non-row statement such
	// as VACUUM must also be rejected before any statement is executed.
	if !sqlTrailingTrivia(query[len(query)-tail:]) {
		return nil, fmt.Errorf("only one SQL statement is allowed")
	}

	result := &SQLResult{Columns: make([]SQLColumn, stmt.ColumnCount()), Rows: make([][]any, 0), Offset: r.Offset, Revision: revision, Observations: st.N}
	for i := range result.Columns {
		result.Columns[i].Name = stmt.ColumnName(i)
	}
	rowLimit := min(r.Limit, 20_000/max(1, len(result.Columns)))
	bytes := 0
	var skipped int64
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		more, e := stmt.Step()
		if denied {
			return nil, fmt.Errorf("external access is disabled in the SQL browser")
		}
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, e
		}
		if !more {
			break
		}
		if skipped < r.Offset && !r.Explain {
			skipped++
			continue
		}
		if len(result.Rows) >= rowLimit {
			result.HasMore = true
			break
		}
		row := make([]any, len(result.Columns))
		size := 0
		shortened := 0
		for i := range row {
			typ := stmt.ColumnType(i)
			if result.Columns[i].Type == "" || result.Columns[i].Type == "SQLITE_NULL" {
				result.Columns[i].Type = typ.String()
			}
			if typ == sqlite.TypeNull {
				continue
			}
			var cell string
			if typ == sqlite.TypeBlob {
				b := make([]byte, min(stmt.ColumnLen(i), 2048))
				stmt.ColumnBytes(i, b)
				cell = "0x" + hex.EncodeToString(b)
				if stmt.ColumnLen(i) > len(b) {
					cell += "…"
					shortened++
				}
			} else {
				cell = stmt.ColumnText(i)
				if len(cell) > 4096 {
					cell = cell[:4096]
					for !utf8.ValidString(cell) {
						cell = cell[:len(cell)-1]
					}
					cell += "…"
					shortened++
				}
			}
			row[i] = cell
			size += len(cell)
		}
		if bytes+size > 2_000_000 {
			result.HasMore = true
			break
		}
		bytes += size
		result.TruncatedCells += shortened
		result.Rows = append(result.Rows, row)
	}
	result.NextOffset = r.Offset + int64(len(result.Rows))
	result.ElapsedSeconds = time.Since(start).Seconds()
	return result, nil
}

func sqlTrailingTrivia(s string) bool {
	for len(s) > 0 {
		switch s[0] {
		case ' ', '\t', '\n', '\r', '\f', ';':
			s = s[1:]
			continue
		}
		if strings.HasPrefix(s, "--") {
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return true
			}
			s = s[i+1:]
			continue
		}
		if strings.HasPrefix(s, "/*") {
			i := strings.Index(s[2:], "*/")
			if i < 0 {
				return true
			}
			s = s[i+4:]
			continue
		}
		return false
	}
	return true
}

func (a *Atlas) SQLSchema(ctx context.Context) (map[string]any, error) {
	c, st, revision, err := a.sqlSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	rows, err := db.All(c, `SELECT 'observations' table_name,name column_name,type data_type FROM pragma_table_info('observations','temp')
 UNION ALL SELECT s.name,p.name,p.type FROM main.sqlite_schema s JOIN pragma_table_info(s.name,'main') p WHERE s.type='table' AND s.name NOT LIKE 'sqlite_%' AND s.name NOT LIKE 'spatial%' AND s.name<>'observations' ORDER BY 1,2`)
	return map[string]any{"columns": rows, "revision": revision, "observations": st.N, "engine": "SQLite SQL over immutable observation columns; metadata read snapshot", "dialect": "SQLite"}, err
}

type observationTable struct {
	st      *obs.State
	ctx     context.Context
	ordered bool
}

func (t *observationTable) Disconnect() error { return nil }
func (t *observationTable) Destroy() error    { return nil }
func (t *observationTable) Open() (sqlite.VTableCursor, error) {
	return &observationCursor{table: t}, nil
}
func (t *observationTable) BestIndex(in *sqlite.IndexInputs) (*sqlite.IndexOutputs, error) {
	out := &sqlite.IndexOutputs{EstimatedCost: float64(max(1, t.st.N)), EstimatedRows: int64(t.st.N), ConstraintUsage: make([]sqlite.IndexConstraintUsage, len(in.Constraints))}
	if t.ordered && len(in.OrderBy) == 1 && (in.OrderBy[0].Column == 0 || in.OrderBy[0].Column == -1) && !in.OrderBy[0].Desc {
		out.OrderByConsumed = true
	}
	// Reuse the map/fitter posting lists for binary equality on SSID or radio.
	for i, c := range in.Constraints {
		if c.Usable && c.Op == sqlite.IndexConstraintEq && c.Collation == "BINARY" && (c.Column == 4 || c.Column == 8) {
			out.ID.Num = int32(c.Column)
			out.ConstraintUsage[i].ArgvIndex = 1
			out.EstimatedCost = 100
			out.EstimatedRows = 100
			break
		}
	}
	return out, nil
}

type observationCursor struct {
	table   *observationTable
	g       int
	rows    []uint32
	indexed bool
	seg     int
	local   int
}

func (c *observationCursor) Filter(id sqlite.IndexID, args []sqlite.Value) error {
	c.g = 0
	c.seg = 0
	c.local = 0
	c.rows = nil
	c.indexed = len(args) > 0
	if c.indexed && args[0].Type() != sqlite.TypeNull {
		value := args[0].Text()
		index := c.table.st.BySSID
		if id.Num == 8 {
			index = c.table.st.ByRadio
		}
		for k, rows := range index {
			if c.table.st.Str[k] == value {
				c.rows = rows
				break
			}
		}
	}
	c.position()
	return c.table.ctx.Err()
}
func (c *observationCursor) position() {
	if c.EOF() {
		return
	}
	if c.indexed {
		s, i := c.table.st.Loc(c.rows[c.g])
		for j, seg := range c.table.st.Segs {
			if seg == s {
				c.seg = j
				break
			}
		}
		c.local = i
	} else {
		for c.seg < len(c.table.st.Segs) && c.local >= c.table.st.Segs[c.seg].N {
			c.local -= c.table.st.Segs[c.seg].N
			c.seg++
		}
	}
}
func (c *observationCursor) Next() error {
	c.g++
	if !c.indexed {
		c.local++
	}
	c.position()
	if c.g%1024 == 0 {
		return c.table.ctx.Err()
	}
	return nil
}
func (c *observationCursor) EOF() bool {
	if c.indexed {
		return c.g >= len(c.rows)
	}
	return c.g >= c.table.st.N
}
func (c *observationCursor) Close() error { return nil }
func (c *observationCursor) RowID() (int64, error) {
	s := c.table.st.Segs[c.seg]
	return s.Base + int64(c.local), nil
}
func (c *observationCursor) Column(col int, _ bool) (sqlite.Value, error) {
	s := c.table.st.Segs[c.seg]
	i := c.local
	st := c.table.st
	text := func(id uint32) sqlite.Value { return sqlite.TextValue(st.Str[id]) }
	real := func(v float64) sqlite.Value {
		if obs.IsNull(v) {
			return sqlite.Value{}
		}
		return sqlite.FloatValue(v)
	}
	switch col {
	case 0:
		return sqlite.IntegerValue(s.Base + int64(i)), nil
	case 1:
		return sqlite.IntegerValue(s.ImportID), nil
	case 2:
		return sqlite.IntegerValue(s.Line[i]), nil
	case 3:
		return sqlite.TextValue(recordID(st, s, i)), nil
	case 4:
		return text(s.SSID[i]), nil
	case 5:
		return text(s.MAC[i]), nil
	case 6:
		return text(s.Band[i]), nil
	case 7:
		return text(s.BandSrc[i]), nil
	case 8:
		return text(s.Radio[i]), nil
	case 9:
		return real(s.Lat[i]), nil
	case 10:
		return real(s.Lon[i]), nil
	case 11:
		return real(s.RSSI[i]), nil
	case 12:
		return real(s.Acc[i]), nil
	case 13:
		return text(s.First[i]), nil
	case 14:
		return text(s.Date[i]), nil
	case 15:
		return text(s.Channel[i]), nil
	case 16:
		return real(s.Freq[i]), nil
	}
	return sqlite.Value{}, fmt.Errorf("unknown observation column %s", strconv.Itoa(col))
}
