// fullprepbench completes an actual preparation sweep on a disposable owned
// workspace. No synthetic locations or pre-completed checkpoints are accepted.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/engine"
	"zombiezen.com/go/sqlite"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func main() {
	data := flag.String("data", "", "owned disposable engine directory")
	workers := flag.Int("workers", 2, "preparation workers")
	solver := flag.String("solver", "reference", "reference or fast, identical between engines")
	profile := flag.String("profile", "", "optional separate diagnostic CPU profile")
	verify := flag.Bool("verify-existing", false, "reopen a completed owned sweep and verify no calculations are repeated")
	flag.Parse()
	runtime.GOMAXPROCS(8)
	root, e := filepath.Abs(*data)
	must(e)
	owned := false
	parent := root
	for range 5 {
		b, e := os.ReadFile(filepath.Join(parent, "owner.json"))
		var m struct {
			Purpose string
			Schema  int
		}
		if e == nil && json.Unmarshal(b, &m) == nil && m.Purpose == "signal-atlas-benchmark" && m.Schema == 1 {
			owned = true
			break
		}
		parent = filepath.Dir(parent)
	}
	if !owned {
		panic("owned benchmark workspace required")
	}
	physical, e := atlas.WorkspaceDir(filepath.Join(root, "store"))
	must(e)
	check, e := db.OpenReadOnly(filepath.Join(physical, "atlas.sqlite3"))
	must(e)
	n, e := db.Scalar(check, "SELECT count(*) FROM estimates")
	must(e)
	cp, e := db.Scalar(check, "SELECT count(*) FROM preparation_checkpoints")
	must(e)
	must(check.Close())
	if !*verify && (db.Int(n) != 0 || db.Int(cp) != 0) {
		panic("fresh unfitted fixture required")
	}
	if *verify && db.Int(cp) == 0 {
		panic("completed fixture required for reopen verification")
	}
	if *solver != "reference" && *solver != "fast" {
		panic("unknown solver")
	}
	if *profile != "" {
		f, e := os.OpenFile(*profile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		must(e)
		must(pprof.StartCPUProfile(f))
		defer func() { pprof.StopCPUProfile(); f.Close() }()
	}
	start := time.Now()
	s, e := engine.New(engine.Options{Data: root, Version: "bench", Hosts: []string{"bench.local"}})
	must(e)
	defer s.Close()
	call := func(method, path string, body any) map[string]any {
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(method, "http://bench.local"+path, bytes.NewReader(b)))
		if w.Code != 200 {
			panic(fmt.Sprintf("%s %d %s", path, w.Code, w.Body.String()))
		}
		var v map[string]any
		must(json.Unmarshal(w.Body.Bytes(), &v))
		return v
	}
	call("POST", "/api/app/preparation/settings", map[string]any{"paused": false, "enabled": true, "workers": *workers, "memory_mb": 256, "grouping": "shared", "fit_mode": "sample"})
	call("POST", "/api/app/settings", map[string]any{"fast_fits": *solver == "fast"})
	samples := []map[string]any{}
	var state map[string]any
	for {
		state = call("GET", "/api/app/preparation/status", nil)
		if state["status"] == "failed" || state["status"] == "limited" {
			panic(fmt.Sprintf("preparation failed: %v", state))
		}
		p, _ := state["progress"].(map[string]any)
		if p != nil {
			sample := map[string]any{"seconds": time.Since(start).Seconds(), "completed": p["completed"]}
			samples = append(samples, sample)
			if len(samples)%10 == 0 {
				fmt.Fprintln(os.Stderr, sample)
			}
		}
		if state["status"] == "complete" {
			break
		}
		time.Sleep(time.Second)
	}
	seconds := time.Since(start).Seconds()
	call("POST", "/api/app/preparation/settings", map[string]any{"paused": true})
	c, put, e := s.Atlas.Conn(nil)
	must(e)
	stats, e := db.One(c, "SELECT count(*) completed,sum(fitted) fitted,sum(unavailable) unavailable FROM preparation_checkpoints")
	must(e)
	saved, e := db.Scalar(c, "SELECT count(*) FROM estimates")
	must(e)
	put()
	c, put, e = s.Atlas.Conn(nil)
	must(e)
	sum := sha256.New()
	must(db.Each(c, "SELECT radio_key,scope_key,result FROM estimates ORDER BY radio_key,scope_key", nil, func(stmt *sqlite.Stmt) error {
		for i := 0; i < 3; i++ {
			sum.Write([]byte(stmt.ColumnText(i)))
			sum.Write([]byte{0})
		}
		return nil
	}))
	put()
	if *verify {
		if db.Int(stats["completed"]) != db.Int(cp) || db.Int(saved) != db.Int(n) || state["targets_committed"] != nil {
			panic("reopen repeated preparation work or changed durable counts")
		}
	}
	report := map[string]any{"schema": 1, "verification_only": *verify, "solver": *solver, "workers": *workers, "threads": 8, "seconds": seconds, "rows": s.Atlas.Obs.State().N, "stats": stats, "saved": saved, "targets_per_second": float64(db.Int(stats["completed"])) / seconds, "fitted_per_second": float64(db.Int(stats["fitted"])) / seconds, "samples": samples, "metrics": state, "estimates_sha256": hex.EncodeToString(sum.Sum(nil))}
	must(json.NewEncoder(os.Stdout).Encode(report))
}
