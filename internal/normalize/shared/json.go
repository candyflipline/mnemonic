package shared

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// JSON is one decoded JSON value. Objects decode to map[string]any, arrays to
// []any, numbers to json.Number so integer literals survive re-serialization
// byte-for-byte the way serde_json's u64/i64/f64 do.
type JSON = any

// DecodeJSON parses one JSON document. Trailing non-space bytes are an error.
func DecodeJSON(data []byte) (JSON, error) {
	return decodeDocument(data)
}

// Compact serializes v the way serde_json::to_string does: object keys sorted,
// no whitespace, numbers in their parsed form.
func Compact(v JSON) string {
	bp := compactBufs.Get().(*[]byte)
	b := appendCompact((*bp)[:0], v)
	out := string(b)
	if cap(b) <= 1<<20 {
		*bp = b
		compactBufs.Put(bp)
	}
	return out
}

var compactBufs = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

func appendCompact(b []byte, v JSON) []byte {
	switch t := v.(type) {
	case nil:
		return append(b, "null"...)
	case bool:
		if t {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case string:
		return appendJSONString(b, t)
	case json.Number:
		return append(b, CanonicalNumber(t)...)
	case []any:
		b = append(b, '[')
		for i, item := range t {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendCompact(b, item)
		}
		return append(b, ']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, k)
			b = append(b, ':')
			b = appendCompact(b, t[k])
		}
		return append(b, '}')
	default:
		return append(b, "null"...)
	}
}

// appendJSONString escapes exactly what serde_json escapes: quote, backslash
// and the C0 controls. Everything else, including non-ASCII, passes through
// as UTF-8.
func appendJSONString(b []byte, s string) []byte {
	const ones, highs = 0x0101010101010101, 0x8080808080808080
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		for i+8 <= len(s) {
			x := uint64(s[i]) | uint64(s[i+1])<<8 | uint64(s[i+2])<<16 | uint64(s[i+3])<<24 |
				uint64(s[i+4])<<32 | uint64(s[i+5])<<40 | uint64(s[i+6])<<48 | uint64(s[i+7])<<56
			quote := x ^ (ones * '"')
			slash := x ^ (ones * '\\')
			if ((x-ones*0x20)&^x|(quote-ones)&^quote|(slash-ones)&^slash)&highs != 0 {
				break
			}
			i += 8
		}
		if i >= len(s) {
			break
		}
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		b = append(b, s[start:i]...)
		start = i + 1
		switch {
		case c == '"':
			b = append(b, `\"`...)
		case c == '\\':
			b = append(b, `\\`...)
		case c == '\n':
			b = append(b, `\n`...)
		case c == '\r':
			b = append(b, `\r`...)
		case c == '\t':
			b = append(b, `\t`...)
		case c == '\b':
			b = append(b, `\b`...)
		case c == '\f':
			b = append(b, `\f`...)
		default:
			const hex = "0123456789abcdef"
			b = append(b, `\u00`...)
			b = append(b, hex[c>>4], hex[c&0xf])
		}
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

// CanonicalNumber re-serializes a numeric literal the way serde_json does:
// integers keep their value (so "-0" becomes "0"), everything else goes
// through f64.
func CanonicalNumber(n json.Number) string {
	s := string(n)
	if isIntegerLiteral(s) {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return strconv.FormatInt(i, 10)
		}
		if u, err := strconv.ParseUint(s, 10, 64); err == nil {
			return strconv.FormatUint(u, 10)
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return FormatJSONFloat(f)
}

func isIntegerLiteral(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// FormatJSONFloat renders an f64 the way serde_json's ryu backend does:
// shortest round-trip decimal, fixed notation inside ryu's fixed window and
// exponential (without a plus sign or padded mantissa) outside it.
func FormatJSONFloat(f float64) string {
	abs := math.Abs(f)
	if f != 0 && (abs >= 1e16 || abs < 1e-5) {
		mant, exp, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
		sign := ""
		if exp[0] == '+' || exp[0] == '-' {
			sign, exp = string(exp[0]), exp[1:]
		}
		exp = strings.TrimLeft(exp, "0")
		if exp == "" {
			exp = "0"
		}
		return mant + "e" + sign + exp
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// RustFloatDisplay renders an f64 the way Rust's Display does (no forced
// fractional part); used by the identity hash for native token units.
func RustFloatDisplay(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func Get(v JSON, key string) JSON {
	if m, ok := v.(map[string]any); ok {
		return m[key]
	}
	return nil
}

func Pointer(v JSON, path string) JSON {
	for path != "" {
		if path[0] != '/' {
			return nil
		}
		path = path[1:]
		seg := path
		if i := strings.IndexByte(path, '/'); i >= 0 {
			seg, path = path[:i], path[i:]
		} else {
			path = ""
		}
		v = Get(v, strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~"))
	}
	return v
}

func AsArray(v JSON) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

func AsObject(v JSON) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func AsString(v JSON) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func AsBool(v JSON) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func IsNull(v JSON) bool {
	return v == nil
}
