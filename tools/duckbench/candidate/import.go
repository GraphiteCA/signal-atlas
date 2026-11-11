// Benchmark-only donor candidate; see README.md for origin and deliberate differences.
package candidate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ImportProgress func(Row)

// Import delegates parsing, validation and insertion to vectorized native SQL.
// No Go object, SQL bind or index mutation is performed for each observation.
func (s *Store) Import(ctx context.Context, path, name string, skipInvalid bool, progress ImportProgress) (Row, error) {
	s.Write.Lock()
	defer s.Write.Unlock()
	start := time.Now()
	report := func(phase string, fields Row) {
		if progress != nil {
			fields["phase"] = phase
			fields["elapsed_seconds"] = time.Since(start).Seconds()
			progress(fields)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	report("Verifying file", Row{"bytes_total": info.Size()})
	h := sha256.New()
	buf := make([]byte, 4<<20)
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		n, e := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
	}
	digest := hex.EncodeToString(h.Sum(nil))
	old, err := s.One(ctx, "SELECT * FROM imports WHERE sha256=?", digest)
	if err != nil {
		return nil, err
	}
	if len(old) > 0 {
		old["duplicate"] = true
		return old, nil
	}
	if _, err = f.Seek(0, 0); err != nil {
		return nil, err
	}
	reader := csv.NewReader(bufio.NewReaderSize(f, 64<<10))
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("CSV header: %w", err)
	}
	skip := 0
	if len(header) > 0 && strings.HasPrefix(strings.ToLower(strings.TrimPrefix(header[0], "\ufeff")), "wiglewifi-") {
		skip = 1
		header, err = reader.Read()
		if err != nil {
			return nil, err
		}
	}
	cols := map[string]string{}
	declarations := []string{}
	for _, c := range header {
		c = strings.TrimSpace(strings.TrimPrefix(c, "\ufeff"))
		if c == "" {
			return nil, fmt.Errorf("CSV has an empty column name")
		}
		if _, ok := cols[strings.ToLower(c)]; ok {
			return nil, fmt.Errorf("duplicate CSV column %q", c)
		}
		cols[strings.ToLower(c)] = c
		declarations = append(declarations, literal(c)+":'VARCHAR'")
	}
	for _, c := range []string{"MAC", "SSID", "RSSI", "CurrentLatitude", "CurrentLongitude"} {
		if _, ok := cols[strings.ToLower(c)]; !ok {
			return nil, fmt.Errorf("missing CSV column %s", c)
		}
	}
	col := func(c string) string {
		if v, ok := cols[strings.ToLower(c)]; ok {
			return "coalesce(" + ident(v) + ",'')"
		}
		return "''"
	}
	if name == "" {
		name = filepath.Base(path)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var importID, baseID int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(id),0)+1 FROM imports").Scan(&importID); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(id),0) FROM observations").Scan(&baseID); err != nil {
		return nil, err
	}
	report("Parsing and validating CSV", Row{"bytes_total": info.Size()})
	// Invalid numeric values are retained as NULL in staging, then rejected or
	// explicitly counted and skipped. Structural CSV errors always abort the file.
	freq := "lower(replace(trim(" + col("Frequency") + "),' ',''))"
	stage := fmt.Sprintf(`CREATE OR REPLACE TEMP TABLE incoming AS
 WITH raw AS (SELECT row_number() OVER ()+%d line,%s ssid,upper(trim(%s)) mac,
 try_cast(%s AS DOUBLE) latitude,try_cast(%s AS DOUBLE) longitude,try_cast(%s AS DOUBLE) rssi_dbm,
 try_cast(nullif(%s,'') AS DOUBLE) accuracy_m,%s accuracy_text,%s first_seen,
 %s channel,%s record_id,upper(trim(%s)) kind,%s f,
 replace(replace(lower(%s),'ghz',''),' ','') declared
 FROM read_csv(%s,header=true,skip=%d,auto_detect=false,columns={%s},delim=',',quote='"',escape='"',null_padding=false,strict_mode=true)),
 frequencies AS (SELECT *,try_cast(regexp_replace(f,'(ghz|mhz|khz|hz)$','') AS DOUBLE) fv FROM raw),
 measured AS (SELECT *,CASE WHEN ends_with(f,'ghz') THEN fv*1000 WHEN ends_with(f,'mhz') THEN fv
 WHEN ends_with(f,'khz') THEN fv/1000 WHEN ends_with(f,'hz') THEN fv/1000000
 WHEN fv>=1e8 THEN fv/1000000 WHEN fv>=1e6 THEN fv/1000 WHEN fv<100 THEN fv*1000 ELSE fv END frequency_mhz FROM frequencies),
 bands AS (SELECT *,CASE WHEN frequency_mhz>0 AND isfinite(frequency_mhz) THEN
 CASE WHEN frequency_mhz>=2400 AND frequency_mhz<2500 THEN '2.4' WHEN frequency_mhz>=4900 AND frequency_mhz<5925 THEN '5'
 WHEN frequency_mhz>=5925 AND frequency_mhz<=7125 THEN '6' ELSE 'other' END ELSE '' END measured_band FROM measured),
 normalized AS (SELECT *,CASE WHEN measured_band<>'' AND declared IN ('2.4','5','6') AND declared<>measured_band THEN 'unknown'
 WHEN measured_band<>'' THEN measured_band WHEN declared IN ('2.4','5','6') THEN declared ELSE 'unknown' END band,
 CASE WHEN measured_band<>'' AND declared IN ('2.4','5','6') AND declared<>measured_band THEN 'conflicting frequency and explicit band'
 WHEN measured_band<>'' THEN 'frequency' WHEN declared IN ('2.4','5','6') THEN 'explicit band' ELSE 'frequency/band unavailable' END band_source
 FROM bands)
 SELECT line,n.ssid,n.mac,coalesce(a.band,n.band) band,
 CASE WHEN a.band IS NOT NULL THEN 'manual assignment' ELSE n.band_source END band_source,
 latitude,longitude,rssi_dbm,accuracy_m,first_seen,channel,record_id,
 CASE WHEN isfinite(frequency_mhz) AND frequency_mhz>0 THEN frequency_mhz ELSE NULL END frequency_mhz,kind,
 CASE WHEN try_cast(substr(first_seen,1,10) AS DATE) IS NOT NULL THEN substr(first_seen,1,10) ELSE '' END date,
 coalesce(n.mac<>'' AND isfinite(latitude) AND isfinite(longitude) AND isfinite(rssi_dbm)
 AND latitude>-85 AND latitude<85 AND longitude BETWEEN -180 AND 180 AND rssi_dbm BETWEEN -150 AND 0
 AND (accuracy_text='' OR (accuracy_m>=0 AND isfinite(accuracy_m))),false) valid
 FROM normalized n LEFT JOIN band_assignments a ON n.ssid=a.ssid AND n.mac=a.mac AND n.band_source='frequency/band unavailable'`,
		skip+1, col("SSID"), col("MAC"), col("CurrentLatitude"), col("CurrentLongitude"), col("RSSI"), col("AccuracyMeters"), col("AccuracyMeters"), col("FirstSeen"), col("Channel"), col("id"), col("Type"), freq, col("Band"), literal(path), skip, strings.Join(declarations, ","))
	if _, err = tx.ExecContext(ctx, stage); err != nil {
		return nil, fmt.Errorf("CSV parsing failed (file rolled back): %w", err)
	}
	rows, err := query(ctx, tx, `SELECT count(*) FILTER(WHERE valid AND kind IN ('','WIFI') AND NOT (latitude=0 AND longitude=0))::BIGINT valid_rows,
 count(*) FILTER(WHERE kind IN ('','WIFI') AND latitude=0 AND longitude=0)::BIGINT zero_excluded_rows,
 count(*) FILTER(WHERE NOT valid AND kind IN ('','WIFI'))::BIGINT invalid_rows,
 count(*) FILTER(WHERE kind NOT IN ('','WIFI'))::BIGINT non_wifi_rows FROM incoming`)
	if err != nil {
		return nil, err
	}
	counts := rows[0]
	valid, invalid := int64(num(counts["valid_rows"])), int64(num(counts["invalid_rows"]))
	if invalid > 0 && !skipInvalid {
		return nil, fmt.Errorf("%d invalid measurements; file rolled back. Enable Skip invalid rows to exclude them", invalid)
	}
	report("Writing observations", Row{"completed": valid, "invalid": invalid, "bytes_total": info.Size(), "bytes_read": info.Size()})
	_, err = tx.ExecContext(ctx, `INSERT INTO observations SELECT ?+row_number() OVER (),?,line,record_id,ssid,mac,band,band_source,
 to_json(list_value(ssid,mac,band))::VARCHAR,latitude,longitude,rssi_dbm,accuracy_m,first_seen,date,channel,frequency_mhz
 FROM incoming WHERE valid AND kind IN ('','WIFI') AND NOT (latitude=0 AND longitude=0) ORDER BY floor(latitude*100),floor(longitude*100)`, baseID, importID)
	if err != nil {
		return nil, err
	}
	report("Updating network catalog", Row{"completed": valid, "invalid": invalid})
	_, err = tx.ExecContext(ctx, `INSERT INTO radio_stats
 SELECT to_json(list_value(ssid,mac,band))::VARCHAR,ssid,mac,band,count(*)::BIGINT,max(rssi_dbm),min(latitude),max(latitude),min(longitude),max(longitude),min(date),max(date)
 FROM incoming WHERE valid AND kind IN ('','WIFI') AND NOT (latitude=0 AND longitude=0) GROUP BY ssid,mac,band
 ON CONFLICT(radio_key) DO UPDATE SET count=radio_stats.count+excluded.count,peak=greatest(radio_stats.peak,excluded.peak),
 south=least(radio_stats.south,excluded.south),north=greatest(radio_stats.north,excluded.north),
 west=least(radio_stats.west,excluded.west),east=greatest(radio_stats.east,excluded.east),
 first_date=least(radio_stats.first_date,excluded.first_date),last_date=greatest(radio_stats.last_date,excluded.last_date)`)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, clearEstimatesSQL); err != nil {
		return nil, err
	}
	elapsed := time.Since(start).Seconds()
	if _, err = tx.ExecContext(ctx, "INSERT INTO imports VALUES(?,?,?,?,?,?,?,?)", importID, name, digest, time.Now().UTC().Format(time.RFC3339), valid, invalid, counts["non_wifi_rows"], elapsed); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "DROP TABLE incoming"); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	// Include commit/WAL time in the measured rate.
	elapsed = time.Since(start).Seconds()
	result := Row{"filename": name, "valid_rows": valid, "invalid_rows": invalid, "non_wifi_rows": counts["non_wifi_rows"], "zero_excluded_rows": counts["zero_excluded_rows"], "elapsed_seconds": elapsed, "rows_per_second": float64(valid) / elapsed, "sha256": digest, "duplicate": false}
	report("Complete", Row{"completed": valid, "invalid": invalid, "rows_per_second": float64(valid) / elapsed})
	return result, nil
}
