package compare

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// cols are one file's rows, the worker's newCols(): the same types, so the same rounding.
type cols struct {
	t     []uint32
	mac   []uint32
	rssi  []int8
	node  []int16
	chn   []uint16
	band  []uint8
	auth  []uint8
	flags []uint8
	ssid  []uint32
	lat   []float64
	lon   []float64
}

func (c *cols) grow(n int) {
	c.t, c.mac, c.rssi, c.node = make([]uint32, 0, n), make([]uint32, 0, n), make([]int8, 0, n), make([]int16, 0, n)
	c.chn, c.band, c.auth, c.flags = make([]uint16, 0, n), make([]uint8, 0, n), make([]uint8, 0, n), make([]uint8, 0, n)
	c.ssid, c.lat, c.lon = make([]uint32, 0, n), make([]float64, 0, n), make([]float64, 0, n)
}

// File is one log, parsed (the worker's f).
type File struct {
	Name                                                 string
	Size                                                 int64
	Path                                                 string
	Solo                                                 bool
	N, Raw, NoGps, Corrupt, BadSignal, Other, OutOfOrder int
	Device, App                                          string
	HasNode                                              bool
	Tmin, Tmax                                           float64
	C                                                    cols
}

var macRe = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)

// splitCsv is the worker's splitCsv: a quote anywhere outside quotes opens quoting.
func splitCsv(line string) []string {
	if strings.IndexByte(line, '"') < 0 {
		return strings.Split(line, ",")
	}
	var out []string
	var cur strings.Builder
	q := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if q {
			if c == '"' {
				if i+1 < len(line) && line[i+1] == '"' {
					cur.WriteByte('"')
					i++
				} else {
					q = false
				}
			} else {
				cur.WriteByte(c)
			}
		} else if c == '"' {
			q = true
		} else if c == ',' {
			out = append(out, cur.String())
			cur.Reset()
		} else {
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

// parseTs is the worker's parseTs: "YYYY-MM-DD HH:MM:SS" (UTC) to seconds, NaN otherwise.
func parseTs(s string, ok bool) float64 {
	if !ok || len(s) < 19 || s[4] != '-' || s[13] != ':' {
		return math.NaN()
	}
	p := func(a, b int) float64 { return jsNumber(s[a:b], true) }
	y, mo, d, h, mi, se := p(0, 4), p(5, 7), p(8, 10), p(11, 13), p(14, 16), p(17, 19)
	for _, v := range []float64{y, mo, d, h, mi, se} {
		if !finite(v) {
			return math.NaN()
		}
	}
	yi := math.Trunc(y)
	if yi >= 0 && yi <= 99 { // Date.UTC maps two-digit years to 19xx
		yi += 1900
	}
	t := time.Date(int(yi), time.Month(int(math.Trunc(mo))-1+1), 1, 0, 0, 0, 0, time.UTC)
	t = t.AddDate(0, 0, int(math.Trunc(d))-1)
	sec := float64(t.Unix()) + math.Trunc(h)*3600 + math.Trunc(mi)*60 + math.Trunc(se)
	return sec
}

// lineFields is one parsed data row before interning.
type lineFields struct {
	mac, ssid, auth, node string
	ts, rssi, lat, lon    float64
	fq, ch                float64
	other, ble            bool
	status                int // 0 kept, 1 corrupt, 2 other type, 3 no GPS, 4 bad signal
}

type header struct {
	ix      map[string]int
	hasNode bool
}

func (h *header) get(c []string, name string) (string, bool) {
	i, ok := h.ix[name]
	if !ok || i >= len(c) {
		return "", false
	}
	return c[i], true
}

func (h *header) parse(ln string) lineFields {
	c := splitCsv(ln)
	var r lineFields
	m, _ := h.get(c, "MAC")
	m = strings.ToUpper(m)
	fs, fsOK := h.get(c, "FirstSeen")
	r.ts = parseTs(fs, fsOK)
	if m == "" || !macRe.MatchString(m) || !(r.ts > 0) {
		r.status = 1
		return r
	}
	ty, _ := h.get(c, "Type")
	if ty == "" {
		ty = "WIFI"
	}
	ty = strings.ToUpper(ty)
	if ty != "WIFI" && ty != "BLE" && ty != "BT" {
		r.status = 2
		return r
	}
	la, laOK := h.get(c, "CurrentLatitude")
	lo, loOK := h.get(c, "CurrentLongitude")
	r.lat, r.lon = jsNumber(la, laOK), jsNumber(lo, loOK)
	if !finite(r.lat) || !finite(r.lon) || (math.Abs(r.lat) < 1e-6 && math.Abs(r.lon) < 1e-6) || math.Abs(r.lat) > 90 || math.Abs(r.lon) > 180 {
		r.status = 3
		return r
	}
	rs, rsOK := h.get(c, "RSSI")
	r.rssi = jsNumber(rs, rsOK)
	if !finite(r.rssi) || r.rssi >= 0 || r.rssi < -127 {
		r.status = 4
		return r
	}
	r.mac = m
	if h.hasNode {
		r.node, _ = h.get(c, "Node")
		if r.node == "" {
			r.node = "?"
		}
	}
	fq, fqOK := h.get(c, "Frequency")
	r.fq = jsNumber(fq, fqOK)
	if !finite(r.fq) && !math.IsInf(r.fq, 0) || r.fq == 0 { // +x||0
		if math.IsNaN(r.fq) {
			r.fq = 0
		}
	}
	ch, chOK := h.get(c, "Channel")
	r.ch = jsNumber(ch, chOK)
	if math.IsNaN(r.ch) {
		r.ch = 0
	}
	r.auth, _ = h.get(c, "AuthMode")
	r.ssid, _ = h.get(c, "SSID")
	r.ble = ty != "WIFI"
	return r
}

var errNoHeader = errors.New("This file has no WiGLE CSV header.")

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

var metaRe = map[string]*regexp.Regexp{}

func metaField(line, k string) string {
	re, ok := metaRe[k]
	if !ok {
		re = regexp.MustCompile(`(?:^|,)` + k + `=([^,]*)`)
		metaRe[k] = re
	}
	m := re.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}

// lineReader yields a file's lines as the worker splits them: on \n, a trailing \r dropped, empty
// lines skipped, a leading BOM dropped, invalid UTF-8 replaced (TextDecoder's non-fatal decoding).
type lineReader struct {
	f     *os.File
	buf   []byte
	start int
	end   int
	eof   bool
	first bool
	data  []byte // in-memory input instead of a file
}

func (r *lineReader) next() (string, bool) {
	for {
		if i := bytes.IndexByte(r.buf[r.start:r.end], '\n'); i >= 0 {
			ln := r.buf[r.start : r.start+i]
			r.start += i + 1
			if s, ok := r.clean(ln); ok {
				return s, true
			}
			continue
		}
		if r.eof {
			if r.start < r.end {
				ln := r.buf[r.start:r.end]
				r.start = r.end
				if s, ok := r.clean(ln); ok {
					return s, true
				}
			}
			return "", false
		}
		// refill: keep the partial line, read more
		copy(r.buf, r.buf[r.start:r.end])
		r.end -= r.start
		r.start = 0
		if r.end == len(r.buf) {
			nb := make([]byte, len(r.buf)*2)
			copy(nb, r.buf[:r.end])
			r.buf = nb
		}
		if r.data != nil {
			n := copy(r.buf[r.end:], r.data)
			r.data = r.data[n:]
			r.end += n
			if len(r.data) == 0 {
				r.eof = true
			}
			continue
		}
		n, err := r.f.Read(r.buf[r.end:])
		r.end += n
		if err != nil {
			r.eof = true
		}
	}
}

func (r *lineReader) clean(ln []byte) (string, bool) {
	if !r.first {
		r.first = true
		ln = bytes.TrimPrefix(ln, []byte{0xef, 0xbb, 0xbf})
	}
	ln = trimCR(ln)
	if len(ln) == 0 {
		return "", false
	}
	s := string(ln)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return s, true
}

// parseFile is the worker's parseFile. Blocks of lines parse on every core; the rows are then
// interned in file order (the ids the worker gives MACs, SSIDs, auth modes and node names), so
// ids match the page's, and memory stays at the size of the columns.
func (e *Engine) parseFile(name string, size int64, path string, data []byte, fi, total int) (*File, error) {
	f := &File{Name: name, Size: size, Device: "Unknown logger", Tmin: math.Inf(1), Tmax: math.Inf(-1)}
	r := &lineReader{buf: make([]byte, 4<<20), data: data}
	if data == nil {
		fh, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer fh.Close()
		r.f = fh
	} else if len(data) == 0 {
		r.eof = true
	}
	var hdr *header
	line0 := true
	for hdr == nil {
		s, ok := r.next()
		if !ok {
			return nil, errNoHeader
		}
		if line0 {
			line0 = false
			if strings.HasPrefix(s, "WigleWifi") {
				parts := []string{}
				for _, k := range []string{"model", "device"} {
					if v := metaField(s, k); v != "" {
						parts = append(parts, v)
					}
				}
				if len(parts) > 0 {
					f.Device = strings.Join(parts, " / ")
				}
				f.App = metaField(s, "appRelease")
				continue
			}
		}
		h := splitCsv(s)
		ix := map[string]int{}
		for i, n := range h {
			ix[strings.TrimSpace(n)] = i
		}
		_, a := ix["MAC"]
		_, b := ix["CurrentLatitude"]
		_, c := ix["FirstSeen"]
		if !a || !b || !c {
			return nil, errNoHeader
		}
		_, hasNode := ix["Node"]
		hdr = &header{ix: ix, hasNode: hasNode}
		f.HasNode = hasNode
	}
	f.C.grow(int(min(size/90+16, 1<<26)))
	type block struct {
		lines []string
		out   []lineFields
		done  chan struct{}
	}
	workers := runtime.NumCPU()
	jobs := make(chan *block, workers*2)
	ordered := make(chan *block, workers*2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				b.out = make([]lineFields, len(b.lines))
				for i, ln := range b.lines {
					b.out[i] = hdr.parse(ln)
				}
				b.lines = nil
				close(b.done)
			}
		}()
	}
	go func() {
		defer close(ordered)
		defer close(jobs)
		cur := &block{done: make(chan struct{})}
		for {
			s, ok := r.next()
			if !ok {
				break
			}
			cur.lines = append(cur.lines, s)
			if len(cur.lines) == 32768 {
				jobs <- cur
				ordered <- cur
				cur = &block{done: make(chan struct{})}
			}
		}
		if len(cur.lines) > 0 {
			jobs <- cur
			ordered <- cur
		}
	}()
	var last float64 = -1
	nb := 0
	for b := range ordered {
		<-b.done
		nb++
		for _, r := range b.out {
			e.addRow(f, &r, &last)
		}
		if nb%16 == 0 {
			e.say(map[string]any{"type": "progress", "text": fmt.Sprintf("Reading %s (%d of %d)", name, fi+1, total), "frac": float64(fi) / float64(total)})
		}
	}
	wg.Wait()
	if f.N == 0 {
		return nil, errors.New("This file has no rows with GPS positions.")
	}
	return f, nil
}

