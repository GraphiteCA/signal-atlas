package atlas

// Experimental policies only. Production fits, imported scans and cache scopes
// are deliberately unchanged until policy/thresholds have real truth validation.
import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func qualityGPS(accuracy []float64, rows []int) []int {
	out := make([]int, 0, len(rows))
	for _, i := range rows {
		if !(accuracy[i] > 100) {
			out = append(out, i)
		}
	}
	return out
}

// A conservative single-site gate for a bounded fit, not a universal Wi-Fi
// range limit. Multiple substantial components withhold a shared estimate.
func qualitySpatial(cols ObsCols, rows []int) ([]int, bool) {
	if len(rows) < 12 {
		return rows, false
	}
	seen := make([]bool, len(rows))
	var components [][]int
	scale := MetresPerDegree * math.Cos(cols.Lat[rows[0]]*math.Pi/180)
	for start := range rows {
		if seen[start] {
			continue
		}
		seen[start] = true
		queue := []int{start}
		component := []int{}
		for at := 0; at < len(queue); at++ {
			a := rows[queue[at]]
			component = append(component, a)
			for j, b := range rows {
				if !seen[j] && math.Hypot((cols.Lat[a]-cols.Lat[b])*MetresPerDegree, (cols.Lon[a]-cols.Lon[b])*scale) <= 300 {
					seen[j] = true
					queue = append(queue, j)
				}
			}
		}
		components = append(components, component)
	}
	best := components[0]
	for _, c := range components {
		if len(c) > len(best) {
			best = c
		}
	}
	if len(best)*10 < len(rows)*9 {
		return nil, true
	}
	slices.Sort(best)
	return best, false
}

func qualityFit(cols ObsCols, rows []int, robust bool) (*Fit, []float64, error) {
	c := ObsCols{}
	for _, i := range rows {
		c.Lat = append(c.Lat, cols.Lat[i])
		c.Lon = append(c.Lon, cols.Lon[i])
		c.RSSI = append(c.RSSI, cols.RSSI[i])
		c.MAC = append(c.MAC, cols.MAC[i])
	}
	idx := make([]int, len(rows))
	weights := make([]float64, len(rows))
	for i := range idx {
		idx[i] = i
		weights[i] = 1
	}
	var fit *Fit
	passes := 1
	if robust && len(rows) >= 12 {
		passes = 4
	}
	for pass := 0; pass < passes; pass++ {
		m, err := NewLocationModel(c, 3, weights, nil, nil)
		if err != nil {
			return nil, nil, err
		}
		fit, err = m.FitFast(idx, 3, 500, 10, false)
		if err != nil {
			return nil, nil, err
		}
		if pass == passes-1 {
			break
		}
		for i := range weights {
			d := math.Max(3, math.Hypot(m.x[i]-fit.EastM, m.y[i]-fit.NorthM))
			residual := math.Abs(c.RSSI[i] - (fit.Intercepts[c.MAC[i]] - 30*math.Log10(d)))
			weights[i] = math.Max(.05, math.Min(1, 6/math.Max(1e-12, residual)))
		}
	}
	return fit, weights, nil
}

