// Package obs is the column store behind Signal Atlas's observations table.
//
// Signal Atlas (Graphite's Python) keeps its observations in SQLite with an R-tree and six
// secondary indexes. At tens of millions of rows that design spends minutes per import building
// indexes; the target here is ten million rows a minute or better. So the observations live in
// columns instead: text dictionary-encoded into one shared string table, numbers in typed arrays,
// one immutable segment per import (one CSV). The indexes Signal Atlas's
// queries need (by radio, by transmitter, by SSID, a spatial grid, the network and transmitter
// catalogues) are rebuilt in memory, in parallel, whenever the set of segments changes. Queries
// read an immutable State, so they never wait on an import.
//
// Ids follow SQLite's rowid rule (a new import's rows start at the highest id stored + 1), so the
// ids, and the queries ordered by them, are the ones Signal Atlas's own store would give.
//
// On disk (a folder): manifest.json (the committed segments and string count), strings.bin
// (length-prefixed strings, append-only), and one seg-*.bin per segment. A commit writes the new
// files, then replaces the manifest atomically; anything not in the manifest is ignored at open.
package obs

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Null is how a NULL REAL (accuracy_m, frequency_mhz) is stored.
var Null = math.Float64frombits(0x7ff8_dead_beef_0001)

// IsNull reports a stored NULL.
func IsNull(v float64) bool { return math.IsNaN(v) }

// Record ids: str(line), a decimal integer, or a string from the dictionary.
const (
	RecLine uint8 = iota
	RecInt
	RecStr
)

// Segment is one import's rows. Its slices are never modified once it is committed.
type Segment struct {
	ImportID                                              int64
	Base                                                  int64 // id of row 0; row i has id Base+i
	N                                                     int
	SSID, MAC, Band, BandSrc, Radio, Date, Channel, First []uint32
	Lat, Lon, RSSI, Acc, Freq                             []float64
	Line                                                  []int64
	RecKind                                               []uint8
	RecVal                                                []int64
	file                                                  string
	// OriginalIDs is only for bounded read-only fitting views; never persisted.
	OriginalIDs []int64
}

// ID preserves source identity in bounded views whose rows are not contiguous.
func (s *Segment) ID(i int) int64 {
	if s.OriginalIDs != nil {
		return s.OriginalIDs[i]
	}
	return s.Base + int64(i)
}

// NewDictionary builds a dictionary for a bounded external read view.
func NewDictionary() *Dict { return newDict() }

// NewReadView creates an immutable fitting snapshot without opening or copying
// a persistent observation store. Rows must be ordered by original ID.
// Commits are forbidden on this view.
func NewReadView(segments []*Segment, dict *Dict) *Store {
	s := &Store{Dict: dict, readOnly: true}
	st := &State{Segs: segments, Str: dict.Snapshot()}
	st.index()
	s.state.Store(st)
	return s
}

// NewSegment makes an empty segment to append to.
func NewSegment(importID, base int64, capacity int) *Segment {
	return &Segment{ImportID: importID, Base: base,
		SSID: make([]uint32, 0, capacity), MAC: make([]uint32, 0, capacity), Band: make([]uint32, 0, capacity),
		BandSrc: make([]uint32, 0, capacity), Radio: make([]uint32, 0, capacity), Date: make([]uint32, 0, capacity),
		Channel: make([]uint32, 0, capacity), First: make([]uint32, 0, capacity),
		Lat: make([]float64, 0, capacity), Lon: make([]float64, 0, capacity), RSSI: make([]float64, 0, capacity),
		Acc: make([]float64, 0, capacity), Freq: make([]float64, 0, capacity), Line: make([]int64, 0, capacity),
		RecKind: make([]uint8, 0, capacity), RecVal: make([]int64, 0, capacity)}
}

// Row is one observation as the store keeps it (string ids already interned).
type Row struct {
	SSID, MAC, Band, BandSrc, Radio, Date, Channel, First uint32
	Lat, Lon, RSSI, Acc, Freq                             float64
	Line                                                  int64
	RecKind                                               uint8
	RecVal                                                int64
}

