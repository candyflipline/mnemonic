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
// tenant's when every author in session_authors is a member, so a session
// whose authors span two tenants is no one's; a delete hides it from
// everyone, private hides it from all but its sole author, and link opens a
// session with a sole author by id to any caller. The zero SessionFilter,
// which a ctx without an auth.Identity gets, filters nothing; every predicate
// method then returns "".
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
		mine, err := soleAuthor(ctx, db, id.Author(), sets.private)
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

// soleAuthor is which of sessions have author as their only author.
func soleAuthor(ctx context.Context, db *sql.DB, author string, sessions []string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT session_id FROM session_authors WHERE session_id IN ("+qQuoteJoin(sessions)+
		") GROUP BY session_id HAVING bool_and(author = "+sqlQuote(author)+")")
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
		return col + " IN (SELECT session_id FROM session_authors GROUP BY session_id HAVING bool_and(" + a + "))"
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
// plus link sessions of any tenant that have a sole author.
func (f SessionFilter) Opened(col string) string {
	t := f.inTenant(col)
	if t != "" && len(f.link) > 0 {
		t = "(" + t + " OR " + col + " IN (SELECT session_id FROM session_authors WHERE session_id IN (" + qQuoteJoin(f.link) +
			") GROUP BY session_id HAVING count(*) = 1))"
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
