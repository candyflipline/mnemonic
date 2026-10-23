package trace

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/candyflipline/mnemonic/internal/repo"
)

// Fan scatters one phase to the readers owning the view's parts, or only the
// part holding session when it is set, and returns their phase answers.
type Fan func(phase string, stats json.RawMessage, session string) ([]json.RawMessage, error)

// Corpus is BM25's corpus size summed over every part of a view.
type Corpus struct{ ScoredDocs, TotalLen int64 }

type phaseKey struct{ tool, phase string }

var phases = map[phaseKey]func(db *DB, args, stats json.RawMessage) (any, error){
	{"search_sessions", "df"}:     searchDFPhase,
	{"search_sessions", "score"}:  searchScorePhase,
	{"file_attention", "roots"}:   faRootsPhase,
	{"file_attention", "touches"}: faPhase,
	{"list_sessions", "page"}:     lsPhase,
	{"open", "open"}:              openPhase,
}

// RunPhase answers one phase of a tool scattered over the parts of db; stats
// carries what an earlier phase gathered across every reader.
func RunPhase(db *DB, tool, phase string, args, stats json.RawMessage) (json.RawMessage, error) {
	f, ok := phases[phaseKey{tool, phase}]
	if !ok {
		return nil, fmt.Errorf("unknown phase %s/%s", tool, phase)
	}
	v, err := f(db, args, stats)
	if err != nil {
		return nil, err
	}
	return encodeJSON(v)
}

// AnswerError is a reader's phase answer the merge cannot use, as opposed to
// an error of the call's own arguments.
type AnswerError struct{ error }

func (e AnswerError) Unwrap() error { return e.error }

func decodeAll[T any](answers []json.RawMessage) ([]T, error) {
	out := make([]T, len(answers))
	for i, b := range answers {
		if err := json.Unmarshal(b, &out[i]); err != nil {
			return nil, AnswerError{err}
		}
	}
	return out, nil
}

func searchPlan(args json.RawMessage) (canonicalSearchSessionsArgs, searchQuery, []string, error) {
	a, err := parseSearchSessionsArgs(args)
	if err != nil {
		return a, searchQuery{}, nil, err
	}
	q := a.searchQuery()
	sorted, err := searchTerms(q)
	return a, q, sorted, err
}

func searchDFPhase(db *DB, args, _ json.RawMessage) (any, error) {
	_, _, sorted, err := searchPlan(args)
	if err != nil {
		return nil, err
	}
	return db.termDF(sorted), nil
}

func searchScorePhase(db *DB, args, stats json.RawMessage) (any, error) {
	_, q, sorted, err := searchPlan(args)
	if err != nil {
		return nil, err
	}
	var s searchStats
	if err := json.Unmarshal(stats, &s); err != nil {
		return nil, err
	}
	if len(s.DF) != len(sorted) {
		return nil, fmt.Errorf("search stats carry %d terms, query has %d", len(s.DF), len(sorted))
	}
	h, err := db.scoreHits(q, sorted, s)
	if err != nil {
		return nil, err
	}
	return hitsToWire(h), nil
}

// SearchPartial is search_sessions at rung Partial: df, then score, then the global cut.
func SearchPartial(args json.RawMessage, c Corpus, fan Fan) (json.RawMessage, string, error) {
	a, q, sorted, err := searchPlan(args)
	if err != nil {
		return nil, "", err
	}
	dfs, err := fan("df", nil, "")
	if err != nil {
		return nil, "", err
	}
	parts, err := decodeAll[[]float64](dfs)
	if err != nil {
		return nil, "", err
	}
	stats := searchStats{DF: make([]float64, len(sorted)), Docs: uint64(c.ScoredDocs), TotalLen: uint64(c.TotalLen)}
	for _, df := range parts {
		if len(df) != len(sorted) {
			return nil, "", AnswerError{fmt.Errorf("a reader returned df for %d terms, query has %d", len(df), len(sorted))}
		}
		for i, n := range df {
			stats.DF[i] += n
		}
	}
	sb, err := encodeJSON(stats)
	if err != nil {
		return nil, "", err
	}
	scope := ""
	if q.sessionOK {
		scope = q.sessionID
	}
	scored, err := fan("score", sb, scope)
	if err != nil {
		return nil, "", err
	}
	wires, err := decodeAll[hitsWire](scored)
	if err != nil {
		return nil, "", err
	}
	hits := make([]scoredHits, len(wires))
	for i, w := range wires {
		hits[i] = w.hits()
	}
	return encodeAnswer(searchAnswer(a, mergeHits(q, hits)))
}

