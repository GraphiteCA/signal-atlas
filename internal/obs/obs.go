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
func (st *State) ID(g uint32) int64 { s, i := st.Loc(g); return s.Base + int64(i) }

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

func (st *State) index() {
	st.start = make([]int, len(st.Segs))
	n := 0
	for i, s := range st.Segs {
		st.start[i] = n
		n += s.N
		if s.N > 0 && s.Base+int64(s.N)-1 > st.MaxID {
			st.MaxID = s.Base + int64(s.N) - 1
		}
	}
	st.N = n
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
		m := map[uint32][]uint32{}
		each(func(g uint32, s *Segment, i int) { m[s.Radio[i]] = append(m[s.Radio[i]], g) })
		st.ByRadio = m
	})
	run(func() {
		m := map[uint32][]uint32{}
		each(func(g uint32, s *Segment, i int) { m[s.SSID[i]] = append(m[s.SSID[i]], g) })
		st.BySSID = m
	})
	run(func() {
		m := map[int64][]uint32{}
		each(func(g uint32, s *Segment, i int) { k := CellOf(s.Lat[i], s.Lon[i]); m[k] = append(m[k], g) })
		st.Grid = m
	})
	run(func() {
		m := map[Pair][]uint32{}
		ts := map[Pair]*TransStat{}
		bandSets := map[Pair]map[uint32]bool{}
		each(func(g uint32, s *Segment, i int) {
			k := Pair{s.SSID[i], s.MAC[i]}
			m[k] = append(m[k], g)
			t := ts[k]
			if t == nil {
				t = &TransStat{Peak: math.Inf(-1)}
				ts[k] = t
				bandSets[k] = map[uint32]bool{}
			}
			t.Count++
			if s.RSSI[i] > t.Peak {
				t.Peak = s.RSSI[i]
			}
			bandSets[k][s.Band[i]] = true
			if Editable(st.Str[s.BandSrc[i]]) {
				t.Editable++
			}
		})
		for k, t := range ts {
			t.Bands = sortedIDs(bandSets[k], st.Str)
		}
		st.ByTrans, st.Trans = m, ts
		// the network catalogue from the transmitter one
		net := map[uint32]*NetStat{}
		nb := map[uint32]map[uint32]bool{}
		for k, t := range ts {
			ns := net[k[0]]
			if ns == nil {
				ns = &NetStat{Peak: math.Inf(-1)}
				net[k[0]] = ns
				nb[k[0]] = map[uint32]bool{}
			}
			ns.Count += t.Count
			ns.Macs++
			ns.Radios += int64(len(t.Bands))
			if t.Peak > ns.Peak {
				ns.Peak = t.Peak
			}
			for _, b := range t.Bands {
				nb[k[0]][b] = true
			}
		}
		for k, ns := range net {
			ns.Bands = sortedIDs(nb[k], st.Str)
		}
		st.Net = net
	})
	wg.Wait()
}

func sortedIDs(set map[uint32]bool, str []string) []uint32 {
	out := make([]uint32, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(a, b int) bool { return str[out[a]] < str[out[b]] })
	return out
}

// ---------------------------------------------------------------------------
// the store

// Store is a folder of segments plus the published State.
type Store struct {
	dir   string
	Dict  *Dict
	state atomic.Pointer[State]
	mu    sync.Mutex // one writer at a time
	saved int        // strings persisted
	gen   int64
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

// Open opens (or creates) the store in dir. keep says which import ids are committed in the
// metadata database; segments of other imports (an interrupted commit) are dropped.
func Open(dir string, keep func(importID int64) bool) (*Store, error) {
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
		if keep != nil && !keep(sg.ImportID) {
			dropped = true
			continue
		}
		kept = append(kept, sg)
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
	Add     []*Segment
	Drop    map[int64]bool // import ids whose segments go
	Replace map[*Segment]*Segment
}

// Commit writes a change and publishes the new State. It is all or nothing on disk.
func (s *Store) Commit(ch Change) error {
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
	st.index()
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
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 16 || string(raw[:8]) != segMagic {
		return nil, errors.New("not a segment file")
	}
	hl := int(binary.LittleEndian.Uint32(raw[8:]))
	var h segHeader
	if err := json.Unmarshal(raw[16:16+hl], &h); err != nil {
		return nil, err
	}
	// copy into an 8-aligned buffer (ReadFile's slice is aligned in practice, but do not rely on it)
	buf := make([]uint64, (len(raw)+7)/8)
	b := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), len(raw))
	copy(b, raw)
	pos := 16 + hl
	al := func() { pos = (pos + 7) &^ 7 }
	al()
	n := h.N
	sg := &Segment{ImportID: h.ImportID, Base: h.Base, N: n}
	take := func(size int) []byte {
		if pos+size > len(b) {
			err = errors.New("segment file is truncated")
			return make([]byte, size)
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
