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

const DefaultTenant = "default"

type Grant struct {
	Tenant, Author string
	Scopes         []Scope
}

type AuthorSet struct {
	Names  []string
	Except bool
}

func (s AuthorSet) Has(author string) bool { return slices.Contains(s.Names, author) != s.Except }

type Member struct{ Key, Tenant, Author string }

var MemberScopes = []Scope{Read, Push}

// Roster is the members that are admitted right now. Member answers one key
// in constant time.
type Roster interface {
	Member(key string) (Member, bool)
	Members() []Member
}

type Keys struct {
	grants map[string]Grant
	roster Roster
}

func NewKeys(byKey map[string]Grant) Keys { return Keys{grants: byKey} }

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

func (k Keys) Identify(key string) (Identity, bool) {
	if g, ok := k.grants[key]; ok {
		return Identity{tenant: g.Tenant, author: g.Author, scopes: g.Scopes, keys: &k}, true
	}
	if m, ok := k.member(key); ok {
		return Identity{tenant: m.Tenant, author: m.Author, scopes: MemberScopes, keys: &k, member: key}, true
	}
	return Identity{}, false
}

func (k Keys) tenantOf() map[string]string {
	out := map[string]string{}
	if k.roster != nil {
		for _, m := range k.roster.Members() {
			out[m.Author] = m.Tenant
		}
	}
	for _, g := range k.grants {
		out[g.Author] = g.Tenant
	}
	return out
}

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
	s := AuthorSet{Except: id.tenant == DefaultTenant && id.Can(Admin)}
	for a, t := range id.keys.tenantOf() {
		if (t == id.tenant) != s.Except {
			s.Names = append(s.Names, a)
		}
	}
	sort.Strings(s.Names)
	return s
}

func (id Identity) Admitted() bool {
	if id.member == "" {
		return id.scopes != nil
	}
	_, ok := id.keys.member(id.member)
	return ok
}

func (id Identity) TenantOf(author string) (string, bool) {
	if id.keys == nil {
		return "", false
	}
	t, ok := id.keys.tenantOf()[author]
	return t, ok
}

func Local(author string) Identity {
	return Identity{tenant: DefaultTenant, author: author, scopes: slices.Clone(Scopes[:]), local: true}
}

// NewContext is ctx carrying id. Only a transport that has proven the caller
// calls it.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

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
