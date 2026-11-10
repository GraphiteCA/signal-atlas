package compare

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMatchesThePagesWorker runs the compare page's own JavaScript worker (in Node) and this port
// on the same synthetic drives under seven configurations (drives, legs, files, Wi-Fi/BLE/all,
// cuts, joins, a chosen baseline, an excluded drive, time zones) and compares the whole result
// objects, plus the first drive's CSV export byte for byte. Skipped without Node and Python.
func TestMatchesThePagesWorker(t *testing.T) {
	if exec.Command("node", "--version").Run() != nil || exec.Command("python", "--version").Run() != nil {
		t.Skip("needs node and python")
	}
	root, _ := filepath.Abs("../..")
	work := t.TempDir()
	if out, err := exec.Command("python", filepath.Join(root, "test", "compare", "gen_drives.py"), work).CombinedOutput(); err != nil {
		t.Fatalf("gen_drives: %v %s", err, out)
	}
	cases := filepath.Join(root, "test", "compare", "cases.json")
	raw, err := exec.Command("node", filepath.Join(root, "test", "compare", "ref_compare.js"), filepath.Join(root, "web", "compare.html"), work, cases).Output()
	if err != nil {
		t.Fatalf("ref_compare: %v", err)
	}
	var ref struct {
		Added   any              `json:"added"`
		Results []map[string]any `json:"results"`
		Exports []map[string]any `json:"exports"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	var cs struct {
		Files []string  `json:"files"`
		Cfgs  []*Config `json:"cfgs"`
	}
	b, _ := os.ReadFile(cases)
	json.Unmarshal(b, &cs)
	var got []map[string]any
	e := New(func(m map[string]any) {
		if m["type"] != "progress" {
			got = append(got, m)
		}
	}, filepath.Join(work, "tmp"))
	var files []FileIn
	for _, n := range cs.Files {
		st, _ := os.Stat(filepath.Join(work, n))
		files = append(files, FileIn{Name: n, Size: st.Size(), Path: filepath.Join(work, n)})
	}
	e.Handle(&Message{Type: "add", Files: files, Cfg: cs.Cfgs[0]})
	diffs := []string{}
	compareJSON(ref.Added, roundTrip(got[0]), ".added", &diffs)
	ei := 0
	for i, cfg := range cs.Cfgs {
		got = nil
		e.Handle(&Message{Type: "analyse", Cfg: cfg})
		res := roundTrip(got[len(got)-1]).(map[string]any)
		if r, ok := res["result"].(map[string]any); ok {
			delete(r, "ms")
		}
		compareJSON(ref.Results[i], res, fmt.Sprintf(".results[%d]", i), &diffs)
		if r, ok := res["result"].(map[string]any); ok && len(r["drives"].([]any)) > 0 {
			got = nil
			e.Handle(&Message{Type: "export", Key: r["drives"].([]any)[0].(map[string]any)["key"].(string), ID: 1})
			m := got[len(got)-1]
			text, _ := os.ReadFile(fmt.Sprint(m["file"]))
			want := ref.Exports[ei]
			if string(text) != want["text"] || float64(m["rows"].(int)) != want["rows"] || float64(m["dropped"].(int)) != want["dropped"] {
				diffs = append(diffs, fmt.Sprintf("export %d: rows %v/%v dropped %v/%v, text equal %v", i, m["rows"], want["rows"], m["dropped"], want["dropped"], string(text) == want["text"]))
			}
			ei++
		}
	}
	t.Logf("compared %d values across %d configurations", leaves, len(cs.Cfgs))
	if len(diffs) > 0 {
		t.Errorf("%d differences from the page's worker:\n%s", len(diffs), strings.Join(diffs[:min(len(diffs), 60)], "\n"))
	}
}

func roundTrip(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	json.Unmarshal(b, &out)
	return out
}

var leaves int

func compareJSON(a, b any, p string, out *[]string) {
	switch a.(type) {
	case float64, string, bool, nil:
		leaves++
	}
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		if !ok || math.Abs(x-y) > 1e-9*math.Max(1, math.Max(math.Abs(x), math.Abs(y))) {
			*out = append(*out, fmt.Sprintf("%s: js %v go %v", p, a, b))
		}
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			*out = append(*out, fmt.Sprintf("%s: js %d items, go %v", p, len(x), short(b)))
			return
		}
		for i := range x {
			compareJSON(x[i], y[i], fmt.Sprintf("%s[%d]", p, i), out)
		}
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: object vs %v", p, short(b)))
			return
		}
		for k := range x {
			if _, ok := y[k]; !ok {
				*out = append(*out, p+"."+k+": only in js")
				continue
			}
			compareJSON(x[k], y[k], p+"."+k, out)
		}
		for k := range y {
			if _, ok := x[k]; !ok {
				*out = append(*out, p+"."+k+": only in go")
			}
		}
	default:
		if fmt.Sprint(a) != fmt.Sprint(b) {
			*out = append(*out, fmt.Sprintf("%s: js %v go %v", p, a, b))
		}
	}
}

func short(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}
