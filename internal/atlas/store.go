package atlas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"signal-atlas/internal/db"
	"signal-atlas/internal/geo"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Atlas is one Signal Atlas store: a folder holding atlas.sqlite3 (imports, estimates, comparisons,
// band assignments, the basemap, in Graphite's schema; its observations table stays empty) and
// observations/ (the column store with every observation).
type Atlas struct {
	Root      string // logical workspace, including a generation pointer after maintenance
	Recovered bool
	Dir       string
	MetaPath  string
	Obs       *obs.Store
	pool      *sqlitex.Pool
	closeOnce sync.Once
	closeErr  error
	// WriteMu serialises the writers (imports, band assignments, the bridge); queries never wait.
	WriteMu                 sync.Mutex
	mapCache                mapCache
	beforePreparationCommit func() // optional deterministic commit-boundary diagnostics
}

const schemaSQL = `
  CREATE TABLE IF NOT EXISTS imports(id INTEGER PRIMARY KEY, filename TEXT NOT NULL,
    sha256 TEXT UNIQUE NOT NULL, imported_utc TEXT NOT NULL, valid_rows INTEGER NOT NULL DEFAULT 0,
    invalid_rows INTEGER NOT NULL DEFAULT 0);
  CREATE TABLE IF NOT EXISTS observations(
    id INTEGER PRIMARY KEY, import_id INTEGER NOT NULL REFERENCES imports(id), line INTEGER NOT NULL,
    record_id TEXT, ssid TEXT NOT NULL, mac TEXT NOT NULL, band TEXT NOT NULL, band_source TEXT,
    radio_key TEXT NOT NULL, latitude REAL NOT NULL, longitude REAL NOT NULL, rssi_dbm REAL NOT NULL,
    accuracy_m REAL, first_seen TEXT, date TEXT, channel TEXT, frequency_mhz REAL);
  CREATE VIRTUAL TABLE IF NOT EXISTS spatial USING rtree(id,min_lat,max_lat,min_lon,max_lon);
  CREATE TRIGGER IF NOT EXISTS observation_insert AFTER INSERT ON observations BEGIN
    INSERT INTO spatial VALUES(new.id,new.latitude,new.latitude,new.longitude,new.longitude); END;
  CREATE TRIGGER IF NOT EXISTS observation_delete AFTER DELETE ON observations BEGIN
    DELETE FROM spatial WHERE id=old.id; END;
  CREATE INDEX IF NOT EXISTS observation_network ON observations(ssid,band);
  CREATE INDEX IF NOT EXISTS observation_radio ON observations(radio_key,rssi_dbm DESC);
  CREATE INDEX IF NOT EXISTS observation_radio_date ON observations(radio_key,date);
  CREATE INDEX IF NOT EXISTS observation_date ON observations(date);
  CREATE INDEX IF NOT EXISTS observation_transmitter ON observations(ssid,mac,band);
  CREATE INDEX IF NOT EXISTS observation_mac ON observations(mac,ssid);
  CREATE TABLE IF NOT EXISTS estimates(radio_key TEXT NOT NULL,scope_key TEXT NOT NULL,
    result TEXT NOT NULL,PRIMARY KEY(radio_key,scope_key));
  CREATE TABLE IF NOT EXISTS map_assets(name TEXT PRIMARY KEY,payload TEXT NOT NULL);
  CREATE TABLE IF NOT EXISTS map_revision(id INTEGER PRIMARY KEY CHECK(id=1),value INTEGER NOT NULL);
  INSERT OR IGNORE INTO map_revision VALUES(1,0);
  CREATE TABLE IF NOT EXISTS map_changes(revision INTEGER NOT NULL,radio_key TEXT NOT NULL,scope_key TEXT NOT NULL,PRIMARY KEY(revision,radio_key,scope_key));
  CREATE TRIGGER IF NOT EXISTS map_fit_insert AFTER INSERT ON estimates BEGIN
    UPDATE map_revision SET value=value+1 WHERE id=1;
    INSERT INTO map_changes SELECT value,new.radio_key,new.scope_key FROM map_revision WHERE id=1;
    DELETE FROM map_changes WHERE revision<=(SELECT value-2048 FROM map_revision WHERE id=1); END;
  CREATE TRIGGER IF NOT EXISTS map_fit_update AFTER UPDATE ON estimates BEGIN
    UPDATE map_revision SET value=value+1 WHERE id=1;
    INSERT OR IGNORE INTO map_changes SELECT value,old.radio_key,old.scope_key FROM map_revision WHERE id=1;
    INSERT OR IGNORE INTO map_changes SELECT value,new.radio_key,new.scope_key FROM map_revision WHERE id=1;
    DELETE FROM map_changes WHERE revision<=(SELECT value-2048 FROM map_revision WHERE id=1); END;
  CREATE TRIGGER IF NOT EXISTS map_fit_delete AFTER DELETE ON estimates BEGIN
    UPDATE map_revision SET value=value+1 WHERE id=1;
    INSERT OR IGNORE INTO map_changes SELECT value,old.radio_key,old.scope_key FROM map_revision WHERE id=1;
    DELETE FROM map_changes WHERE revision<=(SELECT value-2048 FROM map_revision WHERE id=1); END;
  CREATE TABLE IF NOT EXISTS comparisons(id TEXT PRIMARY KEY,name TEXT NOT NULL,
    created_utc TEXT NOT NULL,payload TEXT NOT NULL);
  CREATE TABLE IF NOT EXISTS band_assignments(ssid TEXT NOT NULL,mac TEXT NOT NULL,
    band TEXT NOT NULL,updated_utc TEXT NOT NULL,PRIMARY KEY(ssid,mac));
  CREATE VIEW IF NOT EXISTS network_catalog AS SELECT ssid,count(*) count,
    count(DISTINCT mac) macs,count(DISTINCT radio_key) radios,max(rssi_dbm) peak,
    group_concat(DISTINCT band) bands FROM observations GROUP BY ssid;
  CREATE VIEW IF NOT EXISTS transmitter_catalog AS SELECT ssid,mac,count(*) count,
    count(DISTINCT band) radio_count,group_concat(DISTINCT band) bands,max(rssi_dbm) peak
    FROM observations GROUP BY ssid,mac;
  CREATE VIEW IF NOT EXISTS radio_catalog AS SELECT radio_key,ssid,mac,band,count(*) count,
    max(rssi_dbm) peak FROM observations GROUP BY radio_key;
  CREATE INDEX IF NOT EXISTS estimate_scope_lookup ON estimates(scope_key);
  CREATE INDEX IF NOT EXISTS estimate_located_ssid ON estimates(json_extract(result,'$.ssid')) WHERE
    json_extract(result,'$.status')='fitted' AND json_type(result,'$.latitude') IN ('integer','real')
    AND json_type(result,'$.longitude') IN ('integer','real') AND json_extract(result,'$.latitude') BETWEEN -85.051129 AND 85.051129
    AND json_extract(result,'$.longitude') BETWEEN -180 AND 180;
  CREATE INDEX IF NOT EXISTS estimate_viewport ON estimates(
    scope_key,json_extract(result,'$.longitude'),json_extract(result,'$.latitude'))
    WHERE json_extract(result,'$.status')='fitted';
`

func prepareConn(c *sqlite.Conn) error {
	if err := sqlitex.ExecuteTransient(c, "PRAGMA busy_timeout=30000", nil); err != nil {
		return err
	}
	return sqlitex.ExecuteTransient(c, "PRAGMA foreign_keys=ON", nil)
}

// Open opens (or creates) the store in dir.
func Open(dir string) (*Atlas, error) {
	root := dir
	var err error
	if dir, err = WorkspaceDir(root); err != nil {
		return nil, err
	}
	var a *Atlas
	var openErr error
	// A published generation must already exist. Never turn a missing revision
	// into an empty database while attempting recovery.
	if _, err := os.Stat(filepath.Join(root, pointerFile)); err == nil {
		_, openErr = os.Stat(filepath.Join(dir, "atlas.sqlite3"))
	}
	if openErr == nil {
		a, openErr = openGeneration(root, dir)
	}
	if openErr == nil {
		return a, nil
	}
	p, err := ReadWorkspacePointer(root)
	if err != nil || p.Previous == "" || p.Previous == p.Active {
		return nil, openErr
	}
	previous, err := OpenGeneration(root, p.Previous)
	if err != nil {
		return nil, openErr
	}
	if err := previous.Validate(context.Background()); err != nil {
		previous.Close()
		return nil, openErr
	}
	if err := PublishWorkspace(root, p.Previous, p.Active); err != nil {
		previous.Close()
		return nil, openErr
	}
	previous.Recovered = true
	return previous, nil
}
func OpenGeneration(root, relative string) (*Atlas, error) {
	dir, err := workspaceChild(root, relative)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "atlas.sqlite3")); err != nil {
		return nil, err
	}
	return openGeneration(root, dir)
}
func openGeneration(root, dir string) (*Atlas, error) {
	return openGenerationWith(root, dir, nil)
}
func openGenerationWith(root, dir string, columns *obs.Store) (*Atlas, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	a := &Atlas{Root: root, Dir: dir, MetaPath: filepath.Join(dir, "atlas.sqlite3")}
	c, err := db.Open(a.MetaPath, true)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := initialize(c, a.MetaPath); err != nil {
		return nil, err
	}
	committed := map[int64]int64{}
	rows, err := db.All(c, "SELECT id,active_rows FROM imports")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		committed[db.Int(r["id"])] = db.Int(r["active_rows"])
	}
	if columns != nil {
		a.Obs = columns
	} else {
		if a.Obs, err = obs.Open(filepath.Join(dir, "observations"), committed); err != nil {
			return nil, err
		}
	}
	// Missing committed data is an error; never silently discard source history.
	have := map[int64]int64{}
	for _, s := range a.Obs.State().Segs {
		have[s.ImportID] += int64(s.N)
	}
	for _, r := range rows {
		if id := db.Int(r["id"]); db.Int(r["active_rows"]) != have[id] {
			return nil, py.Errorf("Workspace is incomplete: import %d has committed rows but its observation segments are missing", id)
		}
	}
	if err := a.initializePreparationCatalog(c); err != nil {
		return nil, err
	}
	a.pool, err = sqlitex.NewPool(a.MetaPath, sqlitex.PoolOptions{PoolSize: 16, PrepareConn: prepareConn})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Close closes the store.