// Append adds a row.
func (s *Segment) Append(r *Row) {
	s.SSID = append(s.SSID, r.SSID)
	s.MAC = append(s.MAC, r.MAC)
	s.Band = append(s.Band, r.Band)
	s.BandSrc = append(s.BandSrc, r.BandSrc)
	s.Radio = append(s.Radio, r.Radio)
	s.Date = append(s.Date, r.Date)
	s.Channel = append(s.Channel, r.Channel)
	s.First = append(s.First, r.First)
	s.Lat = append(s.Lat, r.Lat)
	s.Lon = append(s.Lon, r.Lon)
	s.RSSI = append(s.RSSI, r.RSSI)
	s.Acc = append(s.Acc, r.Acc)
	s.Freq = append(s.Freq, r.Freq)
	s.Line = append(s.Line, r.Line)
	s.RecKind = append(s.RecKind, r.RecKind)
	s.RecVal = append(s.RecVal, r.RecVal)
	s.N++
}

// Clone copies a segment so its columns can be changed (band assignments).
func (s *Segment) Clone() *Segment {
	c := *s
	c.Band = append([]uint32(nil), s.Band...)
	c.BandSrc = append([]uint32(nil), s.BandSrc...)
	c.Radio = append([]uint32(nil), s.Radio...)
	c.file = ""
	return &c
}

// Range keeps consecutive original IDs without copying immutable column data.
// The new segment is written under a new filename at the next commit.
func (s *Segment) Range(lo, hi int) *Segment {
	result := &Segment{ImportID: s.ImportID, Base: s.Base + int64(lo), N: hi - lo,
		SSID: s.SSID[lo:hi], MAC: s.MAC[lo:hi], Band: s.Band[lo:hi], BandSrc: s.BandSrc[lo:hi], Radio: s.Radio[lo:hi], Date: s.Date[lo:hi], Channel: s.Channel[lo:hi], First: s.First[lo:hi],
		Lat: s.Lat[lo:hi], Lon: s.Lon[lo:hi], RSSI: s.RSSI[lo:hi], Acc: s.Acc[lo:hi], Freq: s.Freq[lo:hi], Line: s.Line[lo:hi], RecKind: s.RecKind[lo:hi], RecVal: s.RecVal[lo:hi]}
	if s.OriginalIDs != nil {
		result.OriginalIDs = s.OriginalIDs[lo:hi]
	}
	return result
}

// ---------------------------------------------------------------------------
// the string dictionary

// Dict interns strings to ids. Ids are never reused or changed; readers take a snapshot slice.
type Dict struct {
	mu   sync.Mutex
	ids  map[string]uint32
	list []string
}

func newDict() *Dict { return &Dict{ids: map[string]uint32{}} }

// ID interns s.
func (d *Dict) ID(s string) uint32 {
	d.mu.Lock()
	id, ok := d.ids[s]
	if !ok {
		id = uint32(len(d.list))
		d.list = append(d.list, s)
		d.ids[s] = id
	}
	d.mu.Unlock()
	return id
}

// Lookup finds s without adding it.
func (d *Dict) Lookup(s string) (uint32, bool) {
	d.mu.Lock()
	id, ok := d.ids[s]
	d.mu.Unlock()
	return id, ok
}

// Snapshot is the strings so far (safe to read while others append).
func (d *Dict) Snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.list[:len(d.list):len(d.list)]
}

// Interner is a single goroutine's front to a Dict, remembering recent answers per column so the
// common case (the same band, date or timestamp as the row before) costs no lock.
type Interner struct {
	d    *Dict
	last [8]string
	id   [8]uint32
	ok   [8]bool
}

// NewInterner makes one.
func (d *Dict) NewInterner() *Interner { return &Interner{d: d} }

// ID interns s for column slot k (0..7).
func (in *Interner) ID(k int, s string) uint32 {
	if in.ok[k] && in.last[k] == s {
		return in.id[k]
	}
	id := in.d.ID(s)
	in.last[k], in.id[k], in.ok[k] = s, id, true
	return id
}

// ---------------------------------------------------------------------------
// State: an immutable view with its indexes

