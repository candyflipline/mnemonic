package otlp

import (
	"slices"
	"strings"
)

func shortName(name string) string {
	for _, p := range [...]string{"claude_code.", "codex."} {
		if s, ok := strings.CutPrefix(name, p); ok {
			return s
		}
	}
	return name
}

func (g Gate) drops(key, record string) bool {
	if len(g.In) > 0 && !slices.Contains(g.In, shortName(record)) {
		return false
	}
	switch {
	case g.Name == "*":
		return key != EventNameKey
	case g.Prefix:
		return key == g.Name || strings.HasPrefix(key, g.Name+".")
	}
	return key == g.Name
}

// gate removes the content attributes of the record named record from attrs.
func gate(attrs map[string]any, record string) {
	for key := range attrs {
		for _, g := range Gates {
			if g.drops(key, record) {
				delete(attrs, key)
				break
			}
		}
	}
}
