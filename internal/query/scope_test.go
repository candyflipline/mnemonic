package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/store"
)

const scopeSearch = `{"query":"cargo game read","n_hits":50,"event_types":["user_input","assistant_response","reasoning","tool_call","tool_response"]}`

var scopeCalls = []struct{ tool, args string }{
	{"list_sessions", `{"start_datetime":"2020-01-01T00:00:00Z","end_datetime":"2030-01-01T00:00:00Z","limit":20}`},
	{"search_sessions", scopeSearch},
	{"open", `{"id":"` + fxClaudeSessionID + `"}`},
	{"file_attention", `{"path":"Cargo.toml","scope":"all"}`},
	{"get_ingest_status", `{}`},
}

// seen is, per tool, what the caller on ctx sees: session ids for list,
// search, open and file_attention (Cargo.toml and game.js), and the source
// count for get_ingest_status.
func seen(t *testing.T, ctx context.Context, db *cache.Index) map[string][]string {
	t.Helper()
	call := func(tool, args string) any {
		t.Helper()
		tl, _ := Lookup(tool)
		data, _, err := tl.Call(ctx, db.DB(), json.RawMessage(args))
		var argErr *ArgError
		if errors.As(err, &argErr) && argErr.Msg == "session not found" {
			return nil
		}
		if err != nil {
			t.Fatalf("%s %s: %v", tool, args, err)
		}
		return data
	}
	sessionIDs := func(data any, key string) []string {
		var out []string
		for _, s := range data.(map[string]any)[key].([]any) {
			out = append(out, s.(map[string]any)["id"].(string))
		}
		return out
	}
	got := map[string][]string{}
	got["list"] = sessionIDs(call("list_sessions", scopeCalls[0].args), "sessions")
	for _, r := range call("search_sessions", scopeSearch).(map[string]any)["results"].([]map[string]any) {
		if id := r["session"].(map[string]any)["id"].(string); !slices.Contains(got["search"], id) {
			got["search"] = append(got["search"], id)
		}
	}
	got["search"] = slices.Sorted(slices.Values(got["search"]))
	for _, id := range []string{fxClaudeSessionID, fxCodexSessionID, fxCursorSessionID} {
		if call("open", `{"id":"`+id+`"}`) != nil {
			got["open"] = append(got["open"], id)
		}
	}
	for _, path := range []string{"Cargo.toml", "game.js"} {
		got["attention"] = append(got["attention"], sessionIDs(call("file_attention", `{"path":"`+path+`","scope":"all"}`), "sessions")...)
	}
	files := 0
	for _, h := range call("get_ingest_status", `{}`).(*stStatusJSONShape).History {
		files += int(h.FilesTotal)
	}
	got["status"] = []string{strconv.Itoa(files)}
	return got
}

