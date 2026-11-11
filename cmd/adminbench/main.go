// adminbench exercises administration on an explicitly owned, disposable fixture.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/engine"
)

type row = map[string]any

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func owned(path string) bool {
	for range 4 {
		b, e := os.ReadFile(filepath.Join(path, "owner.json"))
		var marker struct {
			Purpose string
			Schema  int
		}
		if e == nil && json.Unmarshal(b, &marker) == nil && marker.Purpose == "signal-atlas-benchmark" && marker.Schema == 1 {
			return true
		}
		path = filepath.Dir(path)
	}
	return false
}

// Hash IDs and measurement values in order; exclude exactly the rows to remove.
func fingerprint(a *atlas.Atlas, excludeZero bool) string {
	h := sha256.New()
	var b [8]byte
	st := a.Obs.State()
	for _, s := range st.Segs {
		for i := 0; i < s.N; i++ {
			if excludeZero && s.Lat[i] == 0 && s.Lon[i] == 0 {
				continue
			}
			for _, v := range []uint64{uint64(s.Base + int64(i)), uint64(s.ImportID), uint64(s.Line[i]), math.Float64bits(s.Lat[i]), math.Float64bits(s.Lon[i]), math.Float64bits(s.RSSI[i]), math.Float64bits(s.Acc[i]), math.Float64bits(s.Freq[i])} {
				binary.LittleEndian.PutUint64(b[:], v)
				h.Write(b[:])
			}
			for _, k := range []uint32{s.SSID[i], s.MAC[i], s.Band[i], s.BandSrc[i], s.Radio[i], s.Date[i], s.Channel[i], s.First[i]} {
				v := st.Str[k]
				binary.LittleEndian.PutUint64(b[:], uint64(len(v)))
				h.Write(b[:])
				h.Write([]byte(v))
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
func main() {
	data := flag.String("data", "", "Owned engine data folder (fixture may be changed)")
	flag.Parse()
	root, err := filepath.EvalSymlinks(*data)
	must(err)
	root, err = filepath.Abs(root)
	must(err)
	if !owned(root) {
		panic("adminbench requires an owned disposable benchmark fixture")
	}
	runtime.GOMAXPROCS(8)
	report := row{"schema": 1, "data": root, "threads": 8, "notes": "Filesystem cache uncontrolled. Cleanup/native snapshot share immutable columns; includes candidate validation. Fingerprints and reopen checks are outside operation timings."}
	start := time.Now()
	s, err := engine.New(engine.Options{Data: root, Version: "dev"})
	must(err)
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	report["open_seconds"] = time.Since(start).Seconds()
	s.AllowHost("bench.local")
	request := func(method, path string, body any) row {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://bench.local"+path, bytes.NewReader(b))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 && w.Code != 202 {
			panic(fmt.Sprintf("%s: %d %s", path, w.Code, w.Body.String()))
		}
		var v row
		must(json.Unmarshal(w.Body.Bytes(), &v))
		return v
	}
	operation := func(name string, body row) row {
		t := time.Now()
		v := request("POST", "/api/app/admin/"+name, body)
		for v["status"] == "running" {
			time.Sleep(5 * time.Millisecond)
			v = request("GET", "/api/app/admin/operation", nil)
		}
		if v["status"] != "complete" {
			panic(fmt.Sprint(v))
		}
		return row{"wall_seconds": time.Since(t).Seconds(), "result": v["result"]}
	}
	report["before"] = request("GET", "/api/app/admin/status", nil)["database"]
	wanted := fingerprint(s.Atlas, true)
	t := time.Now()
	preview := request("GET", "/api/app/admin/preview", nil)
	report["preview"] = row{"wall_seconds": time.Since(t).Seconds(), "result": preview}
	report["cleanup"] = operation("cleanup", row{"token": preview["token"]})
	if fingerprint(s.Atlas, false) != wanted {
		panic("cleanup changed surviving measurements/IDs")
	}
	if request("GET", "/api/app/admin/preview", nil)["rows"] != float64(0) {
		panic("cleanup left zero pairs")
	}
	report["after"] = request("GET", "/api/app/admin/status", nil)["database"]
	snapshot := filepath.Join(root, "adminbench-snapshot.atlas")
	report["workspace_snapshot"] = operation("snapshot", row{"path": snapshot, "format": "workspace"})
	t = time.Now()
	copy, err := atlas.Open(snapshot)
	must(err)
	if fingerprint(copy, false) != wanted {
		panic("snapshot changed surviving measurements/IDs")
	}
	report["snapshot_reopen_seconds"] = time.Since(t).Seconds()
	copy.Close()
	s.Close()
	s = nil
	t = time.Now()
	s, err = engine.New(engine.Options{Data: root, Version: "dev"})
	must(err)
	report["reopen_seconds"] = time.Since(t).Seconds()
	if fingerprint(s.Atlas, false) != wanted {
		panic("restart changed surviving measurements/IDs")
	}
	report["survivor_sha256"] = wanted
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	report["final_go_memory"] = row{"heap_alloc": mem.HeapAlloc, "heap_sys": mem.HeapSys, "total_alloc": mem.TotalAlloc}
	must(json.NewEncoder(os.Stdout).Encode(report))
}
