package cache

import (
	"context"
	"database/sql"
	"fmt"
)

// sessionAuthorsDDL adds session_authors, its queue and its built flag to an
// index of any age; every statement is a no-op when already applied.
func sessionAuthorsDDL() []string {
	return []string{
		"CREATE TABLE IF NOT EXISTS session_authors (session_id VARCHAR NOT NULL, author VARCHAR NOT NULL)",
		"CREATE TABLE IF NOT EXISTS pending_session_authors (session_id VARCHAR)",
		"ALTER TABLE meta ADD COLUMN IF NOT EXISTS session_authors_built BOOLEAN DEFAULT false",
	}
}

// lakeAuthorSQL is the author of a lake_rows row, from its lake path.
const lakeAuthorSQL = `regexp_extract(l.lake_file, 'author=([^/]*)/machine=[^/]*/harness=[^/]*/[^/]*$', 1)`

// deriveSessionAuthors rebuilds the session_authors rows of the sessions
// matching inS: every author of a lake row carrying one of the session's
// event uids, winner or not, so a copy that took the winning rows keeps the
// session's first author in the set.
func deriveSessionAuthors(inS string) []string {
	return []string{
		"DELETE FROM session_authors WHERE " + inS,
		`INSERT INTO session_authors SELECT DISTINCT session_id, author FROM (
  WITH e AS (SELECT session_id, author, uid_key, event_uid FROM events WHERE ` + inS + `)
  SELECT session_id, author FROM e
  UNION ALL SELECT e.session_id, ` + lakeAuthorSQL + ` FROM e JOIN lake_rows l ON l.uid_key = e.uid_key AND l.event_uid = e.event_uid)
WHERE session_id IS NOT NULL AND author IS NOT NULL AND author <> ''`,
	}
}

// convergeSessionAuthors derives the session_authors of every session reorder
// queued, or of every session on an index that never built them, in a
// transaction of its own; the queue is retried next cycle on failure.
func convergeSessionAuthors(ctx context.Context, c *sql.Conn) error {
	var built bool
	if err := c.QueryRowContext(ctx, "SELECT session_authors_built FROM meta").Scan(&built); err != nil {
		return err
	}
	stmts := []string{"BEGIN"}
	if !built {
		stmts = append(append(stmts, deriveSessionAuthors("true")...), "UPDATE meta SET session_authors_built = true")
	} else {
		var n int64
		if err := c.QueryRowContext(ctx, "SELECT count(*) FROM pending_session_authors").Scan(&n); err != nil || n == 0 {
			return err
		}
		stmts = append(append(stmts, "CREATE OR REPLACE TEMP TABLE sa AS SELECT DISTINCT session_id FROM pending_session_authors"),
			deriveSessionAuthors("session_id IN (SELECT session_id FROM sa)")...)
		stmts = append(stmts, "DROP TABLE sa")
	}
	if err := execAll(ctx, c, append(stmts, "DELETE FROM pending_session_authors", "COMMIT")); err != nil {
		c.ExecContext(context.Background(), "ROLLBACK")
		c.ExecContext(context.Background(), "DROP TABLE IF EXISTS temp.sa")
		return fmt.Errorf("session authors: %w", err)
	}
	return nil
}
