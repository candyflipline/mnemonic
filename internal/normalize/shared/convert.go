package shared

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ToStr mirrors shared::to_str: absent/null is "", strings pass through, any
// other value is its compact JSON.
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

// ToU64 mirrors shared::to_u64: numbers truncate through u64, numeric strings
// parse, anything else is 0.
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

// ToU32 mirrors shared::to_u32, clamping to the u32 range.
func ToU32(v JSON) uint32 {
	return uint32(min(ToU64(v), uint64(^uint32(0))))
}

// ToU16 mirrors shared::to_u16, clamping to the u16 range.
func ToU16(v JSON) uint16 {
	return uint16(min(ToU32(v), uint64(^uint16(0))))
}

// ToU8Bool mirrors shared::to_u8_bool: bools directly, numbers by non-zero,
// strings "true"/"1" case-insensitively.
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

// SumNumericObject sums the numeric members of a JSON object.
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

// FirstString returns the first present, non-blank string member among keys,
// trimmed.
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

// CanonicalizeModel mirrors shared::canonicalize_model.
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

// TruncateChars keeps at most max runes of s.
func TruncateChars(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}

// ExtractMessageText walks a JSON value collecting human text the way
// shared::extract_message_text does, joining chunks with newlines.
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

// ExtractContentTypes mirrors shared::extract_content_types: the "type" of
// each array member, sorted and deduplicated; empty for non-arrays.
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
