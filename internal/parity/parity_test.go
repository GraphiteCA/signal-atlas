package parity

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMatchesGraphitesPython runs the same awkward synthetic survey through Graphite's Python
// Signal Atlas (python) and through the Go port, and compares every result: imports,
// every query under twenty filter sets (errors included), group keys, every fit in four modes,
// stored estimates, area plans, comparisons, and the single-network
// report. Numbers must agree to a relative 1e-9 (positions: 1e-7, a centimetre). It is skipped
// when Python with numpy is not installed.
func TestMatchesGraphitesPython(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	if exec.Command("python", "-c", "import numpy").Run() != nil {
		t.Skip("Python with numpy is not available")
	}
	root, _ := filepath.Abs("../..")
	work := t.TempDir()
	if out, err := exec.Command("python", filepath.Join(root, "test", "parity", "gen_survey.py"), work).CombinedOutput(); err != nil {
		t.Fatalf("gen_survey: %v\n%s", err, out)
	}
	ref, err := exec.Command("python", filepath.Join(root, "test", "parity", "ref_atlas.py"), work).Output()
	if err != nil {
		t.Fatalf("ref_atlas: %v", err)
	}
	got, err := Run(work, root)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(regexp.MustCompile(`\bNaN\b`).ReplaceAll(ref, []byte("null")), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(got), &b); err != nil {
		t.Fatal(err)
	}
	var diffs []string
	compare(a, b, "", &diffs)
	if len(diffs) > 0 {
		t.Errorf("%d differences from Graphite's Python:\n%s", len(diffs), strings.Join(diffs[:min(60, len(diffs))], "\n"))
	}
}

var skip = map[string]bool{"imported_utc": true, "created_utc": true, "compare_save": true, "sqlite_version": true, "source": true, "type": true}
var loose = regexp.MustCompile(`latitude|longitude|east_m|north_m|sse|rms|shift_m|intercept|predicted|residual|distance_m`)

func compare(a, b any, p string, out *[]string) {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: py %v go %v", p, a, b))
			return
		}
		tol := 1e-9
		if loose.MatchString(p) {
			tol = 1e-7
		}
		if math.Abs(x-y) > tol*math.Max(1, math.Max(math.Abs(x), math.Abs(y))) {
			*out = append(*out, fmt.Sprintf("%s: py %v go %v", p, x, y))
		}
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			*out = append(*out, fmt.Sprintf("%s: py %d items, go %v", p, len(x), describe(b)))
			return
		}
		for i := range x {
			compare(x[i], y[i], fmt.Sprintf("%s[%d]", p, i), out)
		}
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: object vs %v", p, describe(b)))
			return
		}
		for k := range x {
			if skip[k] || (k == "id" && strings.Contains(p, "comparisons")) || (k == "filename" && p == ".info") {
				continue
			}
			if _, ok := y[k]; !ok {
				*out = append(*out, p+"."+k+": only in Python")
				continue
			}
			compare(x[k], y[k], p+"."+k, out)
		}
		for k := range y {
			if _, ok := x[k]; !ok && !skip[k] {
				*out = append(*out, p+"."+k+": only in Go")
			}
		}
	default:
		if fmt.Sprint(a) != fmt.Sprint(b) {
			*out = append(*out, fmt.Sprintf("%s: py %v go %v", p, a, b))
		}
	}
}

func describe(v any) string {
	if l, ok := v.([]any); ok {
		return fmt.Sprintf("%d items", len(l))
	}
	return fmt.Sprintf("%v", v)
}

var _ = os.Getenv
