package shared

import (
	"strings"
)

type member struct {
	key, value string
	start, end int
}

func objectMembers(s string) ([]member, bool) {
	if len(s) < 2 || s[0] != '{' {
		return nil, false
	}
	if s == "{}" {
		return nil, true
	}
	var out []member
	i := 1
	for {
		start := i
		if i >= len(s) || s[i] != '"' {
			return nil, false
		}
		keyEnd, ok := skipString(s, i)
		if !ok || keyEnd >= len(s) || s[keyEnd] != ':' {
			return nil, false
		}
		valueStart := keyEnd + 1
		valueEnd, ok := skipValue(s, valueStart)
		if !ok {
			return nil, false
		}
		out = append(out, member{key: s[i+1 : keyEnd-1], value: s[valueStart:valueEnd], start: start, end: valueEnd})
		if valueEnd >= len(s) {
			return nil, false
		}
		switch s[valueEnd] {
		case ',':
			i = valueEnd + 1
		case '}':
			if valueEnd != len(s)-1 {
				return nil, false
			}
			return out, true
		default:
			return nil, false
		}
	}
}

func skipString(s string, i int) (int, bool) {
	for i++; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1, true
		}
	}
	return 0, false
}

func skipValue(s string, i int) (int, bool) {
	if i >= len(s) {
		return 0, false
	}
	switch s[i] {
	case '"':
		return skipString(s, i)
	case '{', '[':
		depth := 0
		for ; i < len(s); i++ {
			switch s[i] {
			case '"':
				end, ok := skipString(s, i)
				if !ok {
					return 0, false
				}
				i = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
		}
		return 0, false
	default:
		start := i
		for i < len(s) && s[i] != ',' && s[i] != '}' && s[i] != ']' {
			i++
		}
		return i, i > start
	}
}

func joinMembers(s string, members []member, drop map[string]bool, insertKey, insertValue string) string {
	var b strings.Builder
	b.Grow(len(s) + len(insertKey) + len(insertValue) + 4)
	b.WriteByte('{')
	first := true
	write := func(part ...string) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		for _, p := range part {
			b.WriteString(p)
		}
	}
	inserted := insertKey == ""
	for _, m := range members {
		if !inserted && m.key >= insertKey {
			write(`"`, insertKey, `":`, insertValue)
			inserted = true
		}
		if drop[m.key] {
			continue
		}
		write(s[m.start:m.end])
	}
	if !inserted {
		write(`"`, insertKey, `":`, insertValue)
	}
	b.WriteByte('}')
	return b.String()
}

func canonicalMembers(s string) ([]member, bool) {
	members, ok := objectMembers(s)
	if !ok {
		return nil, false
	}
	for _, m := range members {
		if strings.IndexByte(m.key, '\\') >= 0 {
			return nil, false
		}
	}
	return members, true
}

var foldDropped = map[string]bool{"moraine_tool_io": true}
