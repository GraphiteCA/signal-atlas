package atlas

import (
	"context"
	"encoding/binary"
	"math"
	"os"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
)

// duplicateChecker uses a disposable disk-backed exact-key index, rather than
// keeping an unbounded set of observations in memory. Provenance is excluded.
func duplicateKeyChecker(ctx context.Context) (func([]byte) (bool, error), func(), error) {
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
	for _, sql := range []string{"PRAGMA journal_mode=OFF", "PRAGMA synchronous=OFF", "PRAGMA cache_size=-16384", "CREATE TABLE seen (key BLOB PRIMARY KEY) WITHOUT ROWID", "BEGIN"} {
		if _, err = db.Exec(c, sql); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	stmt, _, err := c.PrepareTransient("INSERT OR IGNORE INTO seen VALUES (?)")
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	closeAll := func() { stmt.Finalize(); cleanup() }
	check := func(key []byte) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		stmt.BindBytes(1, key)
		_, err := stmt.Step()
		duplicate := c.Changes() == 0
		resetErr := stmt.Reset()
		if err == nil {
			err = resetErr
		}
		return duplicate, err
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
func duplicateChecker(ctx context.Context) (func(*obs.Segment, int) (bool, error), func(), error) {
	check, close, err := duplicateKeyChecker(ctx)
	if err != nil {
		return nil, nil, err
	}
	return func(s *obs.Segment, i int) (bool, error) { return check(segmentKey(s, i)) }, close, nil
}

// Reuse this disposable index across a batch instead of rescanning the entire
// survey for every file. Any failed import or changed observation state invalidates it.
// All access is serialised by WriteMu; it is never part of workspace snapshots.
type importDuplicateIndex struct {
	state *obs.State
	check func([]byte) (bool, error)
	close func()
}

func (a *Atlas) importDuplicateChecker(pulse func() error) (func(*obs.Row) (bool, error), func(bool), error) {
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
				if _, err := check(segmentKey(s, i)); err != nil {
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
	return func(r *obs.Row) (bool, error) {
		return index.check(observationKey([8]uint32{r.SSID, r.MAC, r.Band, r.BandSrc, r.Radio, r.Date, r.Channel, r.First}, [5]float64{r.Lat, r.Lon, r.RSSI, r.Acc, r.Freq}))
	}, finish, nil
}

func (a *Atlas) DuplicatePreview(ctx context.Context) (ZeroPreview, error) {
	var out ZeroPreview
	check, close, err := duplicateChecker(ctx)
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
	check, close, err := duplicateChecker(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer close()
	return a.prepareFiltered(ctx, dest, progress, "Removing duplicate observations in candidate", false, check)
}
