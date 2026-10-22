package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"sort"
	"strings"
)

// SpanDerivation returns one SELECT that yields spans rows, every column of
// schema.SpansDDL but tenant, by name, from the events that match where: the
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
		stmts = append(stmts, "INSERT INTO spans BY NAME SELECT 'default' AS tenant, * FROM ("+spanDerivations[h](where)+")")
	}
	return stmts
}

// spansFingerprint names the registered derivations, so an index built under
// other ones re-derives every session's spans on Open.
func spansFingerprint() string {
	sum := sha256.Sum256([]byte(strings.Join(deriveSpans("true"), "\n")))
	return hex.EncodeToString(sum[:])
}

func rederiveSpans(ctx context.Context, db *sql.DB) error {
	var have string
	if err := db.QueryRowContext(ctx, "SELECT spans_fingerprint FROM meta").Scan(&have); err != nil {
		return err
	}
	want := spansFingerprint()
	if have == want {
		return nil
	}
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	stmts := append(append([]string{"BEGIN"}, deriveSpans("true")...),
		"UPDATE meta SET spans_fingerprint = "+sqlStr(want), "COMMIT")
	if err := execAll(ctx, c, stmts); err != nil {
		c.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	return nil
}
