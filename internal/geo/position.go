// Package geo holds the import coordinate policy shared by the survey and archive.
package geo

import (
	"strconv"
	"strings"
)

// ZeroPair excludes only two numeric zero coordinates, including signed zero.
// A missing/invalid coordinate and a single zero are not a zero pair.
func ZeroPair(lat, lon float64) bool { return lat == 0 && lon == 0 }
func ZeroPairText(lat, lon string) bool {
	lat, lon = strings.TrimSpace(lat), strings.TrimSpace(lon)
	// Normal GPS text begins with a nonzero digit. Avoid parsing it twice in
	// the import hot path; exponents still need parsing because of underflow.
	possible := func(s string) bool {
		if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
			s = s[1:]
		}
		return len(s) > 0 && (s[0] == '0' || s[0] == '.' || strings.ContainsAny(s, "eEpP"))
	}
	if !possible(lat) || !possible(lon) {
		return false
	}
	a, e1 := strconv.ParseFloat(lat, 64)
	if e1 != nil || a != 0 {
		return false
	}
	b, e2 := strconv.ParseFloat(lon, 64)
	return e2 == nil && b == 0
}
