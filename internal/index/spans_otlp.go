package index

import (
	"encoding/json"
	"iter"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// otlpSpans: each events row with payload_type 'span' and an op_kind of chat,
// execute_tool or invoke_agent is a span keyed by the author and the OTLP
// span id (trace_id, item_id); other span rows are not emitted and their
// children hang under the nearest emitted ancestor, else the root. Of the
// emitted spans on a parent cycle the one with the smallest item_id is the
// root. A chat carries its own usage; an invoke_agent carries the sum over
// the chats beneath it, else its own. The parent link is origin_event_id, the
// event_uid of the parent span's row. Walks run at most 65 edges out, the
// depth the derivation's recursive CTE walks.
func otlpSpans(_, sid string, session iter.Seq[*Row]) []schema.Span {
	rows := map[string]*otlpNode{}
	for r := range session {
		if r.PayloadType == "span" && r.UID != "" {
			n := &otlpNode{origin: r.OriginEventID, opKind: r.OpKind, itemID: r.ItemID, traceID: r.TraceID,
				author: r.Author, tok: tokens{r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens}}
			if r.OpKind == string(schema.SpanChat) {
				n.cost = otlpCostUsd(r)
			}
			rows[r.UID] = n
		}
	}
	if len(rows) == 0 {
		return nil
	}

	nearest := map[string]*otlpNode{}
	otlpWalk(rows, func(from, uid string, anc *otlpNode) bool {
		if otlpEmitted(anc.opKind) {
			nearest[from] = anc
			return false
		}
		return true
	})

	root := map[string]bool{}
	color := map[string]bool{}
	for uid := range rows {
		if color[uid] {
			continue
		}
		var path []string
		pos := map[string]int{}
		for c := uid; ; {
			if p, seen := pos[c]; seen {
				if members := path[p:]; len(members) <= 64 {
					for _, m := range members {
						if !otlpEmitted(rows[m].opKind) {
							continue
						}
						isRoot := true
						for _, n := range members {
							if n != m && otlpEmitted(rows[n].opKind) && rows[n].itemID < rows[m].itemID {
								isRoot = false
								break
							}
						}
						if isRoot {
							root[m] = true
						}
					}
				}
				break
			}
			if color[c] {
				break
			}
			color[c] = true
			pos[c] = len(path)
			path = append(path, c)
			next := rows[c].origin
			if next == "" || rows[next] == nil {
				break
			}
			c = next
		}
	}

	type sums struct {
		tok                   tokens
		cost                  float64
		hasCost, hasChatBelow bool
	}
	under := map[string]*sums{}
	otlpWalk(rows, func(from, uid string, anc *otlpNode) bool {
		if rows[from].opKind != "chat" || anc.opKind != string(schema.SpanInvokeAgent) {
			return true
		}
		s := under[uid]
		if s == nil {
			s = &sums{}
			under[uid] = s
		}
		s.tok.add(rows[from].tok)
		if c := rows[from].cost; c != nil {
			s.cost += *c
			s.hasCost = true
		}
		s.hasChatBelow = true
		return true
	})

	out := make([]schema.Span, 0, len(rows))
	for r := range session {
		if r.PayloadType != "span" || r.UID == "" || !otlpEmitted(r.OpKind) {
			continue
		}
		parent := ""
		if !root[r.UID] {
			if n := nearest[r.UID]; n != nil {
				parent = spanIDHex("otlp", n.author, n.traceID, n.itemID)
			}
		}
		name := r.OpKind
		if r.Text != "" {
			name = r.Text
		}
		s := spanAcc{sid: sid, id: spanIDHex("otlp", r.Author, r.TraceID, r.ItemID),
			parent: parent, kind: schema.SpanKind(r.OpKind), name: name,
			b: spanBounds{startUs: eventTimeUs(r), endUs: eventTimeUs(r) + int64(r.LatencyMs)*1000,
				first: r.UID, last: r.UID},
			status: schema.SpanOK, model: r.Model}
		if r.OpStatus == "error" {
			s.status = schema.SpanError
		}
		if u := under[r.UID]; u != nil && u.hasChatBelow {
			u.tok.set(&s)
			if u.hasCost {
				s.cost = &u.cost
			} else {
				s.cost = otlpCostUsd(r)
			}
		} else {
			tokens{r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens}.set(&s)
			s.cost = otlpCostUsd(r)
		}
		out = append(out, s.span())
	}
	return out
}

type otlpNode struct {
	origin, opKind, itemID, traceID, author string
	tok                                     tokens
	cost                                    *float64
}

func otlpEmitted(opKind string) bool {
	switch opKind {
	case string(schema.SpanChat), string(schema.SpanExecuteTool), string(schema.SpanInvokeAgent):
		return true
	}
	return false
}

// otlpWalk follows each row's origin_event_id chain upward, calling yield on
// every distinct strict ancestor, nearest first, at most 65 edges out (the CTE's depth 0..64), and
// stopping where yield returns false.
func otlpWalk(rows map[string]*otlpNode, yield func(from, uid string, anc *otlpNode) bool) {
	for from := range rows {
		seen := map[string]bool{from: true}
		cur := from
		for i := 0; i <= 64; i++ {
			next := rows[cur].origin
			if next == "" || seen[next] {
				break
			}
			anc, ok := rows[next]
			if !ok {
				break
			}
			seen[next] = true
			if !yield(from, next, anc) {
				break
			}
			cur = next
		}
	}
}

func otlpCostUsd(r *Row) *float64 {
	if r.Native == "" {
		return nil
	}
	var m map[string]float64
	if json.Unmarshal([]byte(r.Native), &m) != nil {
		return nil
	}
	if c, ok := m["cost_usd"]; ok {
		return &c
	}
	return nil
}
