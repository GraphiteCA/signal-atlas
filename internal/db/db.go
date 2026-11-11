// Package db is a thin layer over SQLite (zombiezen.com/go/sqlite: pure Go, no C compiler) that
// reads rows the way Python's sqlite3 does: INTEGER as int64, REAL as float64, TEXT as string,
// BLOB as []byte, NULL as nil, each row a map of column name to value.
package db

import (
	"fmt"
	"strings"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Row is one result row, as Python's dict(sqlite3.Row).
type Row = map[string]any

// Open opens a database read-write (creating it), with cancellable lock waits
// and, when fk, foreign keys on (Signal Atlas's connect()).
func Open(path string, fk bool) (*sqlite.Conn, error) {
	c, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenCreate|sqlite.OpenURI|sqlite.OpenNoMutex)
	if err != nil {
		return nil, err
	}
	if err := setup(c, fk); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// OpenReadOnly opens an existing database read-only.
func OpenReadOnly(path string) (*sqlite.Conn, error) {
	c, err := sqlite.OpenConn(path, sqlite.OpenReadOnly|sqlite.OpenURI|sqlite.OpenNoMutex)
	if err != nil {
		return nil, err
	}
	if err := setup(c, false); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func setup(c *sqlite.Conn, fk bool) error {
	// SQLite's sleeping busy_timeout handler does not promptly observe
	// SetInterrupt while waiting for a writer. The driver's handler selects on
	// the active context/connection cancellation channel instead.
	c.SetBlockOnBusy()
	if fk {
		return sqlitex.ExecuteTransient(c, "PRAGMA foreign_keys=ON", nil)
	}
	return nil
}

// Bind binds Python-style arguments (1-based).
func Bind(st *sqlite.Stmt, args []any) error {
	if n := st.BindParamCount(); n != len(args) {
		return fmt.Errorf("Incorrect number of bindings supplied. The current statement uses %d, and there are %d supplied.", n, len(args))
	}
	for i, a := range args {
		if err := BindOne(st, i+1, a); err != nil {
			return err
		}
	}
	return nil
}

// BindOne binds one Python-style value to parameter k (1-based).
func BindOne(st *sqlite.Stmt, k int, a any) error {
	switch v := a.(type) {
	case nil:
		st.BindNull(k)
	case int:
		st.BindInt64(k, int64(v))
	case int64:
		st.BindInt64(k, v)
	case float64:
		st.BindFloat(k, v)
	case string:
		st.BindText(k, v)
	case []byte:
		st.BindBytes(k, v)
	case bool:
		st.BindBool(k, v)
	default:
		return fmt.Errorf("Error binding parameter %d: type '%T' is not supported", k, a)
	}
	return nil
}

// Value reads column i with its SQLite storage type.
func Value(st *sqlite.Stmt, i int) any {
	switch st.ColumnType(i) {
	case sqlite.TypeInteger:
		return st.ColumnInt64(i)
	case sqlite.TypeFloat:
		return st.ColumnFloat(i)
	case sqlite.TypeText:
		return st.ColumnText(i)
	case sqlite.TypeBlob:
		b := make([]byte, st.ColumnLen(i))
		st.ColumnBytes(i, b)
		return b
	}
	return nil
}

// Columns are a statement's result column names.
func Columns(st *sqlite.Stmt) []string {
	n := st.ColumnCount()
	out := make([]string, n)
	for i := range out {
		out[i] = st.ColumnName(i)
	}
	return out
}

func prepare(c *sqlite.Conn, sql string, args []any) (*sqlite.Stmt, func(), error) {
	st, _, err := c.PrepareTransient(sql)
	if err != nil {
		return nil, nil, err
	}
	if err := Bind(st, args); err != nil {
		st.Finalize()
		return nil, nil, err
	}
	return st, func() { st.Finalize() }, nil
}

// Each runs a query and calls fn for every row with the open statement.
func Each(c *sqlite.Conn, sql string, args []any, fn func(st *sqlite.Stmt) error) error {
	st, done, err := prepare(c, sql, args)
	if err != nil {
		return err
	}
	defer done()
	for {
		ok, err := st.Step()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := fn(st); err != nil {
			return err
		}
	}
}

// All is cursor.execute(sql, args).fetchall() as dicts.
func All(c *sqlite.Conn, sql string, args ...any) ([]Row, error) {
	out := []Row{}
	var cols []string
	err := Each(c, sql, args, func(st *sqlite.Stmt) error {
		if cols == nil {
			cols = Columns(st)
		}
		r := make(Row, len(cols))
		for i, name := range cols {
			r[name] = Value(st, i)
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// One is fetchone() as a dict (nil when there is no row).
func One(c *sqlite.Conn, sql string, args ...any) (Row, error) {
	rows, err := All(c, sql, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// Scalar is the first column of the first row.
func Scalar(c *sqlite.Conn, sql string, args ...any) (any, error) {
	var v any
	err := Each(c, sql, args, func(st *sqlite.Stmt) error {
		if v == nil {
			v = Value(st, 0)
		}
		return nil
	})
	return v, err
}

// Exec runs one statement and returns sqlite3_changes().
func Exec(c *sqlite.Conn, sql string, args ...any) (int64, error) {
	st, done, err := prepare(c, sql, args)
	if err != nil {
		return 0, err
	}
	defer done()
	for {
		ok, err := st.Step()
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
	}
	return int64(c.Changes()), nil
}

// CachedEach reuses a fixed internal statement on its exclusively leased
// connection. Keep arbitrary SQL browser text on the transient path.
func CachedEach(c *sqlite.Conn, query string, args []any, fn func(*sqlite.Stmt) error) error {
	return sqlitex.Execute(c, query, &sqlitex.ExecOptions{Args: args, ResultFunc: fn})
}
func CachedExec(c *sqlite.Conn, query string, args ...any) (int64, error) {
	e := CachedEach(c, query, args, nil)
	return int64(c.Changes()), e
}
func CachedScalar(c *sqlite.Conn, query string, args ...any) (any, error) {
	var value any
	e := CachedEach(c, query, args, func(s *sqlite.Stmt) error {
		if value == nil {
			value = Value(s, 0)
		}
		return nil
	})
	return value, e
}

// Script runs several statements (executescript).
func Script(c *sqlite.Conn, sql string) error { return sqlitex.ExecuteScript(c, sql, nil) }

// Tx runs fn in a transaction (BEGIN … COMMIT, ROLLBACK on an error), as Python's `with conn:`.
// Inside an open transaction it just runs fn.
func Tx(c *sqlite.Conn, fn func() error) error { return transaction(c, "BEGIN", fn) }

// WriteTx reserves the writer before any version/checkpoint reads. This avoids
// a deferred read snapshot racing a concurrent commit and failing to upgrade.
// Read-only map snapshots deliberately continue to use Tx (deferred BEGIN).
func WriteTx(c *sqlite.Conn, fn func() error) error { return transaction(c, "BEGIN IMMEDIATE", fn) }
func transaction(c *sqlite.Conn, begin string, fn func() error) (err error) {
	if !c.AutocommitEnabled() {
		return fn()
	}
	if _, err = Exec(c, begin); err != nil {
		return fmt.Errorf("%s: %w", begin, err)
	}
	rollback := func() {
		if !c.AutocommitEnabled() {
			previous := c.SetInterrupt(nil)
			_, cleanup := Exec(c, "ROLLBACK")
			c.SetInterrupt(previous)
			if cleanup != nil {
				err = fmt.Errorf("%w; rollback: %v", err, cleanup)
			}
		}
	}
	defer func() {
		if p := recover(); p != nil {
			rollback()
			panic(p)
		}
		if err == nil {
			_, err = Exec(c, "COMMIT")
		}
		if err != nil {
			rollback()
		}
	}()
	return fn()
}

// Int is a SQLite integer (or nil as 0).
func Int(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

// Float is a SQLite number as float64.
func Float(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

// Str is a SQLite text value (or "").
func Str(v any) string { s, _ := v.(string); return s }

// Q quotes an identifier.
func Q(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// Marks is "?,?,?" for n values.
func Marks(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// Version is sqlite_version().
func Version() string {
	c, err := sqlite.OpenConn(":memory:")
	if err != nil {
		return ""
	}
	defer c.Close()
	v, _ := Scalar(c, "SELECT sqlite_version()")
	return Str(v)
}
