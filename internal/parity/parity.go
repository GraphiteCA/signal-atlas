// Package parity runs test/parity/scenario.json against the Go port of Signal Atlas, for the
// comparison with recorded reference results (see parity_test.go).
package parity

import (
	"encoding/json"
	"os"
	"path/filepath"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/py"
)

// Run runs the scenario with the Go port; root is the project folder.
func Run(work, here string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(here, "test", "parity", "scenario.json"))
	if err != nil {
		panic(err)
	}
	var sc struct {
		Params    []py.Params `json:"params"`
		FitParams []py.Params `json:"fitParams"`
		Area      []py.Params `json:"area"`
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		panic(err)
	}
	open := func(name string) *atlas.Atlas {
		d := filepath.Join(work, name)
		os.RemoveAll(d)
		a, err := atlas.Open(d)
		if err != nil {
			panic(err)
		}
		return a
	}
	A, B := open("go-a"), open("go-b")
	c, put, _ := A.Conn(nil)
	defer func() { A.Close(); B.Close() }()
	safe := func(v any, err error) any {
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		return v
	}
	out := map[string]any{}
	csv := filepath.Join(work, "survey.csv")
	out["import"] = safe(A.ImportCSV(csv, true, nil, "", nil))
	out["import_dup"] = safe(A.ImportCSV(csv, true, nil, "", nil))
	out["import_strict"] = safe(B.ImportCSV(csv, false, nil, "", nil))
	var queries []any
	for _, p := range sc.Params {
		q := map[string]any{}
		q["summary"] = safe(A.Summary(c, p))
		q["radios"] = safe(A.Radios(p))
		q["observations"] = safe(A.Observations(p))
		q["points"] = safe(A.Points(p))
		q["dates"] = safe(A.AvailableDates(p))
		q["selected"] = safe(A.SelectedGroups(p))
		scope, key, err := atlas.EstimateScope(p)
		q["scope"] = safe([]any{scope, key}, err)
		q["points100"] = safe(A.Points(p.With("limit", []string{"100"})))
		queries = append(queries, q)
	}
	out["queries"] = queries
	n1, _ := A.Networks(py.Params{})
	n2, _ := A.Networks(py.Params{"q": {"e"}, "limit": {"2"}})
	out["networks"] = []any{n1, n2}
	out["transmitters"] = []any{safe(A.Transmitters(c, py.Params{"ssid": {"Mesh"}})), safe(A.Transmitters(c, py.Params{"ssid": {"Exampé \U0001F437"}, "q": {"aa:00"}})), safe(A.Transmitters(c, py.Params{}))}
	out["assign"] = []any{safe(A.AssignBand(c, "Lonely", " aa:00:00:00:00:40", "5")), safe(A.AssignBand(c, "Lonely", "AA:00:00:00:00:40", "7")), safe(A.AssignBand(c, "Nope", "AA", "5"))}
	sel, _ := A.SelectedGroups(py.Params{})
	out["groups"] = map[string]any{"plain": atlas.EstimationGroups(sel, py.Params{}), "shared": atlas.EstimationGroups(sel, py.Params{"shared": {"1"}}),
		"sharedonly": atlas.EstimationGroups(sel, py.Params{"shared": {"1"}, "individual": {"0"}})}
	al := map[string]any{}
	for k, v := range atlas.UnknownBandAliases(sel) {
		al[k] = v
	}
	out["aliases"] = al
	var fits, fitted []any
	for _, fp := range sc.FitParams {
		sg, _ := A.SelectedGroups(fp)
		groups := atlas.EstimationGroups(sg, fp)
		_, key, _ := atlas.EstimateScope(fp)
		var res []any
		for _, g := range groups {
			r, err := A.FitRadio(g, fp, nil)
			if err != nil {
				panic(err)
			}
			res = append(res, r)
			if _, err := A.FitAndStore(c, g, fp, key, nil); err != nil {
				panic(err)
			}
		}
		fits = append(fits, res)
		fitted = append(fitted, safe(A.FittedEstimates(c, fp)))
	}
	out["fits"], out["fitted"] = fits, fitted
	var area, plans []any
	for _, p := range sc.Area {
		area = append(area, safe(A.AreaEstimates(c, p)))
		r, scoped, groups, err := A.AreaPlan(c, p, false, true)
		gl := []any{}
		for _, g := range groups {
			gl = append(gl, g)
		}
		plans = append(plans, safe([]any{r, scoped, gl}, err))
	}
	out["area"], out["area_plan"] = area, plans
	out["compare_save"] = safe(A.SaveComparison(c, sc.FitParams[0], "January"))
	out["comparisons"] = safe(atlas.Comparisons(c))
	out["info"] = safe(A.DatabaseInfo(c))
	ssid := "ExampleNet"
	obs, err := atlas.ReadObservations(filepath.Join(here, "test", "parity", "demo-survey.csv"), &ssid, "")
	if err != nil {
		panic(err)
	}
	o := atlas.DefaultEstimateOpts()
	o.StepM = 10
	out["estimate_report"] = safe(atlas.Estimate(obs, o))
	put()
	return py.Dumps(out, py.DumpOpts{AllowNaN: true, NoEnsureASCII: true})
}