// NetStat is one network_catalog row's numbers (all rows of one SSID).
type NetStat struct {
	Count, Macs, Radios int64
	Peak                float64
	Bands               []uint32 // distinct, in string order
}

// TransStat is one transmitter_catalog row's numbers (all rows of one SSID + MAC).
type TransStat struct {
	Count    int64
	Peak     float64
	Bands    []uint32 // distinct, in string order
	Editable int64    // rows whose band can be assigned by hand
}

// Pair is (ssid id, mac id).
type Pair [2]uint32

// GridDeg is the spatial grid's cell size in degrees.
const GridDeg = 0.01

// State is everything a query needs, never changed once published.
type State struct {
	Segs  []*Segment
	Str   []string
	start []int // global row number of each segment's row 0
	N     int
	// indexes (global row numbers, ascending = id order)
	ByRadio map[uint32][]uint32
	ByTrans map[Pair][]uint32
	BySSID  map[uint32][]uint32
	Grid    map[int64][]uint32
	Net     map[uint32]*NetStat
	Trans   map[Pair]*TransStat
	MaxID   int64
}

// Loc finds global row g.
func (st *State) Loc(g uint32) (*Segment, int) {
	i := sort.Search(len(st.start), func(i int) bool { return st.start[i] > int(g) }) - 1
	return st.Segs[i], int(g) - st.start[i]
}

// ID is the observation id of global row g.
func (st *State) ID(g uint32) int64 { s, i := st.Loc(g); return s.ID(i) }

// S is the string with id.
func (st *State) S(id uint32) string { return st.Str[id] }

// CellOf is the grid cell of a position.
func CellOf(lat, lon float64) int64 {
	return int64(math.Floor(lat/GridDeg))<<32 | int64(uint32(int32(math.Floor(lon/GridDeg))))
}

// Editable reports a band_source whose band can be assigned by hand (assign_band's rule).
func Editable(bandSource string) bool {
	return bandSource == "frequency/band unavailable" || bandSource == "manual assignment"
}

func (st *State) shape() {
	st.start = make([]int, len(st.Segs))
	st.MaxID = 0
	n := 0
	for i, s := range st.Segs {
		st.start[i] = n
		n += s.N
		if s.N > 0 && s.ID(s.N-1) > st.MaxID {
			st.MaxID = s.ID(s.N - 1)
		}
	}
	st.N = n
}

func (st *State) index() {
	st.shape()
	var wg sync.WaitGroup
	each := func(fn func(g uint32, s *Segment, i int)) {
		g := uint32(0)
		for _, s := range st.Segs {
			for i := 0; i < s.N; i++ {
				fn(g, s, i)
				g++
			}
		}
	}
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() {
		m := postings(st, func(s *Segment, i int) uint32 { return s.Radio[i] })
		st.ByRadio = m
	})
	run(func() {
		m := postings(st, func(s *Segment, i int) uint32 { return s.SSID[i] })
		st.BySSID = m
	})
	run(func() {
		m := postings(st, func(s *Segment, i int) int64 { return CellOf(s.Lat[i], s.Lon[i]) })
		st.Grid = m
	})
	run(func() {
		m := postings(st, func(s *Segment, i int) Pair { return Pair{s.SSID[i], s.MAC[i]} })
		ts := map[Pair]*TransStat{}
		each(func(g uint32, s *Segment, i int) {
			k := Pair{s.SSID[i], s.MAC[i]}
			t := ts[k]
			if t == nil {
				t = &TransStat{Peak: math.Inf(-1)}
				ts[k] = t
			}
			t.Count++
			if s.RSSI[i] > t.Peak {
				t.Peak = s.RSSI[i]
			}
			if !slices.Contains(t.Bands, s.Band[i]) {
				t.Bands = append(t.Bands, s.Band[i])
			}
			if Editable(st.Str[s.BandSrc[i]]) {
				t.Editable++
			}
		})
		for _, t := range ts {
			sort.Slice(t.Bands, func(i, j int) bool { return st.Str[t.Bands[i]] < st.Str[t.Bands[j]] })
		}
		st.ByTrans, st.Trans = m, ts
		// the network catalogue from the transmitter one
		net := map[uint32]*NetStat{}
		for k, t := range ts {
			ns := net[k[0]]
			if ns == nil {
				ns = &NetStat{Peak: math.Inf(-1)}
				net[k[0]] = ns
			}
			ns.Count += t.Count
			ns.Macs++
			ns.Radios += int64(len(t.Bands))
			if t.Peak > ns.Peak {
				ns.Peak = t.Peak
			}
			for _, b := range t.Bands {
				if !slices.Contains(ns.Bands, b) {
					ns.Bands = append(ns.Bands, b)
				}
			}
		}
		for _, ns := range net {
			sort.Slice(ns.Bands, func(i, j int) bool { return st.Str[ns.Bands[i]] < st.Str[ns.Bands[j]] })
		}
		st.Net = net
	})
	wg.Wait()
}

