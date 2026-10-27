package auth

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
)

func TestKeys(t *testing.T) {
	keys := NewKeys(map[string]Grant{
		"k-chris": {Tenant: DefaultTenant, Author: "chris", Scopes: []Scope{Read, Push}},
		"k-sam":   {Tenant: DefaultTenant, Author: "sam", Scopes: []Scope{Read}},
		"k-acme":  {Tenant: "acme", Author: "ann", Scopes: []Scope{Read, Annotate}},
		"k-bob":   {Tenant: "acme", Author: "bob", Scopes: []Scope{Read, Push}},
	})
	for _, tc := range []struct {
		key             string
		author, tenant  string
		read, push, ann bool
		members         AuthorSet
	}{
		{"k-chris", "chris", "default", true, true, false, AuthorSet{Names: []string{"chris", "sam"}}},
		{"k-sam", "sam", "default", true, false, false, AuthorSet{Names: []string{"chris", "sam"}}},
		{"k-acme", "ann", "acme", true, false, true, AuthorSet{Names: []string{"ann", "bob"}}},
		{"k-bob", "bob", "acme", true, true, false, AuthorSet{Names: []string{"ann", "bob"}}},
	} {
		id, ok := keys.Identify(tc.key)
		if !ok {
			t.Fatalf("%s: not identified", tc.key)
		}
		got := []any{id.Author(), id.Tenant(), id.Can(Read), id.Can(Push), id.Can(Annotate), id.Members()}
		want := []any{tc.author, tc.tenant, tc.read, tc.push, tc.ann, tc.members}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", tc.key, got, want)
		}
	}
	if _, ok := keys.Identify("k-unknown"); ok {
		t.Error("unknown key identified")
	}
}

type roster map[string]Member

func (r roster) Member(key string) (Member, bool) { m, ok := r[key]; return m, ok }

func (r roster) Members() []Member { return slices.Collect(maps.Values(r)) }

func TestUnnamedAuthorIsRefused(t *testing.T) {
	keys := NewKeys(map[string]Grant{
		"k-op":  {Tenant: DefaultTenant, Author: "chris", Scopes: []Scope{Read, Push, Admin}},
		"k-sam": {Tenant: DefaultTenant, Author: "sam", Scopes: []Scope{Read, Push}},
	}).WithMembers(roster{"k-dana": {Key: "k-dana", Tenant: DefaultTenant, Author: "dana"}})
	for _, tc := range []struct {
		key, author string
		reads       bool
	}{
		{"k-sam", "sam", true},
		{"k-sam", "chris", true},
		{"k-sam", "dana", true},
		{"k-sam", "eve", false},
		{"k-dana", "sam", true},
		{"k-dana", "eve", false},
		{"k-op", "dana", true},
		{"k-op", "eve", true},
	} {
		id, _ := keys.Identify(tc.key)
		if got := id.Members().Has(tc.author); got != tc.reads {
			t.Errorf("%s reads %s: %v, want %v", tc.key, tc.author, got, tc.reads)
		}
	}
	op, _ := keys.Identify("k-op")
	if got := op.Members(); !reflect.DeepEqual(got, AuthorSet{Except: true}) {
		t.Errorf("admin k-op reads %+v, want every author (export include_private's scope)", got)
	}
}

func TestRequire(t *testing.T) {
	reader, _ := NewKeys(map[string]Grant{"k": {Tenant: DefaultTenant, Author: "a", Scopes: []Scope{Read}}}).Identify("k")
	h := Require(Push)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for name, tc := range map[string]struct {
		ctx  context.Context
		code int
		body string
	}{
		"no identity":   {context.Background(), http.StatusUnauthorized, "unauthorized\n"},
		"missing scope": {NewContext(context.Background(), reader), http.StatusForbidden, "key lacks scope push\n"},
		"local":         {NewContext(context.Background(), Local("me")), http.StatusNoContent, ""},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil).WithContext(tc.ctx))
		if rec.Code != tc.code || rec.Body.String() != tc.body {
			t.Errorf("%s: %d %q, want %d %q", name, rec.Code, rec.Body.String(), tc.code, tc.body)
		}
	}
}

func TestParseKey(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo", true},
		{"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURp", false},
		{"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo=", false},
		{"short", false},
	} {
		if _, err := ParseKey(tc.in); (err == nil) != tc.ok {
			t.Errorf("ParseKey(%q) error %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}
