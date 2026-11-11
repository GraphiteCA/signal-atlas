package atlas

import (
	"math"
	"path/filepath"
	"strconv"
	"time"

	"signal-atlas/internal/db"
	"signal-atlas/internal/geo"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"

	"zombiezen.com/go/sqlite"
)

// ---------------------------------------------------------------------------
// SQLite databases from the user's computer: a Signal Atlas database (the explorer's own saved
// format, which is Graphite's) and the WiGLE Android app's database backup.

// Database kinds DatabaseKind tells apart.
const (
	KindAtlas = "atlas" // Signal Atlas: observations + imports tables
	KindWiGLE = "wigle" // WiGLE Android backup: network + location tables
)

// DatabaseKind says what an SQLite file is, or why it can't be opened.
func DatabaseKind(path string) (string, error) {
	c, err := db.OpenReadOnly(path)
	if err != nil {
		return "", py.Errorf("Couldn't open %s as an SQLite database", filepath.Base(path))
	}
	defer c.Close()
	rows, err := db.All(c, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return "", py.Errorf("%s isn't an SQLite database", filepath.Base(path))
	}
	t := map[string]bool{}
	for _, r := range rows {
		t[db.Str(r["name"])] = true
	}
	switch {
	case t["observations"] && t["imports"]:
		v, _ := db.Scalar(c, "PRAGMA user_version")
		if db.Int(v) > SchemaVersion {
			return "", py.Errorf("This database requires a newer Signal Atlas version")
		}
		return KindAtlas, nil
	case t["network"] && t["location"]:
		return KindWiGLE, nil
	}
	return "", py.Errorf("%s is neither a Signal Atlas database nor a WiGLE app backup", filepath.Base(path))
}

// Count is how many observations a database holds (for progress), or 0.
func Count(path, kind string) int64 {
	c, err := db.OpenReadOnly(path)
	if err != nil {
		return 0
	}
	defer c.Close()
	table := "observations"
	if kind == KindWiGLE {
		table = "location"
	}
	v, _ := db.Scalar(c, "SELECT count(*) FROM "+table)
	return db.Int(v)
}

func colText(st *sqlite.Stmt, i int) string {
	if st.ColumnType(i) == sqlite.TypeNull {
		return ""
	}
	return st.ColumnText(i)
}

func colNullable(st *sqlite.Stmt, i int) float64 {
	if st.ColumnType(i) == sqlite.TypeNull {
		return obs.Null
	}
	return st.ColumnFloat(i)
}

