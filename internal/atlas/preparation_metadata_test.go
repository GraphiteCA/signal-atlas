package atlas

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"signal-atlas/internal/db"
)

func TestPreparationMetadataMigrationPreservesDurableWork(t *testing.T) {
	a := preparationFixture(t)
	ns := preparationNamespace(t, a, "sample", "shared", true)
	targets, err := a.NextPreparationTargets(context.Background(), ns.ID, 32<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.PrepareTarget(context.Background(), ns, targets[0], 32<<20); err != nil {
		t.Fatal(err)
	}
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer put()
	if _, err := db.Exec(c, "UPDATE preparation_pending SET blocked_bytes=123456 WHERE target_key=?", targets[1].Key); err != nil {
		t.Fatal(err)
	}
	tables := []string{"preparation_targets", "preparation_pending", "preparation_checkpoints", "preparation_namespaces", "estimates", "map_locations", "located_networks"}
	snapshot := func() map[string][]db.Row {
		t.Helper()
		out := map[string][]db.Row{}
		for _, table := range tables {
			rows, err := db.All(c, "SELECT * FROM "+table+" ORDER BY 1")
			if err != nil {
				t.Fatal(err)
			}
			out[table] = rows
		}
		return out
	}
	before := snapshot()
	// Recreate the old rowid representation, including its duplicate ready
	// index, while retaining existing checkpoints, versions and blocked work.
	if err := db.WriteTx(c, func() error {
		if err := db.Script(c, "DROP TRIGGER preparation_enqueue;DROP TRIGGER preparation_dequeue;"); err != nil {
			return err
		}
		for _, table := range tables[:3] {
			v, err := db.Scalar(c, "SELECT sql FROM sqlite_schema WHERE name=?", table)
			if err != nil {
				return err
			}
			ddl := strings.TrimSuffix(strings.TrimSpace(db.Str(v)), "WITHOUT ROWID")
			if err := db.Script(c, "ALTER TABLE "+table+" RENAME TO old_metadata;"+ddl+";INSERT INTO "+table+" SELECT * FROM old_metadata;DROP TABLE old_metadata;"); err != nil {
				return err
			}
		}
		return db.Script(c, "CREATE INDEX preparation_ready ON preparation_pending(namespace,blocked_bytes,target_key);")
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := initializePreparationStorage(c); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, snapshot()) {
			t.Fatal("migration changed durable metadata or result JSON")
		}
	}
	if _, err := a.PrepareTarget(context.Background(), ns, targets[1], 32<<20); err != nil {
		t.Fatal(err)
	}
	progress, err := a.PreparationProgress(context.Background(), ns.ID, 32<<20)
	if err != nil || progress.Completed != 2 || progress.Pending != 1 {
		t.Fatal(progress, err)
	}
}

func TestPreparationBulkGuardRollsBackAfterProjectionPublication(t *testing.T) {
	a := preparationFixture(t)
	ns := preparationNamespace(t, a, "sample", "shared", true)
	targets, err := a.NextPreparationTargets(context.Background(), ns.ID, 32<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	batch := []PreparedTarget{}
	for _, target := range targets {
		p, err := a.ComputePreparationTarget(context.Background(), ns, target, 32<<20)
		if err != nil {
			t.Fatal(err)
		}
		batch = append(batch, p)
	}
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer put()
	before, err := db.Scalar(c, "SELECT value FROM map_revision")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Script(c, `CREATE TRIGGER reject_checkpoint BEFORE INSERT ON preparation_checkpoints BEGIN SELECT raise(ABORT,'injected checkpoint failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CommitPreparationBatch(context.Background(), ns, batch); err == nil {
		t.Fatal("injected failure ignored")
	}
	for _, table := range []string{"estimates", "map_locations", "located_networks", "preparation_checkpoints"} {
		n, err := db.Scalar(c, "SELECT count(*) FROM "+table)
		if err != nil || db.Int(n) != 0 {
			t.Fatal("partial publication", table, n, err)
		}
	}
	guard, err := db.Scalar(c, "SELECT active FROM metadata_batch_guard")
	if err != nil || db.Int(guard) != 0 {
		t.Fatal("guard escaped rollback", guard, err)
	}
	after, err := db.Scalar(c, "SELECT value FROM map_revision")
	if err != nil || after != before {
		t.Fatal("uncommitted map revision", after, before, err)
	}
	if err := db.Script(c, "DROP TRIGGER reject_checkpoint;"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CommitPreparationBatch(context.Background(), ns, batch); err != nil {
		t.Fatal(err)
	}
	progress, err := a.PreparationProgress(context.Background(), ns.ID, 32<<20)
	if err != nil || progress.Pending != 0 || progress.Completed != 3 {
		t.Fatal(progress, err)
	}
	check, err := db.Scalar(c, "SELECT value FROM map_revision")
	if err != nil || db.Int(check) != db.Int(before)+1 {
		t.Fatal("expected one revision per location batch", check, before, err)
	}
}

func TestPreparationBulkProjectionReplacementAndBoundedWholeRevisions(t *testing.T) {
	_, c, done := mapFixture(t, 3)
	defer done()
	ns := PreparationNamespace{ScopeKey: "bulk-scope"}
	writes := make([]PreparedEstimate, 2500)
	for i := range writes {
		writes[i] = PreparedEstimate{fmt.Sprintf("bulk-%04d", i), fmt.Sprintf(`{"status":"fitted","ssid":"Bulk","latitude":48.4,"longitude":%f}`, -123.4+float64(i)*1e-7)}
	}
	commit := func(w []PreparedEstimate) {
		t.Helper()
		if err := db.WriteTx(c, func() error {
			if err := commitPreparedEstimates(context.Background(), c, ns, []PreparedTarget{{Writes: w}}); err != nil {
				return err
			}
			_, err := db.Exec(c, "UPDATE metadata_batch_guard SET active=0")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	commit(writes)
	changes, err := db.Scalar(c, "SELECT count(*) FROM map_changes")
	if err != nil || db.Int(changes) != 0 {
		t.Fatal("oversized revision must be discarded in full", changes, err)
	}
	commit([]PreparedEstimate{{writes[0].Key, `{"status":"insufficient"}`}, {writes[1].Key, `{"status":"fitted","ssid":"Renamed","latitude":48.4,"longitude":-123.4}`}})
	counts, err := db.All(c, "SELECT ssid,locations FROM located_networks WHERE ssid IN ('Bulk','Renamed') ORDER BY ssid")
	want := []db.Row{{"ssid": "Bulk", "locations": int64(2498)}, {"ssid": "Renamed", "locations": int64(1)}}
	if err != nil || !reflect.DeepEqual(want, counts) {
		t.Fatal(counts, err)
	}
	// Ordinary SQL/manual writes retain the trigger path after a bulk commit.
	if _, err := db.Exec(c, "DELETE FROM estimates WHERE radio_key=? AND scope_key=?", writes[1].Key, ns.ScopeKey); err != nil {
		t.Fatal(err)
	}
	n, err := db.Scalar(c, "SELECT count(*) FROM located_networks WHERE ssid='Renamed'")
	if err != nil || db.Int(n) != 0 {
		t.Fatal(n, err)
	}
}
