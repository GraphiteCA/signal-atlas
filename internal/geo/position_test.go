package geo

import "testing"

func TestZeroPairPolicy(t *testing.T) {
	for _, c := range []struct {
		lat, lon string
		zero     bool
	}{
		{"0", "0", true}, {" -0.000 ", "+0e20", true}, {".0", "0.0", true},
		{"1e-9999", "-0", true}, {"0x0p0", "0", true},
		{"0", "123.4", false}, {"48.4", "0", false}, {"", "0", false},
		{"NaN", "0", false}, {"Inf", "0", false}, {"0.01", "0", false},
	} {
		if got := ZeroPairText(c.lat, c.lon); got != c.zero {
			t.Errorf("%q,%q: got %v", c.lat, c.lon, got)
		}
	}
}