func (e *Engine) addRow(f *File, r *lineFields, last *float64) {
	f.Raw++
	switch r.status {
	case 1:
		f.Corrupt++
		return
	case 2:
		f.Other++
		return
	case 3:
		f.NoGps++
		return
	case 4:
		f.BadSignal++
		return
	}
	id, ok := e.macIdx[r.mac]
	if !ok {
		id = uint32(len(e.macStr))
		e.macIdx[r.mac] = id
		e.macStr = append(e.macStr, r.mac)
		rnd := uint8(0)
		if strings.ContainsRune("26AEae", rune(r.mac[1])) {
			rnd = 1
		}
		e.macRand = append(e.macRand, rnd)
	}
	C := &f.C
	C.t = append(C.t, uint32(toUint(r.ts, 32)))
	C.mac = append(C.mac, id)
	C.rssi = append(C.rssi, int8(toInt(r.rssi, 8)))
	C.lat = append(C.lat, r.lat)
	C.lon = append(C.lon, r.lon)
	if r.ts < *last {
		f.OutOfOrder++
	}
	*last = r.ts
	if r.ts < f.Tmin {
		f.Tmin = r.ts
	}
	if r.ts > f.Tmax {
		f.Tmax = r.ts
	}
	nd := int16(-1)
	if f.HasNode {
		nd = int16(toInt(float64(intern(e.nodeIdx, &e.nodeNames, r.node)), 16))
	}
	C.node = append(C.node, nd)
	bd := uint8(0)
	switch {
	case r.fq > 5925:
		bd = 3
	case r.fq >= 4900:
		bd = 2
	case r.fq >= 2400:
		bd = 1
	}
	C.band = append(C.band, bd)
	C.chn = append(C.chn, uint16(toUint(r.ch, 16)))
	C.auth = append(C.auth, uint8(toUint(float64(intern(e.authIdx, &e.authNames, r.auth)), 8)))
	fl := uint8(0)
	if r.ble {
		fl |= 1
	}
	if r.ssid == "" {
		fl |= 2
	}
	C.flags = append(C.flags, fl)
	sid := uint32(0)
	if r.ssid != "" {
		sid = intern(e.ssidIdx, nil, r.ssid)
	}
	C.ssid = append(C.ssid, sid)
	f.N++
}

func intern(m map[string]uint32, list *[]string, v string) uint32 {
	if id, ok := m[v]; ok {
		return id
	}
	id := uint32(len(m))
	m[v] = id
	if list != nil {
		*list = append(*list, v)
	}
	return id
}
