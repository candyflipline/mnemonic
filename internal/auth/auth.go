// Package auth maps client public keys to identities. The transport proves
// the key and attaches the Identity to the request's context; who a request
// reads and writes as comes from that key, never from the request.
package auth

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/candyflipline/mnemonic/internal/wallet"
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

// Identity is who an authenticated request is: one sealed principal, either
// a configured key's or a wallet reader's. Keys, Local and WalletReader make
// it and no one else can, so a wallet reader never carries a tenant, a member
// set or a scope beyond Read. The zero Identity grants nothing.
type Identity struct{ p principal }

type principal interface {
	tenant() string
	author() string
	can(Scope) bool
	members() AuthorSet
}

type configured struct {
	tenantName, authorName string
	scopes                 []Scope
	set                    AuthorSet
}

func (c configured) tenant() string     { return c.tenantName }
func (c configured) author() string     { return c.authorName }
func (c configured) can(s Scope) bool   { return slices.Contains(c.scopes, s) }
func (c configured) members() AuthorSet { return c.set }

// walletReader is a device a wallet bound: no tenant, an empty inclusive
// member set and Read alone. What it reads is the sessions shared with its
// wallet, which the session controls answer per request, until expires.
type walletReader struct {
	wallet  wallet.Address
	expires time.Time
}

func (walletReader) tenant() string     { return "" }
func (w walletReader) author() string   { return w.wallet.Author() }
func (walletReader) can(s Scope) bool   { return s == Read }
func (walletReader) members() AuthorSet { return AuthorSet{} }

func (id Identity) Tenant() string {
	if id.p == nil {
		return ""
	}
	return id.p.tenant()
}

func (id Identity) Author() string {
	if id.p == nil {
		return ""
	}
	return id.p.author()
}

func (id Identity) Can(s Scope) bool { return id.p != nil && id.p.can(s) }

func (id Identity) Members() AuthorSet {
	if id.p == nil {
		return AuthorSet{}
	}
	return id.p.members()
}

// Wallet is the wallet a wallet reader reads as and the moment its binding
// expires, false for any other Identity.
func (id Identity) Wallet() (wallet.Address, time.Time, bool) {
	w, ok := id.p.(walletReader)
	return w.wallet, w.expires, ok
}

func WalletReader(b wallet.Binding) Identity {
	c := b.Claim()
	return Identity{p: walletReader{wallet: c.Wallet, expires: c.Expires}}
}

type Keys struct{ byKey map[string]Identity }

// NewKeys builds the table from byKey, which maps key text to its grant.
// Members are computed over every grant.
func NewKeys(byKey map[string]Grant) Keys {
	tenantOf := map[string]string{}
	for _, g := range byKey {
		tenantOf[g.Author] = g.Tenant
	}
	ids := make(map[string]Identity, len(byKey))
	for k, g := range byKey {
		ids[k] = Identity{p: configured{tenantName: g.Tenant, authorName: g.Author, scopes: g.Scopes, set: members(tenantOf, g.Tenant)}}
	}
	return Keys{byKey: ids}
}

func (k Keys) Identify(key string) (Identity, bool) {
	id, ok := k.byKey[key]
	return id, ok
}

// Local is the in-process identity of local mode: DefaultTenant, every scope.
func Local(author string) Identity {
	return Identity{p: configured{tenantName: DefaultTenant, authorName: author, scopes: slices.Clone(Scopes[:]), set: AuthorSet{Except: true}}}
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
