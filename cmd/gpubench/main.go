// gpubench measures real float64 OpenCL calls, including transfers and fences.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"math"
	"os"
	"signal-atlas/internal/gpu"
	"time"
)

func main() {
	n := flag.Int("rows", 50, "observations per AP")
	loops := flag.Int("loops", 200, "scoring calls")
	fixed := flag.Bool("fixed-grid", true, "reuse immutable coarse grid")
	flag.Parse()
	if *n < 1 || *n > 4096 || *loops < 1 {
		panic("invalid benchmark shape")
	}
	in := gpu.Input{Floor: 3, Factor: 30, WeightSums: []float64{float64(*n)}}
	for i := 0; i < *n; i++ {
		in.X = append(in.X, float64(i%17)*3)
		in.Y = append(in.Y, float64(i%13)*2)
		in.RSSI = append(in.RSSI, -40-float64(i%37))
		in.Weights = append(in.Weights, 1)
		in.Groups = append(in.Groups, 0)
	}
	for y := -500; y <= 500; y += 10 {
		for x := -500; x <= 500; x += 10 {
			in.CandidateX = append(in.CandidateX, float64(x))
			in.CandidateY = append(in.CandidateY, float64(y))
		}
	}
	if *fixed {
		g, e := gpu.NewCandidateGrid(in.CandidateX, in.CandidateY)
		if e != nil {
			panic(e)
		}
		in.Grid = g
	}
	if _, e := gpu.Score(in, nil); e != nil {
		panic(e)
	}
	sum := sha256.New()
	var bits [8]byte
	start := time.Now()
	for i := 0; i < *loops; i++ {
		in.RSSI[i%*n] = -40 - float64(i%37) + float64(i%7)*.001
		out, e := gpu.Score(in, nil)
		if e != nil {
			panic(e)
		}
		for _, v := range out {
			binary.LittleEndian.PutUint64(bits[:], math.Float64bits(v))
			sum.Write(bits[:])
		}
	}
	seconds := time.Since(start).Seconds()
	json.NewEncoder(os.Stdout).Encode(map[string]any{"rows": *n, "calls": *loops, "seconds": seconds, "calls_per_second": float64(*loops) / seconds, "score_sha256": hex.EncodeToString(sum.Sum(nil)), "gpu": gpu.Status()})
}
