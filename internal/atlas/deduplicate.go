package atlas

import (
	"context"
	"encoding/binary"
	"math"
	"os"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"zombiezen.com/go/sqlite"
)

// duplicateChecker uses a disposable disk-backed exact-key index, rather than
// keeping an unbounded set of observations in memory. Provenance is excluded.
func duplicateKeyChecker(ctx context.Context) (func([]byte, int64) (int64, bool, error), func(), error) {
	f, err := os.CreateTemp("", "atlas-dedup-*.sqlite3")
	if err != nil {
		return nil, nil, err
	}
	path := f.Name()
	f.Close()
	c, err := db.Open(path, false)
	if err != nil {
		os.Remove(path)
		return nil, nil, err
	}
	cleanup := func() { c.Close(); os.Remove(path) }
	c.SetInterrupt(ctx.Done())
	for _, sql := range []string{"PRAGMA journal_mode=OFF", "PRAGMA synchronous=OFF", "PRAGMA cache_size=-16384", "CREATE TABLE seen (key BLOB PRIMARY KEY, id INTEGER NOT NULL) WITHOUT ROWID", "BEGIN"} {
		if _, err = db.Exec(c, sql); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	stmt, _, err := c.PrepareTransient("INSERT OR IGNORE INTO seen VALUES (?,?)")
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	closeAll := func() { stmt.Finalize(); cleanup() }
	check := func(key []byte, id int64) (int64, bool, error) {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		stmt.BindBytes(1, key)
		stmt.BindInt64(2, id)
		_, err := stmt.Step()
		duplicate := c.Changes() == 0
		resetErr := stmt.Reset()
		if err == nil {
			err = resetErr
		}
		if duplicate && err == nil {
			v, e := db.Scalar(c, "SELECT id FROM seen WHERE key=?", key)
			return db.Int(v), true, e
		}
		return id, duplicate, err
	}
	return check, closeAll, nil
}

func observationKey(text [8]uint32, numbers [5]float64) []byte {
	var key [72]byte
	for k, v := range text {
		binary.LittleEndian.PutUint32(key[k*4:], v)
	}
	for k, v := range numbers {
		if v == 0 {
			v = 0
		}
		if math.IsNaN(v) {
			v = obs.Null
		}
		binary.LittleEndian.PutUint64(key[32+k*8:], math.Float64bits(v))
	}
	return key[:]
}
func segmentKey(s *obs.Segment, i int) []byte {
	return observationKey([8]uint32{s.SSID[i], s.MAC[i], s.Band[i], s.BandSrc[i], s.Radio[i], s.Date[i], s.Channel[i], s.First[i]}, [5]float64{s.Lat[i], s.Lon[i], s.RSSI[i], s.Acc[i], s.Freq[i]})
}

// Details are streamed by observation ID; they never form a survey-sized Go map.
func detailReader(c *sqlite.Conn) (func(int64) (string, error), func(), error) {
	s, _, err := c.PrepareTransient("SELECT observation_id,fields_json FROM observation_details ORDER BY observation_id")
	if err != nil {
		return nil, nil, err
	}
	ok, err := s.Step()
	if err != nil {
		s.Finalize()
		return nil, nil, err
	}
	return func(id int64) (string, error) {
		for ok && s.ColumnInt64(0) < id {
			ok, err = s.Step()
			if err != nil {
				return "", err
			}
		}
		if ok && s.ColumnInt64(0) == id {
			return s.ColumnText(1), nil
		}
		return "", nil
	}, func() { s.Finalize() }, nil
}

func (a *Atlas) duplicateChecker(ctx context.Context, onDuplicate func(*obs.Segment, int, int64) error) (func(*obs.Segment, int) (bool, error), func(), error) {
	check, close, err := duplicateKeyChecker(ctx)
	if err != nil {
		return nil, nil, err
	}
	c, put, err := a.Conn(ctx)
	if err != nil {
		close()
		return nil, nil, err
	}
	details, done, err := detailReader(c)
	if err != nil {
		put()
		close()
		return nil, nil, err
	}
	return func(s *obs.Segment, i int) (bool, error) {
		extra, err := details(s.ID(i))
		if err != nil {
			return false, err
		}
		id, duplicate, err := check(append(segmentKey(s, i), extra...), s.ID(i))
		if err == nil && duplicate && onDuplicate != nil {
			err = onDuplicate(s, i, id)
		}
		return duplicate, err
	}, func() { done(); put(); close() }, nil
}

// Reuse this disposable index across a batch instead of rescanning the entire
// survey for every file. Any failed import or changed observation state invalidates it.
// All access is serialised by WriteMu; it is never part of workspace snapshots.
type importDuplicateIndex struct {
	state *obs.State
	check func([]byte, int64) (int64, bool, error)
	close func()
}

func (a *Atlas) importDuplicateChecker(c *sqlite.Conn, pulse func() error) (func(*obs.Row, string, int64) (int64, bool, error), func(bool), error) {
	st := a.Obs.State()
	if a.importDuplicates != nil && a.importDuplicates.state != st {
		a.importDuplicates.close()
		a.importDuplicates = nil
	}
	if a.importDuplicates == nil {
		check, close, err := duplicateKeyChecker(context.Background())
		if err != nil {
			return nil, nil, err
		}
		details, done, err := detailReader(c)
		if err != nil {
			close()
			return nil, nil, err
		}
		defer done()
		var n int
		for _, s := range st.Segs {
			for i := 0; i < s.N; i++ {
				if n%4096 == 0 && pulse != nil {
					if err := pulse(); err != nil {
						close()
						return nil, nil, err
					}
				}
				n++
				extra, err := details(s.ID(i))
				if err != nil {
					close()
					return nil, nil, err
				}
				if _, _, err := check(append(segmentKey(s, i), extra...), s.ID(i)); err != nil {
					close()
					return nil, nil, err
				}
			}
		}
		a.importDuplicates = &importDuplicateIndex{state: st, check: check, close: close}
	}
	index := a.importDuplicates
	finish := func(committed bool) {
		if committed {
			index.state = a.Obs.State()
		} else {
			index.close()
			a.importDuplicates = nil
		}
	}
	return func(r *obs.Row, extra string, id int64) (int64, bool, error) {
		return index.check(append(observationKey([8]uint32{r.SSID, r.MAC, r.Band, r.BandSrc, r.Radio, r.Date, r.Channel, r.First}, [5]float64{r.Lat, r.Lon, r.RSSI, r.Acc, r.Freq}), extra...), id)
	}, finish, nil
}

func (a *Atlas) DuplicatePreview(ctx context.Context) (ZeroPreview, error) {
	var out ZeroPreview
	check, close, err := a.duplicateChecker(ctx, nil)
	if err != nil {
		return out, err
	}
	defer close()
	imports := map[int64]bool{}
	networks := map[uint32]bool{}
	for _, s := range a.Obs.State().Segs {
		for i := 0; i < s.N; i++ {
			duplicate, err := check(s, i)
			if err != nil {
				return out, err
			}
			if duplicate {
				out.Rows++
				imports[s.ImportID] = true
				networks[s.Radio[i]] = true
			}
		}
	}
	out.Imports = len(imports)
	out.Networks = len(networks)
	return out, nil
}

func (a *Atlas) PrepareDeduplication(ctx context.Context, dest string, progress func(int64, int64, string)) (*Atlas, map[string]any, error) {
	stage, err := newPreservationStage()
	if err != nil {
		return nil, nil, err
	}
	defer stage.close()
	if _, err = db.Exec(stage.c, "CREATE TABLE remaps(old_id INTEGER PRIMARY KEY,new_id INTEGER)"); err != nil {
		return nil, nil, err
	}
	check, close, err := a.duplicateChecker(ctx, func(s *obs.Segment, i int, id int64) error {
		if _, err := db.Exec(stage.c, "INSERT INTO remaps VALUES(?,?)", s.ID(i), id); err != nil {
			return err
		}
		// Legacy rows may predate the provenance table.
		_, err := db.Exec(stage.c, "INSERT OR IGNORE INTO observation_provenance VALUES(?,?,?,?,?,?)", s.ImportID, s.Line[i], recordID(a.Obs.State(), s, i), id, "", "")
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	defer close()
	candidate, result, err := a.prepareFiltered(ctx, dest, progress, "Removing duplicate observations in candidate", false, check)
	if err != nil {
		return nil, nil, err
	}
	c, put, err := candidate.Conn(ctx)
	if err != nil {
		candidate.Close()
		return nil, nil, err
	}
	err = db.WriteTx(c, func() error {
		if err := db.Each(stage.c, "SELECT old_id,new_id FROM remaps", nil, func(s *sqlite.Stmt) error {
			_, err := db.Exec(c, "UPDATE observation_provenance SET observation_id=? WHERE observation_id=?", s.ColumnInt64(1), s.ColumnInt64(0))
			return err
		}); err != nil {
			return err
		}
		return db.Each(stage.c, "SELECT * FROM observation_provenance", nil, func(s *sqlite.Stmt) error {
			_, err := db.Exec(c, "INSERT OR IGNORE INTO observation_provenance VALUES(?,?,?,?,?,?)", s.ColumnInt64(0), s.ColumnInt64(1), s.ColumnText(2), s.ColumnInt64(3), s.ColumnText(4), s.ColumnText(5))
			return err
		})
	})
	put()
	if err != nil {
		candidate.Close()
		return nil, nil, err
	}
	return candidate, result, nil
}
