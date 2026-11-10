// Package compare is Wardrive Compare's analysis (the Web Worker inside
// web/compare.html) in Go, line for line, so the page shows the same numbers
// whether its own worker runs it (in a browser) or the app's engine does: same parsing rules, same typed-array
// rounding (Int8 signal, Float32 speed), same groupings, same output object. What changes is the
// scale: files parse on every core, rows live in compact columns, and there is no tab memory limit.
package compare

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// jsNumber is JavaScript's Number(string) (the unary + the worker uses).
func jsNumber(s string, present bool) float64 {
	if !present {
		return math.NaN() // +undefined
	}
	t := strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '\uFEFF' })
	if t == "" {
		return 0
	}
	switch t {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if len(t) > 2 && t[0] == '0' {
		base := 0
		switch t[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base > 0 {
			v, err := strconv.ParseUint(t[2:], base, 64)
			if err != nil {
				return math.NaN()
			}
			return float64(v)
		}
	}
	if !decRe.MatchString(t) {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		var ne *strconv.NumError
		if ok := asNumErr(err, &ne); ok && ne.Err == strconv.ErrRange {
			return v
		}
		return math.NaN()
	}
	return v
}

var decRe = regexp.MustCompile(`^[+-]?(?:\d+\.?\d*(?:[eE][+-]?\d+)?|\.\d+(?:[eE][+-]?\d+)?)$`)

func asNumErr(err error, out **strconv.NumError) bool {
	ne, ok := err.(*strconv.NumError)
	if ok {
		*out = ne
	}
	return ok
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// toUint is ToUint8/16/32: truncate, then wrap modulo 2^bits (NaN and infinities give 0).
func toUint(x float64, bits uint) uint64 {
	if !finite(x) {
		return 0
	}
	t := math.Trunc(x)
	m := math.Mod(t, math.Pow(2, float64(bits)))
	if m < 0 {
		m += math.Pow(2, float64(bits))
	}
	return uint64(m)
}

// toInt is ToInt8/16: as toUint, then two's complement.
func toInt(x float64, bits uint) int64 {
	u := toUint(x, bits)
	if u >= 1<<(bits-1) {
		return int64(u) - 1<<bits
	}
	return int64(u)
}

// jsKey is JavaScript's String(number) for the integers used in drive keys.
func jsKey(x float64) string {
	if x == math.Trunc(x) && math.Abs(x) < 1e21 {
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}

// Num is a number that serialises as JavaScript's JSON.stringify does: NaN and infinities as null.
type Num float64

// MarshalJSON writes the number as JSON.stringify does.
func (n Num) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if !finite(f) {
		return []byte("null"), nil
	}
	return json.Marshal(f)
}

// num wraps a float that may be null (nil stays nil).
func num(f float64) Num { return Num(f) }

// orNull is the JS pattern cond ? value : null.
func orNull(ok bool, f float64) any {
	if !ok {
		return nil
	}
	return Num(f)
}
