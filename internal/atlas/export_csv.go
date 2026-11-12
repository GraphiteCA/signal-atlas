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
)

// ExportSurveyCSV streams a committed observation state. dest is an owned
// staging file or directory; the caller publishes it only after success.
func (a *Atlas) ExportSurveyCSV(ctx context.Context, dest, version string, days bool, progress func(int64, int64)) (int64, []string, error) {
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
	var clock, n int64
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
		if days {
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
	if !days {
		if _, err := get(""); err != nil {
			return 0, nil, err
		}
	}
	num := func(v float64) string {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return ""
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	st := a.Obs.State()
	for _, s := range st.Segs {
		for i := 0; i < s.N; i++ {
			if n%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return 0, nil, err
				}
				if progress != nil {
					progress(n, int64(st.N))
				}
			}
			first := st.Str[s.First[i]]
			name := ""
			if days {
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
		if days {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	if progress != nil {
		progress(n, int64(st.N))
	}
	return n, files, nil
}