// RANSAC-style hypotheses use the actual location solver, independent robust
// intercepts per radio, consensus scoring and a final fit of the accepted rows.
// Fixed trial count is a measured candidate, not a statistical success guarantee.
func qualityConsensus(cols ObsCols, rows []int) (*Fit, []int, bool, error) {
	r := rand.New(rand.NewPCG(821, 419))
	var bestRows []int
	bestLoss := math.Inf(1)
	for trial := 0; trial < 13; trial++ {
		subset := rows
		if trial > 0 {
			perm := r.Perm(len(rows))
			subset = make([]int, min(12, len(rows)))
			for i := range subset {
				subset[i] = rows[perm[i]]
			}
			slices.Sort(subset)
		}
		fit, _, err := qualityFit(cols, subset, false)
		if err != nil {
			continue
		}
		z := make(map[string][]float64)
		residualBase := make([]float64, len(rows))
		for j, i := range rows {
			distance := math.Hypot((cols.Lat[i]-fit.Latitude)*MetresPerDegree, (cols.Lon[i]-fit.Longitude)*MetresPerDegree*math.Cos(fit.Latitude*math.Pi/180))
			v := cols.RSSI[i] + 30*math.Log10(math.Max(3, distance))
			residualBase[j] = v
			z[cols.MAC[i]] = append(z[cols.MAC[i]], v)
		}
		medians := map[string]float64{}
		for key, values := range z {
			slices.Sort(values)
			mid := len(values) / 2
			m := values[mid]
			if len(values)%2 == 0 {
				m = (m + values[mid-1]) * .5
			}
			medians[key] = m
		}
		inliers := []int{}
		loss := 0.
		for j, i := range rows {
			residual := math.Abs(residualBase[j] - medians[cols.MAC[i]])
			if residual <= 8 {
				inliers = append(inliers, i)
			}
			loss += math.Pow(math.Min(8, residual), 2)
		}
		if len(inliers) > len(bestRows) || len(inliers) == len(bestRows) && loss < bestLoss {
			bestRows = inliers
			bestLoss = loss
		}
	}
	if len(bestRows) < 12 || len(bestRows)*10 < len(rows)*7 {
		return nil, nil, true, nil
	}
	fit, _, err := qualityFit(cols, bestRows, false)
	return fit, bestRows, false, err
}

func TestObservationQualityConservativeGates(t *testing.T) {
	rows := []int{0, 1, 2, 3}
	got := qualityGPS([]float64{math.NaN(), 0, 100, 101}, rows)
	if !slices.Equal(got, rows[:3]) {
		t.Fatal("unknown accuracy must be retained", got)
	}
	cols := ObsCols{}
	for i := 0; i < 24; i++ {
		lat := 48.4
		if i >= 12 {
			lat += .02
		}
		cols.Lat = append(cols.Lat, lat+float64(i%3)*.00001)
		cols.Lon = append(cols.Lon, -123.4)
		rows = append(rows, i)
	}
	rows = rows[4:]
	if _, ambiguous := qualitySpatial(cols, rows); !ambiguous {
		t.Fatal("two sites were silently collapsed")
	}
}

