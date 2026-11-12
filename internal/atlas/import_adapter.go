package atlas

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"signal-atlas/internal/db"
	"signal-atlas/internal/drivedb"
	"zombiezen.com/go/sqlite"
)

// DatabaseImportCSV preserves vendor columns while adapting logger tables or a
// portable Atlas survey. The caller archives the database, not this scratch CSV.
func DatabaseImportCSV(ctx context.Context, path, dest string) (err error) {
	c, err := db.OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetInterrupt(ctx.Done())
	tables, err := db.All(c, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return err
	}
	columns := append([]string{}, drivedb.Columns...)
	columns = append(columns, "Band", "id", "day_table", "source")
	indices := map[string]int{}
	for i, k := range columns {
		indices[k] = i
	}
	canonicalNames := map[string]string{}
	for _, name := range columns {
		canonicalNames[strings.ToLower(name)] = name
	}
	canonical := func(k string) string {
		if name, ok := canonicalNames[strings.ToLower(k)]; ok {
			return name
		}
		return k
	}
	add := func(k string) {
		if _, ok := indices[k]; !ok {
			indices[k] = len(columns)
			columns = append(columns, k)
		}
	}
	type input struct {
		name  string
		names []string
		atlas bool
	}
	var inputs []input
	hasDetails := false
	for _, t := range tables {
		if db.Str(t["name"]) == "observation_details" {
			hasDetails = true
		}
	}
	for _, t := range tables {
		name := db.Str(t["name"])
		cols, e := db.All(c, "SELECT name FROM pragma_table_info(?)", name)
		if e != nil {
			return e
		}
		names := []string{}
		has := map[string]bool{}
		for _, col := range cols {
			k := db.Str(col["name"])
			names = append(names, k)
			has[strings.ToLower(k)] = true
		}
		isAtlas := name == "observations" && has["latitude"] && has["rssi_dbm"] && has["mac"]
		if !isAtlas && !(has["mac"] && has["currentlatitude"] && has["currentlongitude"]) {
			continue
		}
		inputs = append(inputs, input{name, names, isAtlas})
		if !isAtlas {
			for _, k := range names {
				add(canonical(k))
			}
		}
	}
	if len(inputs) == 0 {
		return fmt.Errorf("Database has no supported scan tables")
	}
	if hasDetails {
		keys, e := db.All(c, "SELECT DISTINCT j.key FROM observation_details d,json_each(d.fields_json) j ORDER BY j.key")
		if e != nil {
			return e
		}
		for _, r := range keys {
			add(db.Str(r["key"]))
		}
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
	w := csv.NewWriter(f)
	if err = w.Write(columns); err != nil {
		return err
	}
	for _, in := range inputs {
		query := "SELECT * FROM " + db.Q(in.name) + " ORDER BY rowid"
		if in.atlas {
			query = "SELECT o.*, "
			if hasDetails {
				query += "coalesce(d.fields_json,'')"
			} else {
				query += "''"
			}
			query += " AS atlas_extra FROM observations o"
			if hasDetails {
				query += " LEFT JOIN observation_details d ON d.observation_id=o.id"
			}
			query += " ORDER BY o.id"
		}
		err = db.Each(c, query, nil, func(s *sqlite.Stmt) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			row := make([]string, len(columns))
			put := func(k, v string) {
				if i, ok := indices[k]; ok {
					row[i] = v
				}
			}
			if in.atlas {
				mapping := map[string]string{"mac": "MAC", "ssid": "SSID", "first_seen": "FirstSeen", "channel": "Channel", "frequency_mhz": "Frequency", "rssi_dbm": "RSSI", "latitude": "CurrentLatitude", "longitude": "CurrentLongitude", "accuracy_m": "AccuracyMeters", "band": "Band", "record_id": "id"}
				for i := 0; i < s.ColumnCount(); i++ {
					if k, ok := mapping[s.ColumnName(i)]; ok {
						put(k, colText(s, i))
					}
				}
				extra := colText(s, s.ColumnCount()-1)
				if extra != "" {
					var fields map[string]string
					if err := json.Unmarshal([]byte(extra), &fields); err != nil {
						return err
					}
					for k, v := range fields {
						put(k, v)
					}
				}
				put("Type", "WIFI")
			} else {
				for i, k := range in.names {
					put(canonical(k), colText(s, i))
				}
				put("day_table", in.name)
			}
			return w.Write(row)
		})
		if err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
