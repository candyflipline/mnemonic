package shared

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func ToStr(v JSON) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return Compact(t)
	}
}

func ToU64(v JSON) uint64 {
	switch t := v.(type) {
	case json.Number:
		if u, err := strconv.ParseUint(string(t), 10, 64); err == nil {
			return u
		}
		return 0
	case string:
		if u, err := strconv.ParseUint(strings.TrimSpace(t), 10, 64); err == nil {
			return u
		}
		return 0
	default:
		return 0
	}
}

func ToU32(v JSON) uint32 {
	return uint32(min(ToU64(v), uint64(^uint32(0))))
}

func ToU16(v JSON) uint16 {
	return uint16(min(uint64(ToU32(v)), uint64(^uint16(0))))
}

func ToU8Bool(v JSON) uint8 {
	switch t := v.(type) {
	case bool:
		if t {
			return 1
		}
		return 0
	case json.Number:
		i, err := strconv.ParseInt(string(t), 10, 64)
		if err == nil && i != 0 {
			return 1
		}
		return 0
	case string:
		switch strings.ToLower(t) {
		case "true", "1":
			return 1
		default:
			return 0
		}
	default:
		return 0
	}
}

func SumNumericObject(v JSON) uint64 {
	m, ok := AsObject(v)
	if !ok {
		return 0
	}
	var sum uint64
	for _, val := range m {
		sum += ToU64(val)
	}
	return sum
}

func FirstString(v JSON, keys ...string) (string, bool) {
	for _, key := range keys {
		if s, ok := AsString(Get(v, key)); ok {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				return trimmed, true
			}
		}
	}
	return "", false
}

func CanonicalizeModel(harness, rawModel string) string {
	model := strings.ToLower(strings.TrimSpace(rawModel))
	if model == "" {
		return ""
	}
	model = strings.ReplaceAll(model, " ", "-")
	if harness == "codex" && model == "codex" {
		return "gpt-5.3-codex-xhigh"
	}
	return model
}

const (
	textLimit    = 200_000
	previewLimit = 320
)

func TruncateChars(s string, max int) string {
	if len(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			if prefix := s[:i]; utf8.ValidString(prefix) {
				return prefix
			}
			return string([]rune(s)[:max])
		}
		n++
	}
	return s
}

func ExtractMessageText(content JSON) string {
	var chunks []string
	walkMessageText(content, &chunks)
	return TruncateChars(strings.Join(chunks, "\n"), textLimit)
}

func walkMessageText(node JSON, out *[]string) {
	switch t := node.(type) {
	case string:
		if strings.TrimSpace(t) != "" {
			*out = append(*out, t)
		}
	case []any:
		for _, item := range t {
			walkMessageText(item, out)
		}
	case map[string]any:
		for _, key := range []string{"text", "message", "output", "thinking", "summary"} {
			if s, ok := t[key].(string); ok && strings.TrimSpace(s) != "" {
				*out = append(*out, s)
			}
		}
		for _, key := range []string{"content", "text_elements", "input"} {
			if val, present := t[key]; present {
				walkMessageText(val, out)
			}
		}
	}
}

func ExtractContentTypes(content JSON) []string {
	items, ok := AsArray(content)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range items {
		if t, ok := AsString(Get(item, "type")); ok && t != "" {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	out = slicesCompact(out)
	return out
}

func slicesCompact(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
