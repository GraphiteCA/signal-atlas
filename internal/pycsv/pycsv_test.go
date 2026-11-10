package pycsv

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// The cases are run through Python's csv module (test/cases/csv_cases.py) and must split the same.
func TestMatchesPython(t *testing.T) {
	script, _ := filepath.Abs("../../test/cases/csv_cases.py")
	out, err := exec.Command("python", script).Output()
	if err != nil {
		t.Skip("python not available: ", err)
	}
	var ref struct {
		Cases []string     `json:"cases"`
		Out   [][][]string `json:"out"`
	}
	if err := json.Unmarshal(out, &ref); err != nil {
		t.Fatal(err)
	}
	for i, c := range ref.Cases {
		got, err := ParseString(c)
		if err != nil {
			t.Errorf("case %q: %v", c, err)
			continue
		}
		if got == nil {
			got = [][]string{}
		}
		if !reflect.DeepEqual(got, ref.Out[i]) {
			t.Errorf("case %q: got %q, Python %q", c, got, ref.Out[i])
		}
	}
}

func TestFieldLimit(t *testing.T) {
	big := make([]byte, FieldLimit+5)
	for i := range big {
		big[i] = 'x'
	}
	if _, err := ParseString(string(big) + "\n"); err == nil {
		t.Error("a field over the limit must fail")
	}
}
