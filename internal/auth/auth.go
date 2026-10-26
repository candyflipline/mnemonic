// Package auth maps bearer tokens to identities. The server stores only the
// sha256 of each token; who a request reads and writes as comes from its
// token, never from the request.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// Scope is one thing a token may do.
type Scope string

const (
	Read     Scope = "read"
	Push     Scope = "push"
	Annotate Scope = "annotate"
	Admin    Scope = "admin"
)

// Scopes is every Scope a token entry may name.
var Scopes = [...]Scope{Read, Push, Annotate, Admin}

// DefaultTenant is the tenant of a token entry that names none, and of every
// author no token entry names.
const DefaultTenant = "default"

// DefaultScopes are the scopes of a token entry that names none.
var DefaultScopes = []Scope{Read, Push}

// Token is one configured token: the tenant and author it acts as and what
// it may do. An empty Tenant is DefaultTenant and nil Scopes are
// DefaultScopes.
type Token struct {
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

// Identity is who an authenticated request is; Tokens makes it.
type Identity struct {
	tenant, author string
	scopes         []Scope
	members        AuthorSet
}

func (id Identity) Tenant() string     { return id.tenant }
func (id Identity) Author() string     { return id.author }
func (id Identity) Can(s Scope) bool   { return slices.Contains(id.scopes, s) }
func (id Identity) Members() AuthorSet { return id.members }
func (id Identity) withContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// Tokens is the server's token table, keyed by Hash(token).
type Tokens struct{ byHash map[string]Identity }

// NewTokens builds the table from byHash, which maps Hash(token) to its
// entry.
func NewTokens(byHash map[string]Token) Tokens {
	tenantOf := map[string]string{}
	for _, t := range byHash {
		tenantOf[t.Author] = tenantOr(t.Tenant)
	}
	ids := make(map[string]Identity, len(byHash))
	for h, t := range byHash {
		scopes := t.Scopes
		if scopes == nil {
			scopes = DefaultScopes
		}
		tenant := tenantOr(t.Tenant)
		ids[h] = Identity{tenant: tenant, author: t.Author, scopes: scopes, members: members(tenantOf, tenant)}
	}
	return Tokens{byHash: ids}
}

// Authors is the table where every token in authors (Hash(token) to author)
// is DefaultTenant with DefaultScopes.
func Authors(authors map[string]string) Tokens {
	byHash := make(map[string]Token, len(authors))
	for h, a := range authors {
		byHash[h] = Token{Author: a}
	}
	return NewTokens(byHash)
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

// Hash is the form tokens take in the server config.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Context is ctx carrying the identity of token, false when token is not in
// the table.
func (t Tokens) Context(ctx context.Context, token string) (context.Context, bool) {
	id, ok := t.byHash[Hash(token)]
	if !ok {
		return ctx, false
	}
	return id.withContext(ctx), true
}

// ErrUnauthorized distinguishes an unknown token from a missing scope.
var ErrUnauthorized = errors.New("unauthorized")

// Check authenticates the authorization header, checks scopes, and attaches
// the token identity to ctx. Both HTTP and gRPC use this one policy.
func (t Tokens) Check(ctx context.Context, authorization string, need ...Scope) (context.Context, error) {
	token, bearer := strings.CutPrefix(authorization, "Bearer ")
	ctx, known := t.Context(ctx, token)
	if !bearer || !known {
		return ctx, ErrUnauthorized
	}
	id, _ := From(ctx)
	for _, s := range need {
		if !id.Can(s) {
			return ctx, errors.New("token lacks scope " + string(s))
		}
	}
	return ctx, nil
}

// Require rejects a request without a known bearer token (401) or whose
// token lacks one of need (403), and puts the token's Identity on its ctx.
func (t Tokens) Require(need ...Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, err := t.Check(r.Context(), r.Header.Get("Authorization"), need...)
			if err != nil {
				code := http.StatusForbidden
				if errors.Is(err, ErrUnauthorized) {
					code = http.StatusUnauthorized
				}
				http.Error(w, err.Error(), code)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Middleware is Authors(authors).Require(): any known token, as its author in
// DefaultTenant.
func Middleware(authors map[string]string) func(http.Handler) http.Handler {
	return Authors(authors).Require()
}

type ctxKey struct{}

// From is the Identity Require put on ctx, false when there is none.
func From(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// Author is the authenticated author, "" without one.
func Author(ctx context.Context) string {
	id, _ := From(ctx)
	return id.author
}
