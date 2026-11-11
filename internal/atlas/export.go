package atlas

import (
	"context"
	"os"
	"path/filepath"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
)

// ExportSQLite writes the whole store as one Signal Atlas SQLite database in Graphite's own schema
// (observations, spatial index, indexes, imports, estimates, comparisons, band assignments, the
// basemap), which Graphite's Python (python) opens as its own. dest must not exist.
// say reports the stage.
func (a *Atlas) ExportSQLite(dest string, say func(string)) error {
	return a.ExportSQLiteContext(context.Background(), dest, say, nil)
}
func (a *Atlas) ExportSQLiteContext(ctx context.Context, dest string, say func(string), progress func(int64, int64)) error {
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	if _, err := os.Stat(dest); err == nil {
		return os.ErrExist
	}
	os.MkdirAll(filepath.Dir(dest), 0o755)
	c, put, err := a.Conn(ctx)
	if err != nil {
		return err
	}
	_, err = db.Exec(c, "VACUUM INTO ?", dest)
	put()
	if err != nil {
		os.Remove(dest)
		return err
	}
	out, err := db.Open(dest, false)
	if err != nil {
		return err
	}
	defer out.Close()
	out.SetInterrupt(ctx.Done())
	fail := func(err error) error { out.Close(); os.Remove(dest); return err }
	for _, p := range []string{"PRAGMA journal_mode=DELETE", "PRAGMA synchronous=OFF", "PRAGMA cache_size=-1048576", "PRAGMA temp_store=MEMORY"} {
		db.Exec(out, p)
	}
	drop := "DROP TRIGGER IF EXISTS observation_insert;"
	for _, ix := range []string{"observation_network", "observation_radio", "observation_radio_date", "observation_date", "observation_transmitter", "observation_mac"} {
		drop += "DROP INDEX IF EXISTS " + ix + ";"
	}
	st := a.Obs.State()
	var done int64
	err = db.Tx(out, func() error {
		if err := db.Script(out, drop); err != nil {
			return err
		}
		ins, err := out.Prepare("INSERT INTO observations(id,import_id,line,record_id,ssid,mac,band,band_source,radio_key,latitude,longitude,rssi_dbm,accuracy_m,first_seen,date,channel,frequency_mhz) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
		if err != nil {
			return err
		}
		if say != nil {
			say("Writing observations")
		}
		for _, s := range st.Segs {
			for i := 0; i < s.N; i++ {
				if done%20000 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
					if progress != nil {
						progress(done, int64(st.N))
					}
				}
				vals := []any{s.Base + int64(i), s.ImportID, s.Line[i], recordID(st, s, i), st.Str[s.SSID[i]], st.Str[s.MAC[i]], st.Str[s.Band[i]], st.Str[s.BandSrc[i]],
					st.Str[s.Radio[i]], s.Lat[i], s.Lon[i], s.RSSI[i], nullable(s.Acc[i]), st.Str[s.First[i]], st.Str[s.Date[i]], st.Str[s.Channel[i]], nullable(s.Freq[i])}
				if err := db.Bind(ins, vals); err != nil {
					return err
				}
				if _, err := ins.Step(); err != nil {
					return err
				}
				if err := ins.Reset(); err != nil {
					return err
				}
				done++
			}
		}
		if say != nil {
			say("Building the spatial index and indexes")
		}
		if _, err := db.Exec(out, "INSERT INTO spatial SELECT id,latitude,latitude,longitude,longitude FROM observations ORDER BY id"); err != nil {
			return err
		}
		return db.Script(out, schemaSQL)
	})
	if err != nil {
		return fail(err)
	}
	db.Exec(out, "PRAGMA journal_mode=WAL")
	return nil
}

var _ = obs.Null
