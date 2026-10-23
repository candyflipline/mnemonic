package index

import (
	"encoding/json"

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
func otlpSpans(_ string, session []*Row) []schema.Span {
	rows := map[string]*Row{}
	for _, r := range session {
		if r.PayloadType == "span" && r.UID != "" {
			rows[r.UID] = r
		}
	}
	if len(rows) == 0 {
		return nil
	}
	sid := session[0].Session

	// nearest: each row's closest emitted strict ancestor.
	nearest := map[string]*Row{}
	otlpWalk(rows, func(from, uid string, anc *Row) bool {
		if otlpEmitted(anc) {
			nearest[from] = anc
			return false
		}
		return true
	})

	// root: of the emitted members of each parent cycle, the smallest
	// item_id; the derivation calls the others non-roots, not cycle members.
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
						if !otlpEmitted(rows[m]) {
							continue
						}
						isRoot := true
						for _, n := range members {
							if n != m && otlpEmitted(rows[n]) && rows[n].ItemID < rows[m].ItemID {
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
			next := rows[c].OriginEventID
			if next == "" || rows[next] == nil {
				break
			}
			c = next
		}
	}

	// sums: what each invoke_agent carries over the chats beneath it.
	type sums struct {
		tok                   tokens
		cost                  float64
		hasCost, hasChatBelow bool
	}
	under := map[string]*sums{}
	otlpWalk(rows, func(from, uid string, anc *Row) bool {
		if rows[from].OpKind != "chat" || anc.OpKind != string(schema.SpanInvokeAgent) {
			return true
		}
		s := under[uid]
		if s == nil {
			s = &sums{}
			under[uid] = s
		}
		s.tok.add(tokens{rows[from].InputTokens, rows[from].OutputTokens,
			rows[from].CacheReadTokens, rows[from].CacheWriteTokens})
		if c := otlpCostUsd(rows[from]); c != nil {
			s.cost += *c
			s.hasCost = true
		}
		s.hasChatBelow = true
		return true
	})

	out := make([]schema.Span, 0, len(rows))
	for _, r := range session {
		if r.PayloadType != "span" || r.UID == "" || !otlpEmitted(r) {
			continue
		}
		parent := ""
		if !root[r.UID] {
			if n := nearest[r.UID]; n != nil {
				parent = spanIDHex("otlp", n.Author, n.TraceID, n.ItemID)
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

// otlpEmitted is op_kind IN ('chat', 'execute_tool', 'invoke_agent'): the
// span rows the derivation emits; the rest only carry children.
func otlpEmitted(r *Row) bool {
	switch r.OpKind {
	case string(schema.SpanChat), string(schema.SpanExecuteTool), string(schema.SpanInvokeAgent):
		return true
	}
	return false
}

// otlpWalk follows each row's origin_event_id chain upward, calling yield on
// every distinct strict ancestor, nearest first, at most 65 edges out (the CTE's depth 0..64), and
// stopping where yield returns false.
func otlpWalk(rows map[string]*Row, yield func(from, uid string, anc *Row) bool) {
	for from := range rows {
		seen := map[string]bool{from: true}
		cur := from
		for i := 0; i <= 64; i++ {
			next := rows[cur].OriginEventID
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

// otlpCostUsd is token_usage_native_units['cost_usd'], nil when the row
// carries none.
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
