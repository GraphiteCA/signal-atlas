package atlas

import (
	"signal-atlas/internal/db"
	"zombiezen.com/go/sqlite"
)

// A rebuildable projection of saved locations. Keep large solver provenance
// JSON in estimates; pan/summary queries only need these small typed columns.
// Triggers cover every writer, including legacy/manual fit and deletion paths.
var locationSchema = `
DROP TRIGGER IF EXISTS map_fit_insert;
DROP TRIGGER IF EXISTS map_fit_update;
DROP TRIGGER IF EXISTS map_fit_delete;
CREATE TABLE IF NOT EXISTS map_locations(radio_key TEXT NOT NULL,scope_key TEXT NOT NULL,ssid TEXT NOT NULL,mac TEXT NOT NULL,band TEXT NOT NULL,latitude REAL NOT NULL,longitude REAL NOT NULL,shared INTEGER NOT NULL,used INTEGER NOT NULL,rms REAL NOT NULL,edge INTEGER NOT NULL,PRIMARY KEY(radio_key,scope_key));
CREATE TABLE IF NOT EXISTS located_networks(ssid TEXT PRIMARY KEY,locations INTEGER NOT NULL);
CREATE TRIGGER IF NOT EXISTS location_count_insert AFTER INSERT ON map_locations WHEN new.ssid<>'' BEGIN
 INSERT INTO located_networks VALUES(new.ssid,1) ON CONFLICT(ssid) DO UPDATE SET locations=locations+1; END;
CREATE TRIGGER IF NOT EXISTS location_count_delete AFTER DELETE ON map_locations WHEN old.ssid<>'' BEGIN
 UPDATE located_networks SET locations=locations-1 WHERE ssid=old.ssid;
 DELETE FROM located_networks WHERE ssid=old.ssid AND locations=0; END;
CREATE TRIGGER IF NOT EXISTS location_fit_insert AFTER INSERT ON estimates BEGIN
 DELETE FROM map_locations WHERE radio_key=new.radio_key AND scope_key=new.scope_key;
 INSERT INTO map_locations ` + locationSelect("new.radio_key", "new.scope_key", "new.result", "") + `;
END;
CREATE TRIGGER IF NOT EXISTS location_fit_update AFTER UPDATE ON estimates BEGIN
 DELETE FROM map_locations WHERE (radio_key=old.radio_key AND scope_key=old.scope_key) OR (radio_key=new.radio_key AND scope_key=new.scope_key);
 INSERT INTO map_locations ` + locationSelect("new.radio_key", "new.scope_key", "new.result", "") + `;
END;
CREATE TRIGGER IF NOT EXISTS location_fit_delete AFTER DELETE ON estimates BEGIN
 DELETE FROM map_locations WHERE radio_key=old.radio_key AND scope_key=old.scope_key;
END;
`

// CASE guards malformed externally supplied JSON. Only numeric valid locations
// qualify, matching MapSummary and the previous map decoder's coordinate bounds.
func locationSelect(key, scope, result, from string) string {
	return `SELECT ` + key + `,` + scope + `,coalesce(json_extract(` + result + `,'$.ssid'),''),coalesce(json_extract(` + result + `,'$.mac'),''),coalesce(json_extract(` + result + `,'$.band'),''),json_extract(` + result + `,'$.latitude'),json_extract(` + result + `,'$.longitude'),coalesce(json_extract(` + result + `,'$.co_location_assumed'),0),coalesce(json_extract(` + result + `,'$.used_observations'),0),coalesce(json_extract(` + result + `,'$.residual_rms_db'),0),coalesce(json_extract(` + result + `,'$.boundary_limited'),0)` + from + `
 WHERE CASE WHEN json_valid(` + result + `) THEN
 json_extract(` + result + `,'$.status')='fitted' AND json_type(` + result + `,'$.latitude') IN ('integer','real') AND json_type(` + result + `,'$.longitude') IN ('integer','real') AND json_extract(` + result + `,'$.latitude') BETWEEN -85.051129 AND 85.051129 AND json_extract(` + result + `,'$.longitude') BETWEEN -180 AND 180 ELSE 0 END`
}
func initializeLocations(c *sqlite.Conn) error {
	if err := db.Script(c, locationSchema); err != nil {
		return err
	}
	ready, err := db.Scalar(c, "SELECT value FROM preparation_meta WHERE name='map-locations-v1'")
	if err != nil {
		return err
	}
	if ready == nil {
		err = db.WriteTx(c, func() error {
			// The schema is installed once and the projection is backfilled atomically.
			// No observation segments or saved estimate JSON are rewritten.
			if _, err := db.Exec(c, "DELETE FROM map_locations"); err != nil {
				return err
			}
			query := locationSelect("radio_key", "scope_key", "result", " FROM estimates")
			if _, err := db.Exec(c, "INSERT INTO map_locations "+query); err != nil {
				return err
			}
			_, err := db.Exec(c, "INSERT INTO preparation_meta VALUES('map-locations-v1',1)")
			return err
		})
		if err != nil {
			return err
		}
	}
	return db.Script(c, locationRevisionSchema)
}

// An unavailable fit has no map location to invalidate. Keeping map revisions
// on the small projection avoids reloading the map for failed calculations.
const locationRevisionSchema = `
CREATE TRIGGER IF NOT EXISTS location_map_insert AFTER INSERT ON map_locations BEGIN
 UPDATE map_revision SET value=value+1 WHERE id=1;
 INSERT INTO map_changes SELECT value,new.radio_key,new.scope_key FROM map_revision WHERE id=1;
 DELETE FROM map_changes WHERE revision<=(SELECT value-2048 FROM map_revision WHERE id=1); END;
CREATE TRIGGER IF NOT EXISTS location_map_delete AFTER DELETE ON map_locations BEGIN
 UPDATE map_revision SET value=value+1 WHERE id=1;
 INSERT INTO map_changes SELECT value,old.radio_key,old.scope_key FROM map_revision WHERE id=1;
 DELETE FROM map_changes WHERE revision<=(SELECT value-2048 FROM map_revision WHERE id=1); END;
CREATE TRIGGER IF NOT EXISTS location_map_update AFTER UPDATE ON map_locations BEGIN
 UPDATE map_revision SET value=value+1 WHERE id=1;
 INSERT OR IGNORE INTO map_changes SELECT value,old.radio_key,old.scope_key FROM map_revision WHERE id=1;
 INSERT OR IGNORE INTO map_changes SELECT value,new.radio_key,new.scope_key FROM map_revision WHERE id=1;
 DELETE FROM map_changes WHERE revision<=(SELECT value-2048 FROM map_revision WHERE id=1); END;
`

// EnsureLocationProjection initializes the derived location tables for storage
// adapters that already own valid Atlas metadata, without loading observations.
func EnsureLocationProjection(c *sqlite.Conn) error { return initializeLocations(c) }
