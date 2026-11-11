// samplerbench isolates FitRadio sampling by using one receiver position.
// Model validation returns before any CPU/GPU grid scoring, on both revisions.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"signal-atlas/internal/atlas"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"
	"time"
)

func main() {
	n := flag.Int("rows", 200000, "observations")
	iterations := flag.Int("iterations", 12, "sampling repetitions")
	flag.Parse()
	runtime.GOMAXPROCS(8)
	d := obs.NewDictionary()
	empty := d.ID("")
	date := d.ID("2026-11-01")
	ssid := d.ID("Dense sampler")
	band := d.ID("5")
	keys := []uint32{}
	var members []any
	for i := 0; i < 3; i++ {
		k := fmt.Sprintf("radio-%d", i)
		keys = append(keys, d.ID(k))
		members = append(members, atlas.Group{"radio_key": k, "ssid": "Dense sampler", "mac": k, "band": "5", "count": *n / 3})
	}
	s := obs.NewSegment(1, 1, *n)
	for i := 0; i < *n; i++ {
		r := obs.Row{SSID: ssid, MAC: keys[i%3], Radio: keys[i%3], Band: band, BandSrc: empty, Date: date, Channel: empty, First: date, Lat: 48.4, Lon: -123.4, RSSI: float64(-30 - (i*17)%80), Acc: obs.Null, Freq: obs.Null}
		s.Append(&r)
	}
	a := &atlas.Atlas{Obs: obs.NewReadView([]*obs.Segment{s}, d)}
	g := atlas.Group{"radio_key": "shared:test", "ssid": "Dense sampler", "members": members, "count": *n}
	params := py.Params{"fit_mode": {"sample"}, "min_rssi": {"-150"}}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	var result map[string]any
	for range *iterations {
		var e error
		result, e = a.FitRadio(g, params, nil)
		if e != nil {
			panic(e)
		}
		if result["status"] != "insufficient" {
			panic("unexpected numerical scoring")
		}
	}
	elapsed := time.Since(start).Seconds()
	runtime.ReadMemStats(&after)
	b, _ := json.Marshal(result)
	hash := sha256.Sum256(b)
	json.NewEncoder(os.Stdout).Encode(map[string]any{"rows": *n, "iterations": *iterations, "seconds": elapsed, "ms_per_fit": elapsed * 1000 / float64(*iterations), "allocated_bytes_per_fit": (after.TotalAlloc - before.TotalAlloc) / uint64(*iterations), "result_sha256": hex.EncodeToString(hash[:]), "result": result})
}