func TestObservationQualityCandidates(t *testing.T) {
	folder := os.Getenv("SIGNAL_ATLAS_QUALITY_REPORT")
	if folder == "" {
		t.Skip("explicit owned diagnostic output required")
	}
	b, err := os.ReadFile(filepath.Join(folder, "owner.json"))
	var owner struct {
		Purpose string
		Schema  int
	}
	if err != nil || json.Unmarshal(b, &owner) != nil || owner.Purpose != "signal-atlas-benchmark" || owner.Schema != 1 {
		t.Fatal("owned benchmark root required")
	}
	policies := []string{"baseline", "accuracy-100m", "spatial-300m", "huber-6db", "combined", "consensus-8db", "quality-candidate"}
	scenarios := []string{"clean", "rssi-spikes", "gps-teleports", "poor-gps-fixes", "legitimate-distant", "two-sites-shared-ssid"}
	report := []map[string]any{}
	for scenario, kind := range scenarios {
		for trial := 0; trial < 8; trial++ {
			r := rand.New(rand.NewPCG(uint64(49+scenario), uint64(917+trial)))
			cols := ObsCols{}
			accuracy := []float64{}
			bad := []bool{}
			const n = 96
			for i := 0; i < n; i++ {
				angle := 2 * math.Pi * float64(i) / n
				x, y := (50+r.Float64()*150)*math.Cos(angle), (50+r.Float64()*150)*math.Sin(angle)
				apx, apy := 20., -15.
				mac := "A"
				if kind == "two-sites-shared-ssid" && i >= n/2 {
					x += 2500
					apx += 2500
					mac = "B"
				}
				if kind == "legitimate-distant" && i < 8 {
					x += 1800
					y += 1200
				}
				rssi := -30 - 30*math.Log10(math.Max(3, math.Hypot(x-apx, y-apy))) + 2*r.NormFloat64()
				acc := 5.
				spurious := false
				if i < 8 {
					switch kind {
					case "rssi-spikes":
						rssi += 35
						spurious = true
					case "gps-teleports":
						x += 2500
						y += 1500
						spurious = true
					case "poor-gps-fixes":
						x += 400
						y -= 350
						acc = 250
						spurious = true
					}
				}
				cols.Lat = append(cols.Lat, 48.4+y/MetresPerDegree)
				cols.Lon = append(cols.Lon, -123.4+x/(MetresPerDegree*math.Cos(48.4*math.Pi/180)))
				cols.RSSI = append(cols.RSSI, rssi)
				cols.MAC = append(cols.MAC, mac)
				accuracy = append(accuracy, acc)
				bad = append(bad, spurious)
			}
			for _, policy := range policies {
				start := time.Now()
				rows := make([]int, n)
				for i := range rows {
					rows[i] = i
				}
				if policy == "accuracy-100m" || policy == "combined" || policy == "quality-candidate" {
					rows = qualityGPS(accuracy, rows)
				}
				ambiguous := false
				if policy == "spatial-300m" || policy == "combined" {
					rows, ambiguous = qualitySpatial(cols, rows)
				}
				if policy == "quality-candidate" {
					_, ambiguous = qualitySpatial(cols, rows)
				} // flag multiple sites; retain legitimate isolated scans for consensus

				row := map[string]any{"scenario": kind, "trial": trial, "policy": policy, "input_rows": n, "withheld_multiple_sites": ambiguous}
				var fit *Fit
				var weights []float64
				noConsensus := false
				if !ambiguous && (policy == "consensus-8db" || policy == "quality-candidate") {
					fit, rows, noConsensus, err = qualityConsensus(cols, rows)
					if err != nil {
						t.Fatal(kind, policy, err)
					}
				}
				row["withheld_no_consensus"] = noConsensus
				if !ambiguous && !noConsensus {
					kept := map[int]bool{}
					for _, i := range rows {
						kept[i] = true
					}
					falseRemoved, removedBad := 0, 0
					for i := range n {
						if !kept[i] {
							if bad[i] {
								removedBad++
							} else {
								falseRemoved++
							}
						}
					}
					row["removed_spurious"], row["removed_legitimate"] = removedBad, falseRemoved
					if fit == nil {
						fit, weights, err = qualityFit(cols, rows, policy == "huber-6db" || policy == "combined")
						if err != nil {
							t.Fatal(kind, policy, err)
						}
					}
					downweighted := 0
					for _, w := range weights {
						if w < .5 {
							downweighted++
						}
					}
					row["downweighted_below_half"] = downweighted
					if kind != "two-sites-shared-ssid" {
						row["truth_error_m"] = math.Hypot((fit.Latitude-(48.4-15/MetresPerDegree))*MetresPerDegree, (fit.Longitude-(-123.4+20/(MetresPerDegree*math.Cos(48.4*math.Pi/180))))*MetresPerDegree*math.Cos(48.4*math.Pi/180))
					}
				}
				if policy == "quality-candidate" {
					if kind == "two-sites-shared-ssid" {
						if !ambiguous {
							t.Fatal("candidate collapsed distinct sites")
						}
					} else {
						if ambiguous || noConsensus || row["truth_error_m"].(float64) > 20 {
							t.Fatal("known-position candidate failed", kind, trial, row)
						}
						if row["removed_legitimate"] != 0 {
							t.Fatal("candidate discarded valid fixture scans", kind, trial, row)
						}
					}
				}
				row["seconds"] = time.Since(start).Seconds()
				report = append(report, row)
			}
		}
	}
	f, err := os.OpenFile(filepath.Join(folder, "observation-quality.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(map[string]any{"schema": 1, "synthetic_only": true, "production_defaults_changed": false, "cases": report}); err != nil {
		t.Fatal(err)
	}
}
