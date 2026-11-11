package parity

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMatchesReference checks recorded reference results against Go: imports,
// every query under twenty filter sets (errors included), group keys, every fit in four modes,
// stored estimates, area plans, comparisons, and the single-network
// report. Numbers must agree to a relative 1e-9 (positions: 1e-7, a centimetre). It is skipped
// with -short. The fixtures and expected results were captured from the original
// Python implementation at 017022f1ba7faefcb7c3b181fe834edfe93f29e8 before its removal.
// Expected results adopt the current Go API: numeric bands before unknown, the
// current invalid-date message, and added_rows/duplicate_rows import counters.
func TestMatchesReference(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	root, _ := filepath.Abs("../..")
	work := t.TempDir()
	fixture, err := os.ReadFile(filepath.Join(root, "test", "parity", "fixtures", "survey.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "survey.csv"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := os.ReadFile(filepath.Join(root, "test", "parity", "expected.json"))
	if err != nil {
		t.Fatal(err)
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
	// Phase 2 adds import accounting without changing this fixture's accepted
	// rows. Assert those new fields before comparing the unchanged Python API.
	goResult := b.(map[string]any)
	for _, key := range []string{"import", "import_dup"} {
		row := goResult[key].(map[string]any)
		if row["zero_excluded_rows"] != float64(0) {
			t.Fatalf("%s unexpectedly excluded rows: %v", key, row)
		}
		delete(row, "zero_excluded_rows")
		if key == "import" {
			if row["empty"] != false {
				t.Fatalf("nonempty fixture: %v", row)
			}
			delete(row, "empty")
		} else {
			if row["added_rows"] != float64(0) {
				t.Fatalf("duplicate added rows: %v", row)
			}
			delete(row, "added_rows")
		}
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
