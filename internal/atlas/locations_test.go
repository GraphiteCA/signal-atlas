package atlas

import (
	"context"
	"errors"
	"reflect"
	"signal-atlas/internal/db"
	"signal-atlas/internal/py"
	"testing"
)

func TestLocationProjectionSurvivesReplaceUpdateAndRollback(t *testing.T) {
	a, c, done := mapFixture(t, 3)
	defer done()
	verify := func() {
		t.Helper()
		want, err := db.All(c, `SELECT json_extract(result,'$.ssid') ssid,count(*) locations FROM estimates WHERE json_extract(result,'$.status')='fitted' AND json_type(result,'$.latitude') IN ('integer','real') AND json_type(result,'$.longitude') IN ('integer','real') AND json_extract(result,'$.latitude') BETWEEN -85.051129 AND 85.051129 AND json_extract(result,'$.longitude') BETWEEN -180 AND 180 AND json_extract(result,'$.ssid')<>'' GROUP BY 1 ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.All(c, "SELECT * FROM located_networks ORDER BY ssid")
		if err != nil || !reflect.DeepEqual(want, got) {
			t.Fatal(want, got, err)
		}
	}
	verify()
	sentinel := errors.New("rollback")
	if err := db.WriteTx(c, func() error {
		if _, err := db.Exec(c, "DELETE FROM estimates"); err != nil {
			return err
		}
		return sentinel
	}); err != sentinel {
		t.Fatal(err)
	}
	verify()
	for _, q := range []string{
		`INSERT OR REPLACE INTO estimates SELECT radio_key,scope_key,result FROM estimates LIMIT 1`,
		`UPDATE estimates SET result=json_set(result,'$.ssid','Renamed') WHERE rowid=(SELECT min(rowid) FROM estimates)`,
		`UPDATE estimates SET result=json_set(result,'$.status','insufficient') WHERE rowid=(SELECT min(rowid) FROM estimates)`,
		`UPDATE estimates SET result=json_set(result,'$.latitude',1000)`,
		`DELETE FROM estimates`,
	} {
		if _, err := db.Exec(c, q); err != nil {
			t.Fatal(err)
		}
		verify()
	}
	if _, err := a.MapFeatures(context.Background(), c, py.Params{"bbox": {"-124,48,-123,49"}}); err != nil {
		t.Fatal(err)
	}
}
func TestLocationProjectionBackfillPreservesSavedResults(t *testing.T) {
	a, c, done := mapFixture(t, 3)
	defer done()
	before, err := db.All(c, "SELECT * FROM estimates ORDER BY radio_key,scope_key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(c, "DELETE FROM preparation_meta WHERE name='map-locations-v1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(c, "DELETE FROM map_locations"); err != nil {
		t.Fatal(err)
	}
	if err := initializeLocations(c); err != nil {
		t.Fatal(err)
	}
	after, _ := db.All(c, "SELECT * FROM estimates ORDER BY radio_key,scope_key")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("backfill changed original fits")
	}
	n, _ := db.Scalar(c, "SELECT count(*) FROM map_locations")
	if db.Int(n) != 3 {
		t.Fatal(n)
	}
	summary, err := a.MapSummary(c)
	if err != nil || summary.LocatedSSIDs == 0 {
		t.Fatal(summary, err)
	}
}

func TestUnavailablePreparationDoesNotInvalidateTheMap(t *testing.T) {
	_, c, done := mapFixture(t, 3)
	defer done()
	before, err := db.Scalar(c, "SELECT value FROM map_revision")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(c, `INSERT INTO estimates VALUES('unavailable','scope','{"status":"insufficient","ssid":"Net0"}')`); err != nil {
		t.Fatal(err)
	}
	after, err := db.Scalar(c, "SELECT value FROM map_revision")
	if err != nil || after != before {
		t.Fatal("unavailable fit invalidated the map", before, after, err)
	}
	if _, err := db.Exec(c, `UPDATE estimates SET result='{"status":"fitted","ssid":"Net0","latitude":48.4,"longitude":-123.4}' WHERE radio_key='unavailable'`); err != nil {
		t.Fatal(err)
	}
	located, err := db.Scalar(c, "SELECT value FROM map_revision")
	if err != nil || db.Int(located) <= db.Int(after) {
		t.Fatal("new location was not published", located, err)
	}
	if _, err := db.Exec(c, `UPDATE estimates SET result='{"status":"insufficient"}' WHERE radio_key='unavailable'`); err != nil {
		t.Fatal(err)
	}
	removed, err := db.Scalar(c, "SELECT value FROM map_revision")
	if err != nil || db.Int(removed) <= db.Int(located) {
		t.Fatal("removed location was not published", removed, err)
	}
}
