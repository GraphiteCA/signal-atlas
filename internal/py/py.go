// Package py reproduces the Python behaviour the Signal Atlas port has to match exactly.
//
// Signal Atlas (Graphite's Python, python) keys its cache and its shared fits on text Python
// builds: json.dumps with its own spacing and escaping, float() and int() parsing, sorted() in
// code-point order, parse_qs. A store written by the Python and one written here must use
// the same keys, so either can open the other's. Values follow Python's types: int64 is a Python
// int, float64 a Python float (json.dumps prints it as -150.0, never -150).
package py

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ValueError is Python's ValueError: a bad request, reported to the page as its message.
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// Errorf makes a ValueError.
func Errorf(format string, a ...any) error { return &ValueError{fmt.Sprintf(format, a...)} }

// IsValueError reports whether err is (or wraps) a ValueError.
func IsValueError(err error) bool { var v *ValueError; return errors.As(err, &v) }

// FloatRepr is Python's repr(float): shortest round-trip digits, an exponent when it is < -4 or
// >= 16, spelled 1e-05 / 1e+16, and a trailing .0 on whole numbers.
func FloatRepr(x float64) string {
	switch {
	case math.IsNaN(x):
		return "NaN"
	case math.IsInf(x, 1):
		return "Infinity"
	case math.IsInf(x, -1):
		return "-Infinity"
	case x == 0:
		if math.Signbit(x) {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(x, 'e', -1, 64) // d.dddde±nn
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	i := strings.IndexByte(e, 'e')
	mant, exp := e[:i], e[i+1:]
	n, _ := strconv.Atoi(exp)
	digits := strings.Replace(mant, ".", "", 1)
	if n < -4 || n >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		a, s := n, "+"
		if a < 0 {
			a, s = -a, "-"
		}
		return fmt.Sprintf("%s%se%s%02d", sign, m, s, a)
	}
	if n < 0 {
		return sign + "0." + strings.Repeat("0", -n-1) + digits
	}
	if len(digits) <= n+1 {
		return sign + digits + strings.Repeat("0", n+1-len(digits)) + ".0"
	}
	return sign + digits[:n+1] + "." + digits[n+1:]
}

func jsonString(b *strings.Builder, s string, ensureASCII bool) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(b, `\u%04x`, r)
			case ensureASCII && r > 0x7e:
				if r > 0xffff {
					r -= 0x10000
					fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10)&0x3ff, 0xdc00+r&0x3ff)
				} else {
					fmt.Fprintf(b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// DumpOpts are json.dumps' keyword arguments that matter here.
type DumpOpts struct {
	Compact       bool // separators=(",", ":"); default (", ", ": ")
	NoEnsureASCII bool // ensure_ascii=False
	AllowNaN      bool
}

// Raw is JSON text inserted as it is.
type Raw string

// Dumps is json.dumps. Maps print with sorted keys (Go maps have no order); the Python dicts whose
// text is hashed are either sort_keys=True or lists, so the hashed text is the same.
func Dumps(v any, o DumpOpts) (string, error) {
	var b strings.Builder
	item, key := ", ", ": "
	if o.Compact {
		item, key = ",", ":"
	}
	var enc func(v any) error
	list := func(n int, at func(i int) error) error {
		b.WriteByte('[')
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(item)
			}
			if err := at(i); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	}
	enc = func(v any) error {
		switch x := v.(type) {
		case nil:
			b.WriteString("null")
		case Raw:
			b.WriteString(string(x))
		case bool:
			if x {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		case int:
			b.WriteString(strconv.Itoa(x))
		case int64:
			b.WriteString(strconv.FormatInt(x, 10))
		case float64:
			if (math.IsNaN(x) || math.IsInf(x, 0)) && !o.AllowNaN {
				return Errorf("Out of range float values are not JSON compliant: %s", FloatRepr(x))
			}
			b.WriteString(FloatRepr(x))
		case string:
			jsonString(&b, x, !o.NoEnsureASCII)
		case []string:
			return list(len(x), func(i int) error { jsonString(&b, x[i], !o.NoEnsureASCII); return nil })
		case []any:
			return list(len(x), func(i int) error { return enc(x[i]) })
		case []map[string]any:
			return list(len(x), func(i int) error { return enc(x[i]) })
		case []float64:
			return list(len(x), func(i int) error { return enc(x[i]) })
		case [][]float64:
			return list(len(x), func(i int) error { return enc(x[i]) })
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					b.WriteString(item)
				}
				jsonString(&b, k, !o.NoEnsureASCII)
				b.WriteString(key)
				if err := enc(x[k]); err != nil {
					return err
				}
			}
			b.WriteByte('}')
		case Params:
			return enc(map[string][]string(x))
		case map[string][]string:
			m := make(map[string]any, len(x))
			for k, s := range x {
				m[k] = s
			}
			return enc(m)
		default:
			return fmt.Errorf("Object of type %T is not JSON serializable", v)
		}
		return nil
	}
	if err := enc(v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// MustDumps is Dumps for values known to be serialisable.
func MustDumps(v any, o DumpOpts) string {
	s, err := Dumps(v, o)
	if err != nil {
		panic(err)
	}
	return s
}

// ReprStr is repr() of a str, for error messages.
func ReprStr(s string) string {
	q := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteRune(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == q:
			b.WriteRune('\\')
			b.WriteRune(q)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteRune(q)
	return b.String()
}

// Strip is str.strip(): Python's whitespace.
func Strip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) || r == 0x85 })
}

