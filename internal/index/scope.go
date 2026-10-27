package index

import (
	"context"
	"errors"

	"github.com/candyflipline/mnemonic/internal/auth"
)

// SessionFilter is what the caller on a ctx may read, as predicates over raw
// session ids. A session is the tenant's when every author in its session
// authors is a member, so a session whose authors span two tenants is no
// one's; a delete hides it from everyone, and private hides it from all but
// its sole author. The zero SessionFilter, which a ctx without an
// auth.Identity gets, passes everything.
type SessionFilter struct {
	db     *DB
	scoped bool
	author string
	tenant auth.AuthorSet
	hidden map[string]bool
}

func Sessions(ctx context.Context, db *DB) (SessionFilter, error) {
	id, ok := auth.From(ctx)
	if !ok {
		return SessionFilter{}, nil
	}
	c := controlsFrom(ctx)
	if c == nil {
		return SessionFilter{}, errors.New("index: an identity without session controls on its ctx")
	}
	sets := c.sets()
	f := SessionFilter{db: db, scoped: true, author: id.Author(), tenant: id.Members(), hidden: map[string]bool{}}
	for _, s := range sets.deleted {
		f.hidden[s] = true
	}
	for _, s := range sets.private {
		if !f.soleAuthor(s, f.author) {
			f.hidden[s] = true
		}
	}
	return f, nil
}

func (f SessionFilter) all() bool {
	return !f.scoped || (f.allTenants() && len(f.hidden) == 0)
}

func (f SessionFilter) allTenants() bool { return f.tenant.Except && len(f.tenant.Names) == 0 }

func (f SessionFilter) Author(author string) bool {
	return !f.scoped || f.allTenants() || f.tenant.Has(author)
}

func (f SessionFilter) inTenant(sessionID string) bool {
	if !f.scoped || f.allTenants() {
		return true
	}
	authors := f.db.sessionAuthors(sessionID)
	for _, a := range authors {
		if !f.tenant.Has(a) {
			return false
		}
	}
	return len(authors) > 0
}

func (f SessionFilter) soleAuthor(sessionID, author string) bool {
	authors := f.db.sessionAuthors(sessionID)
	return len(authors) == 1 && (author == "" || authors[0] == author)
}

// Readable is whether the caller may list, search or open sessionID.
func (f SessionFilter) Readable(sessionID string) bool {
	return !f.hidden[sessionID] && f.inTenant(sessionID)
}
