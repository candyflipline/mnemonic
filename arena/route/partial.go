package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/candyflipline/mnemonic/arena/trace"
)

type merge func(args json.RawMessage, c trace.Corpus, fan trace.Fan) (json.RawMessage, string, error)

var merges = map[string]merge{
	"search_sessions":   trace.SearchPartial,
	"file_attention":    trace.AttentionPartial,
	"list_sessions":     trace.ListPartial,
	"open":              trace.OpenPartial,
	"get_ingest_status": keeperAnswers,
}

func keeperAnswers(json.RawMessage, trace.Corpus, trace.Fan) (json.RawMessage, string, error) {
	return nil, "", errors.New("get_ingest_status is answered by the keeper, never scattered")
}

// partial answers a call at rung Partial: scatter each phase, then merge
// exactly by the tool's merge rule.
func partial(ctx context.Context, v View, readers []Reader, tool string, args json.RawMessage) (json.RawMessage, string, error) {
	m, ok := merges[tool]
	if !ok {
		return nil, "", fmt.Errorf("unknown tool %s", tool)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var c trace.Corpus
	for _, p := range v.Parts {
		c.ScoredDocs += p.ScoredDocs
		c.TotalLen += p.TotalLen
	}
	fan := func(phase string, stats json.RawMessage, session string) ([]json.RawMessage, error) {
		w := v
		if session != "" {
			w.Parts = holding(v.Parts, session)
		}
		if len(w.Parts) == 0 {
			return nil, nil
		}
		resps, err := scatter(ctx, w, readers, tool, phase, args, stats)
		if err != nil {
			return nil, InfraError{err}
		}
		out := make([]json.RawMessage, len(resps))
		for i, r := range resps {
			out[i] = r.Phase
		}
		return out, nil
	}
	data, text, err := m(args, c, fan)
	var bad trace.AnswerError
	if errors.As(err, &bad) {
		err = InfraError{err}
	}
	return data, text, err
}

func holding(parts []GrantedPart, session string) []GrantedPart {
	for _, p := range parts {
		if slices.Contains(p.Sessions, session) {
			return []GrantedPart{p}
		}
	}
	return nil
}