func faRootsPhase(db *DB, _, _ json.RawMessage) (any, error) {
	return db.rootSeen(), nil
}

func faPhase(db *DB, _, stats json.RawMessage) (any, error) {
	var q faQuery
	if err := json.Unmarshal(stats, &q); err != nil {
		return nil, err
	}
	return db.faLists(q)
}

// AttentionPartial is file_attention at rung Partial.
func AttentionPartial(args json.RawMessage, _ Corpus, fan Fan) (json.RawMessage, string, error) {
	c, err := newFaCall(args, func() ([]repo.Seen, error) {
		answers, err := fan("roots", nil, "")
		if err != nil {
			return nil, err
		}
		lists, err := decodeAll[[]repo.Seen](answers)
		if err != nil {
			return nil, err
		}
		return mergeSeen(lists...), nil
	})
	if err != nil {
		return nil, "", err
	}
	touches, err := faGather(c.q, func() ([]faLists, error) {
		qb, err := encodeJSON(c.q)
		if err != nil {
			return nil, err
		}
		answers, err := fan("touches", qb, "")
		if err != nil {
			return nil, err
		}
		return decodeAll[faLists](answers)
	})
	if err != nil {
		return nil, "", err
	}
	return encodeAnswer(c.answer(touches))
}

func lsPhase(db *DB, args, _ json.RawMessage) (any, error) {
	a, err := lsParseArgs(args)
	if err != nil {
		return nil, err
	}
	cursor, err := lsCursorOf(a)
	if err != nil {
		return nil, err
	}
	return db.lsCandidates(a, cursor)
}

// ListPartial is list_sessions at rung Partial.
func ListPartial(args json.RawMessage, _ Corpus, fan Fan) (json.RawMessage, string, error) {
	a, err := lsParseArgs(args)
	if err != nil {
		return nil, "", err
	}
	page, err := lsQueryPage(a, func(*lsCursor) ([]lsCand, error) {
		answers, err := fan("page", nil, "")
		if err != nil {
			return nil, err
		}
		lists, err := decodeAll[[]lsCand](answers)
		if err != nil {
			return nil, err
		}
		var all []lsCand
		for _, l := range lists {
			all = append(all, l...)
		}
		return all, nil
	})
	if err != nil {
		return nil, "", err
	}
	return encodeAnswer(lsAnswer(a, page))
}

