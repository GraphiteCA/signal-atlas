package atlas

import (
	"encoding/json"

	"signal-atlas/internal/db"
	"signal-atlas/internal/py"
	"zombiezen.com/go/sqlite"
)

// Saved Wardrive Compare results live with the survey's metadata, so snapshots and portable
// exports carry them. A run is a snapshot: the result as it was computed, the page settings that
// produced it and the drives it compared. It is never recomputed from the survey.
const compareRunsSchema = `
CREATE TABLE IF NOT EXISTS compare_runs(id TEXT PRIMARY KEY, name TEXT NOT NULL, created_utc TEXT NOT NULL,
  drives INTEGER NOT NULL, sources_json TEXT NOT NULL, state_json TEXT NOT NULL, result_json TEXT NOT NULL);
`

const (
	// MaxCompareRuns bounds the saved runs of one survey.
	MaxCompareRuns = 100
	// MaxCompareRunBytes bounds one saved result; a result with its routes runs to a few megabytes.
	MaxCompareRunBytes = 48 << 20
)

// CompareRunInput is one Compare result to save: JSON as the page holds it.
type CompareRunInput struct {
	Name    string
	Drives  int64
	Sources json.RawMessage // the drives compared: key, name, files, start, end, rows, km
	State   json.RawMessage // the page's settings, names and notes
	Result  json.RawMessage // the analysis result
}

// CompareRuns lists the saved runs, newest first, without their results.
func CompareRuns(c *sqlite.Conn) ([]Row, error) {
	rows, err := db.All(c, "SELECT id,name,created_utc,drives,sources_json,length(result_json) AS bytes FROM compare_runs ORDER BY created_utc DESC,id")
	if err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		var sources any
		if err := json.Unmarshal([]byte(db.Str(r["sources_json"])), &sources); err != nil {
			sources = []any{}
		}
		out = append(out, Row{"id": r["id"], "name": r["name"], "created_utc": r["created_utc"], "drives": r["drives"], "bytes": r["bytes"], "sources": sources})
	}
	return out, nil
}

// CompareRun is one saved run with its state and result as stored, or nil when there is none.
func CompareRun(c *sqlite.Conn, id string) (Row, error) {
	return db.One(c, "SELECT id,name,created_utc,drives,sources_json,state_json,result_json FROM compare_runs WHERE id=?", id)
}

// SaveCompareRun stores a Compare result under a name and answers its id.
func (a *Atlas) SaveCompareRun(c *sqlite.Conn, in CompareRunInput) (string, error) {
	name := py.Head(py.Strip(in.Name), 100)
	if name == "" {
		return "", py.Errorf("Name this comparison, for example New antenna or Firmware 2.66")
	}
	for _, part := range []json.RawMessage{in.Sources, in.State, in.Result} {
		if len(part) == 0 || !json.Valid(part) {
			return "", py.Errorf("That comparison could not be read; run it again and save")
		}
	}
	if len(in.Sources)+len(in.State)+len(in.Result) > MaxCompareRunBytes {
		return "", py.Errorf("That comparison is too large to save; compare fewer drives")
	}
	if in.Drives < 1 {
		return "", py.Errorf("There is no comparison to save yet")
	}
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	id := py.UUIDHex()
	err := db.WriteTx(c, func() error {
		n, err := db.Scalar(c, "SELECT count(*) FROM compare_runs")
		if err != nil {
			return err
		}
		if db.Int(n) >= MaxCompareRuns {
			return py.Errorf("Keep up to %d saved comparisons; delete one before saving another", MaxCompareRuns)
		}
		_, err = db.Exec(c, "INSERT INTO compare_runs VALUES(?,?,?,?,?,?,?)", id, name, py.UTCNowISO(), in.Drives, string(in.Sources), string(in.State), string(in.Result))
		return err
	})
	return id, err
}

// DeleteCompareRun removes a saved run.
func (a *Atlas) DeleteCompareRun(c *sqlite.Conn, id string) error {
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	_, err := db.Exec(c, "DELETE FROM compare_runs WHERE id=?", id)
	return err
}
