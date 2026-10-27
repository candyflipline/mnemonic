package index

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/candyflipline/mnemonic/internal/store"
)

type setVisibility struct{}

func init() { register(setVisibility{}) }

func (setVisibility) Name() string { return "set_visibility" }

func (setVisibility) Call(ctx context.Context, db *DB, raw json.RawMessage) (any, string, error) {
	a, session, err := parseControlArgs(raw)
	if err != nil {
		return nil, "", err
	}
	if !slices.Contains([]visibility{visPrivate, visTeam}, a.Visibility) {
		return nil, "", argErrorf("visibility must be private or team")
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
	dup, err := c.record(store.From(ctx), id.Author(), controlRecord{SessionID: session, Action: actVisibility, Visibility: a.Visibility,
		RequestKey: recordKey(actVisibility, session, string(a.Visibility), key)})
	if err != nil {
		return nil, "", err
	}
	data := map[string]any{"session_id": a.SessionID, "visibility": string(a.Visibility), "request_key": key, "duplicate": dup}
	return data, "Session " + a.SessionID + " is now " + string(a.Visibility) + ".", nil
}
