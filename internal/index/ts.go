package index

import (
	"strings"
	"time"
)

// duckTimestampUs approximates DuckDB's try_cast(s AS TIMESTAMPTZ) with
// TimeZone=UTC: ISO dates with an optional time, fraction and offset.
func duckTimestampUs(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 10 {
		return 0, false
	}
	num := func(t string) (int, bool) {
		if t == "" {
			return 0, false
		}
		n := 0
		for _, c := range t {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		return n, true
	}
	y, ok1 := num(s[0:4])
	mo, ok2 := num(s[5:7])
	d, ok3 := num(s[8:10])
	if !ok1 || !ok2 || !ok3 || s[4] != '-' || s[7] != '-' {
		return 0, false
	}
	rest := s[10:]
	var h, mi, sec, us int
	if rest != "" && (rest[0] == 'T' || rest[0] == ' ' || rest[0] == 't') {
		rest = rest[1:]
		if len(rest) < 5 || rest[2] != ':' {
			return 0, false
		}
		var ok bool
		if h, ok = num(rest[0:2]); !ok {
			return 0, false
		}
		if mi, ok = num(rest[3:5]); !ok {
			return 0, false
		}
		rest = rest[5:]
		if len(rest) >= 3 && rest[0] == ':' {
			if sec, ok = num(rest[1:3]); !ok {
				return 0, false
			}
			rest = rest[3:]
			if len(rest) > 0 && rest[0] == '.' {
				rest = rest[1:]
				i := 0
				scale := 100000
				for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
					if scale > 0 {
						us += int(rest[i]-'0') * scale
						scale /= 10
					}
					i++
				}
				if i == 0 {
					return 0, false
				}
				rest = rest[i:]
			}
		}
	}
	rest = strings.TrimSpace(rest)
	offset := 0
	switch {
	case rest == "" || rest == "Z" || rest == "z" || rest == "UTC":
	case rest[0] == '+' || rest[0] == '-':
		sign := 1
		if rest[0] == '-' {
			sign = -1
		}
		z := strings.ReplaceAll(rest[1:], ":", "")
		var oh, om int
		var ok bool
		switch len(z) {
		case 2:
			oh, ok = num(z)
		case 4:
			oh, ok = num(z[:2])
			if ok {
				om, ok = num(z[2:])
			}
		default:
			return 0, false
		}
		if !ok {
			return 0, false
		}
		offset = sign * (oh*3600 + om*60)
	default:
		return 0, false
	}
	if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 24 || mi > 59 || sec > 60 {
		return 0, false
	}
	t := time.Date(y, time.Month(mo), d, h, mi, sec, 0, time.UTC)
	if t.Day() != d {
		return 0, false
	}
	return t.Unix()*1e6 - int64(offset)*1e6 + int64(us), true
}
