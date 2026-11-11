// Package drivedb is the Drive database: one SQLite file on the user's computer that collects
// every sighting from any number of wardriving logs and databases, without duplicates, and writes
// them all back out as WiGLE CSV.
//
// It adds WiGLE CSV logs (from any logger), SQLite files whose tables have WiGLE's columns (a
// logger's per-day database files, or another Drive database), Signal Atlas databases and WiGLE
// Android app backups. A sighting already in the database, from whichever file, is not added again;
// a file already added (the same bytes) is skipped whole.
//
// Rows are kept as the text they were read as, so a WiGLE CSV written back out says what the logs
// said. The file is ordinary SQLite: any SQLite tool opens it.
package drivedb

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"signal-atlas/internal/db"
	"signal-atlas/internal/geo"
	"signal-atlas/internal/pycsv"

	"zombiezen.com/go/sqlite"
)

// Columns are the columns kept for every sighting: WiGLE's CSV columns, then the logging node.
var Columns = []string{"MAC", "SSID", "AuthMode", "FirstSeen", "Channel", "Frequency", "RSSI",
	"CurrentLatitude", "CurrentLongitude", "AltitudeMeters", "AccuracyMeters", "RCOIs", "MfgrId", "Type", "Node"}

const (
	cMAC = iota
	cSSID
	cAuth
	cFirst
	cChannel
	cFreq
	cRSSI
	cLat
	cLon
	cAlt
	cAcc
	cRCOIs
	cMfgr
	cType
	cNode
	nCols
)

// Row is one sighting, as text.
type Row [nCols]string

// Format is the value of drive_meta's "format": what marks a file as a Drive database.
const Format = "signal-atlas-drive-database"

const schema = `
CREATE TABLE IF NOT EXISTS drive_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS drive_sources(id INTEGER PRIMARY KEY, name TEXT NOT NULL, path TEXT NOT NULL,
  size INTEGER NOT NULL, sha256 TEXT NOT NULL UNIQUE, kind TEXT NOT NULL, added_utc TEXT NOT NULL,
  rows_read INTEGER NOT NULL, rows_added INTEGER NOT NULL, duplicates INTEGER NOT NULL, skipped INTEGER NOT NULL,
  first_seen TEXT, last_seen TEXT, zero_excluded_rows INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS sightings(MAC TEXT, SSID TEXT, AuthMode TEXT, FirstSeen TEXT, Channel TEXT,
  Frequency TEXT, RSSI TEXT, CurrentLatitude TEXT, CurrentLongitude TEXT, AltitudeMeters TEXT,
  AccuracyMeters TEXT, RCOIs TEXT, MfgrId TEXT, Type TEXT, Node TEXT, source INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sighting_keys(k INTEGER PRIMARY KEY);
`

// DB is an open Drive database.
type DB struct {
	Path string
	c    *sqlite.Conn
}

func pragmas(c *sqlite.Conn) {
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA cache_size=-524288", "PRAGMA temp_store=FILE"} {
		db.Exec(c, p)
	}
}

// Create makes a new, empty Drive database at path (which must not exist).
func Create(path string) (*DB, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s already exists; open it instead, or choose another name", filepath.Base(path))
	}
	c, err := db.Open(path, false)
	if err != nil {
		return nil, err
	}
	pragmas(c)
	if err := db.Script(c, schema); err != nil {
		c.Close()
		os.Remove(path)
		return nil, err
	}
	db.Exec(c, "INSERT INTO drive_meta VALUES('format',?),('version','1'),('created_utc',?)", Format, nowUTC())
	return &DB{Path: path, c: c}, nil
}

// Open opens an existing Drive database.
func Open(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	c, err := db.Open(path, false)
	if err != nil {
		return nil, err
	}
	v, err := db.Scalar(c, "SELECT value FROM drive_meta WHERE key='format'")
	if err != nil || db.Str(v) != Format {
		c.Close()
		return nil, fmt.Errorf("%s isn't a Drive database. To bring its rows in, open or create a Drive database and add this file to it", filepath.Base(path))
	}
	pragmas(c)
	if err := db.Script(c, schema); err != nil {
		c.Close()
		return nil, err
	}
	cols, err := db.All(c, "PRAGMA table_info(drive_sources)")
	if err != nil {
		c.Close()
		return nil, err
	}
	found := false
	for _, r := range cols {
		found = found || db.Str(r["name"]) == "zero_excluded_rows"
	}
	if !found {
		if _, err := db.Exec(c, "ALTER TABLE drive_sources ADD COLUMN zero_excluded_rows INTEGER NOT NULL DEFAULT 0"); err != nil {
			c.Close()
			return nil, err
		}
	}
	return &DB{Path: path, c: c}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.c.Close() }

