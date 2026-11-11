// solvercompare computes matched real-survey fits without saving or reusing any fit.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/py"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func main() {
	data := flag.String("store", "", "owned observation workspace copy")
	count := flag.Int("groups", 256, "deterministic count-stratified groups")
	accelerated := flag.Bool("gpu", false, "also compute both versioned float64 GPU variants")
	flag.Parse()
	if *count < 1 {
		panic("Positive group count required")
	}
	p, e := filepath.Abs(*data)
	must(e)
	owned := false
	for dir, i := p, 0; i < 6; dir, i = filepath.Dir(dir), i+1 {
		b, e := os.ReadFile(filepath.Join(dir, "owner.json"))
		var v struct {
			Purpose string
			Schema  int
		}
		if e == nil && json.Unmarshal(b, &v) == nil && v.Purpose == "signal-atlas-benchmark" && v.Schema == 1 {
			owned = true
			break
		}
	}
	if !owned {
		panic("Owned benchmark copy required")
	}
	runtime.GOMAXPROCS(1)
	a, e := atlas.Open(p)
	must(e)
	defer a.Close()
	groups, e := a.SelectedGroups(py.Params{})
	must(e)
	groups = atlas.EstimationGroups(groups, py.Params{"shared": {"1"}, "individual": {"0"}})
	sort.Slice(groups, func(i, j int) bool {
		ni, nj := fmt.Sprint(groups[i]["count"]), fmt.Sprint(groups[j]["count"])
		if ni != nj {
			return len(ni) < len(nj) || len(ni) == len(nj) && ni < nj
		}
		return fmt.Sprint(groups[i]["radio_key"]) < fmt.Sprint(groups[j]["radio_key"])
	})
	selected := []atlas.Group{}
	for i := 0; i < min(*count, len(groups)); i++ {
		selected = append(selected, groups[i*len(groups)/min(*count, len(groups))])
	}
	report := map[string]any{"schema": 1, "groups_total": len(groups), "groups_tested": len(selected), "threads": 1, "notes": "Count-stratified fixed identities; identical source rows and fit mode across solvers. No cache reads/writes. Delta is agreement with reference, not surveyed transmitter error."}
	for _, mode := range []string{"sample", "all"} {
		var refSeconds, fastSeconds float64
		gpuSeconds := map[string]float64{}
		rows := []map[string]any{}
		deltas := []float64{}
		same, statusMismatch := 0, 0
		refHash, fastHash := sha256.New(), sha256.New()
		for i, g := range selected {
			fits := map[string]map[string]any{}
			order := []string{"reference", "fast"}
			if i%2 == 1 {
				order[0], order[1] = order[1], order[0]
			}
			if *accelerated {
				order = append(order, atlas.GPUReference, atlas.GPUFast)
			}
			for _, solver := range order {
				params := py.Params{"fit_mode": {mode}}
				if solver != "reference" {
					params["solver"] = []string{solver}
				}
				t := time.Now()
				fit, e := a.FitRadio(g, params, nil)
				must(e)
				seconds := time.Since(t).Seconds()
				if solver == "reference" {
					refSeconds += seconds
				} else if solver == "fast" {
					fastSeconds += seconds
				} else {
					gpuSeconds[solver] += seconds
				}
				fits[solver] = fit
			}
			ref, fast := fits["reference"], fits["fast"]
			for solver, fit := range fits {
				if solver != "reference" && solver != "fast" {
					continue
				}
				v, e := py.Dumps(fit, py.DumpOpts{})
				must(e)
				h := refHash
				if solver == "fast" {
					h = fastHash
				}
				h.Write([]byte(v))
				h.Write([]byte{0})
			}
			row := map[string]any{"radio_key": g["radio_key"], "count": g["count"], "reference": ref, "fast": fast}
			if *accelerated {
				row[atlas.GPUReference] = fits[atlas.GPUReference]
				row[atlas.GPUFast] = fits[atlas.GPUFast]
			}
			if ref["status"] != fast["status"] {
				statusMismatch++
			} else if ref["status"] == "fitted" {
				lat, lon := ref["latitude"].(float64), ref["longitude"].(float64)
				d := math.Hypot((fast["latitude"].(float64)-lat)*atlas.MetresPerDegree, (fast["longitude"].(float64)-lon)*atlas.MetresPerDegree*math.Cos(lat*math.Pi/180))
				deltas = append(deltas, d)
				if d == 0 {
					same++
				}
				row["delta_m"] = d
			}
			rows = append(rows, row)
		}
		sort.Float64s(deltas)
		var p50, p95, worst float64
		if len(deltas) > 0 {
			p50 = deltas[(len(deltas)-1)/2]
			p95 = deltas[(len(deltas)*95+99)/100-1]
			worst = deltas[len(deltas)-1]
		}
		report[mode] = map[string]any{"reference_seconds": refSeconds, "fast_seconds": fastSeconds, "speedup": refSeconds / fastSeconds, "fitted_pairs": len(deltas), "same_point": same, "status_mismatch": statusMismatch, "delta_p50_m": p50, "delta_p95_m": p95, "delta_max_m": worst, "reference_sha256": hex.EncodeToString(refHash.Sum(nil)), "fast_sha256": hex.EncodeToString(fastHash.Sum(nil)), "results": rows}
		report[mode].(map[string]any)["gpu_seconds"] = gpuSeconds
	}
	must(json.NewEncoder(os.Stdout).Encode(report))
}
