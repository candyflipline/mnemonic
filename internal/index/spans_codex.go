package index

import (
	"iter"
	"slices"
	"strconv"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// codexSpans: flatSpans whose chat is each token_count row, the only row a
// codex session records a model call boundary on.
func codexSpans(harness, sid string, rows iter.Seq[*borrowedRow]) []schema.Span {
	return flatSpans("codex", func(r *borrowedRow) string {
		if r.PayloadType == "token_count" {
			return r.UID
		}
		return ""
	})(harness, sid, rows)
}

// flatSpans derives the tree for harnesses whose rows name no subagent: the
// session is the root invoke_agent, each turn_seq a turn under it, each
// chatKey a chat under its turn, and each tool_call_id an execute_tool under
// the chat whose chatKey its call row carries, else under its turn. chatKey
// is "" for rows outside a chat; a harness that records no model call
// boundary passes nil and gets no chats. A chat's tokens are the max over
// its rows, because a provider reports one usage per call; root and turns
// carry the sum over the chats below them.
func flatSpans(root string, chatKey func(*borrowedRow) string) func(string, string, iter.Seq[*borrowedRow]) []schema.Span {
	return func(_, sid string, rows iter.Seq[*borrowedRow]) []schema.Span {
		var rb spanBounds
		type spanAgg struct {
			b  spanBounds
			id string
		}
		turns := map[uint32]*spanAgg{}
		type chatAgg struct {
			spanAgg
			model   string
			turnID  string
			turnSeq uint32
			tok     tokens
		}
		chats := map[string]*chatAgg{}
		type toolAgg struct {
			spanAgg
			name, ckey string
			turnSeq    uint32
			err        uint64
		}
		tools := map[string]*toolAgg{}
		for r := range rows {
			uid, t := r.UID, eventTimeUs((*Row)(r))
			rb.add(uid, t)
			seq := uint32(r.TurnIndex)
			tr := turns[seq]
			if tr == nil {
				tr = &spanAgg{id: schema.SpanIDOf("turn", sid, strconv.FormatUint(uint64(seq), 10))}
				turns[seq] = tr
			}
			tr.b.add(uid, t)
			if ck := chatKeyOf(chatKey, r); ck != "" {
				c := chats[ck]
				if c == nil {
					c = &chatAgg{spanAgg: spanAgg{id: schema.SpanIDOf("chat", sid, ck)}}
					chats[ck] = c
				}
				c.model = maxStr(c.model, r.Model)
				if c.turnID == "" || seq < c.turnSeq {
					c.turnSeq, c.turnID = seq, tr.id
				}
				c.tok = tokens{maxU64(c.tok.in, r.InputTokens), maxU64(c.tok.out, r.OutputTokens),
					maxU64(c.tok.cr, r.CacheReadTokens), maxU64(c.tok.cw, r.CacheWriteTokens)}
				c.b.add(uid, t)
			}
			if call := r.ToolCallID; call != "" {
				o := tools[call]
				if o == nil {
					o = &toolAgg{spanAgg: spanAgg{id: schema.SpanIDOf("tool", sid, call)}}
					tools[call] = o
				}
				o.name = maxStr(o.name, r.ToolName)
				o.err = maxU64(o.err, r.ToolError)
				o.ckey = maxNN(o.ckey, chatKeyOf(chatKey, r))
				if o.b.first == "" || seq < o.turnSeq {
					o.turnSeq = seq
				}
				o.b.add(uid, t)
			}
		}

		rootID := schema.SpanIDOf("session", sid)
		var rootTok tokens
		turnTok := map[string]tokens{}
		for _, c := range chats {
			rootTok.add(c.tok)
			s := turnTok[c.turnID]
			s.add(c.tok)
			turnTok[c.turnID] = s
		}
		out := []schema.Span{{SessionID: sid, SpanID: rootID, Kind: schema.SpanInvokeAgent, Name: root,
			Start: spanTime(rb.startUs), End: spanTime(rb.endUs), Status: schema.SpanOK,
			FirstEventUID: rb.first, LastEventUID: rb.last}}
		rootTok.setSpan(&out[0])
		for _, q := range sortedTurnSeqs(turns) {
			tr := turns[q]
			s := spanAcc{sid: sid, id: tr.id, parent: rootID, kind: schema.SpanTurn,
				name: "turn " + strconv.FormatUint(uint64(q), 10), b: tr.b, status: schema.SpanOK}
			turnTok[tr.id].set(&s)
			out = append(out, s.span())
		}
		for _, ck := range sortedMapKeys(chats) {
			c := chats[ck]
			s := spanAcc{sid: sid, id: c.id, parent: c.turnID, kind: schema.SpanChat,
				name: orChat(c.model), model: c.model, b: c.b, status: schema.SpanOK}
			c.tok.set(&s)
			out = append(out, s.span())
		}
		for _, call := range sortedMapKeys(tools) {
			o := tools[call]
			parent := ""
			if o.ckey != "" {
				if c := chats[o.ckey]; c != nil {
					parent = c.id
				}
			}
			if parent == "" {
				if tr := turns[o.turnSeq]; tr != nil {
					parent = tr.id
				}
			}
			status := schema.SpanOK
			if o.err > 0 {
				status = schema.SpanError
			}
			acc := spanAcc{sid: sid, id: o.id, parent: parent, kind: schema.SpanExecuteTool,
				name: orTool(o.name), b: o.b, status: status}
			out = append(out, acc.span())
		}
		return out
	}
}

func chatKeyOf(chatKey func(*borrowedRow) string, r *borrowedRow) string {
	if chatKey == nil {
		return ""
	}
	return chatKey(r)
}

func orChat(model string) string {
	if model == "" {
		return "chat"
	}
	return model
}

func orTool(name string) string {
	if name == "" {
		return "tool"
	}
	return name
}

func sortedTurnSeqs[T any](m map[uint32]T) []uint32 {
	out := make([]uint32, 0, len(m))
	for q := range m {
		out = append(out, q)
	}
	slices.Sort(out)
	return out
}

func sortedMapKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
