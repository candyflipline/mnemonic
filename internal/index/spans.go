package index

import (
	"crypto/sha256"
	"encoding/hex"
	"iter"
	"time"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// spanBuilders derive a session's spans section from its winner rows of one
// harness, in event order with turn_seq current:
// 59796bc:internal/cache/index_spans_<harness>.go. rows may be ranged over
// more than once; a yielded *Row is valid only during its yield, so a builder
// copies the fields it keeps.
var spanBuilders = map[string]func(harness, sid string, rows iter.Seq[*Row]) []schema.Span{
	"claude-code":     claudeCodeSpans,
	"codex":           codexSpans,
	"cursor":          cursorSpans,
	"otlp":            otlpSpans,
	"pi-coding-agent": piSpans,
	"prime-agent":     primeAgentSpans,
}

func (db *DB) sessionSpans(sessionID string) []schema.Span {
	st, ok := db.part(sessionID)
	if !ok {
		return nil
	}
	return st.spansOf(sessionID)
}

func (db *DB) sessionSpansSize(sessionID string) int {
	st, ok := db.part(sessionID)
	if !ok {
		return 0
	}
	return len(st.sessionSpansRaw(sessionID))
}

func spanIDHex(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0x1f})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

type spanBounds struct {
	startUs, endUs int64
	first, last    string
}

func (b *spanBounds) add(uid string, timeUs int64) {
	if b.first == "" {
		b.first, b.startUs = uid, timeUs
	}
	b.last, b.endUs = uid, timeUs
}

type spanAcc struct {
	sid          string
	id, parent   string
	kind         schema.SpanKind
	name, model  string
	b            spanBounds
	status       schema.SpanStatus
	in, out      uint64
	cr, cw       uint64
	cost         *float64
	first, lastE string
}

func (a *spanAcc) span() schema.Span {
	return schema.Span{
		SessionID:        a.sid,
		SpanID:           a.id,
		ParentSpanID:     a.parent,
		Kind:             a.kind,
		Name:             a.name,
		Start:            spanTime(a.b.startUs),
		End:              spanTime(a.b.endUs),
		Status:           a.status,
		Model:            a.model,
		InputTokens:      a.in,
		OutputTokens:     a.out,
		CacheReadTokens:  a.cr,
		CacheWriteTokens: a.cw,
		Cost:             a.cost,
		FirstEventUID:    a.b.first,
		LastEventUID:     a.b.last,
	}
}

func spanTime(us int64) time.Time { return time.Unix(us/1e6, (us%1e6)*1e3).UTC() }

func maxStr(a, b string) string {
	if b > a {
		return b
	}
	return a
}

func maxNN(a, b string) string {
	if b != "" && b > a {
		return b
	}
	return a
}

func maxU64(a, b uint64) uint64 {
	if b > a {
		return b
	}
	return a
}

type tokens struct{ in, out, cr, cw uint64 }

func (t *tokens) add(o tokens) { t.in += o.in; t.out += o.out; t.cr += o.cr; t.cw += o.cw }

func (t tokens) set(a *spanAcc) { a.in, a.out, a.cr, a.cw = t.in, t.out, t.cr, t.cw }

func (t tokens) setSpan(s *schema.Span) {
	s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheWriteTokens = t.in, t.out, t.cr, t.cw
}
