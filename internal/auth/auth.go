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

// Roster is the members that are admitted right now. Member answers one key
// in constant time; EachMember visits every member and fn must not call back
// into the roster.
type Roster interface {
	Member(key string) (Member, bool)
	EachMember(fn func(Member))
}

// Keys is who may connect: the configured grants, and the members the roster
// holds at each call, so a member admitted or removed after boot counts at
// once.
type Keys struct {
	grants map[string]Grant
	roster Roster
}

func NewKeys(byKey map[string]Grant) Keys { return Keys{grants: byKey} }

// WithMembers is k admitting the members r holds, which it asks on every
// Identify and on every read of an Identity's member set.
func (k Keys) WithMembers(r Roster) Keys {
	k.roster = r
	return k
}

func (k Keys) member(key string) (Member, bool) {
	if k.roster == nil {
		return Member{}, false
	}
	return k.roster.Member(key)
}

// Identify is the Identity of key: its grant, else the live member it is.
func (k Keys) Identify(key string) (Identity, bool) {
	if g, ok := k.grants[key]; ok {
		return Identity{tenant: g.Tenant, author: g.Author, scopes: g.Scopes, keys: &k}, true
	}
	if m, ok := k.member(key); ok {
		return Identity{tenant: m.Tenant, author: m.Author, scopes: MemberScopes, keys: &k, member: key}, true
	}
	return Identity{}, false
}

// tenantOf is every author's tenant: the grants' and the live members'. A
// grant wins over a member naming the same author.
func (k Keys) tenantOf() map[string]string {
	out := map[string]string{}
	if k.roster != nil {
		k.roster.EachMember(func(m Member) { out[m.Author] = m.Tenant })
	}
	for _, g := range k.grants {
		out[g.Author] = g.Tenant
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
	_, ok := id.keys.member(id.member)
	return ok
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

// Require rejects a request whose ctx carries no Identity, or a member's
// whose invite was revoked since its connection opened (401), or whose
// Identity lacks one of need (403).
func Require(need ...Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := From(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if !id.Admitted() {
				http.Error(w, "unauthorized: this machine's invite was revoked", http.StatusUnauthorized)
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