func (a *Atlas) Close() error {
	a.closeOnce.Do(func() { a.closeErr = a.pool.Close() })
	return a.closeErr
}

// Conn takes a metadata connection; call the returned function to give it back.
func (a *Atlas) Conn(ctx context.Context) (*sqlite.Conn, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c, err := a.pool.Take(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c, func() { a.pool.Put(c) }, nil
}

// initialize is survey_data.initialize on the metadata database.
func initialize(c *sqlite.Conn, path string) error {
	v, err := db.Scalar(c, "PRAGMA user_version")
	if err != nil {
		return err
	}
	if db.Int(v) > SchemaVersion {
		return py.Errorf("This database requires a newer Signal Atlas version")
	}
	rows, err := db.All(c, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return err
	}
	tables := map[string]bool{}
	for _, r := range rows {
		tables[db.Str(r["name"])] = true
	}
	if len(tables) > 0 && !(tables["observations"] && tables["imports"]) {
		return py.Errorf("This is not a Signal Atlas database; choose a new file or an existing Signal Atlas store")
	}
	if _, err := db.Exec(c, "PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	if !tables["map_changes"] {
		if err := db.Script(c, "DROP TRIGGER IF EXISTS map_fit_insert;DROP TRIGGER IF EXISTS map_fit_update;DROP TRIGGER IF EXISTS map_fit_delete;"); err != nil {
			return err
		}
	}
	if err := db.Script(c, schemaSQL+preparationSchema); err != nil {
		return err
	}
	cols, err := db.All(c, "PRAGMA table_info(imports)")
	if err != nil {
		return err
	}
	has := false
	for _, r := range cols {
		has = has || db.Str(r["name"]) == "non_wifi_rows"
	}
	if !has {
		if _, err := db.Exec(c, "ALTER TABLE imports ADD COLUMN non_wifi_rows INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	for _, column := range []string{"zero_excluded_rows INTEGER NOT NULL DEFAULT 0", "zero_removed_rows INTEGER NOT NULL DEFAULT 0", "active_rows INTEGER NOT NULL DEFAULT -1"} {
		name := strings.Fields(column)[0]
		found := false
		for _, r := range cols {
			found = found || db.Str(r["name"]) == name
		}
		if !found {
			if _, err := db.Exec(c, "ALTER TABLE imports ADD COLUMN "+column); err != nil {
				return err
			}
		}
	}
	if _, err := db.Exec(c, "UPDATE imports SET active_rows=valid_rows WHERE active_rows<0"); err != nil {
		return err
	}
	if _, err := db.Exec(c, "PRAGMA user_version=5"); err != nil {
		return err
	}
	// Adopt an old basemap sidecar once; the database is authoritative thereafter.
	sidecar := filepath.Join(filepath.Dir(path), "basemap.geojson")
	if r, _ := db.One(c, "SELECT 1 x FROM map_assets WHERE name='basemap'"); r == nil {
		if payload, err := os.ReadFile(sidecar); err == nil && len(payload) <= 20000000 && validBasemap(payload) {
			db.Exec(c, "INSERT INTO map_assets VALUES('basemap',?)", compactJSON(payload))
		}
	}
	return nil
}

// LoadBasemap is load_basemap: the stored GeoJSON, or null.
func (a *Atlas) LoadBasemap(c *sqlite.Conn) (py.Raw, error) {
	v, err := db.Scalar(c, "SELECT payload FROM map_assets WHERE name='basemap'")
	if err != nil || v == nil {
		return "null", err
	}
	return py.Raw(db.Str(v)), nil
}

// DatabaseInfo is database_info.
func (a *Atlas) DatabaseInfo(c *sqlite.Conn) (map[string]any, error) {
	st := a.Obs.State()
	out := map[string]any{"filename": "signal-atlas.sqlite3", "sqlite_version": SQLiteVersion,
		"observations": int64(st.N), "networks": int64(len(st.Net)), "transmitters": int64(len(st.Trans))}
	for _, q := range [][2]string{{"schema_version", "PRAGMA user_version"}, {"cached_estimates", "SELECT count(*) FROM estimates"}, {"imports", "SELECT count(*) FROM imports"}} {
		v, err := db.Scalar(c, q[1])
		if err != nil {
			return nil, err
		}
		out[q[0]] = db.Int(v)
	}
	r, err := db.One(c, "SELECT 1 x FROM map_assets WHERE name='basemap'")
	out["has_basemap"] = r != nil
	return out, err
}

// ---------------------------------------------------------------------------
// rows out, as Python's dict(sqlite3.Row)

func nullable(v float64) any {
	if obs.IsNull(v) {
		return nil
	}
	return v
}

func recordID(st *obs.State, s *obs.Segment, i int) string {
	switch s.RecKind[i] {
	case obs.RecInt:
		return strconv.FormatInt(s.RecVal[i], 10)
	case obs.RecStr:
		return st.Str[s.RecVal[i]]
	}
	return strconv.FormatInt(s.Line[i], 10)
}

// fullRow is SELECT o.* for one row.
func fullRow(st *obs.State, s *obs.Segment, i int) Row {
	return Row{"id": s.Base + int64(i), "import_id": s.ImportID, "line": s.Line[i], "record_id": recordID(st, s, i),
		"ssid": st.Str[s.SSID[i]], "mac": st.Str[s.MAC[i]], "band": st.Str[s.Band[i]], "band_source": st.Str[s.BandSrc[i]],
		"radio_key": st.Str[s.Radio[i]], "latitude": s.Lat[i], "longitude": s.Lon[i], "rssi_dbm": s.RSSI[i],
		"accuracy_m": nullable(s.Acc[i]), "first_seen": st.Str[s.First[i]], "date": st.Str[s.Date[i]],
		"channel": st.Str[s.Channel[i]], "frequency_mhz": nullable(s.Freq[i])}
}

// recordKind stores a record id compactly: most are the line number or a plain integer.
func recordKind(d *obs.Dict, id string, line int64) (uint8, int64) {
	if id == strconv.FormatInt(line, 10) {
		return obs.RecLine, 0
	}
	if n, err := strconv.ParseInt(id, 10, 64); err == nil && strconv.FormatInt(n, 10) == id {
		return obs.RecInt, n
	}
	return obs.RecStr, int64(d.ID(id))
}

// rowOf interns an observation for the column store.
func rowOf(in *obs.Interner, d *obs.Dict, o *Observation, line int64) obs.Row {
	r := obs.Row{SSID: in.ID(0, o.SSID), MAC: in.ID(1, o.MAC), Band: in.ID(2, o.Band), BandSrc: in.ID(3, o.BandSource),
		Radio: in.ID(4, o.RadioKey), Date: in.ID(5, o.Date), Channel: in.ID(6, o.Channel), First: in.ID(7, o.FirstSeen),
		Lat: o.Latitude, Lon: o.Longitude, RSSI: o.RSSI, Acc: obs.Null, Freq: obs.Null, Line: line}
	if v, ok := o.Accuracy.(float64); ok {
		r.Acc = v
	}
	if v, ok := o.FrequencyMHz.(float64); ok {
		r.Freq = v
	}
	r.RecKind, r.RecVal = recordKind(d, o.ID, line)
	return r
}

// nextImportID is the id SQLite gives the next imports row (max(id)+1).
func nextImportID(c *sqlite.Conn) (int64, error) {
	v, err := db.Scalar(c, "SELECT coalesce(max(id),0)+1 FROM imports")
	return db.Int(v), err
}

func bandAssignments(c *sqlite.Conn) (map[string]string, error) {
	rows, err := db.All(c, "SELECT * FROM band_assignments")
	out := map[string]string{}
	for _, r := range rows {
		out[db.Str(r["ssid"])+"\x00"+db.Str(r["mac"])] = db.Str(r["band"])
	}
	return out, err
}

// ---------------------------------------------------------------------------
// importing a CSV

// ImportCSV is import_csv(path, database, skip_invalid, progress, filename). The rows are parsed
// on every core while the file is hashed, gathered into one new segment, and committed with the
// imports row; a failure or a cancel leaves nothing behind.
func (a *Atlas) ImportCSV(path string, skipInvalid bool, progress Progress, filename string, say func(string)) (map[string]any, error) {
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	c, put, err := a.Conn(nil)
	if err != nil {
		return nil, err
	}
	defer put()
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if filename == "" {
		filename = filepath.Base(path)
	}
	type hashed struct {
		sum string
		err error
	}
	hc := make(chan hashed, 1)
	go func() { s, e := FileSHA256(path); hc <- hashed{s, e} }()
	h := <-hc // the duplicate check comes first, as in Python (hashing is a few seconds per GB)
	if h.err != nil {
		return nil, h.err
	}
	existing, err := db.One(c, "SELECT * FROM imports WHERE sha256=?", h.sum)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return map[string]any{"duplicate": true, "valid_rows": existing["valid_rows"], "invalid_rows": existing["invalid_rows"], "non_wifi_rows": existing["non_wifi_rows"], "zero_excluded_rows": existing["zero_excluded_rows"], "added_rows": int64(0)}, nil
	}
	importID, err := nextImportID(c)
	if err != nil {
		return nil, err
	}
	assignments, err := bandAssignments(c)
	if err != nil {
		return nil, err
	}
	seg := obs.NewSegment(importID, a.Obs.NextBase(nil), int(min(size/100+16, 1<<26)))
	in := a.Obs.Dict.NewInterner()
	var valid, invalid, nonWifi, zero, processed int64
	var read atomic.Int64
	examples := []string{}
	var hdr csvHeader
	var line int64
	err = parallelRows(func(emit func([]string, int64) error) error {
		return readCSV(path, func(rec []string) error {
			if !hdr.done {
				_, err := hdr.take(rec)
				if err == nil && hdr.done {
					line = hdr.firstLine - 1
				}
				return err
			}
			if len(rec) == 0 { // DictReader skips blank lines
				return nil
			}
			line++
			return emit(rec, line)
		}, func(r, _ int64) { read.Store(r) })
	}, func(rec []string, line int64) parsedRow {
		f := hdr.fields(rec)
		if !IsWifi(f["Type"]) {
			return parsedRow{nonWifi: true}
		}
		if f["CurrentLatitude"] != nil && f["CurrentLongitude"] != nil && geo.ZeroPairText(*f["CurrentLatitude"], *f["CurrentLongitude"]) {
			return parsedRow{zeroPair: true}
		}
		o, err := ParseObservation(f, line)
		return parsedRow{obs: o, line: line, err: err}
	}, func(r parsedRow) error {
		processed++
		if processed%5000 == 0 && progress != nil {
			if err := progress(valid, invalid, read.Load(), size); err != nil {
				return err
			}
		}
		if r.zeroPair {
			zero++
			return nil
		}
		if r.nonWifi {
			nonWifi++
			return nil
		}
		if r.err != nil {
			if !skipInvalid {
				return r.err
			}
			invalid++
			if len(examples) < 10 {
				examples = append(examples, r.err.Error())
			}
			return nil
		}
		o := r.obs
		if b, ok := assignments[o.SSID+"\x00"+o.MAC]; ok && o.BandSource == "frequency/band unavailable" {
			o.Band, o.BandSource, o.RadioKey = b, "manual assignment", RadioKey(o.SSID, o.MAC, b)
		}
		row := rowOf(in, a.Obs.Dict, o, r.line)
		seg.Append(&row)
		valid++
		if valid%5000 == 0 && progress != nil {
			return progress(valid, invalid, read.Load(), size)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !hdr.done {
		return nil, py.Errorf("Missing CSV columns: %s", strings.Join(RequiredColumns, ", "))
	}
	if progress != nil {
		if err := progress(valid, invalid, size, size); err != nil {
			return nil, err
		}
	}
	if valid == 0 && zero == 0 {
		return nil, py.Errorf("CSV contains no valid observations")
	}
	if say != nil {
		say("Saving and indexing")
	}
	if valid > 0 {
		if err := a.Obs.Commit(obs.Change{Add: []*obs.Segment{seg}}); err != nil {
			return nil, err
		}
	}
	err = db.Tx(c, func() error {
		if _, err := db.Exec(c, "INSERT INTO imports(id,filename,sha256,imported_utc,valid_rows,invalid_rows,non_wifi_rows,zero_excluded_rows,active_rows) VALUES(?,?,?,?,?,?,?,?,?)",
			importID, filename, h.sum, py.UTCNowISO(), valid, invalid, nonWifi, zero, valid); err != nil {
			return err
		}
		// New sightings can move a network's fitted location: its fits are dropped, so they are
		// fitted again; every other network keeps its fits (Graphite's Python drops them all).
		return DropStaleEstimates(c, a.Obs.State(), seg)
	})
	if err != nil {
		a.Obs.Commit(obs.Change{Drop: map[int64]bool{importID: true}})
		return nil, err
	}
	return map[string]any{"duplicate": false, "valid_rows": valid, "invalid_rows": invalid, "non_wifi_rows": nonWifi, "zero_excluded_rows": zero, "empty": valid == 0, "invalid_examples": examples}, nil
}

// DropStaleEstimates deletes the saved fits of every network that has sightings in seg: named
// networks by SSID (shared and single-radio fits alike), hidden ones by MAC. Fits of networks seg
// doesn't touch stay, so adding a drive only refits what it changed.
func DropStaleEstimates(c *sqlite.Conn, st *obs.State, seg *obs.Segment) error {
	ssids, macs := map[string]bool{}, map[string]bool{}
	for i := 0; i < seg.N; i++ {
		if st.Str[seg.SSID[i]] == "" {
			macs[st.Str[seg.MAC[i]]] = true
		} else {
			ssids[st.Str[seg.SSID[i]]] = true
		}
	}
	return dropChangedFits(c, ssids, macs)
}

func dropChangedFits(c *sqlite.Conn, ssids, macs map[string]bool) error {
	if _, err := db.Exec(c, "CREATE TEMP TABLE IF NOT EXISTS stale_ssid(v TEXT PRIMARY KEY)"); err != nil {
		return err
	}
	if _, err := db.Exec(c, "CREATE TEMP TABLE IF NOT EXISTS stale_mac(v TEXT PRIMARY KEY)"); err != nil {
		return err
	}
	db.Exec(c, "DELETE FROM stale_ssid")
	db.Exec(c, "DELETE FROM stale_mac")
	for id := range ssids {
		if _, err := db.Exec(c, "INSERT OR IGNORE INTO stale_ssid VALUES(?)", id); err != nil {
			return err
		}
	}
	for id := range macs {
		if _, err := db.Exec(c, "INSERT OR IGNORE INTO stale_mac VALUES(?)", id); err != nil {
			return err
		}
	}
	if err := dirtyPreparationTargets(c); err != nil {
		return err
	}
	_, err := db.Exec(c, `DELETE FROM estimates WHERE
		(coalesce(json_extract(result,'$.ssid'),'') <> '' AND json_extract(result,'$.ssid') IN (SELECT v FROM stale_ssid))
		OR (coalesce(json_extract(result,'$.ssid'),'') = '' AND (upper(json_extract(result,'$.mac')) IN (SELECT v FROM stale_mac) OR EXISTS (SELECT 1 FROM json_each(result,'$.members') m WHERE upper(json_extract(m.value,'$.mac')) IN (SELECT v FROM stale_mac))))`)
	db.Exec(c, "DELETE FROM stale_ssid")
	db.Exec(c, "DELETE FROM stale_mac")
	return err
}

// errCancelled is what a cancelled job reports.
var errCancelled = errors.New("cancelled")