type openWire struct {
	Found bool   `json:"found"`
	Data  string `json:"data,omitempty"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

func openPhase(db *DB, args, _ json.RawMessage) (any, error) {
	a, err := parseOpenArgs(args)
	if err != nil {
		return nil, err
	}
	found := false
	if a.target.kind == kindEvent {
		if _, found, err = db.sessionOfUID(a.target.eventUID); err != nil {
			return nil, err
		}
	} else {
		_, found = db.part(a.target.session)
	}
	if !found {
		return openWire{}, nil
	}
	data, text, err := Call(db, "open", args)
	w := openWire{Found: true, Data: string(data), Text: text}
	var ae *argError
	switch {
	case errors.As(err, &ae):
		w.Error = ae.Msg
	case err != nil:
		return nil, err
	}
	return w, nil
}

// OpenPartial is open at rung Partial, answered by the owner of the target's session.
func OpenPartial(args json.RawMessage, _ Corpus, fan Fan) (json.RawMessage, string, error) {
	a, err := parseOpenArgs(args)
	if err != nil {
		return nil, "", err
	}
	scope := a.target.session
	if a.target.kind == kindEvent {
		scope = ""
	}
	answers, err := fan("open", nil, scope)
	if err != nil {
		return nil, "", err
	}
	wires, err := decodeAll[openWire](answers)
	if err != nil {
		return nil, "", err
	}
	for _, w := range wires {
		if !w.Found {
			continue
		}
		var data json.RawMessage
		if w.Data != "" {
			data = json.RawMessage(w.Data)
		}
		if w.Error != "" {
			return data, w.Text, &argError{Msg: w.Error}
		}
		return data, w.Text, nil
	}
	empty, err := OpenParts(nil)
	if err != nil {
		return nil, "", err
	}
	return Call(empty, "open", args)
}

type hitsWire struct {
	Rows    []rowWire `json:"rows"`
	Dropped []float64 `json:"dropped,omitempty"`
	Exists  bool      `json:"exists"`
}

type rowWire struct {
	UID              string  `json:"uid"`
	Session          string  `json:"session"`
	Score            float64 `json:"score"`
	TsMs             int64   `json:"ts"`
	UnixMs           int64   `json:"unix"`
	SourceName       string  `json:"source"`
	Harness          string  `json:"harness"`
	Provider         string  `json:"provider"`
	Class            string  `json:"class"`
	PayloadType      string  `json:"payload"`
	Actor            string  `json:"actor"`
	Phase            string  `json:"phase"`
	SourceRef        string  `json:"ref"`
	Preview          string  `json:"preview"`
	Text             string  `json:"text"`
	TextHash         uint64  `json:"hash"`
	EventOrder       int64   `json:"order"`
	TurnSeq          uint32  `json:"turn"`
	EventOrdinal     uint32  `json:"ordinal"`
	TurnEventCount   int64   `json:"turn_events"`
	TurnCompleted    bool    `json:"turn_done"`
	TurnTerminalUID  string  `json:"terminal"`
	SessionStartedMs int64   `json:"started"`
	SessionUpdatedMs int64   `json:"updated"`
	SessionCompleted bool    `json:"done"`
}

func hitsToWire(h scoredHits) hitsWire {
	w := hitsWire{Rows: make([]rowWire, len(h.rows)), Dropped: h.dropped, Exists: h.exists}
	for i, x := range h.rows {
		w.Rows[i] = rowWire{
			UID: x.eventUID, Session: x.sessionID, Score: x.rawScore, TsMs: x.eventTsMs, UnixMs: x.eventUnixMs,
			SourceName: x.sourceName, Harness: x.harness, Provider: x.inferenceProvider,
			Class: x.eventClass, PayloadType: x.payloadType, Actor: x.actorRole, Phase: x.phase,
			SourceRef: x.sourceRef, Preview: x.textPreview, Text: x.textContent, TextHash: x.textHash,
			EventOrder: x.eventOrder, TurnSeq: x.turnSeq, EventOrdinal: x.eventOrdinal,
			TurnEventCount: x.turnEventCount, TurnCompleted: x.turnCompleted, TurnTerminalUID: x.turnTerminalUID,
			SessionStartedMs: x.sessionStartedMs, SessionUpdatedMs: x.sessionUpdatedMs, SessionCompleted: x.sessionCompleted,
		}
	}
	return w
}

func (w hitsWire) hits() scoredHits {
	h := scoredHits{rows: make([]*mcpRow, len(w.Rows)), dropped: w.Dropped, exists: w.Exists}
	for i, r := range w.Rows {
		h.rows[i] = &mcpRow{eventUID: r.UID, sessionID: r.Session, rawScore: r.Score, eventTsMs: r.TsMs, eventUnixMs: r.UnixMs,
			mcpDetail: mcpDetail{
				sourceName: r.SourceName, harness: r.Harness, inferenceProvider: r.Provider,
				eventClass: r.Class, payloadType: r.PayloadType, actorRole: r.Actor, phase: r.Phase,
				sourceRef: r.SourceRef, textPreview: r.Preview, textContent: r.Text, textHash: r.TextHash,
				eventOrder: r.EventOrder, turnSeq: r.TurnSeq, eventOrdinal: r.EventOrdinal,
				turnEventCount: r.TurnEventCount, turnCompleted: r.TurnCompleted, turnTerminalUID: r.TurnTerminalUID,
				sessionStartedMs: r.SessionStartedMs, sessionUpdatedMs: r.SessionUpdatedMs, sessionCompleted: r.SessionCompleted,
			}}
	}
	return h
}