// postings builds exact-size, contiguous posting lists. Two linear passes avoid
// per-key slice growth, spare capacity and the old generations of backing arrays
// that otherwise coexist during every import/reopen. Lists remain row-ordered.
func postings[K comparable](st *State, key func(*Segment, int) K) map[K][]uint32 {
	counts := make(map[K]int)
	for _, s := range st.Segs {
		for i := 0; i < s.N; i++ {
			counts[key(s, i)]++
		}
	}
	rows := make([]uint32, st.N)
	out := make(map[K][]uint32, len(counts))
	offset := 0
	for k, n := range counts {
		out[k] = rows[offset : offset : offset+n]
		offset += n
	}
	var g uint32
	for _, s := range st.Segs {
		for i := 0; i < s.N; i++ {
			k := key(s, i)
			out[k] = append(out[k], g)
			g++
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// the store

// Store is a folder of segments plus the published State.
type Store struct {
	dir      string
	Dict     *Dict
	state    atomic.Pointer[State]
	mu       sync.Mutex // one writer at a time
	saved    int        // strings persisted
	gen      int64
	readOnly bool
}

type manifest struct {
	Version  int        `json:"version"`
	Strings  int        `json:"strings"`
	Gen      int64      `json:"gen"`
	Segments []segEntry `json:"segments"`
}

type segEntry struct {
	File     string `json:"file"`
	ImportID int64  `json:"import_id"`
	Base     int64  `json:"base"`
	N        int    `json:"n"`
}

// Open checks committed import row counts before publishing or sweeping files.
// Segments of uncommitted imports (an interrupted commit) are dropped.
func Open(dir string, committed map[int64]int64) (*Store, error) {
	keep := func(id int64) bool { _, ok := committed[id]; return committed == nil || ok }
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, Dict: newDict()}
	var m manifest
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err == nil {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("observation store manifest: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		// A lost manifest must not make committed segment files look like garbage.
		// Uncommitted first-import files may still be discarded using metadata IDs.
		entries, e := os.ReadDir(dir)
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "seg-") && strings.HasSuffix(entry.Name(), ".bin") {
				f, e := os.Open(filepath.Join(dir, entry.Name()))
				if e != nil {
					return nil, e
				}
				header := make([]byte, 16)
				_, e = io.ReadFull(f, header)
				if e != nil || string(header[:8]) != segMagic {
					f.Close()
					return nil, errors.New("observation manifest missing; preserved segment files")
				}
				n := binary.LittleEndian.Uint32(header[8:])
				if n > 4096 {
					f.Close()
					return nil, errors.New("invalid observation segment header")
				}
				b := make([]byte, n)
				_, e = io.ReadFull(f, b)
				f.Close()
				var h segHeader
				if e != nil || json.Unmarshal(b, &h) != nil || keep(h.ImportID) {
					return nil, errors.New("observation manifest missing; preserved committed segment files")
				}
			}
		}
	}
	if err := s.loadStrings(m.Strings); err != nil {
		return nil, err
	}
	s.saved = m.Strings
	s.gen = m.Gen
	segs := make([]*Segment, len(m.Segments))
	errs := make([]error, len(m.Segments))
	var wg sync.WaitGroup
	for i, e := range m.Segments {
		wg.Add(1)
		go func(i int, e segEntry) {
			defer wg.Done()
			segs[i], errs[i] = readSegment(filepath.Join(dir, e.File))
			if segs[i] != nil {
				segs[i].file = e.File
			}
		}(i, e)
	}
	wg.Wait()
	var kept []*Segment
	dropped := false
	for i, sg := range segs {
		if errs[i] != nil {
			return nil, fmt.Errorf("observation segment %s: %w", m.Segments[i].File, errs[i])
		}
		if !keep(sg.ImportID) {
			dropped = true
			continue
		}
		kept = append(kept, sg)
	}
	counts := map[int64]int64{}
	for _, sg := range kept {
		counts[sg.ImportID] += int64(sg.N)
	}
	for id, n := range committed {
		if counts[id] != n {
			return nil, fmt.Errorf("observation store incomplete: import %d expects %d rows, found %d; preserved segment files", id, n, counts[id])
		}
	}
	st := &State{Segs: kept, Str: s.Dict.Snapshot()}
	st.index()
	s.state.Store(st)
	if dropped {
		if err := s.writeManifest(kept); err != nil {
			return nil, err
		}
	}
	s.sweep(kept)
	return s, nil
}

// State is the current published view.
func (s *Store) State() *State { return s.state.Load() }

// ForkTo is for a quiescent staged workspace copy. Columns/indexes are immutable
// and can be shared until candidate publication; future writes use dir only.
// The source must stay quiescent until the fork is closed or replaces it.
func (s *Store) ForkTo(dir string) *Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	fork := &Store{dir: dir, Dict: s.Dict, saved: s.saved, gen: s.gen, readOnly: s.readOnly}
	fork.state.Store(s.State())
	return fork
}

