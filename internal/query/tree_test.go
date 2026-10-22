package query

import (
	"encoding/json"
	"testing"
)

// TestBuildTreeKeepsCycles pins that spans on a parent cycle still reach the
// tree, each once, with the totals summed over the roots the tree shows.
func TestBuildTreeKeepsCycles(t *testing.T) {
	span := func(id, parent string, in uint64) *traceSpan {
		return &traceSpan{SpanID: id, parent: parent, Tokens: traceTokens{Input: in}, Children: []*traceSpan{}}
	}
	shape := func(roots []*traceSpan) string {
		var walk func(s *traceSpan) any
		walk = func(s *traceSpan) any {
			kids := []any{}
			for _, c := range s.Children {
				kids = append(kids, walk(c))
			}
			return map[string]any{s.SpanID: kids}
		}
		out := []any{}
		for _, r := range roots {
			out = append(out, walk(r))
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	for name, tc := range map[string]struct {
		spans []*traceSpan
		tree  string
		total traceTotals
	}{
		"ordinary": {
			[]*traceSpan{span("r", "", 10), span("a", "r", 4), span("b", "r", 6)},
			`[{"r":[{"a":[]},{"b":[]}]}]`,
			traceTotals{Spans: 3, Tokens: traceTokens{Input: 10}},
		},
		"two-cycle": {
			[]*traceSpan{span("r", "", 10), span("a", "b", 5), span("b", "a", 3)},
			`[{"r":[]},{"a":[{"b":[]}]}]`,
			traceTotals{Spans: 3, Tokens: traceTokens{Input: 15}},
		},
		"cycle-with-a-hanger-on": {
			[]*traceSpan{span("c", "a", 1), span("a", "b", 5), span("b", "a", 3)},
			`[{"c":[]},{"a":[{"b":[]}]}]`,
			traceTotals{Spans: 3, Tokens: traceTokens{Input: 6}},
		},
	} {
		roots, totals := buildTree(tc.spans)
		if got := shape(roots); got != tc.tree || totals != tc.total {
			t.Errorf("%s: tree %s totals %+v, want %s %+v", name, got, totals, tc.tree, tc.total)
		}
	}
}
