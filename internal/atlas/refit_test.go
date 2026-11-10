package atlas

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"signal-atlas/internal/db"
	"signal-atlas/internal/py"
)

func surveyCSV(nets []string, seconds int) string {
	var b strings.Builder
	b.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n")
	for n, ssid := range nets {
		for k := 0; k < 8; k++ {
			fmt.Fprintf(&b, "02:5A:00:00:%02X:00,%s,2026-05-01 10:%02d:%02d,2437,%d,%.6f,%.6f,4,WIFI\n",
				n, ssid, seconds/60, (seconds+k)%60, -55-k*3, 48.449+float64(k)*0.0002, 2.668+float64(n)*0.001+float64(k%3)*0.0002)
		}
	}
	return b.String()
}

// Importing a drive drops only the fits of networks it has sightings of; the rest are kept.
func TestImportKeepsUntouchedFits(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	first := filepath.Join(dir, "first.csv")
	writeFile(t, first, surveyCSV([]string{"Alpha", "Bravo", "Charlie", ""}, 0))
	if _, err := a.ImportCSV(first, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	p := py.Params{"min_rssi": {"-150"}}
	scoped, sampleKey, _, targets, err := a.AllTargets(p)
	if err != nil {
		t.Fatal(err)
	}
	c, put, _ := a.Conn(nil)
	for _, tg := range targets {
		groups, _ := a.TargetGroups(scoped, tg, false, true)
		for _, g := range groups {
			if _, err := a.FitAndStore(c, g, scoped, sampleKey, func() bool { return false }); err != nil {
				t.Fatal(err)
			}
		}
	}
	put()
	fitted := func() []string {
		var out []string
		for _, r := range table(t, a, "SELECT result FROM estimates") {
			var fit map[string]any
			json.Unmarshal([]byte(db.Str(r["result"])), &fit)
			name, _ := fit["ssid"].(string)
			if name == "" {
				name = "hidden " + fmt.Sprint(fit["mac"])
			}
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}
	before := fitted()
	if len(before) != 4 {
		t.Fatalf("fits before: %v", before)
	}
	// a second drive: more of Bravo, and of the hidden network (column 3, MAC ...03:00)
	second := filepath.Join(dir, "second.csv")
	text := surveyCSV([]string{"Bravo"}, 30)
	text += strings.Split(surveyCSV([]string{"x", "x", "x", ""}, 40), "\n")[25] + "\n"
	writeFile(t, second, strings.Replace(text, "02:5A:00:00:00:00,Bravo", "02:5A:00:00:01:00,Bravo", -1))
	if _, err := a.ImportCSV(second, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := fitted(); strings.Join(got, ",") != "Alpha,Charlie" {
		t.Fatalf("fits after the second drive: %v (before %v)", got, before)
	}
}
