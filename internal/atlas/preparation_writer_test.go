package atlas

import (
	"context"
	"signal-atlas/internal/db"
	"testing"
)

func TestPreparationWriterDoesNotChangeImportDurability(t *testing.T) {
	a := preparationFixture(t)
	ns := preparationNamespace(t, a, "sample", "shared", true)
	targets, err := a.NextPreparationTargets(context.Background(), ns.ID, 256<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.PrepareTarget(context.Background(), ns, targets[0], 256<<20); err != nil {
		t.Fatal(err)
	}
	sync, err := db.Scalar(a.preparationWriter, "PRAGMA synchronous")
	if err != nil || db.Int(sync) != 1 {
		t.Fatal(sync, err)
	}
	for pragma, want := range map[string]int64{"cache_size": -32768, "temp_store": 2, "wal_autocheckpoint": 16384} {
		got, err := db.Scalar(a.preparationWriter, "PRAGMA "+pragma)
		if err != nil || db.Int(got) != want {
			t.Fatal("cache writer tuning", pragma, got, err)
		}
	}
	c, put, err := a.Conn(nil)
	if err != nil {
		t.Fatal(err)
	}
	sync, err = db.Scalar(c, "PRAGMA synchronous")
	put()
	if err != nil || db.Int(sync) != 2 {
		t.Fatal("import connection lost FULL durability", sync, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.CommitPreparationBatch(ctx, ns, nil); err == nil {
		t.Fatal("cancelled save accepted")
	}
	if _, err := a.CommitPreparationBatch(context.Background(), ns, nil); err != nil {
		t.Fatal("cancelled connection was not reusable", err)
	}
}
