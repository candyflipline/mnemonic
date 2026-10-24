package index

import (
	"context"
	"encoding/json"

	"github.com/candyflipline/mnemonic/internal/store"
)

type deleteSession struct{}

func init() { register(deleteSession{}) }

func (deleteSession) Name() string { return "delete_session" }

func (deleteSession) Call(ctx context.Context, db *DB, raw json.RawMessage) (any, string, error) {
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
		dup, err = c.record(store.From(ctx), id.Author(), controlRecord{SessionID: session, Action: actDelete, RequestKey: recordKey(actDelete, session, "", key)})
		if err != nil {
			return nil, "", err
		}
	}
	data := map[string]any{"session_id": a.SessionID, "deleted": true, "request_key": key, "duplicate": dup}
	return data, "Session " + a.SessionID + " is deleted; no read shows it.", nil
}