// Reindex publishes fresh derived indexes without changing observation files.
func (s *Store) Reindex() {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.State()
	st := &State{Segs: old.Segs, Str: old.Str}
	st.index()
	s.state.Store(st)
}

func (s *Store) loadStrings(n int) error {
	f, err := os.Open(filepath.Join(s.dir, "strings.bin"))
	if errors.Is(err, os.ErrNotExist) {
		if n > 0 {
			return errors.New("observation store: strings.bin is missing")
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	s.Dict.list = make([]string, 0, n)
	for i := 0; i < n; i++ {
		l, err := binary.ReadUvarint(r)
		if err != nil {
			return fmt.Errorf("observation store strings: %w", err)
		}
		b := make([]byte, l)
		if _, err := io.ReadFull(r, b); err != nil {
			return fmt.Errorf("observation store strings: %w", err)
		}
		str := string(b)
		s.Dict.ids[str] = uint32(len(s.Dict.list))
		s.Dict.list = append(s.Dict.list, str)
	}
	return nil
}

// persistStrings appends strings added since the last commit, truncating any torn tail first.
func (s *Store) persistStrings() (int, error) {
	list := s.Dict.Snapshot()
	p := filepath.Join(s.dir, "strings.bin")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// find the end of the committed strings (the file may hold a torn, uncommitted tail)
	r := bufio.NewReaderSize(f, 1<<20)
	var off int64
	for i := 0; i < s.saved; i++ {
		l, err := binary.ReadUvarint(r)
		if err != nil {
			return 0, err
		}
		if _, err := r.Discard(int(l)); err != nil {
			return 0, err
		}
		off += int64(uvarintLen(l)) + int64(l)
	}
	if err := f.Truncate(off); err != nil {
		return 0, err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	var tmp [binary.MaxVarintLen64]byte
	for _, str := range list[s.saved:] {
		n := binary.PutUvarint(tmp[:], uint64(len(str)))
		w.Write(tmp[:n])
		w.WriteString(str)
	}
	if err := w.Flush(); err != nil {
		return 0, err
	}
	return len(list), f.Sync()
}

func uvarintLen(v uint64) int { var b [binary.MaxVarintLen64]byte; return binary.PutUvarint(b[:], v) }

func (s *Store) writeManifest(segs []*Segment) error {
	m := manifest{Version: 1, Strings: s.saved, Gen: s.gen}
	for _, sg := range segs {
		m.Segments = append(m.Segments, segEntry{sg.file, sg.ImportID, sg.Base, sg.N})
	}
	raw, _ := json.MarshalIndent(m, "", " ")
	tmp := filepath.Join(s.dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err == nil {
		f.Sync()
		f.Close()
	}
	return os.Rename(tmp, filepath.Join(s.dir, "manifest.json"))
}

// sweep deletes segment files no manifest refers to.
func (s *Store) sweep(segs []*Segment) {
	live := map[string]bool{}
	for _, sg := range segs {
		live[sg.file] = true
	}
	ents, _ := os.ReadDir(s.dir)
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "seg-") && !live[n] {
			os.Remove(filepath.Join(s.dir, n))
		}
	}
}

// Change is one commit: segments to add, import ids to drop, segments to replace.
type Change struct {
	Segments []*Segment // non-nil replaces the full segment set (staged maintenance)
	Add      []*Segment
	Drop     map[int64]bool // import ids whose segments go
	Replace  map[*Segment]*Segment
}

// Commit writes a change and publishes the new State. It is all or nothing on disk.
func (s *Store) Commit(ch Change) error {
	if s.readOnly {
		return errors.New("read-only fitting view cannot be committed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.State()
	var segs []*Segment
	for _, sg := range old.Segs {
		if ch.Drop[sg.ImportID] {
			continue
		}
		if r, ok := ch.Replace[sg]; ok {
			sg = r
		}
		segs = append(segs, sg)
	}
	if ch.Segments != nil {
		segs = ch.Segments
	}
	s.gen++
	for _, sg := range segs {
		if sg.file == "" {
			sg.file = fmt.Sprintf("seg-%d-%d-g%d.bin", sg.ImportID, sg.Base, s.gen)
			if err := writeSegment(filepath.Join(s.dir, sg.file), sg); err != nil {
				return err
			}
		}
	}
	for _, sg := range ch.Add {
		if sg.N == 0 {
			continue
		}
		sg.file = fmt.Sprintf("seg-%d-%d-g%d.bin", sg.ImportID, sg.Base, s.gen)
		if err := writeSegment(filepath.Join(s.dir, sg.file), sg); err != nil {
			return err
		}
		segs = append(segs, sg)
	}
	sort.SliceStable(segs, func(a, b int) bool { return segs[a].Base < segs[b].Base })
	n, err := s.persistStrings()
	if err != nil {
		return err
	}
	s.saved = n
	if err := s.writeManifest(segs); err != nil {
		return err
	}
	st := &State{Segs: segs, Str: s.Dict.Snapshot()}
	if ch.Segments != nil || len(ch.Drop) > 0 || len(ch.Replace) > 0 || !st.appendIndex(old) {
		st.index()
	}
	s.state.Store(st)
	s.sweep(segs)
	return nil
}

// NextBase is the id a new import's first row gets (SQLite: max(rowid)+1), given the import ids
// about to be dropped in the same change.
func (s *Store) NextBase(dropping map[int64]bool) int64 {
	var m int64
	for _, sg := range s.State().Segs {
		if dropping[sg.ImportID] || sg.N == 0 {
			continue
		}
		if last := sg.Base + int64(sg.N) - 1; last > m {
			m = last
		}
	}
	return m + 1
}

// ---------------------------------------------------------------------------
// segment files: a small header, then each column's raw little-endian bytes, 8-byte aligned so
// a read can use them in place.

const segMagic = "WHOBSEG1"

type segHeader struct {
	ImportID int64 `json:"import_id"`
	Base     int64 `json:"base"`
	N        int   `json:"n"`
}

func colBytes[T any](s []T) []byte {
	if len(s) == 0 {
		return nil
	}
	var z T
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), len(s)*int(unsafe.Sizeof(z)))
}

func colFrom[T any](b []byte, n int) []T {
	if n == 0 {
		return []T{}
	}
	return unsafe.Slice((*T)(unsafe.Pointer(&b[0])), n)
}

func (sg *Segment) columns() [][]byte {
	return [][]byte{colBytes(sg.SSID), colBytes(sg.MAC), colBytes(sg.Band), colBytes(sg.BandSrc), colBytes(sg.Radio),
		colBytes(sg.Date), colBytes(sg.Channel), colBytes(sg.First), colBytes(sg.Lat), colBytes(sg.Lon), colBytes(sg.RSSI),
		colBytes(sg.Acc), colBytes(sg.Freq), colBytes(sg.Line), colBytes(sg.RecKind), colBytes(sg.RecVal)}
}

func writeSegment(path string, sg *Segment) error {
	if sg.OriginalIDs != nil {
		return errors.New("source-ID views cannot be persisted as segments")
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 4<<20)
	hdr, _ := json.Marshal(segHeader{sg.ImportID, sg.Base, sg.N})
	head := make([]byte, 64)
	copy(head, segMagic)
	binary.LittleEndian.PutUint32(head[8:], uint32(len(hdr)))
	w.Write(head[:16])
	w.Write(hdr)
	pos := 16 + len(hdr)
	pad := func() {
		for pos%8 != 0 {
			w.WriteByte(0)
			pos++
		}
	}
	pad()
	for _, c := range sg.columns() {
		w.Write(c)
		pos += len(c)
		pad()
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}

func readSegment(path string) (*Segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 16 || info.Size() > int64(int(^uint(0)>>1))-7 {
		return nil, errors.New("invalid segment length")
	}
	// Read directly into the final aligned backing allocation. Never keep a
	// second file-sized ReadFile copy alive while rebuilding observation indexes.
	buf := make([]uint64, (int(info.Size())+7)/8)
	b := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), int(info.Size()))
	if _, err = io.ReadFull(f, b); err != nil {
		return nil, err
	}
	if string(b[:8]) != segMagic {
		return nil, errors.New("not a segment file")
	}
	hl := int(binary.LittleEndian.Uint32(b[8:]))
	if hl < 1 || hl > 4096 || hl > len(b)-16 {
		return nil, errors.New("invalid segment header")
	}
	var h segHeader
	if err := json.Unmarshal(b[16:16+hl], &h); err != nil {
		return nil, err
	}
	if h.N < 0 || h.N > (len(b)-16-hl)/89 || h.Base < 1 || int64(h.N) > math.MaxInt64-h.Base {
		return nil, errors.New("invalid segment row count or IDs")
	}
	pos := 16 + hl
	al := func() { pos = (pos + 7) &^ 7 }
	al()
	n := h.N
	expected := pos
	for _, width := range []int{4, 4, 4, 4, 4, 4, 4, 4, 8, 8, 8, 8, 8, 8, 1, 8} {
		expected = (expected + width*n + 7) &^ 7
	}
	if expected != len(b) {
		return nil, errors.New("segment file is truncated or has trailing data")
	}
	sg := &Segment{ImportID: h.ImportID, Base: h.Base, N: n}
	take := func(size int) []byte {
		if pos+size > len(b) {
			err = errors.New("segment file is truncated")
			return nil
		}
		x := b[pos : pos+size]
		pos += size
		al()
		return x
	}
	sg.SSID = colFrom[uint32](take(4*n), n)
	sg.MAC = colFrom[uint32](take(4*n), n)
	sg.Band = colFrom[uint32](take(4*n), n)
	sg.BandSrc = colFrom[uint32](take(4*n), n)
	sg.Radio = colFrom[uint32](take(4*n), n)
	sg.Date = colFrom[uint32](take(4*n), n)
	sg.Channel = colFrom[uint32](take(4*n), n)
	sg.First = colFrom[uint32](take(4*n), n)
	sg.Lat = colFrom[float64](take(8*n), n)
	sg.Lon = colFrom[float64](take(8*n), n)
	sg.RSSI = colFrom[float64](take(8*n), n)
	sg.Acc = colFrom[float64](take(8*n), n)
	sg.Freq = colFrom[float64](take(8*n), n)
	sg.Line = colFrom[int64](take(8*n), n)
	sg.RecKind = colFrom[uint8](take(n), n)
	sg.RecVal = colFrom[int64](take(8*n), n)
	return sg, err
}
