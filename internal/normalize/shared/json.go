package shared

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// JSON is one decoded JSON value. Objects decode to map[string]any, arrays to
// []any, numbers to json.Number so integer literals survive re-serialization
// byte-for-byte the way serde_json's u64/i64/f64 do.
type JSON = any

// DecodeJSON parses one JSON document. Trailing non-space bytes are an error,
// mirroring serde_json::from_str.
func DecodeJSON(data []byte) (JSON, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v JSON
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var trailing JSON
	if err := dec.Decode(&trailing); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, io.ErrUnexpectedEOF
	}
	return v, nil
}

// Compact serializes v the way serde_json::to_string does: object keys sorted,
// no whitespace, numbers in their parsed form.
func Compact(v JSON) string {
	var b strings.Builder
	writeCompact(&b, v)
	return b.String()
}

func writeCompact(b *strings.Builder, v JSON) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeJSONString(b, t)
	case json.Number:
		b.WriteString(CanonicalNumber(t))
	case []any:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCompact(b, item)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, k)
			b.WriteByte(':')
			writeCompact(b, t[k])
		}
		b.WriteByte('}')
	default:
		b.WriteString("null")
	}
}

// writeJSONString escapes exactly what serde_json escapes: quote, backslash
// and the C0 controls. Everything else, including non-ASCII, passes through
// as UTF-8.
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c < 0x20:
			const hex = "0123456789abcdef"
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
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

// Get returns v[key], or nil when v is not an object or lacks the key.
func Get(v JSON, key string) JSON {
	if m, ok := v.(map[string]any); ok {
		return m[key]
	}
	return nil
}

// Pointer walks a slash-separated path like serde_json's Value::pointer.
func Pointer(v JSON, path string) JSON {
	if path == "" {
		return v
	}
	if path[0] != '/' {
		return nil
	}
	return Pointer(Get(v, path[1:]), path[1:])
}

// AsArray returns the underlying slice when v is an array.
func AsArray(v JSON) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

// AsObject returns the underlying map when v is an object.
func AsObject(v JSON) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// AsString returns the underlying string when v is a JSON string.
func AsString(v JSON) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// AsBool returns the underlying bool when v is a JSON bool.
func AsBool(v JSON) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// IsNull reports whether v is absent or JSON null.
func IsNull(v JSON) bool {
	return v == nil
}
