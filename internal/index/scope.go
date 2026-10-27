package index

import (
	"context"
	"errors"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
)

// SessionFilter is what the caller on a ctx may read, as predicates over raw
// session ids. A session is the tenant's when every author in its session
// authors is a member, so a session whose authors span two tenants is no
// one's; a delete hides it from everyone, private hides it from all but its
// sole author and the wallets it is shared with, and link opens a session
// with a sole author by id to any caller. A wallet reader has no members: it
// lists exactly the sessions shared with its wallet, read from the controls
// when the filter is made, and nothing once its binding has expired. The zero SessionFilter, which a ctx without an
// auth.Identity gets, passes everything.
type SessionFilter struct {
	db     *DB
	scoped bool
	author string
	tenant auth.AuthorSet
	hidden map[string]bool
	link   map[string]bool
	shared map[string]bool
}

// Sessions is the SessionFilter of the caller on ctx over db.
func Sessions(ctx context.Context, db *DB) (SessionFilter, error) {
	id, ok := auth.From(ctx)
	if !ok {
		return SessionFilter{}, nil
	}
	c := controlsFrom(ctx)
	if c == nil {
		return SessionFilter{}, errors.New("index: an identity without session controls on its ctx")
	}
	w, expires, isWallet := id.Wallet()
	if isWallet && !time.Now().Before(expires) {
		return SessionFilter{db: db, scoped: true}, argErrorf("the wallet binding of %s expired at %s; bind again with mnemonic wallet bind", w, expires.Format(time.RFC3339))
	}
	sets := c.sets()
	f := SessionFilter{db: db, scoped: true, author: id.Author(), tenant: id.Members(), hidden: map[string]bool{}, link: map[string]bool{}}
	for _, s := range sets.deleted {
		f.hidden[s] = true
	}
	if isWallet {
		f.shared = c.sharedWith(w)
	}
	for _, s := range sets.private {
		if !f.soleAuthor(s, f.author) && !f.shared[s] {
			f.hidden[s] = true
		}
	}
	for _, s := range sets.link {
		f.link[s] = true
	}
	return f, nil
}

// all reports that the filter passes every session, so callers skip it.
func (f SessionFilter) all() bool {
	return !f.scoped || (f.allTenants() && len(f.hidden) == 0)
}

func (f SessionFilter) allTenants() bool { return f.tenant.Except && len(f.tenant.Names) == 0 }

// Author is the tenant predicate over one author.
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

// Listed is whether the caller may list or search sessionID: in its tenant,
// or shared with its wallet.
func (f SessionFilter) Listed(sessionID string) bool {
	return !f.hidden[sessionID] && (f.inTenant(sessionID) || f.shared[sessionID])
}

// Opened is whether the caller may open sessionID by id: Listed, plus link
// sessions of any tenant that have a sole author.
func (f SessionFilter) Opened(sessionID string) bool {
	if f.hidden[sessionID] {
		return false
	}
	return f.inTenant(sessionID) || f.shared[sessionID] || (f.link[sessionID] && f.soleAuthor(sessionID, ""))
}
