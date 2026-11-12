package atlas

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"signal-atlas/internal/db"
	"sort"
	"strconv"
	"time"

	"signal-atlas/internal/drivedb"
	"signal-atlas/internal/obs"
)

// ExportSurveyCSV streams a committed observation state. dest is an owned
// staging file or directory; the caller publishes it only after success.
func (a *Atlas) ExportSurveyCSV(ctx context.Context, dest, version string, days bool, progress func(int64, int64)) (int64, []string, error) {
	return a.exportSurveyCSV(ctx, dest, version, days, nil, progress)
}

// exportSurveyCSV writes the survey as one CSV (dest is the file), one CSV a day (days; dest is a
// folder), or the files pick names (dest is a folder): pick gets a row's import and its day ("" when
// undated) and answers the file the row belongs in, or false to leave the row out.
func (a *Atlas) exportSurveyCSV(ctx context.Context, dest, version string, days bool, pick func(importID int64, day string) (string, bool), progress func(int64, int64)) (int64, []string, error) {
	split := days || pick != nil
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	c, put, err := a.Conn(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer put()
	details, done, err := detailReader(c)
	if err != nil {
		return 0, nil, err
	}
	defer done()
	columns := append([]string{}, drivedb.Columns...)
	columns = append(columns, "Band")
	known := map[string]bool{}
	for _, k := range columns {
		known[k] = true
	}
	keys, err := db.All(c, "SELECT DISTINCT j.key FROM observation_details d,json_each(d.fields_json) j ORDER BY j.key")
	if err != nil {
		return 0, nil, err
	}
	for _, r := range keys {
		k := db.Str(r["key"])
		if !known[k] {
			columns = append(columns, k)
			known[k] = true
		}
	}
	type output struct {
		f    *os.File
		w    *csv.Writer
		used int64
	}
	opened := map[string]*output{}
	started := map[string]bool{}
	var clock, n, seen int64
	closeOutput := func(o *output) error {
		o.w.Flush()
		err := o.w.Error()
		if e := o.f.Close(); err == nil {
			err = e
		}
		return err
	}
	defer func() {
		for _, o := range opened {
			closeOutput(o)
		}
	}()
	get := func(name string) (*output, error) {
		clock++
		if o := opened[name]; o != nil {
			o.used = clock
			return o, nil
		}
		if len(opened) >= 64 {
			var oldest string
			age := int64(math.MaxInt64)
			for k, o := range opened {
				if o.used < age {
					oldest = k
					age = o.used
				}
			}
			o := opened[oldest]
			delete(opened, oldest)
			if err := closeOutput(o); err != nil {
				return nil, err
			}
		}
		path := dest
		if split {
			path = filepath.Join(dest, name)
		}
		flags := os.O_CREATE | os.O_WRONLY | os.O_EXCL
		if started[name] {
			flags = os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(path, flags, 0644)
		if err != nil {
			return nil, err
		}
		o := &output{f: f, w: csv.NewWriter(f), used: clock}
		opened[name] = o
		if !started[name] {
			if _, err = fmt.Fprintln(f, drivedb.Preamble(version)); err != nil {
				return nil, err
			}
			if err = o.w.Write(columns); err != nil {
				return nil, err
			}
			started[name] = true
		}
		return o, nil
	}
	if !split {
		if _, err := get(""); err != nil {
			return 0, nil, err
		}
	}
	// a picked export asks about each distinct day once per import, not once per row
	type choice struct {
		name string
		keep bool
	}
	var picked map[uint32]choice
	var pickedFor *obs.Segment
	num := func(v float64) string {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return ""
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	st := a.Obs.State()
	for _, s := range st.Segs {
		for i := 0; i < s.N; i++ {
			if seen%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return 0, nil, err
				}
				if progress != nil {
					progress(seen, int64(st.N))
				}
			}
			seen++
			first := st.Str[s.First[i]]
			name := ""
			if pick != nil {
				if pickedFor != s {
					picked, pickedFor = map[uint32]choice{}, s
				}
				ch, known := picked[s.Date[i]]
				if !known {
					ch.name, ch.keep = pick(s.ImportID, surveyDay(st.Str[s.Date[i]]))
					picked[s.Date[i]] = ch
				}
				if !ch.keep {
					continue
				}
				name = ch.name
			} else if days {
				day := "undated"
				if len(first) >= 10 {
					if _, err := time.Parse("2006-01-02", first[:10]); err == nil {
						day = first[:10]
					}
				}
				name = "wigle_" + day + ".csv"
			}
			o, err := get(name)
			if err != nil {
				return 0, nil, err
			}
			row := []string{st.Str[s.MAC[i]], st.Str[s.SSID[i]], "", first, st.Str[s.Channel[i]], num(s.Freq[i]), num(s.RSSI[i]), num(s.Lat[i]), num(s.Lon[i]), "", num(s.Acc[i]), "", "", "WIFI", ""}
			row = append(row, st.Str[s.Band[i]])
			extra, e := details(s.ID(i))
			if e != nil {
				return 0, nil, e
			}
			fields := map[string]string{}
			if extra != "" {
				if e := json.Unmarshal([]byte(extra), &fields); e != nil {
					return 0, nil, e
				}
			}
			for len(row) < len(columns) {
				row = append(row, "")
			}
			for k, name := range columns {
				if value, ok := fields[name]; ok {
					row[k] = value
				}
			}
			if err = o.w.Write(row); err != nil {
				return 0, nil, err
			}
			n++
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	for key, o := range opened {
		delete(opened, key)
		if err := closeOutput(o); err != nil {
			return 0, nil, err
		}
	}
	files := make([]string, 0, len(started))
	for name := range started {
		if split {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	if progress != nil {
		progress(seen, int64(st.N))
	}
	return n, files, nil
}

// surveyDay is a stored date when it is a calendar day, "" otherwise.
func surveyDay(date string) string {
	if len(date) == 10 {
		if _, err := time.Parse("2006-01-02", date); err == nil {
			return date
		}
	}
	return ""
}

// SurveyDrives lists what Wardrive Compare can take from the survey: its days and its imports,
// each with the rows it holds now. Rows without a readable day are counted, not listed: Compare
// has no use for a sighting it cannot place in time.
func (a *Atlas) SurveyDrives(ctx context.Context) (map[string]any, error) {
	c, put, err := a.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer put()
	named, err := db.All(c, "SELECT id,filename FROM imports")
	if err != nil {
		return nil, err
	}
	filename := map[int64]string{}
	for _, r := range named {
		filename[db.Int(r["id"])] = db.Str(r["filename"])
	}
	st := a.Obs.State()
	type span struct {
		rows     int64
		from, to string
	}
	byDay, byImport := map[string]int64{}, map[int64]*span{}
	var undated int64
	for _, s := range st.Segs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		counts := map[uint32]int64{}
		for i := 0; i < s.N; i++ {
			counts[s.Date[i]]++
		}
		for id, k := range counts {
			day := surveyDay(st.Str[id])
			if day == "" {
				undated += k
				continue
			}
			byDay[day] += k
			sp := byImport[s.ImportID]
			if sp == nil {
				sp = &span{from: day, to: day}
				byImport[s.ImportID] = sp
			}
			sp.rows += k
			sp.from, sp.to = min(sp.from, day), max(sp.to, day)
		}
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)
	dayRows := make([]Row, 0, len(days))
	for _, d := range days {
		dayRows = append(dayRows, Row{"key": d, "name": d, "rows": byDay[d]})
	}
	ids := make([]int64, 0, len(byImport))
	for id := range byImport {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	importRows := make([]Row, 0, len(ids))
	for _, id := range ids {
		sp := byImport[id]
		name := filename[id]
		if name == "" {
			name = "Import " + strconv.FormatInt(id, 10)
		}
		importRows = append(importRows, Row{"key": strconv.FormatInt(id, 10), "name": name, "rows": sp.rows, "from": sp.from, "to": sp.to})
	}
	return map[string]any{"observations": int64(st.N), "undated": undated, "days": dayRows, "imports": importRows}, nil
}

// SurveyDriveFile is one drive written for Wardrive Compare.
type SurveyDriveFile struct {
	Key, Name, Path string
	Size            int64
}

// ExportSurveyDrives writes the chosen days (by "day") or imports (by "import") of the survey as
// one WiGLE CSV each in dir, an owned empty folder, in the order of keys. A key with no dated rows
// writes no file.
func (a *Atlas) ExportSurveyDrives(ctx context.Context, dir, version, by string, keys []string, progress func(int64, int64)) ([]SurveyDriveFile, error) {
	if by != "day" && by != "import" {
		return nil, fmt.Errorf("Choose survey drives by day or by import")
	}
	file := map[string]string{}
	for i, k := range keys {
		if _, dup := file[k]; !dup {
			file[k] = fmt.Sprintf("drive-%04d.csv", i)
		}
	}
	pick := func(importID int64, day string) (string, bool) {
		if day == "" {
			return "", false
		}
		key := day
		if by == "import" {
			key = strconv.FormatInt(importID, 10)
		}
		name, ok := file[key]
		return name, ok
	}
	if _, _, err := a.exportSurveyCSV(ctx, dir, version, false, pick, progress); err != nil {
		return nil, err
	}
	var out []SurveyDriveFile
	done := map[string]bool{}
	for _, k := range keys {
		if done[k] {
			continue
		}
		done[k] = true
		path := filepath.Join(dir, file[k])
		info, err := os.Stat(path)
		if err != nil {
			continue // nothing in the survey for this key any more
		}
		out = append(out, SurveyDriveFile{Key: k, Path: path, Size: info.Size()})
	}
	return out, nil
}
