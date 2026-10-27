package index

import (
	"iter"
	"strconv"

	"github.com/candyflipline/mnemonic/internal/schema"
)

type claudeAgg struct {
	b  spanBounds
	id string
}

type claudeAgentAgg struct {
	claudeAgg
	label string
	tok   tokens
}

type claudeChatAgg struct {
	claudeAgg
	model   string
	sub     string
	turnID  string
	turnSeq uint32
	hasTurn bool
	tok     tokens
}

type claudeToolAgg struct {
	claudeAgg
	name    string
	sub     string
	err     uint64
	req     string
	pcall   string
	turnSeq uint32
	hasTurn bool
}

type claudeApAgg struct {
	id, turnID string
	hasTurn    bool
}

func claudeCodeSpans(_, sid string, rows iter.Seq[*borrowedRow]) []schema.Span {
	rootID := spanIDHex("session", sid)
	reqOf := func(r *borrowedRow) string {
		if r.RequestID != "" {
			return r.RequestID
		}
		return r.TraceID
	}
	subOf := func(r *borrowedRow) string {
		if r.IsSubstream != 0 {
			return r.AgentRunID
		}
		return ""
	}

	var rb spanBounds
	turns := map[uint32]*claudeAgg{}
	agents := map[string]*claudeAgentAgg{}
	chats := map[string]*claudeChatAgg{}
	tools := map[string]*claudeToolAgg{}
	for r := range rows {
		uid, t := r.UID, eventTimeUs((*Row)(r))
		rb.add(uid, t)
		sub := subOf(r)
		seq := uint32(r.TurnIndex)
		if sub == "" {
			tr := turns[seq]
			if tr == nil {
				tr = &claudeAgg{id: spanIDHex("turn", sid, strconv.FormatUint(uint64(seq), 10))}
				turns[seq] = tr
			}
			tr.b.add(uid, t)
		} else {
			a := agents[sub]
			if a == nil {
				a = &claudeAgentAgg{claudeAgg: claudeAgg{id: spanIDHex("agent", sid, sub)}}
				agents[sub] = a
			}
			a.label = maxStr(a.label, r.AgentLabel)
			a.b.add(uid, t)
		}
		if req := reqOf(r); req != "" {
			c := chats[req]
			if c == nil {
				c = &claudeChatAgg{claudeAgg: claudeAgg{id: spanIDHex("chat", sid, req)}}
				chats[req] = c
			}
			c.model = maxStr(c.model, r.Model)
			c.sub = maxNN(c.sub, sub)
			if sub == "" && (!c.hasTurn || seq < c.turnSeq) {
				c.turnSeq, c.hasTurn, c.turnID = seq, true, turns[seq].id
			}
			c.tok = tokens{maxU64(c.tok.in, r.InputTokens), maxU64(c.tok.out, r.OutputTokens),
				maxU64(c.tok.cr, r.CacheReadTokens), maxU64(c.tok.cw, r.CacheWriteTokens)}
			c.b.add(uid, t)
		}
		if call := r.ToolCallID; call != "" {
			o := tools[call]
			if o == nil {
				o = &claudeToolAgg{claudeAgg: claudeAgg{id: spanIDHex("tool", sid, call)}}
				tools[call] = o
			}
			o.name = maxStr(o.name, r.ToolName)
			o.err = maxU64(o.err, r.ToolError)
			o.sub = maxNN(o.sub, sub)
			if r.EventKind == "tool_call" && o.req == "" {
				o.req = reqOf(r)
			}
			if pc := r.ParentToolCallID; pc != "" && pc != call {
				o.pcall = maxNN(o.pcall, pc)
			}
			if sub == "" && (!o.hasTurn || seq < o.turnSeq) {
				o.turnSeq, o.hasTurn = seq, true
			}
			o.b.add(uid, t)
		}
	}

	ap := map[string]*claudeApAgg{}
	for r := range rows {
		sub := subOf(r)
		if sub == "" {
			continue
		}
		p := tools[r.ParentToolCallID]
		if p == nil || p.sub == sub {
			continue
		}
		a := ap[sub]
		if a == nil {
			a = &claudeApAgg{}
			ap[sub] = a
		}
		if a.id == "" {
			a.id = p.id
		}
		if !a.hasTurn && p.hasTurn {
			a.turnID, a.hasTurn = turns[p.turnSeq].id, true
		}
	}

	var rootTok tokens
	turnTok := map[string]tokens{}
	for _, c := range chats {
		rootTok.add(c.tok)
		turn := ""
		if c.hasTurn {
			turn = c.turnID
		} else if a := ap[c.sub]; a != nil && a.hasTurn {
			turn = a.turnID
		}
		if turn != "" {
			s := turnTok[turn]
			s.add(c.tok)
			turnTok[turn] = s
		}
		if a := agents[c.sub]; a != nil {
			a.tok.add(c.tok)
		}
	}

	out := []schema.Span{{SessionID: sid, SpanID: rootID, Kind: schema.SpanInvokeAgent, Name: "claude-code",
		Start: spanTime(rb.startUs), End: spanTime(rb.endUs), Status: schema.SpanOK,
		FirstEventUID: rb.first, LastEventUID: rb.last}}
	rootTok.setSpan(&out[0])
	for _, sub := range sortedMapKeys(agents) {
		a := agents[sub]
		parent := rootID
		if p := ap[sub]; p != nil && p.id != "" {
			parent = p.id
		}
		s := spanAcc{sid: sid, id: a.id, parent: parent, kind: schema.SpanInvokeAgent,
			name: orAgent(a.label, sub), b: a.b, status: schema.SpanOK}
		a.tok.set(&s)
		out = append(out, s.span())
	}
	for _, q := range sortedTurnSeqs(turns) {
		tr := turns[q]
		s := spanAcc{sid: sid, id: tr.id, parent: rootID, kind: schema.SpanTurn,
			name: "turn " + strconv.FormatUint(uint64(q), 10), b: tr.b, status: schema.SpanOK}
		turnTok[tr.id].set(&s)
		out = append(out, s.span())
	}
	for _, req := range sortedMapKeys(chats) {
		c := chats[req]
		s := spanAcc{sid: sid, id: c.id, kind: schema.SpanChat,
			name: orChat(c.model), model: c.model, b: c.b, status: schema.SpanOK}
		s.parent = claudeChatParent(c.sub, c.turnID, c.hasTurn, agents, ap, rootID)
		c.tok.set(&s)
		out = append(out, s.span())
	}
	for _, call := range sortedMapKeys(tools) {
		o := tools[call]
		status := schema.SpanOK
		if o.err > 0 {
			status = schema.SpanError
		}
		acc := spanAcc{sid: sid, id: o.id, kind: schema.SpanExecuteTool,
			name: orTool(o.name), b: o.b, status: status,
			parent: claudeToolParent(o.req, o.pcall, o.sub, o.hasTurn, o.turnSeq, chats, tools, agents, turns, rootID),
		}
		out = append(out, acc.span())
	}
	return out
}

func claudeChatParent(sub, turnID string, hasTurn bool,
	agents map[string]*claudeAgentAgg, ap map[string]*claudeApAgg, rootID string) string {
	if sub != "" {
		if a := agents[sub]; a != nil {
			return a.id
		}
	}
	if hasTurn {
		return turnID
	}
	if sub != "" {
		if p := ap[sub]; p != nil && p.hasTurn {
			return p.turnID
		}
	}
	return rootID
}

func claudeToolParent(req, pcall, sub string, hasTurn bool, turnSeq uint32,
	chats map[string]*claudeChatAgg, tools map[string]*claudeToolAgg,
	agents map[string]*claudeAgentAgg, turns map[uint32]*claudeAgg, rootID string) string {
	if req != "" {
		if c := chats[req]; c != nil {
			return c.id
		}
	}
	if pcall != "" {
		if p := tools[pcall]; p != nil {
			return p.id
		}
	}
	if sub != "" {
		if a := agents[sub]; a != nil {
			return a.id
		}
	}
	if hasTurn {
		if t := turns[turnSeq]; t != nil {
			return t.id
		}
	}
	return rootID
}

func orAgent(label, sub string) string {
	if label != "" {
		return label
	}
	return sub
}