// OpenDatabase replaces everything in the store with a Signal Atlas database's contents: its
// observations (keeping their ids, so saved estimates still match), imports, estimates,
// comparisons, band assignments and basemap. progress gets rows read so far.
func (a *Atlas) OpenDatabase(path string, progress func(done int64) error, say func(string)) (map[string]any, error) {
	src, err := db.OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	tables := []string{"imports", "estimates", "comparisons", "band_assignments", "map_assets"}
	meta := map[string][]db.Row{}
	for _, t := range tables {
		exists, err := db.Scalar(src, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", t)
		if err != nil {
			return nil, err
		}
		if db.Int(exists) == 0 {
			continue
		}
		if meta[t], err = db.All(src, "SELECT * FROM "+t); err != nil {
			return nil, err
		}
	}
	if say != nil {
		say("Reading observations")
	}
	var segs []*obs.Segment
	var cur *obs.Segment
	in := a.Obs.Dict.NewInterner()
	var n, prev int64 = 0, -1
	var zero int64
	zeroByImport := map[int64]int64{}
	changedNames, changedMACs := map[string]bool{}, map[string]bool{}
	err = db.Each(src, "SELECT id,import_id,line,record_id,ssid,mac,band,band_source,radio_key,latitude,longitude,rssi_dbm,accuracy_m,first_seen,date,channel,frequency_mhz FROM observations ORDER BY id", nil, func(st *sqlite.Stmt) error {
		id, imp, line := st.ColumnInt64(0), st.ColumnInt64(1), st.ColumnInt64(2)
		coords := [2]float64{}
		for k, column := range []int{9, 10} {
			switch st.ColumnType(column) {
			case sqlite.TypeInteger, sqlite.TypeFloat:
				coords[k] = st.ColumnFloat(column)
			case sqlite.TypeText:
				var e error
				coords[k], e = strconv.ParseFloat(st.ColumnText(column), 64)
				if e != nil {
					return py.Errorf("Invalid coordinate at observation %d", id)
				}
			default:
				return py.Errorf("Missing coordinate at observation %d", id)
			}
		}
		if math.IsNaN(coords[0]) || math.IsNaN(coords[1]) || math.IsInf(coords[0], 0) || math.IsInf(coords[1], 0) || math.Abs(coords[0]) > 90 || math.Abs(coords[1]) > 180 {
			return py.Errorf("Invalid coordinate at observation %d", id)
		}
		if geo.ZeroPair(coords[0], coords[1]) {
			zero++
			zeroByImport[imp]++
			ssid, mac := colText(st, 4), colText(st, 5)
			if ssid == "" {
				changedMACs[mac] = true
			} else {
				changedNames[ssid] = true
			}
			if (zero+n)%20000 == 0 && progress != nil {
				return progress(n + zero)
			}
			return nil
		}

		// a segment's ids are consecutive: a gap or a new import starts another
		if cur == nil || imp != cur.ImportID || id != prev+1 {
			cur = obs.NewSegment(imp, id, 1024)
			segs = append(segs, cur)
		}
		prev = id
		rec := colText(st, 3)
		if rec == "" {
			rec = strconv.FormatInt(line, 10)
		}
		r := obs.Row{SSID: in.ID(0, colText(st, 4)), MAC: in.ID(1, colText(st, 5)), Band: in.ID(2, colText(st, 6)),
			BandSrc: in.ID(3, colText(st, 7)), Radio: in.ID(4, colText(st, 8)), Date: in.ID(5, colText(st, 14)),
			Channel: in.ID(6, colText(st, 15)), First: in.ID(7, colText(st, 13)),
			Lat: coords[0], Lon: coords[1], RSSI: st.ColumnFloat(11), Acc: colNullable(st, 12),
			Freq: colNullable(st, 16), Line: line}
		r.RecKind, r.RecVal = recordKind(a.Obs.Dict, rec, line)
		cur.Append(&r)
		n++
		if n%20000 == 0 && progress != nil {
			return progress(n + zero)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if progress != nil {
		if err := progress(n); err != nil {
			return nil, err
		}
	}
	if say != nil {
		say("Saving and indexing")
	}
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	c, put, err := a.Conn(nil)
	if err != nil {
		return nil, err
	}
	defer put()
	drop := map[int64]bool{}
	for _, s := range a.Obs.State().Segs {
		drop[s.ImportID] = true
	}
	err = db.Tx(c, func() error {
		for _, t := range tables {
			if _, err := db.Exec(c, "DELETE FROM "+t); err != nil {
				return err
			}
		}
		for _, t := range tables {
			for _, r := range meta[t] {
				cols, vals := make([]string, 0, len(r)), make([]any, 0, len(r))
				for k, v := range r {
					cols = append(cols, db.Q(k))
					vals = append(vals, v)
				}
				q := "INSERT INTO " + t + "(" + join(cols) + ") VALUES(" + db.Marks(len(cols)) + ")"
				if _, err := db.Exec(c, q, vals...); err != nil {
					return py.Errorf("Couldn't copy the database's %s: %s", t, err.Error())
				}
			}
		}
		// Adopt active counts from the surviving rows without losing import history.
		if _, err := db.Exec(c, "UPDATE imports SET active_rows=valid_rows WHERE active_rows<0"); err != nil {
			return err
		}
		for id, z := range zeroByImport {
			if _, err := db.Exec(c, "UPDATE imports SET active_rows=active_rows-?,zero_excluded_rows=zero_excluded_rows+? WHERE id=?", z, z, id); err != nil {
				return err
			}
		}
		if err := dropChangedFits(c, changedNames, changedMACs); err != nil {
			return err
		}
		// the observations go in last: if they can't be saved, nothing above is kept either
		return a.Obs.Commit(obs.Change{Drop: drop, Add: segs})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"opened": filepath.Base(path), "valid_rows": n, "invalid_rows": 0,
		"zero_excluded_rows": zero, "empty": n == 0, "imports": len(meta["imports"]), "estimates": len(meta["estimates"])}, nil
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

// wifiChannel is the channel number for a frequency in MHz, or "".
func wifiChannel(mhz int64) string {
	switch {
	case mhz == 2484:
		return "14"
	case mhz >= 2412 && mhz <= 2472:
		return strconv.FormatInt((mhz-2407)/5, 10)
	case mhz >= 5160 && mhz <= 5885:
		return strconv.FormatInt((mhz-5000)/5, 10)
	case mhz >= 5955 && mhz <= 7115:
		return strconv.FormatInt((mhz-5950)/5, 10)
	}
	return ""
}

// ImportWiGLE adds a WiGLE Android app database backup as one import: each Wi-Fi sighting in its
// location table becomes an observation, read exactly as the same row of a WiGLE CSV would be.
// Other radio types (Bluetooth, cell) are counted and left out, as in a CSV import.
func (a *Atlas) ImportWiGLE(path string, skipInvalid bool, progress func(valid, invalid int64) error, say func(string)) (map[string]any, error) {
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	c, put, err := a.Conn(nil)
	if err != nil {
		return nil, err
	}
	defer put()
	sum, err := FileSHA256(path)
	if err != nil {
		return nil, err
	}
	if existing, err := db.One(c, "SELECT * FROM imports WHERE sha256=?", sum); err != nil {
		return nil, err
	} else if existing != nil {
		return map[string]any{"duplicate": true, "valid_rows": existing["valid_rows"], "invalid_rows": existing["invalid_rows"], "non_wifi_rows": existing["non_wifi_rows"], "zero_excluded_rows": existing["zero_excluded_rows"], "added_rows": int64(0)}, nil
	}
	src, err := db.OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	importID, err := nextImportID(c)
	if err != nil {
		return nil, err
	}
	assignments, err := bandAssignments(c)
	if err != nil {
		return nil, err
	}
	if say != nil {
		say("Reading the WiGLE database")
	}
	seg := obs.NewSegment(importID, a.Obs.NextBase(nil), 1<<16)
	in := a.Obs.Dict.NewInterner()
	var valid, invalid, nonWifi, zero, line int64
	examples := []string{}
	s := func(v string) *string { return &v }
	err = db.Each(src, `SELECT l._id, l.bssid, n.ssid, n.capabilities, n.frequency, n.type, l.level, l.lat, l.lon,
		l.altitude, l.accuracy, l.time FROM location l LEFT JOIN network n ON n.bssid = l.bssid ORDER BY l._id`, nil, func(st *sqlite.Stmt) error {
		line++
		if line%20000 == 0 && progress != nil {
			if err := progress(valid, invalid); err != nil {
				return err
			}
		}
		typ := colText(st, 5)
		if typ != "" && typ != "W" {
			nonWifi++
			return nil
		}
		if geo.ZeroPairText(colText(st, 7), colText(st, 8)) {
			zero++
			if line%20000 == 0 && progress != nil {
				return progress(valid, invalid)
			}
			return nil
		}
		mhz := st.ColumnInt64(4)
		freq := ""
		if mhz > 0 {
			freq = strconv.FormatInt(mhz, 10)
		}
		first := ""
		if ms := st.ColumnInt64(11); ms > 0 {
			first = time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05")
		}
		f := Fields{"id": s(colText(st, 0)), "MAC": s(colText(st, 1)), "SSID": s(colText(st, 2)), "AuthMode": s(colText(st, 3)),
			"Frequency": s(freq), "Channel": s(wifiChannel(mhz)), "RSSI": s(colText(st, 6)), "CurrentLatitude": s(colText(st, 7)),
			"CurrentLongitude": s(colText(st, 8)), "AltitudeMeters": s(colText(st, 9)), "AccuracyMeters": s(colText(st, 10)),
			"FirstSeen": s(first), "Type": s("WIFI")}
		o, err := ParseObservation(f, line)
		if err != nil {
			if !skipInvalid {
				return py.Errorf("%s (WiGLE location row %s). Choose Skip invalid rows to leave such rows out.", err.Error(), colText(st, 0))
			}
			invalid++
			if len(examples) < 10 {
				examples = append(examples, err.Error())
			}
			return nil
		}
		if b, ok := assignments[o.SSID+"\x00"+o.MAC]; ok && o.BandSource == "frequency/band unavailable" {
			o.Band, o.BandSource, o.RadioKey = b, "manual assignment", RadioKey(o.SSID, o.MAC, b)
		}
		row := rowOf(in, a.Obs.Dict, o, line)
		seg.Append(&row)
		valid++
		if valid%20000 == 0 && progress != nil {
			return progress(valid, invalid)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if progress != nil {
		if err := progress(valid, invalid); err != nil {
			return nil, err
		}
	}
	if valid == 0 && zero == 0 {
		return nil, py.Errorf("The WiGLE database has no Wi-Fi observations with a GPS position")
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
			importID, filepath.Base(path), sum, py.UTCNowISO(), valid, invalid, nonWifi, zero, valid); err != nil {
			return err
		}
		return DropStaleEstimates(c, a.Obs.State(), seg) // only networks with new sightings refit
	})
	if err != nil {
		a.Obs.Commit(obs.Change{Drop: map[int64]bool{importID: true}})
		return nil, err
	}
	return map[string]any{"duplicate": false, "valid_rows": valid, "invalid_rows": invalid, "non_wifi_rows": nonWifi, "zero_excluded_rows": zero, "empty": valid == 0, "invalid_examples": examples}, nil
}
