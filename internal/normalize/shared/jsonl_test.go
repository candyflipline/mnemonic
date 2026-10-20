package shared_test

import (
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/normalize/claudecode"
)

func TestParseJSONLSkippedLineKeepsOffsets(t *testing.T) {
	input := `{"timestamp":"2026-01-01T00:00:00.000Z","type":"system","subtype":"init"}
not json
{"timestamp":"2026-01-01T00:00:01.000Z","type":"system"}`

	events, err := claudecode.Parser{}.Parse(strings.NewReader(input), normalize.SourceMeta{
		SourceName: "golden-claude-code", Harness: "claude-code",
		SourceFile: "/fixtures/claude-code/session.jsonl", Generation: 1, Inode: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	last := events[len(events)-1]
	if last.SourceLineNo != 3 || last.SourceOffset != 83 {
		t.Fatalf("last event source_line_no=%d source_offset=%d, want 3 and 83",
			last.SourceLineNo, last.SourceOffset)
	}
	if events[0].SourceOffset != 0 {
		t.Fatalf("first event source_offset=%d, want 0", events[0].SourceOffset)
	}
}
