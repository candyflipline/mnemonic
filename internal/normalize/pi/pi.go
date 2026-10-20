// Package pi parses pi-coding-agent sources. Port of upstream
// upstream ingest-core/src/sources/pi.rs onto the shared pi_family
// normalizer; testdata/goldens pins the output.
package pi

import (
	"io"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/normalize/pifamily"
	"github.com/candyflipline/mnemonic/internal/normalize/shared"
	"github.com/candyflipline/mnemonic/internal/schema"
)

// Parser parses pi-coding-agent JSONL transcripts.
type Parser struct{}

func (Parser) Harness() string { return "pi-coding-agent" }

// Parse walks the file line by line through the shared JSONL loop.
func (Parser) Parse(r io.Reader, src normalize.SourceMeta) ([]schema.Event, error) {
	return shared.ParseJSONL(r, src, piSource{})
}

func init() { normalize.Register(Parser{}) }

// piSource is the pi-coding-agent adapter (IngestSource for PiCodingAgent).
type piSource struct{}

func (piSource) Harness() string                  { return "pi-coding-agent" }
func (piSource) DefaultInferenceProvider() string { return "" }

func (piSource) Preflight(rec shared.JSON) (shared.JSON, bool) { return rec, true }

func (piSource) SourceMetadata(rec shared.JSON) shared.SourceMetadata {
	return pifamily.SourceMetadata(rec)
}

func (piSource) RecordTS(rec shared.JSON) string {
	return shared.ToStr(shared.Get(rec, "timestamp"))
}

func (piSource) TopType(rec shared.JSON) string { return shared.ToStr(shared.Get(rec, "type")) }

func (piSource) Cwd(rec shared.JSON) string {
	// Only the session header record carries the working directory; later
	// records inherit it through the normalizer's cwd hint.
	return shared.ToStr(shared.Get(rec, "cwd"))
}

func (piSource) SessionID(rec shared.JSON, sctx shared.SourceRecordContext) string {
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

func (piSource) Normalize(rec shared.JSON, ctx *shared.Ctx, topType, baseUID, modelHint string) *shared.Partials {
	return pifamily.NormalizeRecord(rec, ctx, topType, baseUID, modelHint, pifamily.PI)
}
