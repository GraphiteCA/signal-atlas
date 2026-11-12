package atlas

import (
	"bytes"
	"testing"

	"signal-atlas/internal/db"
	"zombiezen.com/go/sqlite"
)

func TestPreservationInsertStatementLifecycle(t *testing.T) {
	c, err := db.Open(":memory:", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Conn.Close owns finalization of the cached statements.
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.Exec(c, "CREATE TABLE probe(id INTEGER UNIQUE, payload BLOB)"); err != nil {
		t.Fatal(err)
	}
	const insert = "INSERT INTO probe(id,payload) VALUES(?,?)"
	cached, err := c.Prepare(insert)
	if err != nil {
		t.Fatal(err)
	}
	assertCleared := func(t *testing.T) {
		t.Helper()
		// Use the original handle directly: another Prepare would itself clear
		// bindings and hide retained payloads or a partially bound failed row.
		if _, err := cached.Step(); err != nil {
			t.Fatal(err)
		}
		if err := cached.Reset(); err != nil {
			t.Fatal(err)
		}
		n, err := db.Scalar(c, "SELECT count(*) FROM probe WHERE id IS NULL AND payload IS NULL")
		if err != nil || db.Int(n) != 1 {
			t.Fatalf("bindings were retained: count=%v error=%v", n, err)
		}
		if _, err := db.Exec(c, "DELETE FROM probe WHERE id IS NULL"); err != nil {
			t.Fatal(err)
		}
	}
	payload := bytes.Repeat([]byte("payload"), 1024)
	for i := 1; i <= 1000; i++ {
		if err := preservationInsert(c, insert, i, payload); err != nil {
			t.Fatal(err)
		}
	}
	assertCleared(t)
	reused, err := c.Prepare(insert)
	if err != nil || reused != cached {
		t.Fatalf("bulk inserts did not reuse the cached statement: %v", err)
	}
	n, err := db.Scalar(c, "SELECT count(*) FROM probe WHERE payload=?", payload)
	if err != nil || db.Int(n) != 1000 {
		t.Fatalf("inserted payloads changed: count=%v error=%v", n, err)
	}
	for _, tc := range []struct {
		name   string
		values []any
		code   sqlite.ResultCode
	}{
		{"wrong arity", []any{2000}, 0},
		{"partially bound", []any{2000, struct{}{}}, 0},
		{"constraint failure", []any{1, payload}, sqlite.ResultConstraintUnique},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := preservationInsert(c, insert, tc.values...)
			if err == nil {
				t.Fatal("expected insert error")
			}
			if tc.code != 0 && sqlite.ErrCode(err) != tc.code {
				t.Fatalf("original SQLite error lost: %v", err)
			}
			assertCleared(t)
		})
	}
	if err := preservationInsert(c, insert, 2000, []byte("retry")); err != nil {
		t.Fatalf("retry after failures: %v", err)
	}
	assertCleared(t)
}
