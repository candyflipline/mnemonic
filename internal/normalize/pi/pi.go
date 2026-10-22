package pi

import (
	"io"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/normalize/pifamily"
	"github.com/candyflipline/mnemonic/internal/normalize/shared"
	"github.com/candyflipline/mnemonic/internal/schema"
)

type Parser struct{}

func (Parser) Harness() string { return "pi-coding-agent" }

func (Parser) Version() int { return 1 }

func (Parser) Parse(r io.Reader, src normalize.SourceMeta, from normalize.State, emit func(*schema.Event) error) (normalize.State, error) {
	return shared.ParseJSONL(r, src, from, piSource{}, emit)
}

func init() { normalize.Register(Parser{}) }

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
	policy := pifamily.PI
	if ctx.SourceName == "omp" {
		policy = pifamily.OMP
	}
	return pifamily.NormalizeRecord(rec, ctx, topType, baseUID, modelHint, policy)
}
