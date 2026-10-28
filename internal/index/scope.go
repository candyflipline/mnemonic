package index

import (
	"context"
	"errors"

	"github.com/candyflipline/mnemonic/internal/auth"
)

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
		if !db.soleAuthor(s, f.author) {
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

func (f SessionFilter) Readable(sessionID string) bool {
	return !f.hidden[sessionID] && f.inTenant(sessionID)
}

func (db *DB) soleAuthor(sessionID, author string) bool {
	authors := db.sessionAuthors(sessionID)
	return len(authors) == 1 && (author == "" || authors[0] == author)
}
