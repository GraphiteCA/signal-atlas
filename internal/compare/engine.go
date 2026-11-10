package compare

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"signal-atlas/internal/drivedb"
	"sort"
	"strings"
	"sync"
	"time"
)

type lastUnit struct {
	files []*File
	a, b  float64
}

// Engine is one compare window's worker: the logs it holds and the ids it has handed out.
type Engine struct {
	mu                        sync.Mutex
	files                     []*File
	macIdx                    map[string]uint32
	macStr                    []string
	macRand                   []uint8
	ssidIdx, authIdx, nodeIdx map[string]uint32
	authNames, nodeNames      []string
	lastUnits                 map[string]lastUnit
	lat0, cos                 float64
	// Say receives every message the worker would post to the page.
	Say func(map[string]any)
	// Tmp is where exports are written.
	Tmp string
}

// New makes an engine.
func New(say func(map[string]any), tmp string) *Engine {
	return &Engine{macIdx: map[string]uint32{}, ssidIdx: map[string]uint32{}, authIdx: map[string]uint32{}, nodeIdx: map[string]uint32{},
		lastUnits: map[string]lastUnit{}, Say: say, Tmp: tmp}
}

func (e *Engine) say(m map[string]any) {
	if e.Say != nil {
		e.Say(m)
	}
}

// FileIn is a log handed to the engine: a file on disk, or bytes (logs with no file behind them).
type FileIn struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	Data []byte `json:"-"`
}

// Message is the worker's onmessage: add, analyse, export, clear, remove.
type Message struct {
	Type  string   `json:"type"`
	Files []FileIn `json:"files"`
	Cfg   *Config  `json:"cfg"`
	Solo  bool     `json:"solo"`
	Key   string   `json:"key"`
	ID    any      `json:"id"`
	Name  string   `json:"name"`
}

// Handle runs one message; replies go to Say.
func (e *Engine) Handle(m *Message) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			e.say(map[string]any{"type": "error", "msg": fmt.Sprint(r)})
		}
	}()
	var err error
	switch m.Type {
	case "add":
		errs := []any{}
		for i, fl := range m.Files {
			dup := false
			for _, f := range e.files {
				dup = dup || (f.Name == fl.Name && f.Size == fl.Size)
			}
			if dup {
				errs = append(errs, map[string]any{"file": fl.Name, "msg": "Already added."})
				continue
			}
			if fl.Path != "" && drivedb.IsDatabase(fl.Path) {
				// a logger's database (or a Drive database, a WiGLE app backup): read as the CSV log
				// it converts to, kept in the session's folder so exports can read its lines again
				os.MkdirAll(e.Tmp, 0o755)
				tmp := filepath.Join(e.Tmp, fmt.Sprintf("db-%d-%s.csv", time.Now().UnixNano(), sanitize(fl.Name)))
				if _, _, _, cerr := drivedb.ToCSV(fl.Path, tmp, "signal-atlas", true, nil); cerr != nil {
					errs = append(errs, map[string]any{"file": fl.Name, "msg": cerr.Error()})
					continue
				}
				fl.Path = tmp
			}
			f, perr := e.parseFile(fl.Name, fl.Size, fl.Path, dataOrNil(fl), i, len(m.Files))
			if perr != nil {
				errs = append(errs, map[string]any{"file": fl.Name, "msg": perr.Error()})
				continue
			}
			f.Path, f.Solo = fl.Path, m.Solo
			if fl.Path == "" {
				f.Path = e.keepBytes(fl.Name, fl.Data)
			}
			e.files = append(e.files, f)
		}
		e.say(map[string]any{"type": "added", "errors": errs})
		e.say(map[string]any{"type": "result", "result": e.Analyse(m.Cfg)})
	case "analyse":
		e.say(map[string]any{"type": "result", "result": e.Analyse(m.Cfg)})
	case "export":
		err = e.export(m.Key, m.ID)
	case "clear":
		e.files = nil
		e.macIdx, e.macStr, e.macRand, e.ssidIdx = map[string]uint32{}, nil, nil, map[string]uint32{}
		e.say(map[string]any{"type": "result", "result": nil})
	case "remove":
		var keep []*File
		for _, f := range e.files {
			if f.Name != m.Name {
				keep = append(keep, f)
			}
		}
		e.files = keep
		e.say(map[string]any{"type": "result", "result": e.Analyse(m.Cfg)})
	}
	if err != nil {
		e.say(map[string]any{"type": "error", "msg": err.Error()})
	}
}

func dataOrNil(fl FileIn) []byte {
	if fl.Path != "" {
		return nil
	}
	if fl.Data == nil {
		return []byte{}
	}
	return fl.Data
}

