package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
	"zombiezen.com/go/sqlite"
)

func TestWriteTransactionReservesBeforeReadAndRecoversCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.sqlite3")
	a, e := Open(path, false)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := Open(path, false)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	Exec(a, "PRAGMA journal_mode=WAL")
	Exec(a, "CREATE TABLE value(n INTEGER)")
	Exec(a, "INSERT INTO value VALUES(1)")
	// Reproduce the old deferred read-to-write snapshot failure deterministically.
	_, e = Exec(a, "BEGIN")
	if e != nil {
		t.Fatal(e)
	}
	Scalar(a, "SELECT n FROM value")
	Exec(b, "UPDATE value SET n=2")
	_, e = Exec(a, "UPDATE value SET n=3")
	if sqlite.ErrCode(e).ToPrimary() != sqlite.ResultBusy {
		t.Fatal("expected stale read upgrade", e)
	}
	Exec(a, "ROLLBACK")
	// Once the immediate writer has read, another writer must wait rather than
	// invalidate the snapshot. This is a correctness check, not a sleep benchmark.
	entered := make(chan struct{})
	released := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WriteTx(a, func() error {
			_, e := Scalar(a, "SELECT n FROM value")
			close(entered)
			<-released
			if e != nil {
				return e
			}
			_, e = Exec(a, "UPDATE value SET n=4")
			return e
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	b.SetInterrupt(ctx.Done())
	start := time.Now()
	_, e = Exec(b, "UPDATE value SET n=9")
	cancel()
	b.SetInterrupt(nil)
	if time.Since(start) > time.Second {
		t.Fatal("busy wait ignored cancellation")
	}
	if e == nil {
		t.Fatal("competing writer bypassed reservation")
	}
	close(released)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	ctx, cancel = context.WithCancel(context.Background())
	a.SetInterrupt(ctx.Done())
	e = WriteTx(a, func() error { Exec(a, "UPDATE value SET n=5"); cancel(); return ctx.Err() })
	a.SetInterrupt(nil)
	if e == nil || !a.AutocommitEnabled() {
		t.Fatal("cancelled transaction poisoned connection", e)
	}
	value, e := Scalar(a, "SELECT n FROM value")
	if e != nil || Int(value) != 4 {
		t.Fatal(value, e)
	}
	if e = WriteTx(a, func() error { _, e := Exec(a, "UPDATE value SET n=6"); return e }); e != nil {
		t.Fatal(e)
	}
}

func TestCachedStatementInterruptRollsBackAndCanBeReused(t *testing.T) {
	c, e := Open(filepath.Join(t.TempDir(), "cached.sqlite3"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if _, e = Exec(c, "CREATE TABLE state(n INTEGER)"); e != nil {
		t.Fatal(e)
	}
	if _, e = Exec(c, "INSERT INTO state VALUES(1)"); e != nil {
		t.Fatal(e)
	}
	const query = "WITH RECURSIVE seq(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM seq WHERE n<?) SELECT sum(n) FROM seq"
	if _, e = CachedScalar(c, query, 10); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c.SetInterrupt(ctx.Done())
	e = WriteTx(c, func() error {
		if _, e := CachedExec(c, "UPDATE state SET n=?", 2); e != nil {
			return e
		}
		_, e := CachedScalar(c, query, 100000000)
		return e
	})
	c.SetInterrupt(nil)
	if e == nil || !c.AutocommitEnabled() {
		t.Fatal("interrupted cached transaction survived", e)
	}
	v, e := Scalar(c, "SELECT n FROM state")
	if e != nil || Int(v) != 1 {
		t.Fatal("partial cached write", v, e)
	}
	v, e = CachedScalar(c, query, 10)
	if e != nil || Int(v) != 55 {
		t.Fatal("interrupted cached statement poisoned connection", v, e)
	}
	if e = WriteTx(c, func() error { _, e := CachedExec(c, "UPDATE state SET n=?", 3); return e }); e != nil {
		t.Fatal(e)
	}
}
