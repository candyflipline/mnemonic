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

// sessionSpans is sessionID's spans, ordered by start then span_id.
func (db *DB) sessionSpans(sessionID string) []schema.Span {
	st, ok := db.part(sessionID)
	if !ok {
		return nil
	}
	return st.spansOf(sessionID)
}

// sessionSpansSize is the stored size of sessionID's spans value, 0 for a
// session db does not hold.
func (db *DB) sessionSpansSize(sessionID string) int {
	st, ok := db.part(sessionID)
	if !ok {
		return 0
	}
	return len(st.sessionSpansRaw(sessionID))
}

// spanIDHex is the cache's spanID: the first 16 hex chars of sha256 over the
// span's source key parts joined by U+001F.
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

// spanBounds is the aggregate list min(event_time), max(event_time),
// arg_min(event_uid, event_order), arg_max(event_uid, event_order) over the
// rows grouped into a span. Rows arrive in event order, so the first row
// carries the start and the last the end.
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

// spanAcc is one spans row under construction.
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

// spanTime is epoch_ms-of-event_time as a UTC time, the cache's event_time at
// the precision its rows carry.
func spanTime(us int64) time.Time { return time.Unix(us/1e6, (us%1e6)*1e3).UTC() }

// maxStr is SQL max over a string column: empty strings take part, only SQL
// NULL (a field the parser never wrote) is out, and engine rows carry those
// as "" too.
func maxStr(a, b string) string {
	if b > a {
		return b
	}
	return a
}

// maxNN is SQL max over a nullif column: "" stands for NULL and is skipped.
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

// tokens is a span's four usage counters.
type tokens struct{ in, out, cr, cw uint64 }

func (t *tokens) add(o tokens) { t.in += o.in; t.out += o.out; t.cr += o.cr; t.cw += o.cw }

func (t tokens) set(a *spanAcc) { a.in, a.out, a.cr, a.cw = t.in, t.out, t.cr, t.cw }

// setSpan is set for a span already built: the root, whose bounds are the
// session's.
func (t tokens) setSpan(s *schema.Span) {
	s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheWriteTokens = t.in, t.out, t.cr, t.cw
}
