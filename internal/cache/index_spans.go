package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// SpanDerivation returns one SELECT that yields spans rows, every column of
// schema.SpansDDL, by name, from the events that match where: the
// rows of one harness in the sessions being rebuilt, with event_order and
// turn_seq already current.
type SpanDerivation func(where string) string

var spanDerivations = map[string]SpanDerivation{}

// RegisterSpans adds the spans derivation for a harness, named as in the
// lake path (the normalize parser's name). Call it from an init in the
// harness's own file.
func RegisterSpans(harness string, d SpanDerivation) { spanDerivations[harness] = d }

// spanID is the SQL for a span id: the first 16 hex chars of sha256 over the
// span's source key parts joined by U+001F.
func spanID(parts ...string) string {
	return "substr(sha256(concat_ws(chr(31), " + strings.Join(parts, ", ") + ")), 1, 16)"
}

// spanBounds is the aggregate list for a span's start_time, end_time,
// first_event_uid and last_event_uid over the events grouped into it.
const spanBounds = `min(event_time) AS start_time, max(event_time) AS end_time,
    arg_min(event_uid, event_order) AS first_event_uid, arg_max(event_uid, event_order) AS last_event_uid`

func spanHarnesses() []string {
	hs := make([]string, 0, len(spanDerivations))
	for h := range spanDerivations {
		hs = append(hs, h)
	}
	sort.Strings(hs)
	return hs
}

// deriveSpans rebuilds the spans of the sessions matching inS.
func deriveSpans(inS string) []string {
	stmts := []string{"DELETE FROM spans WHERE " + inS}
	for _, h := range spanHarnesses() {
		where := "(" + inS + ") AND contains(lake_file, " + sqlStr("/harness="+h+"/") + ")"
		stmts = append(stmts, "INSERT INTO spans BY NAME SELECT * FROM ("+spanDerivations[h](where)+")")
	}
	return stmts
}

// spansFingerprint names the registered derivations, so an index whose
// spans were derived under other ones derives them all again.
func spansFingerprint() string {
	sum := sha256.Sum256([]byte(strings.Join(deriveSpans("true"), "\n")))
	return hex.EncodeToString(sum[:])
}

const pendingSpansDDL = "CREATE TABLE IF NOT EXISTS pending_spans (session_id VARCHAR)"

// spansDDL adds what spans and annotations need to an index of any age;
// every statement is a no-op when already applied.
func spansDDL() []string {
	return []string{
		pendingSpansDDL,
		schema.SpansDDL,
		schema.AnnotationsDDL,
		"ALTER TABLE meta ADD COLUMN IF NOT EXISTS spans_fingerprint VARCHAR DEFAULT ''",
	}
}

// convergeSpans derives the spans of every session reorder queued in
// pending_spans, or of every session when the registered derivations changed,
// in a transaction of its own: a failure costs spans, never events, and the
// queue is retried next cycle.
func convergeSpans(ctx context.Context, c *sql.Conn) error {
	var have string
	if err := c.QueryRowContext(ctx, "SELECT spans_fingerprint FROM meta").Scan(&have); err != nil {
		return err
	}
	want := spansFingerprint()
	stmts := []string{"BEGIN"}
	if have != want {
		stmts = append(append(stmts, deriveSpans("true")...), "UPDATE meta SET spans_fingerprint = "+sqlStr(want))
	} else {
		var n int64
		if err := c.QueryRowContext(ctx, "SELECT count(*) FROM pending_spans").Scan(&n); err != nil || n == 0 {
			return err
		}
		stmts = append(append(stmts, "CREATE OR REPLACE TEMP TABLE sp AS SELECT DISTINCT session_id FROM pending_spans"),
			deriveSpans("session_id IN (SELECT session_id FROM sp)")...)
		stmts = append(stmts, "DROP TABLE sp")
	}
	if err := execAll(ctx, c, append(stmts, "DELETE FROM pending_spans", "COMMIT")); err != nil {
		c.ExecContext(context.Background(), "ROLLBACK")
		c.ExecContext(context.Background(), "DROP TABLE IF EXISTS temp.sp")
		return fmt.Errorf("spans: %w", err)
	}
	return nil
}
