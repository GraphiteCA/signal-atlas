package atlas

import (
	"context"
	"encoding/json"
	"signal-atlas/internal/db"
	"strings"

	"zombiezen.com/go/sqlite"
)

// Small composite-key metadata belongs in one primary B-tree, rather than both
// a rowid table and a duplicate text-key index. Migrate in one transaction before
// opening the pool. Estimates keep their existing rowids and original JSON.
func initializePreparationStorage(c *sqlite.Conn) error {
	return db.WriteTx(c, func() error {
		for _, table := range []string{"preparation_targets", "preparation_pending", "preparation_checkpoints"} {
			v, err := db.Scalar(c, "SELECT sql FROM sqlite_schema WHERE name=? AND type='table'", table)
			if err != nil {
				return err
			}
			ddl := db.Str(v)
			if strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID") {
				continue
			}
			if err := db.Script(c, `DROP TRIGGER IF EXISTS preparation_enqueue;DROP TRIGGER IF EXISTS preparation_dequeue;`); err != nil {
				return err
			}
			// Identifiers come exclusively from the fixed table list above.
			if err := db.Script(c, "ALTER TABLE "+table+" RENAME TO preparation_storage_old;"+ddl+" WITHOUT ROWID;INSERT INTO "+table+" SELECT * FROM preparation_storage_old;DROP TABLE preparation_storage_old;"); err != nil {
				return err
			}
		}
		if err := db.Script(c, `DROP INDEX IF EXISTS preparation_ready;DROP TRIGGER IF EXISTS preparation_enqueue;DROP TRIGGER IF EXISTS preparation_dequeue;`); err != nil {
			return err
		}
		return db.Script(c, preparationSchema)
	})
}

// Called only inside the version-validated batch's immediate transaction. The
// transactional guard suppresses per-row derived-data triggers while this path
// publishes equivalent projections, network counts and map deltas in sets.
// SQLite permits only one writer: no other connection can observe active=1 or
// write through it. COMMIT publishes active=0; rollback/crash restores it too.
func commitPreparedEstimates(ctx context.Context, c *sqlite.Conn, ns PreparationNamespace, accepted []PreparedTarget) error {
	if _, err := db.CachedExec(c, "UPDATE metadata_batch_guard SET active=1 WHERE id=1"); err != nil {
		return err
	}
	writes := make([][2]string, 0)
	for _, p := range accepted {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.BlockedBytes == 0 {
			for _, w := range p.Writes {
				writes = append(writes, [2]string{w.Key, w.Text})
			}
		}
	}
	if len(writes) == 0 {
		return nil
	}
	data, err := json.Marshal(writes)
	if err != nil {
		return err
	}
	if err := db.Script(c, `CREATE TEMP TABLE IF NOT EXISTS preparation_estimates(radio_key TEXT PRIMARY KEY,result TEXT NOT NULL CHECK(json_valid(result))) WITHOUT ROWID;
CREATE TEMP TABLE IF NOT EXISTS preparation_network_delta(ssid TEXT PRIMARY KEY,locations INTEGER NOT NULL) WITHOUT ROWID;
CREATE TEMP TABLE IF NOT EXISTS preparation_changed(radio_key TEXT PRIMARY KEY) WITHOUT ROWID;
DELETE FROM preparation_estimates;DELETE FROM preparation_network_delta;DELETE FROM preparation_changed;`); err != nil {
		return err
	}
	if _, err := db.CachedExec(c, `INSERT OR REPLACE INTO preparation_estimates SELECT json_extract(value,'$[0]'),json_extract(value,'$[1]') FROM json_each(?) ORDER BY cast(key AS INTEGER)`, string(data)); err != nil {
		return err
	}
	if _, err := db.CachedExec(c, `INSERT INTO preparation_network_delta SELECT ssid,-count(*) FROM map_locations WHERE scope_key=? AND radio_key IN (SELECT radio_key FROM preparation_estimates) AND ssid<>'' GROUP BY ssid`, ns.ScopeKey); err != nil {
		return err
	}
	if _, err := db.CachedExec(c, `INSERT INTO preparation_changed SELECT radio_key FROM map_locations WHERE scope_key=? AND radio_key IN (SELECT radio_key FROM preparation_estimates)`, ns.ScopeKey); err != nil {
		return err
	}
	if _, err := db.CachedExec(c, `DELETE FROM map_locations WHERE scope_key=? AND radio_key IN (SELECT radio_key FROM preparation_estimates)`, ns.ScopeKey); err != nil {
		return err
	}
	if _, err := db.CachedExec(c, `INSERT OR REPLACE INTO estimates SELECT radio_key,?,result FROM preparation_estimates`, ns.ScopeKey); err != nil {
		return err
	}
	if _, err := db.CachedExec(c, `INSERT INTO map_locations `+locationSelect("radio_key", "?", "result", " FROM preparation_estimates"), ns.ScopeKey); err != nil {
		return err
	}
	if _, err := db.CachedExec(c, `INSERT INTO preparation_network_delta SELECT ssid,count(*) FROM map_locations WHERE scope_key=? AND radio_key IN (SELECT radio_key FROM preparation_estimates) AND ssid<>'' GROUP BY ssid ON CONFLICT(ssid) DO UPDATE SET locations=locations+excluded.locations`, ns.ScopeKey); err != nil {
		return err
	}
	if err := db.Script(c, `INSERT INTO located_networks SELECT ssid,locations FROM preparation_network_delta WHERE locations<>0 ON CONFLICT(ssid) DO UPDATE SET locations=locations+excluded.locations;
DELETE FROM located_networks WHERE ssid IN (SELECT ssid FROM preparation_network_delta) AND locations=0;`); err != nil {
		return err
	}
	if _, err := db.CachedExec(c, `INSERT OR IGNORE INTO preparation_changed SELECT radio_key FROM map_locations WHERE scope_key=? AND radio_key IN (SELECT radio_key FROM preparation_estimates)`, ns.ScopeKey); err != nil {
		return err
	}
	if err := db.Script(c, `UPDATE map_revision SET value=value+1 WHERE id=1 AND EXISTS(SELECT 1 FROM preparation_changed);`); err != nil {
		return err
	}
	if _, err := db.CachedExec(c, `INSERT INTO map_changes SELECT r.value,p.radio_key,? FROM preparation_changed p CROSS JOIN map_revision r WHERE r.id=1`, ns.ScopeKey); err != nil {
		return err
	}
	// Drop complete revisions, never half a batch: an incomplete change log
	// forces the existing map reader to reload rather than retaining stale APs.
	return db.Script(c, `DELETE FROM map_changes WHERE revision<=(SELECT revision FROM map_changes ORDER BY revision DESC LIMIT 1 OFFSET 2048);`)
}
