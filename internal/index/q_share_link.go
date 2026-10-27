package index

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
)

// LinkSite is where read links are served, which only the server knows:
// BaseURL is the public origin, and MarkdownSize is the size of session n of
// a read link as its .md URL renders it.
type LinkSite struct {
	BaseURL      string
	MarkdownSize func(db *DB, token string, l ReadLink, n int) (int64, error)
}

type linkSiteKey struct{}

func WithLinkSite(ctx context.Context, s LinkSite) context.Context {
	return context.WithValue(ctx, linkSiteKey{}, s)
}

func (s LinkSite) ReadURL(token string) string   { return s.BaseURL + "/s/" + token }
func (s LinkSite) InviteURL(token string) string { return s.BaseURL + "/i/" + token }

const (
	defaultReadDays   = 30
	defaultInviteDays = 7
	maxLinkDays       = 365
	maxLinkSessions   = 50
	maxTitleRunes     = 200
	maxNoteRunes      = 2000
)

// pacific is the zone every time the owner reads is printed in. The binary
// may run where no zoneinfo is installed, hence the time/tzdata import.
var pacific = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}()

// PacificText is t as the owner reads every time: in San Francisco time.
func PacificText(t time.Time) string { return t.In(pacific).Format("Jan 2, 2006 3:04 PM MST") }

type shareLink struct{}

func init() { register(shareLink{}) }

func (shareLink) Name() string { return "share_link" }

type shareLinkArgs struct {
	SessionIDs []string `json:"session_ids"`
	Title      string   `json:"title"`
	Note       string   `json:"note"`
	Days       int      `json:"days"`
	Invite     string   `json:"invite"`
	Revoke     string   `json:"revoke"`
}

func (shareLink) Call(ctx context.Context, db *DB, raw json.RawMessage) (any, string, error) {
	var a shareLinkArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, "", argErrorf("invalid arguments: %s", err)
	}
	id, ok := auth.From(ctx)
	c, st := controlsFrom(ctx), store.From(ctx)
	if !ok || c == nil || st == nil {
		return nil, "", errors.New("index: share_link needs an identity, controls and a store on ctx")
	}
	modes := 0
	for _, set := range []bool{a.SessionIDs != nil, a.Invite != "", a.Revoke != ""} {
		if set {
			modes++
		}
	}
	readOnly := a.Title != "" || a.Note != ""
	switch {
	case modes > 1:
		return nil, "", argErrorf("pass one of session_ids, invite or revoke")
	case a.Revoke != "":
		if readOnly || a.Days != 0 {
			return nil, "", argErrorf("revoke takes no other argument")
		}
		if err := c.revoke(st, id, a.Revoke); err != nil {
			return nil, "", err
		}
		return map[string]any{"link_id": a.Revoke, "revoked": true}, "Link " + a.Revoke + " is revoked.", nil
	case modes == 0 && (readOnly || a.Days != 0):
		return nil, "", argErrorf("title, note and days need session_ids or invite")
	case modes == 0:
		return listLinks(c, id)
	}
	site, ok := ctx.Value(linkSiteKey{}).(LinkSite)
	if !ok {
		return nil, "", argErrorf("this server serves no links: its config has no [share] section")
	}
	if a.Invite != "" {
		if readOnly {
			return nil, "", argErrorf("an invite takes no title or note")
		}
		days, err := linkDays(a.Days, defaultInviteDays)
		if err != nil {
			return nil, "", err
		}
		return createInvite(c, st, site, id, a.Invite, days)
	}
	days, err := linkDays(a.Days, defaultReadDays)
	if err != nil {
		return nil, "", err
	}
	return createRead(ctx, db, c, st, site, id, a, days)
}

