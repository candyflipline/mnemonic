package query

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/candyflipline/mnemonic/internal/auth"
)

// SessionFilter is what the caller on a ctx may read, as SQL predicates over
// a column holding raw session ids (events.session_id). A session is the
// tenant's when one of its authors is a member; a delete hides it from
// everyone, private hides it from all but its authors, and link opens it by
// id to any caller. The zero SessionFilter, which a ctx without an
// auth.Identity gets, filters nothing; every predicate method then returns "".
type SessionFilter struct {
	scoped bool
	tenant auth.AuthorSet
	hidden []string
	link   []string
}

// Sessions is the SessionFilter of the caller on ctx.
func Sessions(ctx context.Context, db *sql.DB) (SessionFilter, error) {
	id, ok := auth.From(ctx)
	if !ok {
		return SessionFilter{}, nil
	}
	c := controlsFrom(ctx)
	if c == nil {
		return SessionFilter{}, errors.New("query: an identity without session controls on its ctx")
	}
	sets := c.sets()
	hidden := sets.deleted
	if len(sets.private) > 0 {
		mine, err := authoredBy(ctx, db, id.Author(), sets.private)
		if err != nil {
			return SessionFilter{}, err
		}
		for _, s := range sets.private {
			if !mine[s] {
				hidden = append(hidden, s)
			}
		}
		slices.Sort(hidden)
	}
	return SessionFilter{scoped: true, tenant: id.Members(), hidden: hidden, link: sets.link}, nil
}

func authoredBy(ctx context.Context, db *sql.DB, author string, sessions []string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT session_id FROM events WHERE author = "+sqlQuote(author)+
		" AND session_id IN ("+qQuoteJoin(sessions)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

// Authors is the tenant predicate over an author column.
func (f SessionFilter) Authors(col string) string {
	if !f.scoped || (f.tenant.Except && len(f.tenant.Names) == 0) {
		return ""
	}
	op := " IN ("
	if f.tenant.Except {
		op = " NOT IN ("
	}
	return col + op + qQuoteJoin(f.tenant.Names) + ")"
}

func (f SessionFilter) inTenant(col string) string {
	if a := f.Authors("author"); a != "" {
		return col + " IN (SELECT session_id FROM events WHERE " + a + ")"
	}
	return ""
}

func (f SessionFilter) notHidden(col string) string {
	if len(f.hidden) == 0 {
		return ""
	}
	return col + " NOT IN (" + qQuoteJoin(f.hidden) + ")"
}

// Listed is the predicate for sessions the caller may list or search.
func (f SessionFilter) Listed(col string) string {
	return and(f.inTenant(col), f.notHidden(col))
}

// Opened is the predicate for sessions the caller may open by id: Listed,
// plus link sessions of any tenant.
func (f SessionFilter) Opened(col string) string {
	t := f.inTenant(col)
	if t != "" && len(f.link) > 0 {
		t = "(" + t + " OR " + col + " IN (" + qQuoteJoin(f.link) + "))"
	}
	return and(t, f.notHidden(col))
}

func and(preds ...string) string {
	var kept []string
	for _, p := range preds {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " AND ")
}
