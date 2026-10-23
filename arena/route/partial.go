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

type viewAnswer func(v View, args json.RawMessage) (json.RawMessage, string, error)

// rule says how one tool is answered: scattered over the parts and merged, or
// from the View alone at either rung.
type rule struct {
	merge merge
	view  viewAnswer
}

var rules = map[string]rule{
	"search_sessions":   {merge: trace.SearchPartial},
	"file_attention":    {merge: trace.AttentionPartial},
	"list_sessions":     {merge: trace.ListPartial},
	"open":              {merge: trace.OpenPartial},
	"get_ingest_status": {view: sourcesAnswer},
}

func sourcesAnswer(v View, args json.RawMessage) (json.RawMessage, string, error) {
	db, err := trace.SourcesDB(v.Sources)
	if err != nil {
		return nil, "", err
	}
	return trace.Call(db, "get_ingest_status", args)
}

// viewOnly reports whether tool is answered from the View without a reader.
func viewOnly(tool string) bool { return rules[tool].view != nil }

// partial answers a call at rung Partial: scatter each phase, then merge
// exactly by the tool's merge rule.
func partial(ctx context.Context, v View, readers []Reader, tool string, args json.RawMessage) (json.RawMessage, string, error) {
	m := rules[tool].merge
	if m == nil {
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
			var infra InfraError
			if errors.As(err, &infra) {
				return nil, infra
			}
			return nil, InfraError{error: err}
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
		err = InfraError{error: err}
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