// keepBytes saves a log that came as bytes, so an export can read it again.
func (e *Engine) keepBytes(name string, data []byte) string {
	if e.Tmp == "" {
		return ""
	}
	os.MkdirAll(e.Tmp, 0o755)
	p := filepath.Join(e.Tmp, fmt.Sprintf("log-%d-%s", time.Now().UnixNano(), sanitize(name)))
	if os.WriteFile(p, data, 0o644) != nil {
		return ""
	}
	return p
}

var unsafeName = regexp.MustCompile(`[^\w.\- ,]+`)

func sanitize(s string) string { return unsafeName.ReplaceAllString(s, "_") }

// Close removes the engine's temporary files.
func (e *Engine) Close() {
	for _, f := range e.files {
		if e.Tmp != "" && strings.HasPrefix(f.Path, e.Tmp) {
			os.Remove(f.Path)
		}
	}
}

func scanLines(path string, fn func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 4<<20)
	first := true
	for {
		ln, err := r.ReadString('\n')
		if len(ln) > 0 || err == nil {
			if strings.HasSuffix(ln, "\n") {
				ln = ln[:len(ln)-1]
			}
			if first {
				ln = strings.TrimPrefix(ln, "\uFEFF")
				first = false
			}
			fn(strings.ToValidUTF8(ln, "�"))
		}
		if err != nil {
			return nil
		}
	}
}

var needsQuote = regexp.MustCompile(`[",\n]`)

// export is the worker's exportUnit: one part's rows as a CSV, written to a file.
func (e *Engine) export(key string, id any) error {
	u, ok := e.lastUnits[key]
	if !ok {
		return errors.New("That part is no longer loaded.")
	}
	fl := append([]*File(nil), u.files...)
	sort.SliceStable(fl, func(a, b int) bool { return fl[a].Tmin < fl[b].Tmin })
	for _, f := range fl {
		if f.Path == "" {
			return errors.New("That part is no longer loaded.")
		}
	}
	var first *string
	var hdrs []string
	for _, f := range fl {
		ln0, got := true, false
		scanLines(f.Path, func(l string) {
			if got {
				return
			}
			l = strings.TrimSuffix(l, "\r")
			if ln0 {
				ln0 = false
				if strings.HasPrefix(l, "WigleWifi") {
					if first == nil {
						s := l
						first = &s
					}
					return
				}
			}
			hdrs = append(hdrs, l)
			got = true
		})
	}
	same := true
	for _, h := range hdrs {
		same = same && h == hdrs[0]
	}
	var cols []string
	for _, h := range hdrs {
		for _, c := range splitCsv(h) {
			c = strings.TrimSpace(c)
			found := false
			for _, x := range cols {
				found = found || x == c
			}
			if !found {
				cols = append(cols, c)
			}
		}
	}
	os.MkdirAll(e.Tmp, 0o755)
	path := filepath.Join(e.Tmp, fmt.Sprintf("compare-export-%d-%v-%d.csv", os.Getpid(), id, time.Now().UnixNano()))
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(out, 4<<20)
	if first != nil {
		w.WriteString(*first + "\n")
	}
	if same && len(hdrs) > 0 {
		w.WriteString(hdrs[0] + "\n")
	} else {
		w.WriteString(strings.Join(cols, ",") + "\n")
	}
	rows, dropped := 0, 0
	q := func(v string) string {
		if needsQuote.MatchString(v) {
			return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
		}
		return v
	}
	for fi, f := range fl {
		ln0 := true
		var ix map[string]int
		e.say(map[string]any{"type": "progress", "text": "Writing CSV from " + f.Name, "frac": (float64(fi) + .5) / float64(len(fl))})
		scanLines(f.Path, func(l string) {
			l = strings.TrimSuffix(l, "\r")
			if l == "" {
				return
			}
			if ln0 {
				ln0 = false
				if strings.HasPrefix(l, "WigleWifi") {
					return
				}
			}
			if ix == nil {
				ix = map[string]int{}
				for i, n := range splitCsv(l) {
					ix[strings.TrimSpace(n)] = i
				}
				return
			}
			c := splitCsv(l)
			get := func(n string) (string, bool) {
				i, ok := ix[n]
				if !ok || i >= len(c) {
					return "", ok && false
				}
				return c[i], true
			}
			fs, ok := get("FirstSeen")
			t := parseTs(fs, ok)
			if !(t > 0) {
				dropped++
				return
			}
			if t < u.a || t >= u.b {
				return
			}
			rows++
			if same {
				w.WriteString(l + "\n")
				return
			}
			parts := make([]string, len(cols))
			for k, n := range cols {
				if _, has := ix[n]; !has {
					continue
				}
				v, _ := get(n)
				if n == "SSID" {
					parts[k] = `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
				} else {
					parts[k] = q(v)
				}
			}
			w.WriteString(strings.Join(parts, ",") + "\n")
		})
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	out.Close()
	e.say(map[string]any{"type": "exported", "id": id, "key": key, "rows": rows, "dropped": dropped, "file": path})
	return nil
}
