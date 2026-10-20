// Package primeagent parses prime-agent sources. Port of upstream
// crates/moraine-ingest-core/src/sources/prime_agent.rs onto the shared
// pi_family normalizer; testdata/goldens pins the output.
package primeagent

import (
	"io"
	"slices"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/normalize/pifamily"
	"github.com/candyflipline/mnemonic/internal/normalize/shared"
	"github.com/candyflipline/mnemonic/internal/schema"
)

// subagentSourceName is upstream SUBAGENT_SOURCE_NAME: subagent transcripts
// share the prime-agent harness but carry their own source name.
const subagentSourceName = "prime-agent-subagents"

// Parser parses prime-agent JSONL transcripts.
type Parser struct{}

func (Parser) Harness() string { return "prime-agent" }

// Parse walks the file line by line through the shared JSONL loop.
func (Parser) Parse(r io.Reader, src normalize.SourceMeta) ([]schema.Event, error) {
	return shared.ParseJSONL(r, src, primeSource{})
}

func init() { normalize.Register(Parser{}) }

// primeSource is the prime-agent adapter (IngestSource for PrimeAgent).
type primeSource struct{}

func (primeSource) Harness() string                  { return "prime-agent" }
func (primeSource) DefaultInferenceProvider() string { return "" }

func (primeSource) Preflight(rec shared.JSON) (shared.JSON, bool) { return rec, true }

func (primeSource) SourceMetadata(rec shared.JSON) shared.SourceMetadata {
	// Records the prime gate rejects never reach the pi-family normalizer,
	// so they carry no provider or model fallback either.
	if !primeRecordCanDelegate(rec, shared.ToStr(shared.Get(rec, "type"))) {
		return shared.SourceMetadata{}
	}
	return pifamily.SourceMetadata(rec)
}

func (primeSource) RecordTS(rec shared.JSON) string {
	return shared.ToStr(shared.Get(rec, "timestamp"))
}

func (primeSource) TopType(rec shared.JSON) string { return shared.ToStr(shared.Get(rec, "type")) }

func (primeSource) Cwd(rec shared.JSON) string {
	// jsonl_carries_cwd: every record carries the working directory.
	return shared.ToStr(shared.Get(rec, "cwd"))
}

func (primeSource) SessionID(rec shared.JSON, sctx shared.SourceRecordContext) string {
	if sctx.TopType == "session" {
		if id := shared.ToStr(shared.Get(rec, "id")); id != "" {
			return id
		}
	}
	if sctx.SessionHint == "" {
		return shared.InferSessionIDFromFile(sctx.SourceFile)
	}
	return sctx.SessionHint
}

func (primeSource) Normalize(rec shared.JSON, ctx *shared.Ctx, topType, baseUID, modelHint string) *shared.Partials {
	if !primeRecordCanDelegate(rec, topType) {
		return &shared.Partials{}
	}

	partials := pifamily.NormalizeRecord(rec, ctx, topType, baseUID, modelHint, pifamily.PRIME)

	if ctx.SourceName != subagentSourceName {
		return partials
	}
	for _, row := range partials.Events {
		row.Event.IsSubstream = 1
	}
	// The subagent_parent link row (session records with rlmDepth > 0) is
	// not kept in this port.
	return partials
}

// primeRecordCanDelegate mirrors upstream prime_record_can_delegate: prime
// only hands a record to the pi-family normalizer when it can represent it,
// and hidden custom messages never normalize.
func primeRecordCanDelegate(rec shared.JSON, topType string) bool {
	switch topType {
	case "custom_message":
		// display is optional; only an explicit false hides the record.
		if display, ok := shared.AsBool(shared.Get(rec, "display")); ok && !display {
			return false
		}
		return true
	case "message":
		return primeMessageIsNormalizable(shared.Get(rec, "message"))
	default:
		return true
	}
}

// primeMessageIsNormalizable mirrors upstream prime_message_is_normalizable:
// the roles and content shapes prime-agent sessions actually carry.
func primeMessageIsNormalizable(message shared.JSON) bool {
	if message == nil {
		return false
	}
	switch shared.ToStr(shared.Get(message, "role")) {
	case "user", "toolResult":
		return contentBlocksAreKnown(shared.Get(message, "content"), "text", "image")
	case "assistant":
		return contentBlocksAreKnown(shared.Get(message, "content"), "text", "thinking", "toolCall")
	case "bashExecution", "branchSummary", "compactionSummary":
		return true
	default:
		return false
	}
}

// contentBlocksAreKnown mirrors upstream content_blocks_are_known: scalar or
// absent content is always known; block content must use allowed types only.
func contentBlocksAreKnown(content shared.JSON, allowed ...string) bool {
	switch t := content.(type) {
	case nil, string:
		return true
	case []any:
		for _, block := range t {
			blockType := shared.ToStr(shared.Get(block, "type"))
			if blockType == "" || !slices.Contains(allowed, blockType) {
				return false
			}
		}
		return true
	case map[string]any:
		blockType := shared.ToStr(shared.Get(t, "type"))
		return blockType != "" && slices.Contains(allowed, blockType)
	default:
		return false
	}
}
