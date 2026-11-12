package atlas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"signal-atlas/internal/db"
	"zombiezen.com/go/sqlite"
)

// Preservation lives in metadata so snapshots and portable exports include it.
// Chunks bound memory even for multi-gigabyte sources. No source paths are needed
// after an import commits.
const preservationSchema = `
CREATE TABLE IF NOT EXISTS source_files(sha256 TEXT PRIMARY KEY, size_bytes INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS source_chunks(sha256 TEXT NOT NULL, chunk INTEGER NOT NULL, payload BLOB NOT NULL, PRIMARY KEY(sha256,chunk)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS import_sources(import_id INTEGER PRIMARY KEY, sha256 TEXT NOT NULL, filename TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS observation_details(observation_id INTEGER PRIMARY KEY, fields_json TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS observation_provenance(import_id INTEGER NOT NULL, line INTEGER NOT NULL, record_id TEXT, observation_id INTEGER, source_table TEXT, source_record_source TEXT, PRIMARY KEY(import_id,line));
CREATE INDEX IF NOT EXISTS provenance_observation ON observation_provenance(observation_id);
`

var preservationTables = []string{"source_files", "source_chunks", "import_sources", "observation_details", "observation_provenance"}

// Extra fields are exact text, not floats: unknown vendor columns survive too.
// Source-local identifiers belong to provenance, not sighting identity.
func extraFields(f Fields) string {
	m := map[string]string{}
	for k, v := range f {
		if v == nil || *v == "" {
			continue
		}
		switch k {
		case "MAC", "SSID", "FirstSeen", "Channel", "Frequency", "RSSI", "CurrentLatitude", "CurrentLongitude", "AccuracyMeters", "Type", "Band", "band", "id", "day_table", "source":
			continue
		}
		m[k] = *v
	}
	if len(m) == 0 {
		return ""
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// A private disk-backed staging transaction avoids keeping provenance in RAM
// or publishing it before the observation/import commit succeeds.
type preservationStage struct {
	c    *sqlite.Conn
	path string
}

func newPreservationStage() (*preservationStage, error) {
	f, err := os.CreateTemp("", "atlas-preservation-*.sqlite3")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	f.Close()
	c, err := db.Open(path, false)
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	p := &preservationStage{c, path}
	if err = db.Script(c, preservationSchema); err != nil {
		p.close()
		return nil, err
	}
	for _, q := range []string{"PRAGMA journal_mode=OFF", "PRAGMA cache_size=-4096", "BEGIN"} {
		if _, err := db.Exec(c, q); err != nil {
			p.close()
			return nil, err
		}
	}
	return p, nil
}
func (p *preservationStage) close() { p.c.Close(); os.Remove(p.path) }
func (p *preservationStage) add(importID, line, id int64, o *Observation, duplicate bool) error {
	if !duplicate && o.Extra != "" {
		if err := preservationInsert(p.c, "INSERT INTO observation_details VALUES(?,?)", id, o.Extra); err != nil {
			return err
		}
	}
	return preservationInsert(p.c, "INSERT INTO observation_provenance VALUES(?,?,?,?,?,?)", importID, line, o.ID, id, o.DayTable, o.Source)
}

// Conn.Prepare caches one statement per SQL string; Conn.Close finalizes it.
// Keep that cache for bulk imports, but release bound row values after each call
// (Reset alone retains them), including partially bound and failed inserts.
func preservationInsert(c *sqlite.Conn, sql string, values ...any) (err error) {
	stmt, err := c.Prepare(sql)
	if err != nil {
		return err
	}
	defer func() {
		resetErr := stmt.Reset()
		// Preserve the primary failure; Reset can repeat an execution error.
		if err == nil {
			err = resetErr
		}
		err = errors.Join(err, stmt.ClearBindings())
	}()
	if err = db.Bind(stmt, values); err != nil {
		return err
	}
	_, err = stmt.Step()
	return err
}

func (p *preservationStage) commit(c *sqlite.Conn) error {
	for _, table := range []string{"observation_details", "observation_provenance"} {
		err := db.Each(p.c, "SELECT * FROM "+table, nil, func(s *sqlite.Stmt) error {
			v := make([]any, s.ColumnCount())
			for i := range v {
				v[i] = db.Value(s, i)
			}
			return preservationInsert(c, "INSERT INTO "+table+" VALUES("+db.Marks(len(v))+")", v...)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// captureSource gives parsing and archiving the same immutable bytes.
func captureSource(path string) (string, func(), error) {
	f, err := os.CreateTemp("", "atlas-source-*")
	if err != nil {
		return "", nil, err
	}
	target := f.Name()
	f.Close()
	os.Remove(target)
	clean := func() { os.Remove(target) }
	if err := copyFile(context.Background(), path, target); err != nil {
		clean()
		return "", nil, err
	}
	return target, clean, nil
}

func archiveSource(c *sqlite.Conn, path, filename string, importID int64, pulse func() error) error {
	sum, err := FileSHA256(path)
	if err != nil {
		return err
	}
	exists, err := db.Scalar(c, "SELECT count(*) FROM source_files WHERE sha256=?", sum)
	if err != nil {
		return err
	}
	if db.Int(exists) == 0 {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		buf := make([]byte, 1<<20)
		var size int64
		chunk := 0
		h := sha256.New()
		for {
			if pulse != nil {
				if err := pulse(); err != nil {
					return err
				}
			}
			n, e := io.ReadFull(f, buf)
			if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
				return e
			}
			if n > 0 {
				h.Write(buf[:n])
				size += int64(n)
				if _, err := db.Exec(c, "INSERT INTO source_chunks VALUES(?,?,?)", sum, chunk, buf[:n]); err != nil {
					return err
				}
				chunk++
			}
			if e != nil {
				break
			}
		}
		if hex.EncodeToString(h.Sum(nil)) != sum {
			return fmt.Errorf("Source changed while archiving")
		}
		if _, err := db.Exec(c, "INSERT INTO source_files VALUES(?,?)", sum, size); err != nil {
			return err
		}
	}
	_, err = db.Exec(c, "INSERT OR REPLACE INTO import_sources VALUES(?,?,?)", importID, sum, filename)
	return err
}

// ExportOriginalSource restores exact bytes, including excluded rows and vendor
// metadata. It never overwrites a destination, and removes partial output.
func (a *Atlas) ExportOriginalSource(ctx context.Context, importID int64, dest string) (err error) {
	c, put, err := a.Conn(ctx)
	if err != nil {
		return err
	}
	defer put()
	r, err := db.One(c, "SELECT f.sha256,f.size_bytes FROM import_sources s JOIN source_files f USING(sha256) WHERE import_id=?", importID)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("Original source unavailable; imports made before source preservation must be reimported")
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		e := f.Close()
		if err == nil {
			err = e
		}
		if err != nil {
			os.Remove(dest)
		}
	}()
	h := sha256.New()
	var size int64
	err = db.Each(c, "SELECT payload FROM source_chunks WHERE sha256=? ORDER BY chunk", []any{r["sha256"]}, func(s *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := db.Value(s, 0).([]byte)
		n, e := io.MultiWriter(f, h).Write(b)
		size += int64(n)
		return e
	})
	if err == nil && (size != db.Int(r["size_bytes"]) || hex.EncodeToString(h.Sum(nil)) != db.Str(r["sha256"])) {
		err = fmt.Errorf("Archived source failed integrity verification")
	}
	return err
}
