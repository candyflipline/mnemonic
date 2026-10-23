package shared

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const maxDecodeDepth = 10000

var errSyntax = errors.New("json: invalid syntax")

type decoder struct {
	s     string
	i     int
	depth int
	buf   []byte
}

func decodeDocument(data []byte) (JSON, error) {
	d := decoder{s: string(data)}
	d.space()
	v, err := d.value()
	if err != nil {
		return nil, err
	}
	d.space()
	if d.i != len(d.s) {
		return nil, errSyntax
	}
	return v, nil
}

func (d *decoder) space() {
	for d.i < len(d.s) {
		switch d.s[d.i] {
		case ' ', '\t', '\n', '\r':
			d.i++
		default:
			return
		}
	}
}

func (d *decoder) value() (JSON, error) {
	if d.i >= len(d.s) {
		return nil, errSyntax
	}
	switch c := d.s[d.i]; {
	case c == '{':
		return d.object()
	case c == '[':
		return d.array()
	case c == '"':
		return d.str()
	case c == '-' || (c >= '0' && c <= '9'):
		return d.number()
	case c == 't':
		return true, d.literal("true")
	case c == 'f':
		return false, d.literal("false")
	case c == 'n':
		return nil, d.literal("null")
	default:
		return nil, errSyntax
	}
}

func (d *decoder) literal(word string) error {
	if !strings.HasPrefix(d.s[d.i:], word) {
		return errSyntax
	}
	d.i += len(word)
	return nil
}

func (d *decoder) enter() error {
	d.depth++
	if d.depth > maxDecodeDepth {
		return errSyntax
	}
	return nil
}

func (d *decoder) object() (JSON, error) {
	if err := d.enter(); err != nil {
		return nil, err
	}
	d.i++
	m := map[string]any{}
	d.space()
	if d.i < len(d.s) && d.s[d.i] == '}' {
		d.i++
		d.depth--
		return m, nil
	}
	for {
		if d.i >= len(d.s) || d.s[d.i] != '"' {
			return nil, errSyntax
		}
		key, err := d.str()
		if err != nil {
			return nil, err
		}
		d.space()
		if d.i >= len(d.s) || d.s[d.i] != ':' {
			return nil, errSyntax
		}
		d.i++
		d.space()
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		m[key] = v
		d.space()
		if d.i >= len(d.s) {
			return nil, errSyntax
		}
		switch d.s[d.i] {
		case ',':
			d.i++
			d.space()
		case '}':
			d.i++
			d.depth--
			return m, nil
		default:
			return nil, errSyntax
		}
	}
}

func (d *decoder) array() (JSON, error) {
	if err := d.enter(); err != nil {
		return nil, err
	}
	d.i++
	a := make([]any, 0)
	d.space()
	if d.i < len(d.s) && d.s[d.i] == ']' {
		d.i++
		d.depth--
		return a, nil
	}
	for {
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		a = append(a, v)
		d.space()
		if d.i >= len(d.s) {
			return nil, errSyntax
		}
		switch d.s[d.i] {
		case ',':
			d.i++
			d.space()
		case ']':
			d.i++
			d.depth--
			return a, nil
		default:
			return nil, errSyntax
		}
	}
}

func (d *decoder) number() (JSON, error) {
	s, start := d.s, d.i
	i := start
	if s[i] == '-' {
		i++
	}
	if i >= len(s) {
		return nil, errSyntax
	}
	switch {
	case s[i] == '0':
		i++
	case s[i] >= '1' && s[i] <= '9':
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return nil, errSyntax
	}
	if i < len(s) && s[i] == '.' {
		i++
		if i >= len(s) || s[i] < '0' || s[i] > '9' {
			return nil, errSyntax
		}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if i >= len(s) || s[i] < '0' || s[i] > '9' {
			return nil, errSyntax
		}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	d.i = i
	return json.Number(s[start:i]), nil
}

func (d *decoder) str() (string, error) {
	s := d.s
	start := d.i + 1
	i := start
	for i < len(s) {
		c := s[i]
		if c == '"' {
			d.i = i + 1
			return s[start:i], nil
		}
		if c == '\\' || c < 0x20 {
			break
		}
		if c < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			break
		}
		i += size
	}
	b := append(d.buf[:0], s[start:i]...)
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			d.i = i + 1
			d.buf = b
			return string(b), nil
		case c < 0x20:
			return "", errSyntax
		case c == '\\':
			i++
			if i >= len(s) {
				return "", errSyntax
			}
			switch s[i] {
			case '"', '\\', '/':
				b = append(b, s[i])
				i++
			case 'b':
				b = append(b, '\b')
				i++
			case 'f':
				b = append(b, '\f')
				i++
			case 'n':
				b = append(b, '\n')
				i++
			case 'r':
				b = append(b, '\r')
				i++
			case 't':
				b = append(b, '\t')
				i++
			case 'u':
				rr := hex4(s, i+1)
				if rr < 0 {
					return "", errSyntax
				}
				i += 5
				if utf16.IsSurrogate(rr) {
					rr1 := rune(-1)
					if i+1 < len(s) && s[i] == '\\' && s[i+1] == 'u' {
						rr1 = hex4(s, i+2)
					}
					if dec := utf16.DecodeRune(rr, rr1); dec != utf8.RuneError {
						i += 6
						b = utf8.AppendRune(b, dec)
						continue
					}
					rr = utf8.RuneError
				}
				b = utf8.AppendRune(b, rr)
			default:
				return "", errSyntax
			}
		case c < utf8.RuneSelf:
			b = append(b, c)
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				b = utf8.AppendRune(b, utf8.RuneError)
			} else {
				b = append(b, s[i:i+size]...)
			}
			i += size
		}
	}
	return "", errSyntax
}

func hex4(s string, i int) rune {
	if i+4 > len(s) {
		return -1
	}
	var r rune
	for _, c := range []byte(s[i : i+4]) {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			c = c - 'A' + 10
		default:
			return -1
		}
		r = r*16 + rune(c)
	}
	return r
}
