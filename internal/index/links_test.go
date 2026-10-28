package index

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
)

func TestAdminLinksAnotherAuthorsSession(t *testing.T) {
	db, _ := newQueryDB(t)
	data := t.TempDir()
	ctl, err := LoadControls(data)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	keys := auth.NewKeys(map[string]auth.Grant{"k-admin": {Tenant: auth.DefaultTenant, Author: "root", Scopes: []auth.Scope{auth.Read, auth.Push, auth.Admin}}})
	admin, _ := keys.Identify("k-admin")
	var sb string
	for _, p := range db.parts {
		for i := 0; i < p.nSessions; i++ {
			if id := p.session(i).id; strings.Join(db.sessionAuthors(id), ",") == "b" {
				sb = id
			}
		}
	}
	site := LinkSite{BaseURL: "https://share.example", MarkdownSize: func(*DB, string, ReadLink, int) (int64, error) { return 1, nil }}
	ctx := WithLinkSite(context.Background(), site)
	raw, _ := json.Marshal(map[string]any{"session_ids": []string{FormatSessionID(sb)}})
	d, _, err := Call(ctx, &Caller{Identity: admin, Controls: ctl, Store: st}, db, "share_link", raw)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		URL      string
		Sessions []linkSessionRow
	}
	json.Unmarshal(d, &m)
	h, _ := ParseToken(strings.TrimPrefix(m.URL, "https://share.example/s/"))
	rl, err := ctl.OpenRead(db, h, time.Now())
	if err != nil || len(rl.SessionIDs) != 1 || rl.SessionIDs[0] != sb || len(m.Sessions) != 1 {
		t.Fatalf("OpenRead sessions %v (%v), create answered %d rows, want [%s] and 1", rl.SessionIDs, err, len(m.Sessions), sb)
	}
}