func linkDays(days, def int) (time.Duration, error) {
	if days == 0 {
		days = def
	}
	if days < 1 || days > maxLinkDays {
		return 0, argErrorf("days must be from 1 to %d", maxLinkDays)
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

func createInvite(c *Controls, st *store.Committer, site LinkSite, id auth.Identity, author string, days time.Duration) (any, string, error) {
	if !wire.ValidSegment(author) {
		return nil, "", argErrorf("invite must be an author name: lowercase letters, digits, '.', '_' or '-'")
	}
	if author != id.Author() && !id.Can(auth.Admin) {
		return nil, "", argErrorf("inviting a machine for another author needs an admin key")
	}
	if t, ok := id.TenantOf(author); ok && t != id.Tenant() {
		return nil, "", argErrorf("author %s is in another tenant", author)
	}
	c.write.Lock()
	defer c.write.Unlock()
	now := time.Now().UTC()
	if _, ok := c.inviteTenant(author, id.Tenant(), now); ok {
		return nil, "", argErrorf("author %s is invited into another tenant", author)
	}
	r, token, err := c.newLink(st, controlRecord{Kind: linkInvite, InviteAuthor: author, Expires: now.Add(days),
		Author: id.Author(), Tenant: id.Tenant()})
	if err != nil {
		return nil, "", err
	}
	url := site.InviteURL(token)
	data := map[string]any{"url": url, "link_id": r.LinkID, "kind": string(linkInvite), "author": author, "expires": PacificText(r.Expires)}
	text := fmt.Sprintf("%s\nInvite %s admits one machine as %s until %s. On that machine: mnemonic join %s",
		url, r.LinkID, author, PacificText(r.Expires), url)
	return data, text, nil
}

type linkSessionRow struct {
	SessionID     string `json:"session_id"`
	Harness       string `json:"harness"`
	Project       string `json:"project"`
	Start         string `json:"start"`
	Messages      int    `json:"messages"`
	MarkdownBytes int64  `json:"markdown_bytes"`
}

func createRead(ctx context.Context, db *DB, c *Controls, st *store.Committer, site LinkSite, id auth.Identity, a shareLinkArgs, days time.Duration) (any, string, error) {
	switch {
	case len(a.SessionIDs) == 0 || len(a.SessionIDs) > maxLinkSessions:
		return nil, "", argErrorf("session_ids must hold 1 to %d session ids", maxLinkSessions)
	case len([]rune(a.Title)) > maxTitleRunes:
		return nil, "", argErrorf("title must be at most %d characters", maxTitleRunes)
	case len([]rune(a.Note)) > maxNoteRunes:
		return nil, "", argErrorf("note must be at most %d characters", maxNoteRunes)
	}
	sessions := make([]string, 0, len(a.SessionIDs))
	authors := make([]string, 0, len(a.SessionIDs))
	for _, s := range a.SessionIDs {
		mid, err := parseMcpID(s)
		if err != nil {
			return nil, "", err
		}
		if mid.kind != kindSession {
			return nil, "", argErrorf("%s is not a session id", s)
		}
		if _, err := actor(ctx, db, c, mid.session); err != nil {
			return nil, "", argErrorf("%s: %s", s, err)
		}
		sole := db.sessionAuthors(mid.session)
		if len(sole) != 1 {
			return nil, "", argErrorf("%s: session has lines from %d authors; a link needs exactly one", s, len(sole))
		}
		if c.deleted(mid.session) {
			return nil, "", argErrorf("%s: session not found", s)
		}
		if slices.Contains(sessions, mid.session) {
			return nil, "", argErrorf("%s is listed twice", s)
		}
		sessions = append(sessions, mid.session)
		authors = append(authors, sole[0])
	}
	c.write.Lock()
	now := time.Now().UTC()
	r, token, err := c.newLink(st, controlRecord{Kind: linkRead, SessionIDs: sessions, SessionAuthors: authors, Title: a.Title, Note: a.Note,
		Cutoff: now, Expires: now.Add(days), Author: id.Author(), Tenant: id.Tenant()})
	c.write.Unlock()
	if err != nil {
		return nil, "", err
	}
	h, _ := ParseToken(token)
	rl, err := c.OpenRead(db, h, time.Now())
	if err != nil {
		return nil, "", fmt.Errorf("link %s was made but did not open: %w", r.LinkID, err)
	}
	rows := make([]linkSessionRow, len(rl.SessionIDs))
	for i, s := range rl.SessionIDs {
		rows[i] = sessionRow(db, s, r.Cutoff)
		if rows[i].MarkdownBytes, err = site.MarkdownSize(db, token, rl, i+1); err != nil {
			return nil, "", fmt.Errorf("link %s was made but session %d did not render: %w", r.LinkID, i+1, err)
		}
	}
	url := site.ReadURL(token)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nLink %s reads %s as of now, until %s.", url, r.LinkID, count(len(rows), "session"), PacificText(r.Expires))
	for i, row := range rows {
		fmt.Fprintf(&b, "\n%d. %s, %s, %s, %s, %s markdown", i+1, row.Harness, row.Project, row.Start, count(row.Messages, "message"), byteText(row.MarkdownBytes))
	}
	data := map[string]any{"url": url, "link_id": r.LinkID, "kind": string(linkRead), "expires": PacificText(r.Expires), "sessions": rows}
	return data, b.String(), nil
}

// sessionRow is what the owner checks before sending a link: the session's
// harness, project (its first working directory's base name), start, and the
// user and assistant messages at or before cutoff.
func sessionRow(db *DB, session string, cutoff time.Time) linkSessionRow {
	row := linkSessionRow{SessionID: FormatSessionID(session), Project: "-", Start: "start unknown"}
	es, ok, err := db.ExportSession(session)
	if err != nil || !ok {
		return row
	}
	row.Harness = es.Harness
	if p := path.Base(es.FirstCwd); es.FirstCwd != "" && p != "/" && p != "." {
		row.Project = p
	}
	limit := cutoff.UnixMicro()
	var start int64
	es.EachRow(func(r *Row) error {
		if r.Bits&bitTimeOK == 0 || r.TimeUs > limit {
			return nil
		}
		if start == 0 {
			start = r.TimeUs
		}
		if r.EventKind == "message" && (r.ActorKind == "user" || r.ActorKind == "assistant") {
			row.Messages++
		}
		return nil
	})
	if start != 0 {
		row.Start = PacificText(time.UnixMicro(start))
	}
	return row
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func byteText(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

type linkListRow struct {
	LinkID   string `json:"link_id"`
	Kind     string `json:"kind"`
	Expires  string `json:"expires"`
	Title    string `json:"title,omitempty"`
	Sessions int    `json:"sessions,omitempty"`
	Invite   string `json:"invite,omitempty"`
	Key      string `json:"key,omitempty"`
	Machine  string `json:"machine,omitempty"`
}

func listLinks(c *Controls, id auth.Identity) (any, string, error) {
	links := c.owned(id, time.Now())
	rows := make([]linkListRow, len(links))
	var b strings.Builder
	fmt.Fprintf(&b, "%s.", count(len(links), "live link"))
	for i, l := range links {
		row := linkListRow{LinkID: l.id, Kind: string(l.kind), Expires: PacificText(l.expires), Title: l.title,
			Sessions: len(l.sessions), Invite: l.invite, Key: l.joinKey, Machine: l.machine}
		rows[i] = row
		switch {
		case l.kind == linkRead:
			fmt.Fprintf(&b, "\n%s read, %s, %q, until %s", l.id, count(row.Sessions, "session"), l.title, row.Expires)
		case l.joinKey != "":
			fmt.Fprintf(&b, "\n%s invite for %s, joined by %s key %s", l.id, l.invite, l.machine, l.joinKey)
		default:
			fmt.Fprintf(&b, "\n%s invite for %s, unused, until %s", l.id, l.invite, row.Expires)
		}
	}
	return map[string]any{"links": rows}, b.String(), nil
}
