package primeagent

import (
	"io"
	"slices"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/normalize/pifamily"
	"github.com/candyflipline/mnemonic/internal/normalize/shared"
	"github.com/candyflipline/mnemonic/internal/schema"
)

const subagentSourceName = "prime-agent-subagents"

type Parser struct{}

func (Parser) Harness() string { return "prime-agent" }

func (Parser) Parse(r io.Reader, src normalize.SourceMeta, from normalize.State) ([]schema.Event, normalize.State, error) {
	return shared.ParseJSONL(r, src, from, primeSource{})
}

func init() { normalize.Register(Parser{}) }

type primeSource struct{}

func (primeSource) Harness() string                  { return "prime-agent" }
func (primeSource) DefaultInferenceProvider() string { return "" }

func (primeSource) Preflight(rec shared.JSON) (shared.JSON, bool) { return rec, true }

func (primeSource) SourceMetadata(rec shared.JSON) shared.SourceMetadata {
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
	return partials
}

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
