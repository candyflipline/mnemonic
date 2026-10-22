package query

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
)

type setVisibility struct{}

func (setVisibility) Name() string { return "set_visibility" }

type deleteSession struct{}

func (deleteSession) Name() string { return "delete_session" }

func init() {
	register(setVisibility{})
	register(deleteSession{})
}

type controlArgs struct {
	SessionID  string     `json:"session_id"`
	Visibility visibility `json:"visibility"`
	RequestKey string     `json:"request_key"`
}

func parseControlArgs(raw json.RawMessage) (controlArgs, string, error) {
	var a controlArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, "", argErrorf("invalid arguments: %s", err)
	}
	id, err := parseMcpID(a.SessionID)
	if err != nil {
		return a, "", err
	}
	if id.kind != kindSession {
		return a, "", argErrorf("session_id must be a session id")
	}
	if len(a.RequestKey) > 128 {
		return a, "", argErrorf("request_key must be at most 128 characters")
	}
	return a, id.session, nil
}

// actor is the caller on ctx when it may act on session: one of the
// session's authors, or an admin token of a tenant the session is in. A
// session outside the caller's tenant is not found.
func actor(ctx context.Context, db *sql.DB, c *Controls, session string) (auth.Identity, error) {
	id, ok := auth.From(ctx)
	if !ok || c == nil || store.From(ctx) == nil {
		return id, errors.New("query: control tools need an identity, controls and a store on ctx")
	}
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT author FROM events WHERE session_id = "+sqlQuote(session))
	if err != nil {
		return id, err
	}
	defer rows.Close()
	var authors []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return id, err
		}
		authors = append(authors, a)
	}
	if err := rows.Err(); err != nil {
		return id, err
	}
	if !slices.ContainsFunc(authors, id.Members().Has) {
		return id, argErrorf("session not found")
	}
	if !slices.Contains(authors, id.Author()) && !id.Can(auth.Admin) {
		return id, argErrorf("only the session's author or an admin token may change it")
	}
	return id, nil
}

func randomKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (setVisibility) Call(ctx context.Context, db *sql.DB, raw json.RawMessage) (any, string, error) {
	a, session, err := parseControlArgs(raw)
	if err != nil {
		return nil, "", err
	}
	if !slices.Contains([]visibility{visPrivate, visTeam, visLink}, a.Visibility) {
		return nil, "", argErrorf("visibility must be private, team or link")
	}
	c := controlsFrom(ctx)
	id, err := actor(ctx, db, c, session)
	if err != nil {
		return nil, "", err
	}
	if c.deleted(session) {
		return nil, "", argErrorf("session not found")
	}
	key := a.RequestKey
	if key == "" {
		key = randomKey()
	}
	dup, err := c.record(store.From(ctx), id.Author(), controlRecord{SessionID: session, Action: actVisibility, Visibility: a.Visibility, RequestKey: key})
	if err != nil {
		return nil, "", err
	}
	data := map[string]any{"session_id": a.SessionID, "visibility": string(a.Visibility), "request_key": key, "duplicate": dup}
	return data, "Session " + a.SessionID + " is now " + string(a.Visibility) + ".", nil
}

func (deleteSession) Call(ctx context.Context, db *sql.DB, raw json.RawMessage) (any, string, error) {
	a, session, err := parseControlArgs(raw)
	if err != nil {
		return nil, "", err
	}
	if a.Visibility != "" {
		return nil, "", argErrorf("delete_session takes session_id and an optional request_key")
	}
	c := controlsFrom(ctx)
	id, err := actor(ctx, db, c, session)
	if err != nil {
		return nil, "", err
	}
	key := a.RequestKey
	if key == "" {
		key = "delete:" + session
	}
	dup := c.deleted(session)
	if !dup {
		dup, err = c.record(store.From(ctx), id.Author(), controlRecord{SessionID: session, Action: actDelete, RequestKey: key})
		if err != nil {
			return nil, "", err
		}
	}
	data := map[string]any{"session_id": a.SessionID, "deleted": true, "request_key": key, "duplicate": dup}
	return data, "Session " + a.SessionID + " is deleted; no read shows it.", nil
}