var (
	floatRe   = regexp.MustCompile(`^[+-]?(?:(?:\d(?:_?\d)*)(?:\.(?:\d(?:_?\d)*)?)?|\.\d(?:_?\d)*)(?:[eE][+-]?\d(?:_?\d)*)?$`)
	specialRe = regexp.MustCompile(`(?i)^([+-]?)(inf|infinity|nan)$`)
	intRe     = regexp.MustCompile(`^[+-]?\d(?:_?\d)*$`)
)

// Float is float(str).
func Float(s string) (float64, error) {
	t := Strip(s)
	if floatRe.MatchString(t) {
		v, err := strconv.ParseFloat(strings.ReplaceAll(t, "_", ""), 64)
		if err != nil {
			var ne *strconv.NumError
			if errors.As(err, &ne) && ne.Err == strconv.ErrRange {
				return v, nil // Python gives inf / 0.0
			}
			return 0, Errorf("could not convert string to float: %s", ReprStr(s))
		}
		return v, nil
	}
	if m := specialRe.FindStringSubmatch(t); m != nil {
		if strings.EqualFold(m[2], "nan") {
			return math.NaN(), nil
		}
		if m[1] == "-" {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	}
	return 0, Errorf("could not convert string to float: %s", ReprStr(s))
}

// Int is int(str).
func Int(s string) (int64, error) {
	t := Strip(s)
	if !intRe.MatchString(t) {
		return 0, Errorf("invalid literal for int() with base 10: %s", ReprStr(s))
	}
	v, err := strconv.ParseInt(strings.ReplaceAll(t, "_", ""), 10, 64)
	if err != nil {
		return 0, Errorf("invalid literal for int() with base 10: %s", ReprStr(s))
	}
	return v, nil
}

// Str is str() of a value read from SQLite.
func Str(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return FloatRepr(x)
	case string:
		return x
	case []byte:
		return "b" + ReprStr(string(x))
	}
	return fmt.Sprint(v)
}

// Params is parse_qs output: name -> values.
type Params map[string][]string

// Get is params.get(key, dflt).
func (p Params) Get(key string, dflt []string) []string {
	if v, ok := p[key]; ok {
		return v
	}
	return dflt
}

// First is params.get(key, [dflt])[0].
func (p Params) First(key, dflt string) string {
	v, ok := p[key]
	if !ok {
		return dflt
	}
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// Is reports params.get(key) == [value].
func (p Params) Is(key, value string) bool {
	v, ok := p[key]
	return ok && len(v) == 1 && v[0] == value
}

// Has reports a non-empty params[key] (the truthiness of Python's list).
func (p Params) Has(key string) bool { return len(p[key]) > 0 }

// With is {**params, key: values}.
func (p Params) With(kv ...any) Params {
	o := Params{}
	for k, v := range p {
		o[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		o[kv[i].(string)] = kv[i+1].([]string)
	}
	return o
}

// Unquote is urllib.parse.unquote(s, errors='replace').
func Unquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			v, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			b = append(b, byte(v))
			i += 3
			continue
		}
		b = append(b, s[i])
		i++
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// ParseQs is urllib.parse.parse_qs(query, keep_blank_values=True).
func ParseQs(query string) Params {
	out := Params{}
	for _, part := range strings.Split(query, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		k = Unquote(strings.ReplaceAll(k, "+", " "))
		v = Unquote(strings.ReplaceAll(v, "+", " "))
		out[k] = append(out[k], v)
	}
	return out
}

// UTCNowISO is datetime.now(timezone.utc).isoformat().
func UTCNowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000") + "+00:00" }

// UUIDHex is uuid.uuid4().hex.
func UUIDHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// SHA256Hex is hashlib.sha256(text.encode()).hexdigest().
func SHA256Hex(text string) string { h := sha256.Sum256([]byte(text)); return hex.EncodeToString(h[:]) }

func validCalendar(y, m, d int) bool {
	if y < 1 || m < 1 || m > 12 || d < 1 {
		return false
	}
	dim := []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[m-1]
	if m == 2 && ((y%4 == 0 && y%100 != 0) || y%400 == 0) {
		dim = 29
	}
	return d <= dim
}

var (
	isoDay  = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	isoDay2 = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})$`)
	isoWeek = regexp.MustCompile(`^(\d{4})-?W(\d{2})(?:-?([1-7]))?$`)
)

// ISODateKind tells date.fromisoformat(v).isoformat() == v apart: "ok", "format" (a valid ISO
// date not spelled YYYY-MM-DD) or "invalid".
func ISODateKind(v string) string {
	at := func(m []string, i int) int { n, _ := strconv.Atoi(m[i]); return n }
	if m := isoDay.FindStringSubmatch(v); m != nil {
		if validCalendar(at(m, 1), at(m, 2), at(m, 3)) {
			return "ok"
		}
		return "invalid"
	}
	if m := isoDay2.FindStringSubmatch(v); m != nil {
		if validCalendar(at(m, 1), at(m, 2), at(m, 3)) {
			return "format"
		}
		return "invalid"
	}
	if m := isoWeek.FindStringSubmatch(v); m != nil && at(m, 1) >= 1 && at(m, 2) >= 1 && at(m, 2) <= 53 {
		return "format"
	}
	return "invalid"
}

// Head is s[:n] by code point.
func Head(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}
