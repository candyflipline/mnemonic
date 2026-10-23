package shared

import (
	"strings"

	"github.com/candyflipline/mnemonic/internal/repo"
)

var fileAttentionPathKeys = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range repo.PathKeys {
		m[k] = true
	}
	return m
}()

func ToolFileAttentionFields(roots repo.Book, cwd, toolPhase, inputJSON string, at uint64) repo.Fields {
	if toolPhase != "request" {
		return repo.Fields{}
	}
	input, err := DecodeJSON([]byte(inputJSON))
	if err != nil {
		return repo.Fields{}
	}
	var candidates []string
	collectStructuredPathCandidates(input, &candidates)
	if len(candidates) != 1 {
		return repo.Fields{}
	}
	path := strings.TrimSpace(candidates[0])
	if !isSinglePathCandidate(path) {
		return repo.Fields{}
	}
	fields, ok := roots.Tool(cwd, path, at)
	if !ok {
		return repo.Fields{}
	}
	return fields
}

func collectStructuredPathCandidates(value JSON, out *[]string) {
	switch t := value.(type) {
	case map[string]any:
		for key, item := range t {
			if fileAttentionPathKeys[key] {
				collectPathValues(item, out)
			}
			collectStructuredPathCandidates(item, out)
		}
	case []any:
		for _, item := range t {
			collectStructuredPathCandidates(item, out)
		}
	}
}

func collectPathValues(value JSON, out *[]string) {
	switch t := value.(type) {
	case string:
		if strings.TrimSpace(t) != "" {
			*out = append(*out, t)
		}
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				*out = append(*out, s)
			}
		}
	}
}

func isSinglePathCandidate(path string) bool {
	if path == "" || strings.ContainsRune(path, 0) {
		return false
	}
	return !strings.ContainsAny(path, "\n\r;|&`")
}
