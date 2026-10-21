package main

import (
	"sort"
)

type faQuery struct {
	Rel             string
	NormalizedPID   string
	NormalizedRoots []string
	DeriveLegacy    bool
	ProjectScope    bool
	StartUnixMs     *int64
	EndUnixMs       *int64
	Tool            string
	Harness         string
	SourceName      string
	MutationsOnly   bool
	MaxRows         int
}

type faTouch struct {
	SessionID     string
	EventUID      string
	ToolCallID    string
	Harness       string
	SourceName    string
	ToolName      string
	ToolPhase     string
	MatchedPath   string
	MatchKind     string
	WorktreeRoot  string
	CWD           string
	EventUnixMs   *int64
	EventOrder    uint64
	TurnSeq       *uint32
	InputPreview  string
	OutputPreview string
}

const faReadToolNames = "'read', 'readfile', 'read_file', 'notebookread', 'notebook_read', 'view', 'cat', 'grep', 'rg', 'glob', 'ls', 'list', 'find'"

var faPathKeys = []string{
	"file_path", "notebook_path", "path", "target_file", "relativeWorkspacePath",
	"relative_workspace_path", "filepath", "file", "filename",
}

func faMergeTouches(touches []faTouch, limit int) []faTouch {
	seen := make(map[string]bool, len(touches))
	merged := make([]faTouch, 0, len(touches))
	for _, t := range touches {
		key := t.SessionID + "\x00" + t.ToolCallID + "\x00" + t.EventUID
		if !seen[key] {
			seen[key] = true
			merged = append(merged, t)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return faTouchLess(&merged[i], &merged[j])
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

func faTouchLess(a, b *faTouch) bool {
	switch {
	case a.EventUnixMs != nil && b.EventUnixMs != nil:
		if *a.EventUnixMs != *b.EventUnixMs {
			return *a.EventUnixMs > *b.EventUnixMs
		}
	case a.EventUnixMs != nil:
		return true
	case b.EventUnixMs != nil:
		return false
	}
	if a.EventOrder != b.EventOrder {
		return a.EventOrder > b.EventOrder
	}
	return a.EventUID > b.EventUID
}

func dedupAdjacent(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
