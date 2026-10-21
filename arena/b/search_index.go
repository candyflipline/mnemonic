package main

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

func searchFlags(e *event) uint16 {
	c, p, a := e.EventKind, e.PayloadType, e.ActorKind
	nonSystem := !strings.EqualFold(a, "system")
	matches := []bool{strings.EqualFold(a, "user") && isMcpMessageEvent(c, p), strings.EqualFold(a, "assistant") && isMcpMessageEvent(c, p), nonSystem && isMcpReasoningEvent(c, p), nonSystem && isMcpToolCallEvent(c, p), nonSystem && isMcpToolResponseEvent(c, p), isMcpCompactionEvent(c, p), isMcpSystemEvent(c, p, a), isMcpRuntimeEvent(c, p)}
	var flags uint16
	for i, yes := range matches {
		if yes {
			flags |= 1 << i
		}
	}
	if e.SourceName != "codex-mcp" && !isMcpInternalToolName(e.ToolName) {
		flags |= 1 << 8
	}
	return flags
}
func searchMcpEvents(_ context.Context, db *Store, q searchQuery) (*searchResult, error) {
	terms := tokenizeQuery(q.query, bm25MaxQueryTerms)
	if len(terms) == 0 {
		return nil, argErrorf("query has no searchable terms (tokens shorter than 2 characters are excluded)")
	}
	sort.Strings(terms)
	res := &searchResult{scopeExists: true}
	if q.sessionOK {
		es := db.sessions[q.sessionID]
		res.scopeExists = len(es) > 0
		if q.turnOK {
			_, res.scopeExists = db.turn(q.sessionID, q.turnSeq)
		}
	}
	var mask uint16
	for i, t := range searchableEventTypes {
		if slices.Contains(q.eventTypes, t) {
			mask |= 1 << i
		}
	}
	db.scoreEpoch++
	if db.scoreEpoch == 0 {
		clear(db.scoreStamps)
		db.scoreEpoch = 1
	}
	epoch := db.scoreEpoch
	db.scoreTouched = db.scoreTouched[:0]
	avg := math.Max(1, float64(db.totalLength)/math.Max(1, float64(db.docs)))
	for _, t := range terms {
		all := db.postingScratch[:0]
		for _, sg := range db.segments {
			sg.each(sg.lookup(t), func(doc, tf uint32) { all = append(all, posting{doc, tf}) })
		}
		df := float64(len(all))
		idf := math.Log(1 + (math.Max(float64(db.docs), df)-df+0.5)/(df+0.5))
		for _, p := range all {
			e := db.events[p.Doc]
			if e.Flags&(1<<8) == 0 || len(q.eventTypes) > 0 && e.Flags&mask == 0 {
				continue
			}
			if q.sessionOK && e.SessionID != q.sessionID || q.turnOK && e.Turn != q.turnSeq || q.harness != nil && e.Harness != *q.harness || q.sourceName != nil && e.SourceName != *q.sourceName {
				continue
			}
			tf := float64(p.TF)
			v := idf * ((tf * 1.2) / (tf + 1.2*(1-0.75+0.75*(float64(e.Length)/avg))))
			if db.scoreStamps[p.Doc] != epoch {
				db.scoreStamps[p.Doc] = epoch
				db.scoreValues[p.Doc] = 0
				db.scoreTouched = append(db.scoreTouched, p.Doc)
			}
			db.scoreValues[p.Doc] += v
		}
		db.postingScratch = all
	}
	top := scoreTop{limit: q.nHits + 1}
	for _, doc := range db.scoreTouched {
		top.offer(scoredEvent{db.events[doc], db.scoreValues[doc]})
	}
	rows := make([]*mcpRow, 0, len(top.h))
	for _, candidate := range top.h {
		e, score := candidate.e, candidate.score
		ts, _ := db.turn(e.SessionID, e.Turn)
		ss := db.stats[e.SessionID]
		rows = append(rows, &mcpRow{eventUID: e.EventUID, sessionID: e.SessionID, rawScore: score, eventTsMs: e.Timestamp, eventUnixMs: e.Time, mcpDetail: mcpDetail{sourceName: e.SourceName, harness: e.Harness, inferenceProvider: e.InferenceProvider, eventClass: e.EventKind, payloadType: e.PayloadType, actorRole: e.ActorKind, phase: firstNonEmpty(e.ToolPhase, e.OpStatus), sourceRef: fmt.Sprintf("%s:%d:%d", e.SourceFile, e.SourceGeneration, e.SourceLineNo), textPreview: truncateRunes(e.TextPreview, previewChars), textContent: e.TextPreview, textHash: e.TextHash, eventOrder: int64(e.Order), turnSeq: e.Turn, eventOrdinal: e.Ordinal, turnEventCount: ts.Count, turnCompleted: ts.Complete, turnTerminalUID: ts.Terminal, sessionStartedMs: ss.Start, sessionUpdatedMs: ss.End, sessionCompleted: ss.Complete}})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.rawScore != b.rawScore {
			return a.rawScore > b.rawScore
		}
		if a.eventTsMs != b.eventTsMs {
			return a.eventTsMs > b.eventTsMs
		}
		return a.eventUID < b.eventUID
	})
	if len(rows) > q.nHits+1 {
		rows = rows[:q.nHits+1]
	}
	sortMcpRows(rows)
	rows = dedupeMcpRows(rows, q.nHits+1)
	res.truncated = len(rows) > q.nHits
	if res.truncated {
		rows = rows[:q.nHits]
	}
	res.hits = mapMcpRowsToHits(rows)
	return res, nil
}

// Keep exactly the hydrated pre-deduplication top k. The SQL candidate stage
// retains cutoff ties, then sorts by this total order before taking k.
type scoredEvent struct {
	e     *event
	score float64
}
type scoreTop struct {
	h     []scoredEvent
	limit int
}

func scoreBetter(a, b scoredEvent) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	if a.e.Timestamp != b.e.Timestamp {
		return a.e.Timestamp > b.e.Timestamp
	}
	return a.e.EventUID < b.e.EventUID
}
func (t *scoreTop) offer(v scoredEvent) {
	if len(t.h) < t.limit {
		t.h = append(t.h, v)
		i := len(t.h) - 1
		for i > 0 {
			p := (i - 1) / 2
			if !scoreBetter(t.h[p], t.h[i]) {
				break
			}
			t.h[p], t.h[i] = t.h[i], t.h[p]
			i = p
		}
		return
	}
	if !scoreBetter(v, t.h[0]) {
		return
	}
	t.h[0] = v
	for i := 0; ; {
		c := 2*i + 1
		if c >= len(t.h) {
			break
		}
		if c+1 < len(t.h) && scoreBetter(t.h[c], t.h[c+1]) {
			c++
		}
		if !scoreBetter(t.h[i], t.h[c]) {
			break
		}
		t.h[i], t.h[c] = t.h[c], t.h[i]
		i = c
	}
}
