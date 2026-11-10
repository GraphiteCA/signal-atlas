// gensurvey writes a large synthetic WiGLE CSV for
// benchmarks: drives that wind through a city, a few hundred thousand access points, realistic
// signal fall-off, a mix of bands and SSIDs.
//
//	go run ./cmd/gensurvey -rows 10000000 -out big.csv
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"time"
)

func main() {
	rows := flag.Int("rows", 1000000, "rows to write")
	aps := flag.Int("aps", 400000, "access points")
	out := flag.String("out", "survey.csv", "output CSV")
	seed := flag.Int64("seed", 7, "random seed")
	flag.Parse()
	rnd := rand.New(rand.NewSource(*seed))
	f, err := os.Create(*out)
	if err != nil {
		panic(err)
	}
	w := bufio.NewWriterSize(f, 8<<20)
	fmt.Fprint(w, "WigleWifi-1.6,appRelease=bench,model=Bench,release=1,device=Bench\r\n")
	fmt.Fprint(w, "MAC,SSID,AuthMode,FirstSeen,Channel,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AltitudeMeters,AccuracyMeters,Type\r\n")
	lat0, lon0 := 41.88, -87.63
	cos := math.Cos(lat0 * math.Pi / 180)
	type ap struct {
		x, y, tx  float64
		mac, ssid string
		freq, ch  int
	}
	list := make([]ap, *aps)
	freqs := []int{2412, 2437, 2462, 5180, 5240, 5745, 5805, 6115}
	for i := range list {
		fq := freqs[rnd.Intn(len(freqs))]
		ch := 1
		switch {
		case fq < 3000:
			ch = (fq - 2407) / 5
		case fq < 5900:
			ch = (fq - 5000) / 5
		default:
			ch = (fq - 5950) / 5
		}
		list[i] = ap{x: rnd.Float64() * 30000, y: rnd.Float64() * 30000, tx: -30 - rnd.Float64()*12,
			mac:  fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", rnd.Intn(256)&0xfc, rnd.Intn(256), rnd.Intn(256), i>>16&255, i>>8&255, i&255),
			ssid: fmt.Sprintf("net-%d", i/3), freq: fq, ch: ch}
	}
	// grid index of APs so each step sees its neighbours
	const cell = 300.0
	grid := map[[2]int][]int{}
	for i, a := range list {
		k := [2]int{int(a.x / cell), int(a.y / cell)}
		grid[k] = append(grid[k], i)
	}
	t := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	x, y, hd := 15000.0, 15000.0, rnd.Float64()*2*math.Pi
	n := 0
	for n < *rows {
		hd += (rnd.Float64() - .5) * .3
		x += math.Cos(hd) * 12
		y += math.Sin(hd) * 12
		if x < 0 || x > 30000 || y < 0 || y > 30000 {
			hd += math.Pi
		}
		t = t.Add(time.Second)
		lat := lat0 + y/111195.08
		lon := lon0 + x/(111195.08*cos)
		cx, cy := int(x/cell), int(y/cell)
		for dx := -1; dx <= 1 && n < *rows; dx++ {
			for dy := -1; dy <= 1 && n < *rows; dy++ {
				for _, i := range grid[[2]int{cx + dx, cy + dy}] {
					a := list[i]
					d := math.Max(1, math.Hypot(a.x-x, a.y-y))
					rssi := a.tx - 30*math.Log10(d) + rnd.NormFloat64()*3
					if rssi < -92 {
						continue
					}
					fmt.Fprintf(w, "%s,%s,[WPA2-PSK-CCMP][ESS],%s,%d,%d,%d,%.7f,%.7f,182,4,WIFI\r\n", a.mac, a.ssid, t.Format("2006-01-02 15:04:05"), a.ch, a.freq, int(math.Round(rssi)), lat, lon)
					n++
					if n >= *rows {
						break
					}
				}
			}
		}
	}
	w.Flush()
	f.Close()
	fmt.Println("wrote", n, "rows to", *out)
}