func nowUTC() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// Info describes the database: its size, how many sightings, the span of time, the sources.
func (d *DB) Info() (map[string]any, error) {
	st, _ := os.Stat(d.Path)
	var size int64
	if st != nil {
		size = st.Size()
	}
	n, err := db.Scalar(d.c, "SELECT coalesce(sum(rows_added),0) FROM drive_sources")
	if err != nil {
		return nil, err
	}
	span, err := db.One(d.c, "SELECT min(first_seen) first, max(last_seen) last FROM drive_sources")
	if err != nil {
		return nil, err
	}
	src, err := db.All(d.c, "SELECT id,name,path,size,kind,added_utc,rows_read,rows_added,duplicates,skipped,zero_excluded_rows,first_seen,last_seen FROM drive_sources ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"path": d.Path, "name": filepath.Base(d.Path), "size": size, "sightings": db.Int(n), "sources": src}
	if span != nil {
		out["first_seen"], out["last_seen"] = span["first"], span["last"]
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// adding files

// Progress hears how far adding one file has got: rows read, and bytes (or rows) read of a total.
type Progress func(file string, rows, done, total int64)

// Result is what adding one file did.
type Result struct {
	ZeroExcluded int64  `json:"zero_excluded_rows"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	RowsRead     int64  `json:"rows_read"`
	RowsAdded    int64  `json:"rows_added"`
	Duplicates   int64  `json:"duplicates"`
	Skipped      int64  `json:"skipped"`
	Already      bool   `json:"already_added,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ErrCancelled is returned when cancelled() turned true; nothing of the file is kept.
var ErrCancelled = errors.New("cancelled")

// Add adds one file. A failure (or a cancel) leaves the database as it was.
func (d *DB) Add(path string, progress Progress, cancelled func() bool) (Result, error) {
	res := Result{Name: filepath.Base(path)}
	if abs, _ := filepath.Abs(path); strings.EqualFold(abs, d.Path) {
		return res, fmt.Errorf("that is this database itself")
	}
	st, err := os.Stat(path)
	if err != nil {
		return res, err
	}
	kind, err := Detect(path)
	if err != nil {
		return res, err
	}
	res.Kind = kind
	sum, err := fileSHA256(path)
	if err != nil {
		return res, err
	}
	if have, err := db.One(d.c, "SELECT rows_added FROM drive_sources WHERE sha256=?", sum); err != nil {
		return res, err
	} else if have != nil {
		res.Already = true
		return res, nil
	}
	w := &writer{d: d, cancelled: cancelled}
	if err := w.begin(); err != nil {
		return res, err
	}
	report := func(rows, done, total int64) {
		if progress != nil {
			progress(res.Name, rows, done, total)
		}
	}
	switch kind {
	case KindCSV:
		err = readCSV(path, w, report)
	case KindWiGLEBackup:
		err = readWiGLEBackup(path, w, report)
	case KindAtlas:
		err = readAtlas(path, w, report)
	default:
		err = readTables(path, w, report)
	}
	if err == nil {
		err = w.finish()
	} else {
		w.abort()
	}
	if err == nil && cancelled != nil && cancelled() {
		err = ErrCancelled
	}
	if err != nil {
		w.rollback()
		return res, err
	}
	res.RowsRead, res.RowsAdded, res.Duplicates, res.Skipped = w.read, w.added, w.dups, w.skipped
	res.ZeroExcluded = w.zero
	if _, err := db.Exec(d.c, `INSERT INTO drive_sources(id,name,path,size,sha256,kind,added_utc,rows_read,rows_added,duplicates,skipped,first_seen,last_seen,zero_excluded_rows)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, w.source, res.Name, path, st.Size(), sum, kind, nowUTC(),
		w.read, w.added, w.dups, w.skipped, nullStr(w.first), nullStr(w.last), w.zero); err != nil {
		w.rollback()
		return res, err
	}
	if _, err := db.Exec(d.c, "COMMIT"); err != nil {
		w.rollback()
		return res, err
	}
	return res, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 4<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writer takes rows in batches, drops the ones the database already has, and inserts the rest.
// Reading and parsing (the caller of add) run alongside the database writes (consume): full
// batches go over a channel, so the two halves overlap.
type writer struct {
	zero      int64
	d         *DB
	cancelled func() bool
	source    int64
	cur       *batch
	ch        chan *batch
	free      chan *batch
	dead      chan struct{} // closed when consume returns
	cerr      error         // consume's error, read after dead is closed
	closed    bool
	read      int64 // the producer's counts
	skipped   int64
	added     int64 // consume's counts, read after dead is closed
	dups      int64
	first     string
	last      string
	keyIns    *sqlite.Stmt
	rowIns    *sqlite.Stmt
}

// sink takes the rows a reader reads: the database writer, or a CSV (ToCSV).
type sink interface {
	add(r *Row) error
	rows() int64
}

func (w *writer) rows() int64 { return w.read }

type batch struct {
	rows []Row
	keys []int64
}

const batchRows = 65536

func (w *writer) begin() error {
	id, err := db.Scalar(w.d.c, "SELECT coalesce(max(id),0)+1 FROM drive_sources")
	if err != nil {
		return err
	}
	w.source = db.Int(id)
	if _, err := db.Exec(w.d.c, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if w.keyIns, err = w.d.c.Prepare("INSERT OR IGNORE INTO sighting_keys(k) VALUES(?)"); err != nil {
		db.Exec(w.d.c, "ROLLBACK")
		return err
	}
	if w.rowIns, err = w.d.c.Prepare("INSERT INTO sightings VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"); err != nil {
		db.Exec(w.d.c, "ROLLBACK")
		return err
	}
	w.ch, w.free, w.dead = make(chan *batch, 2), make(chan *batch, 4), make(chan struct{})
	for i := 0; i < 3; i++ {
		w.free <- &batch{rows: make([]Row, 0, batchRows), keys: make([]int64, 0, batchRows)}
	}
	w.cur = <-w.free
	go w.consume()
	return nil
}

func (w *writer) rollback() { db.Exec(w.d.c, "ROLLBACK") }

// add takes one row; rows without a MAC are skipped.
func (w *writer) add(r *Row) error {
	w.read++
	if w.read&4095 == 0 && w.cancelled != nil && w.cancelled() {
		return ErrCancelled
	}
	for i := range r {
		r[i] = strings.TrimSpace(r[i])
	}
	if r[cMAC] == "" {
		w.skipped++
		return nil
	}
	if geo.ZeroPairText(r[cLat], r[cLon]) {
		w.zero++
		return nil
	}
	w.cur.rows = append(w.cur.rows, *r)
	w.cur.keys = append(w.cur.keys, Key(r))
	if len(w.cur.rows) == batchRows {
		return w.send()
	}
	return nil
}

func (w *writer) send() error {
	select {
	case w.ch <- w.cur:
	case <-w.dead:
		return w.consumeErr()
	}
	select {
	case w.cur = <-w.free:
		return nil
	case <-w.dead:
		return w.consumeErr()
	}
}

func (w *writer) consumeErr() error {
	if w.cerr != nil {
		return w.cerr
	}
	return ErrCancelled
}

// finish sends the last rows and waits for the database writes.
func (w *writer) finish() error {
	if len(w.cur.rows) > 0 {
		if err := w.send(); err != nil {
			w.abort()
			return err
		}
	}
	w.abort()
	return w.cerr
}

// abort stops consume and waits for it (the transaction is still open: roll back or commit).
func (w *writer) abort() {
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
	<-w.dead
}

func (w *writer) consume() {
	for b := range w.ch {
		if err := w.write(b); err != nil {
			// say so first (the producer may be waiting for a free batch), then take whatever is
			// still sent until the channel closes, touching the database no more
			w.cerr = err
			close(w.dead)
			for range w.ch {
			}
			return
		}
		b.rows, b.keys = b.rows[:0], b.keys[:0]
		w.free <- b
	}
	close(w.dead)
}

// write inserts one batch. Keys are looked up and inserted in sorted order, which keeps SQLite's
// index pages in cache however large the database grows; rows go in in the order they were read.
func (w *writer) write(b *batch) error {
	if w.cancelled != nil && w.cancelled() {
		return ErrCancelled
	}
	order := make([]int, len(b.rows))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(x, y int) bool { return b.keys[order[x]] < b.keys[order[y]] })
	fresh := make([]bool, len(b.rows))
	for _, i := range order {
		w.keyIns.BindInt64(1, b.keys[i])
		if _, err := w.keyIns.Step(); err != nil {
			return err
		}
		fresh[i] = w.d.c.Changes() > 0
		w.keyIns.Reset()
	}
	for i := range b.rows {
		if !fresh[i] {
			w.dups++
			continue
		}
		r := &b.rows[i]
		for k := 0; k < nCols; k++ {
			if r[k] == "" {
				w.rowIns.BindNull(k + 1)
			} else {
				w.rowIns.BindText(k+1, r[k])
			}
		}
		w.rowIns.BindInt64(nCols+1, w.source)
		if _, err := w.rowIns.Step(); err != nil {
			return err
		}
		w.rowIns.Reset()
		w.added++
		if t := r[cFirst]; t != "" {
			if w.first == "" || t < w.first {
				w.first = t
			}
			if t > w.last {
				w.last = t
			}
		}
	}
	return nil
}

// Key identifies a sighting across files: the device, when, where (to a tenth of a metre), how
// loud, what kind of radio, and which node heard it. The same sighting read from a CSV log and from
// a logger's own database gets the same key, however each wrote its numbers.
func Key(r *Row) int64 {
	h := fnv.New64a()
	put := func(s string) { io.WriteString(h, s); h.Write([]byte{0}) }
	put(strings.ToUpper(strings.ReplaceAll(r[cMAC], "-", ":")))
	put(r[cFirst])
	t := strings.ToUpper(r[cType])
	if t == "" || t == "W" {
		t = "WIFI"
	}
	put(t)
	put(num(r[cLat], 1e6))
	put(num(r[cLon], 1e6))
	put(num(r[cRSSI], 1))
	put(r[cNode])
	return int64(h.Sum64())
}

func num(s string, scale float64) string {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return s
	}
	return strconv.FormatInt(int64(math.Round(v*scale)), 10)
}

// ---------------------------------------------------------------------------
// what a file is

// Kinds of file Add takes.
const (
	KindCSV         = "WiGLE CSV"
	KindTables      = "SQLite (WiGLE columns)"
	KindAtlas       = "Signal Atlas database"
	KindWiGLEBackup = "WiGLE app backup"
	KindDrive       = "Drive database"
)

// Detect says what a file is, or why it can't be added.
func Detect(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	f.Close()
	if n < 16 || string(head) != "SQLite format 3\x00" {
		if strings.EqualFold(filepath.Ext(path), ".csv") || strings.EqualFold(filepath.Ext(path), ".txt") {
			return KindCSV, nil
		}
		return "", fmt.Errorf("%s is neither a CSV log nor an SQLite database", filepath.Base(path))
	}
	c, err := db.OpenReadOnly(path)
	if err != nil {
		return "", err
	}
	defer c.Close()
	t, err := tablesWithColumns(c)
	if err != nil {
		return "", err
	}
	switch {
	case t["drive_meta"] != nil && t["sightings"] != nil:
		return KindDrive, nil
	case t["network"] != nil && t["location"] != nil:
		return KindWiGLEBackup, nil
	case t["observations"] != nil && t["observations"]["mac"] && t["observations"]["latitude"] && t["observations"]["rssi_dbm"]:
		return KindAtlas, nil
	}
	for _, cols := range t {
		if wigleTable(cols) {
			return KindTables, nil
		}
	}
	return "", fmt.Errorf("%s has no table with WiGLE's columns (MAC, CurrentLatitude, CurrentLongitude)", filepath.Base(path))
}

// tablesWithColumns maps each table to its columns, lower-cased.
func tablesWithColumns(c *sqlite.Conn) (map[string]map[string]bool, error) {
	rows, err := db.All(c, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]bool{}
	for _, r := range rows {
		name := db.Str(r["name"])
		cols, err := db.All(c, "SELECT name FROM pragma_table_info(?)", name)
		if err != nil {
			return nil, err
		}
		m := map[string]bool{}
		for _, col := range cols {
			m[strings.ToLower(db.Str(col["name"]))] = true
		}
		out[strings.ToLower(name)] = m
		out[strings.ToLower(name)]["\x00name="+name] = true // the table's real name
	}
	return out, nil
}

func realName(cols map[string]bool) string {
	for k := range cols {
		if strings.HasPrefix(k, "\x00name=") {
			return strings.TrimPrefix(k, "\x00name=")
		}
	}
	return ""
}

func wigleTable(cols map[string]bool) bool {
	return cols["mac"] && cols["currentlatitude"] && cols["currentlongitude"]
}

// ---------------------------------------------------------------------------
// readers

// readCSV reads a WiGLE CSV log (with or without WiGLE's first "WigleWifi-1.x" line).
func readCSV(path string, w sink, report func(rows, done, total int64)) error {
	var idx []int // Row column -> CSV column, or -1
	header := false
	var r Row
	var bytesRead, bytesTotal int64
	err := pycsv.ReadFile(path, func(rec []string) error {
		if !header {
			if len(rec) > 0 && strings.HasPrefix(strings.ToLower(strings.TrimSpace(rec[0])), "wiglewifi-") {
				return nil
			}
			header = true
			pos := map[string]int{}
			for i, n := range rec {
				pos[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(n, "\uFEFF")))] = i
			}
			if _, ok := pos["mac"]; !ok {
				return fmt.Errorf("%s has no MAC column; is it a WiGLE CSV log?", filepath.Base(path))
			}
			idx = make([]int, nCols)
			for k, name := range Columns {
				if i, ok := pos[strings.ToLower(name)]; ok {
					idx[k] = i
				} else {
					idx[k] = -1
				}
			}
			return nil
		}
		if len(rec) == 0 || (len(rec) == 1 && strings.TrimSpace(rec[0]) == "") {
			return nil
		}
		for k, i := range idx {
			if i >= 0 && i < len(rec) {
				r[k] = rec[i]
			} else {
				r[k] = ""
			}
		}
		if err := w.add(&r); err != nil {
			return err
		}
		if w.rows()%65536 == 0 {
			report(w.rows(), bytesRead, bytesTotal)
		}
		return nil
	}, func(read, total int64) { bytesRead, bytesTotal = read, total })
	report(w.rows(), bytesRead, bytesTotal)
	return err
}

// readTables reads every table with WiGLE's columns (a logger's day tables, another Drive
// database's sightings). Views are left out, so a view stacking the day tables isn't read twice.
func readTables(path string, w sink, report func(rows, done, total int64)) error {
	c, err := db.OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer c.Close()
	t, err := tablesWithColumns(c)
	if err != nil {
		return err
	}
	var names []string
	for _, cols := range t {
		if wigleTable(cols) {
			names = append(names, realName(cols))
		}
	}
	sort.Strings(names)
	var total int64
	for _, n := range names {
		v, err := db.Scalar(c, "SELECT count(*) FROM "+db.Q(n))
		if err != nil {
			return err
		}
		total += db.Int(v)
	}
	for _, n := range names {
		cols := t[strings.ToLower(n)]
		sel := make([]string, nCols)
		for k, name := range Columns {
			if cols[strings.ToLower(name)] {
				sel[k] = db.Q(name)
			} else {
				sel[k] = "NULL"
			}
		}
		var r Row
		err := db.Each(c, "SELECT "+strings.Join(sel, ",")+" FROM "+db.Q(n)+" ORDER BY rowid", nil, func(st *sqlite.Stmt) error {
			for k := 0; k < nCols; k++ {
				r[k] = text(st, k)
			}
			if err := w.add(&r); err != nil {
				return err
			}
			if w.rows()%65536 == 0 {
				report(w.rows(), w.rows(), total)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	report(w.rows(), w.rows(), total)
	return nil
}

// text is a column as the text SQLite gives for it (numbers as SQLite prints them), or "".
func text(st *sqlite.Stmt, i int) string {
	switch st.ColumnType(i) {
	case sqlite.TypeNull:
		return ""
	case sqlite.TypeInteger:
		return strconv.FormatInt(st.ColumnInt64(i), 10)
	case sqlite.TypeFloat:
		return strconv.FormatFloat(st.ColumnFloat(i), 'f', -1, 64)
	}
	return st.ColumnText(i)
}

// readAtlas reads a Signal Atlas database's observations (Wi-Fi only; Signal Atlas keeps no others).
func readAtlas(path string, w sink, report func(rows, done, total int64)) error {
	c, err := db.OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer c.Close()
	v, _ := db.Scalar(c, "SELECT count(*) FROM observations")
	total := db.Int(v)
	var r Row
	return db.Each(c, "SELECT mac,ssid,first_seen,channel,frequency_mhz,rssi_dbm,latitude,longitude,accuracy_m FROM observations ORDER BY id", nil, func(st *sqlite.Stmt) error {
		r = Row{}
		r[cMAC], r[cSSID], r[cFirst], r[cChannel] = text(st, 0), text(st, 1), text(st, 2), text(st, 3)
		r[cFreq], r[cRSSI], r[cLat], r[cLon], r[cAcc] = text(st, 4), text(st, 5), text(st, 6), text(st, 7), text(st, 8)
		r[cType] = "WIFI"
		if err := w.add(&r); err != nil {
			return err
		}
		if w.rows()%65536 == 0 {
			report(w.rows(), w.rows(), total)
		}
		return nil
	})
}

// wigleTypes maps the WiGLE app's one-letter radio types to its CSV's names.
var wigleTypes = map[string]string{"W": "WIFI", "B": "BT", "E": "BLE", "G": "GSM", "C": "CDMA", "L": "LTE", "D": "WCDMA", "N": "NR"}

// readWiGLEBackup reads the WiGLE Android app's database backup: every sighting in its location
// table, with the network it belongs to.
func readWiGLEBackup(path string, w sink, report func(rows, done, total int64)) error {
	c, err := db.OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer c.Close()
	v, _ := db.Scalar(c, "SELECT count(*) FROM location")
	total := db.Int(v)
	var r Row
	return db.Each(c, `SELECT l.bssid, n.ssid, n.capabilities, l.time, n.frequency, l.level, l.lat, l.lon, l.altitude, l.accuracy, n.type
		FROM location l LEFT JOIN network n ON n.bssid = l.bssid ORDER BY l._id`, nil, func(st *sqlite.Stmt) error {
		r = Row{}
		r[cMAC], r[cSSID], r[cAuth] = strings.ToUpper(text(st, 0)), text(st, 1), text(st, 2)
		if ms := st.ColumnInt64(3); ms > 0 {
			r[cFirst] = time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05")
		}
		typ := wigleTypes[text(st, 10)]
		if typ == "" {
			typ = text(st, 10)
		}
		r[cType] = typ
		if f := st.ColumnInt64(4); f > 0 && typ == "WIFI" {
			r[cFreq] = strconv.FormatInt(f, 10)
			r[cChannel] = channel(f)
		}
		r[cRSSI], r[cLat], r[cLon], r[cAlt], r[cAcc] = text(st, 5), text(st, 6), text(st, 7), text(st, 8), text(st, 9)
		if r[cLat] == "0" && r[cLon] == "0" { // recorded before a GPS fix
			r[cLat], r[cLon] = "", ""
		}
		if err := w.add(&r); err != nil {
			return err
		}
		if w.rows()%65536 == 0 {
			report(w.rows(), w.rows(), total)
		}
		return nil
	})
}

func channel(mhz int64) string {
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

// ---------------------------------------------------------------------------
// exporting

// Preamble is the first line of every exported CSV, as WiGLE's own files start.
func Preamble(version string) string {
	return "WigleWifi-1.6,appRelease=" + version + ",model=Signal Atlas,release=" + version + ",device=Signal Atlas,display=,board=,brand=Signal Atlas,star=Sol,body=3,subBody=0"
}

func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\r\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func writeRow(b *bufio.Writer, vals []string) {
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(csvField(v))
	}
	b.WriteByte('\n')
}

// ExportProgress hears rows written of the total.
type ExportProgress func(done, total int64)

func (d *DB) count() int64 {
	v, _ := db.Scalar(d.c, "SELECT count(*) FROM sightings")
	return db.Int(v)
}

func (d *DB) each(fn func(r *Row) error) error {
	var r Row
	return db.Each(d.c, "SELECT "+strings.Join(Columns, ",")+" FROM sightings ORDER BY rowid", nil, func(st *sqlite.Stmt) error {
		for k := 0; k < nCols; k++ {
			r[k] = text(st, k)
		}
		return fn(&r)
	})
}

// ExportCSV writes every sighting to one WiGLE CSV at dest (written to dest.part first).
func (d *DB) ExportCSV(dest, version string, progress ExportProgress, cancelled func() bool) (int64, error) {
	total := d.count()
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	b := bufio.NewWriterSize(f, 1<<20)
	b.WriteString(Preamble(version) + "\n")
	writeRow(b, Columns)
	var n int64
	err = d.each(func(r *Row) error {
		writeRow(b, r[:])
		n++
		if n%65536 == 0 {
			if cancelled != nil && cancelled() {
				return ErrCancelled
			}
			if progress != nil {
				progress(n, total)
			}
		}
		return nil
	})
	if err == nil {
		err = b.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	os.Remove(dest)
	return n, os.Rename(tmp, dest)
}

// ExportDays writes one WiGLE CSV per day (the date FirstSeen starts with) into dir, named
// wigle_YYYY-MM-DD.csv; sightings with no date go to wigle_undated.csv. It returns the files.
func (d *DB) ExportDays(dir, version string, progress ExportProgress, cancelled func() bool) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	total := d.count()
	type out struct {
		f    *os.File
		b    *bufio.Writer
		used int64
	}
	open := map[string]*out{}
	started := map[string]bool{}
	var clock int64
	const maxOpen = 200 // a few hundred days at once; older ones are closed and reopened to append
	get := func(day string) (*out, error) {
		clock++
		if o := open[day]; o != nil {
			o.used = clock
			return o, nil
		}
		if len(open) >= maxOpen {
			var oldest string
			var t int64 = math.MaxInt64
			for k, o := range open {
				if o.used < t {
					oldest, t = k, o.used
				}
			}
			o := open[oldest]
			o.b.Flush()
			o.f.Close()
			delete(open, oldest)
		}
		name := filepath.Join(dir, "wigle_"+day+".csv.part")
		flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if started[day] {
			flags = os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(name, flags, 0o644)
		if err != nil {
			return nil, err
		}
		o := &out{f: f, b: bufio.NewWriterSize(f, 64<<10), used: clock}
		if !started[day] {
			o.b.WriteString(Preamble(version) + "\n")
			writeRow(o.b, Columns)
			started[day] = true
		}
		open[day] = o
		return o, nil
	}
	var n int64
	err := d.each(func(r *Row) error {
		day := "undated"
		if t := r[cFirst]; len(t) >= 10 && t[4] == '-' && t[7] == '-' {
			day = t[:10]
		}
		o, err := get(day)
		if err != nil {
			return err
		}
		writeRow(o.b, r[:])
		n++
		if n%65536 == 0 {
			if cancelled != nil && cancelled() {
				return ErrCancelled
			}
			if progress != nil {
				progress(n, total)
			}
		}
		return nil
	})
	for _, o := range open {
		if ferr := o.b.Flush(); err == nil {
			err = ferr
		}
		if cerr := o.f.Close(); err == nil {
			err = cerr
		}
	}
	var files []string
	for day := range started {
		part := filepath.Join(dir, "wigle_"+day+".csv.part")
		if err != nil {
			os.Remove(part)
			continue
		}
		final := strings.TrimSuffix(part, ".part")
		os.Remove(final)
		if rerr := os.Rename(part, final); rerr != nil && err == nil {
			err = rerr
		}
		files = append(files, final)
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// ExportCopy writes a compact copy of the whole database to dest.
func (d *DB) ExportCopy(dest string) error {
	os.Remove(dest)
	_, err := db.Exec(d.c, "VACUUM INTO ?", dest)
	return err
}

// ---------------------------------------------------------------------------
// any supported file as a WiGLE CSV, for the tools that read CSV logs (Compare, the explorer)

// csvSink writes rows as WiGLE CSV.
type csvSink struct {
	ctx           context.Context
	b             *bufio.Writer
	n, kept, gone int64
	positionsOnly bool
	cols          int // Columns written: all, or all but Node
}

func (c *csvSink) rows() int64 { return c.n }

func (c *csvSink) add(r *Row) error {
	if c.ctx != nil && c.n&4095 == 0 {
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
	c.n++
	for i := range r {
		r[i] = strings.TrimSpace(r[i])
	}
	if r[cMAC] == "" {
		c.gone++
		return nil
	}
	if c.positionsOnly && !hasPosition(r) {
		c.gone++
		return nil
	}
	writeRow(c.b, r[:c.cols])
	c.kept++
	return nil
}

// hasPosition is whether a row has a GPS position (loggers write 0,0 or nothing before a fix).
func hasPosition(r *Row) bool {
	lat, e1 := strconv.ParseFloat(r[cLat], 64)
	lon, e2 := strconv.ParseFloat(r[cLon], 64)
	return e1 == nil && e2 == nil && !(lat == 0 && lon == 0)
}

// IsDatabase is whether a file is an SQLite database (by its first bytes).
func IsDatabase(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	return n == 16 && string(head) == "SQLite format 3\x00"
}

// ToCSV writes every sighting of a database file (a logger's day database, a Drive database, a
// WiGLE app backup, a Signal Atlas database) to dest as a WiGLE CSV log, with the Node column.
// positionsOnly leaves out rows without a GPS position. It returns the kind of file, the rows
// written, and the rows left out.
func ToCSV(path, dest, version string, positionsOnly bool, progress func(rows, done, total int64)) (string, int64, int64, error) {
	return ToCSVContext(context.Background(), path, dest, version, positionsOnly, progress)
}
func ToCSVContext(ctx context.Context, path, dest, version string, positionsOnly bool, progress func(rows, done, total int64)) (string, int64, int64, error) {
	clean := func(s string) string { return strings.NewReplacer(",", " ", "=", " ", "\n", " ", "\r", " ").Replace(s) }
	kind, err := Detect(path)
	if err != nil {
		return "", 0, 0, err
	}
	if kind == KindCSV {
		return kind, 0, 0, fmt.Errorf("%s is already a CSV log", filepath.Base(path))
	}
	f, err := os.Create(dest)
	if err != nil {
		return kind, 0, 0, err
	}
	b := bufio.NewWriterSize(f, 1<<20)
	b.WriteString("WigleWifi-1.6,appRelease=" + version + ",model=" + clean(kind) + ",release=" + version + ",device=" + clean(filepath.Base(path)) +
		",display=,board=,brand=Signal Atlas,star=Sol,body=3,subBody=0\n")
	// the Node column only when the source has one, so Compare offers its per-node view only then
	cols := nCols - 1
	if kind == KindTables || kind == KindDrive {
		if src, err := db.OpenReadOnly(path); err == nil {
			if t, err := tablesWithColumns(src); err == nil {
				for _, tc := range t {
					if wigleTable(tc) && tc["node"] {
						cols = nCols
					}
				}
			}
			src.Close()
		}
	}
	writeRow(b, Columns[:cols])
	c := &csvSink{b: b, positionsOnly: positionsOnly, cols: cols, ctx: ctx}
	report := func(rows, done, total int64) {
		if progress != nil {
			progress(rows, done, total)
		}
	}
	switch kind {
	case KindWiGLEBackup:
		err = readWiGLEBackup(path, c, report)
	case KindAtlas:
		err = readAtlas(path, c, report)
	default:
		err = readTables(path, c, report)
	}
	if err == nil {
		err = b.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
		return kind, 0, 0, err
	}
	return kind, c.kept, c.gone, nil
}
