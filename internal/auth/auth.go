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

type Scope string

const (
	Read     Scope = "read"
	Push     Scope = "push"
	Annotate Scope = "annotate"
	Admin    Scope = "admin"
)

var Scopes = [...]Scope{Read, Push, Annotate, Admin}

// DefaultTenant is the tenant of a key entry that names none, and of every
// author no key entry names.
const DefaultTenant = "default"

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

// Member is a device key an invite admitted. It acts as Author in Tenant
// with MemberScopes, like a configured key.
type Member struct{ Key, Tenant, Author string }

var MemberScopes = []Scope{Read, Push}

// Keys is who may connect: the configured grants, and the members live
// reports at each call, so a member admitted or removed after boot counts at
// once.
type Keys struct {
	grants map[string]Grant
	live   func() []Member
}

func NewKeys(byKey map[string]Grant) Keys { return Keys{grants: byKey} }

// WithMembers is k admitting the members live reports, which it calls on
// every Identify and on every read of an Identity's member set.
func (k Keys) WithMembers(live func() []Member) Keys {
	k.live = live
	return k
}

func (k Keys) members() []Member {
	if k.live == nil {
		return nil
	}
	return k.live()
}

// Identify is the Identity of key: its grant, else the live member it is.
func (k Keys) Identify(key string) (Identity, bool) {
	if g, ok := k.grants[key]; ok {
		scopes := g.Scopes
		if scopes == nil {
			scopes = DefaultScopes
		}
		return Identity{tenant: tenantOr(g.Tenant), author: g.Author, scopes: scopes, keys: &k}, true
	}
	for _, m := range k.members() {
		if m.Key == key {
			return Identity{tenant: m.Tenant, author: m.Author, scopes: MemberScopes, keys: &k, member: key}, true
		}
	}
	return Identity{}, false
}

// tenantOf is every author's tenant: the grants' and the live members'. A
// grant wins over a member naming the same author.
func (k Keys) tenantOf() map[string]string {
	out := map[string]string{}
	for _, m := range k.members() {
		out[m.Author] = m.Tenant
	}
	for _, g := range k.grants {
		out[g.Author] = tenantOr(g.Tenant)
	}
	return out
}

// Identity is who an authenticated request is. Keys.Identify and Local make
// it and no one else can. The zero Identity grants nothing. Its member set
// and, for a member, whether it is still admitted are read from its Keys at
// each call, so an open connection sees a join or a revoke at once.
type Identity struct {
	tenant, author string
	scopes         []Scope
	keys           *Keys
	member         string
	local          bool
}

func (id Identity) Tenant() string { return id.tenant }

func (id Identity) Author() string { return id.author }

func (id Identity) Can(s Scope) bool { return slices.Contains(id.scopes, s) }

func (id Identity) Members() AuthorSet {
	switch {
	case id.local:
		return AuthorSet{Except: true}
	case id.keys == nil:
		return AuthorSet{}
	}
	return members(id.keys.tenantOf(), id.tenant)
}

// Admitted is false once the invite that admitted a member is revoked, and
// true for every other Identity that grants anything.
func (id Identity) Admitted() bool {
	if id.member == "" {
		return id.scopes != nil
	}
	for _, m := range id.keys.members() {
		if m.Key == id.member {
			return true
		}
	}
	return false
}

// TenantOf is the tenant of author as the configured keys and the live
// members name it, false when none does.
func (id Identity) TenantOf(author string) (string, bool) {
	if id.keys == nil {
		return "", false
	}
	t, ok := id.keys.tenantOf()[author]
	return t, ok
}

// Local is the in-process identity of local mode: DefaultTenant, every scope.
func Local(author string) Identity {
	return Identity{tenant: DefaultTenant, author: author, scopes: slices.Clone(Scopes[:]), local: true}
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

func From(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

func Author(ctx context.Context) string {
	id, _ := From(ctx)
	return id.Author()
}
