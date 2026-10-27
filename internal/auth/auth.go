// Package auth maps client public keys to identities. The transport proves
// the key and attaches the Identity to the request's context; who a request
// reads and writes as comes from that key, never from the request.
package auth

import (
	"context"
	"net/http"
	"slices"
	"sort"
)

// Scope is one thing a key may do.
type Scope string

const (
	Read     Scope = "read"
	Push     Scope = "push"
	Annotate Scope = "annotate"
	Admin    Scope = "admin"
)

// Scopes is every Scope a key entry may name.
var Scopes = [...]Scope{Read, Push, Annotate, Admin}

// DefaultTenant is the tenant of a key entry that names none, and of every
// author no key entry names.
const DefaultTenant = "default"

// DefaultScopes are the scopes of a key entry that names none.
var DefaultScopes = []Scope{Read, Push}

// Grant is one configured key's rights: the tenant and author it acts as and
// what it may do. An empty Tenant is DefaultTenant and nil Scopes are
// DefaultScopes.
type Grant struct {
	Tenant, Author string
	Scopes         []Scope
}

// AuthorSet is the authors of one tenant: Names, or every author but Names
// when Except.
type AuthorSet struct {
	Names  []string
	Except bool
}

func (s AuthorSet) Has(author string) bool { return slices.Contains(s.Names, author) != s.Except }

// Identity is who an authenticated request is; Keys makes it.
type Identity struct {
	tenant, author string
	scopes         []Scope
	members        AuthorSet
}

func (id Identity) Tenant() string     { return id.tenant }
func (id Identity) Author() string     { return id.author }
func (id Identity) Can(s Scope) bool   { return slices.Contains(id.scopes, s) }
func (id Identity) Members() AuthorSet { return id.members }

// Keys is the server's key table, keyed by the key text EncodeKey writes.
type Keys struct{ byKey map[string]Identity }

// NewKeys builds the table from byKey, which maps key text to its grant.
// Members are computed over every grant.
func NewKeys(byKey map[string]Grant) Keys {
	tenantOf := map[string]string{}
	for _, g := range byKey {
		tenantOf[g.Author] = tenantOr(g.Tenant)
	}
	ids := make(map[string]Identity, len(byKey))
	for k, g := range byKey {
		scopes := g.Scopes
		if scopes == nil {
			scopes = DefaultScopes
		}
		tenant := tenantOr(g.Tenant)
		ids[k] = Identity{tenant: tenant, author: g.Author, scopes: scopes, members: members(tenantOf, tenant)}
	}
	return Keys{byKey: ids}
}

// Identify is the Identity of key, false when key is not in the table.
func (k Keys) Identify(key string) (Identity, bool) {
	id, ok := k.byKey[key]
	return id, ok
}

// Local is the in-process identity of local mode: DefaultTenant, every scope.
func Local(author string) Identity {
	return Identity{tenant: DefaultTenant, author: author, scopes: slices.Clone(Scopes[:]), members: AuthorSet{Except: true}}
}

func tenantOr(t string) string {
	if t == "" {
		return DefaultTenant
	}
	return t
}

func members(tenantOf map[string]string, tenant string) AuthorSet {
	var s AuthorSet
	s.Except = tenant == DefaultTenant
	for a, t := range tenantOf {
		if (t == tenant) != s.Except {
			s.Names = append(s.Names, a)
		}
	}
	sort.Strings(s.Names)
	return s
}

// NewContext is ctx carrying id. Only a transport that has proven the caller
// calls it.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// Require rejects a request whose ctx carries no Identity (401) or whose
// Identity lacks one of need (403).
func Require(need ...Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := From(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			for _, s := range need {
				if !id.Can(s) {
					http.Error(w, "key lacks scope "+string(s), http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

type ctxKey struct{}

// From is the Identity the transport put on ctx, false when there is none.
func From(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// Author is the authenticated author, "" without one.
func Author(ctx context.Context) string {
	id, _ := From(ctx)
	return id.author
}