func TestSessionScope(t *testing.T) {
	dataDir := t.TempDir()
	for _, s := range []struct{ author, golden string }{
		{"alice", "codex"}, {"bob", "claude_code"}, {"carol", "cursor"}, {"carol", "cursor_sqlite"},
	} {
		if err := cachetest.Source(dataDir, layout.Key{Author: s.author}, s.golden); err != nil {
			t.Fatal(err)
		}
	}
	ix, err := cache.Open(dataDir, cache.Options{MemoryLimit: "1GB", FlagsSQL: SearchFlagsSQL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	if _, err := ix.Converge(context.Background(), cache.RetryFailed); err != nil {
		t.Fatal(err)
	}
	ctl, err := LoadControls(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	st := &store.Store{DataDir: dataDir}
	tokens := auth.NewTokens(map[string]auth.Token{
		auth.Hash("alice"): {Tenant: "a", Author: "alice"},
		auth.Hash("bob"):   {Tenant: "b", Author: "bob"},
		auth.Hash("carol"): {Tenant: "b", Author: "carol"},
	})
	as := func(token string) context.Context {
		ctx, ok := tokens.Context(WithControls(store.WithStore(context.Background(), st), ctl), token)
		if !ok {
			t.Fatalf("token %s", token)
		}
		return ctx
	}
	act := func(token, tool, args string) func() error {
		return func() error {
			tl, _ := Lookup(tool)
			_, _, err := tl.Call(as(token), ix.DB(), json.RawMessage(args))
			return err
		}
	}
	converge := func() error { _, err := ix.Converge(context.Background(), cache.RetryFailed); return err }
	intrude := func(author, golden, srcID string) func() error {
		return func() error {
			if err := cachetest.Source(dataDir, layout.Key{Author: author, SrcID: srcID}, golden); err != nil {
				return err
			}
			paths, err := filepath.Glob(filepath.Join(dataDir, "events", "author="+author, "machine=*", "harness=*", srcID+".parquet"))
			if err != nil || len(paths) != 1 {
				return fmt.Errorf("intrude %s: %v %v", srcID, paths, err)
			}
			db, err := sql.Open("duckdb", "")
			if err != nil {
				return err
			}
			defer db.Close()
			if _, err := db.Exec("COPY (SELECT * REPLACE (event_uid || '-" + author + "' AS event_uid) FROM read_parquet(" + sqlQuote(paths[0]) +
				") ORDER BY event_uid LIMIT 1) TO " + sqlQuote(paths[0]+".one") + " (FORMAT parquet)"); err != nil {
				return err
			}
			if err := os.Rename(paths[0]+".one", paths[0]); err != nil {
				return err
			}
			return converge()
		}
	}
	futureLine := func(session string) func() error {
		return func() error {
			id, err := parseMcpID(session)
			if err != nil {
				return err
			}
			line := `{"session_id":"` + id.session + `","action":"visibility","visibility":"team","ts":"` +
				time.Now().UTC().Format(time.RFC3339Nano) + `","request_key":"later","added_later":1}` + "\n"
			if _, _, err := st.Append(store.Today("bob", controlHarness, ""), "later", []byte(line)); err != nil {
				return err
			}
			ctl, err = LoadControls(dataDir)
			return err
		}
	}
	then := func(acts ...func() error) func() error {
		return func() error {
			for _, a := range acts {
				if err := a(); err != nil {
					return err
				}
			}
			return nil
		}
	}
	claude, codex, cursor := fxClaudeSessionID, fxCodexSessionID, fxCursorSessionID
	carolSees := map[string][]string{"list": nil, "search": nil, "attention": nil, "status": {"5"}}
	for _, step := range []struct {
		name   string
		act    func() error
		actErr string
		as     string
		want   map[string][]string
	}{
		{name: "tenant b sees none of tenant a", as: "carol", want: map[string][]string{
			"list": {cursor, claude}, "search": {cursor, claude}, "open": {claude, cursor},
			"attention": {claude, cursor}, "status": {"3"}}},
		{name: "tenant a sees only its own", as: "alice", want: map[string][]string{
			"list": {codex}, "search": {codex}, "open": {codex}, "attention": {codex}, "status": {"1"}}},
		{name: "another author of the tenant may not change bob's session", as: "carol",
			act:    act("carol", "set_visibility", `{"session_id":"`+claude+`","visibility":"private"}`),
			actErr: "only the session's author or an admin token may change it", want: map[string][]string{
				"list": {cursor, claude}, "search": {cursor, claude}, "open": {claude, cursor},
				"attention": {claude, cursor}, "status": {"3"}}},
		{name: "another tenant does not find it", as: "carol",
			act:    act("alice", "delete_session", `{"session_id":"`+claude+`"}`),
			actErr: "session not found", want: map[string][]string{
				"list": {cursor, claude}, "search": {cursor, claude}, "open": {claude, cursor},
				"attention": {claude, cursor}, "status": {"3"}}},
		{name: "private hides from the same tenant's other author", as: "carol",
			act: act("bob", "set_visibility", `{"session_id":"`+claude+`","visibility":"private"}`), want: map[string][]string{
				"list": {cursor}, "search": {cursor}, "open": {cursor}, "attention": {cursor}, "status": {"3"}}},
		{name: "private still shows to its author", as: "bob", want: map[string][]string{
			"list": {cursor, claude}, "search": {cursor, claude}, "open": {claude, cursor},
			"attention": {claude, cursor}, "status": {"3"}}},
		{name: "link opens by id across tenants, never lists or searches", as: "bob",
			act: act("alice", "set_visibility", `{"session_id":"`+codex+`","visibility":"link"}`), want: map[string][]string{
				"list": {cursor, claude}, "search": {cursor, claude}, "open": {claude, codex, cursor},
				"attention": {claude, cursor}, "status": {"3"}}},
		{name: "a delete hides at once, without a converge", as: "bob",
			act: act("carol", "delete_session", `{"session_id":"`+cursor+`"}`), want: map[string][]string{
				"list": {claude}, "search": {claude}, "open": {claude, codex}, "attention": {claude}, "status": {"3"}}},
		{name: "a delete hides from its own author too", as: "carol", want: map[string][]string{
			"list": nil, "search": nil, "open": {codex}, "attention": nil, "status": {"3"}}},
		{name: "controls rebuilt from raw keep every record", as: "bob",
			act: func() error { ctl, err = LoadControls(dataDir); return err }, want: map[string][]string{
				"list": {claude}, "search": {claude}, "open": {claude, codex}, "attention": {claude}, "status": {"3"}}},
		{name: "a converge keeps them, and control sources are not ingestion", as: "bob",
			act: func() error { _, err := ix.Converge(context.Background(), cache.RetryFailed); return err }, want: map[string][]string{
				"list": {claude}, "search": {claude}, "open": {claude, codex}, "attention": {claude}, "status": {"3"}}},
		{name: "a request key reused for another action is applied: first team", as: "carol",
			act: act("bob", "set_visibility", `{"session_id":"`+claude+`","visibility":"team","request_key":"k"}`), want: map[string][]string{
				"list": {claude}, "search": {claude}, "open": {claude, codex}, "attention": {claude}, "status": {"3"}}},
		{name: "a request key reused for another action is applied: then private", as: "carol",
			act: act("bob", "set_visibility", `{"session_id":"`+claude+`","visibility":"private","request_key":"k"}`), want: map[string][]string{
				"list": nil, "search": nil, "open": {codex}, "attention": nil, "status": {"3"}}},
		{name: "a control line with a field this release does not know loads and applies", as: "carol",
			act: futureLine(claude), want: map[string][]string{
				"list": {claude}, "search": {claude}, "open": {claude, codex}, "attention": {claude}, "status": {"3"}}},
		{name: "a private session with two authors hides from the first", as: "bob",
			act:  then(act("bob", "set_visibility", `{"session_id":"`+claude+`","visibility":"private"}`), intrude("carol", "claude_code", "ca401")),
			want: map[string][]string{"list": nil, "search": nil, "open": {codex}, "attention": nil, "status": {"4"}}},
		{name: "a private session with two authors hides from the second", as: "carol",
			want: map[string][]string{"list": nil, "search": nil, "open": {codex}, "attention": nil, "status": {"4"}}},
		{name: "another tenant's line in a session hides it from its tenant", as: "alice",
			act:  intrude("bob", "codex", "b0b"),
			want: map[string][]string{"list": nil, "search": nil, "attention": nil, "status": {"1"}}},
		{name: "and from the tenant that pushed the line, link or not", as: "carol", want: carolSees},
		{name: "the pushing tenant cannot trace it", as: "carol",
			act: act("carol", "get_trace", `{"session_id":"`+codex+`"}`), actErr: "session not found", want: carolSees},
		{name: "the pushing tenant cannot delete it", as: "carol",
			act: act("carol", "delete_session", `{"session_id":"`+codex+`"}`), actErr: "session not found", want: carolSees},
		{name: "the pushing author cannot change its visibility", as: "carol",
			act: act("bob", "set_visibility", `{"session_id":"`+codex+`","visibility":"team"}`), actErr: "session not found", want: carolSees},
		{name: "the first tenant cannot delete it either", as: "carol",
			act: act("alice", "delete_session", `{"session_id":"`+codex+`"}`), actErr: "session not found", want: carolSees},
	} {
		if step.act != nil {
			err := step.act()
			if got := ""; err != nil || step.actErr != "" {
				if err != nil {
					got = err.Error()
				}
				if got != step.actErr {
					t.Fatalf("%s: act error %q, want %q", step.name, got, step.actErr)
				}
			}
		}
		if got := seen(t, as(step.as), ix); !reflect.DeepEqual(got, step.want) {
			t.Fatalf("%s:\n got %v\nwant %v", step.name, got, step.want)
		}
	}
}

var observedAt = regexp.MustCompile(`"observed_at_unix_ms":[0-9]+`)

func TestDefaultScopeIsUnscoped(t *testing.T) {
	db := fixtureDB(t)
	ctl, err := LoadControls(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scoped, ok := auth.Authors(map[string]string{auth.Hash("tok"): "test"}).Context(WithControls(context.Background(), ctl), "tok")
	if !ok {
		t.Fatal("token")
	}
	for _, c := range scopeCalls {
		tl, _ := Lookup(c.tool)
		var out [2]string
		for i, ctx := range []context.Context{context.Background(), scoped} {
			data, text, err := tl.Call(ctx, db, json.RawMessage(c.args))
			if err != nil {
				t.Fatalf("%s: %v", c.tool, err)
			}
			b, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			out[i] = observedAt.ReplaceAllString(string(b), "") + "\n" + text
		}
		if out[0] != out[1] {
			t.Fatalf("%s: default scope differs from unscoped:\n%s\n---\n%s", c.tool, out[0], out[1])
		}
	}
}
