package index

import (
	"context"
	"encoding/json"
)

type deleteSession struct{}

func init() { register(deleteSession{}) }

func (deleteSession) Name() string { return "delete_session" }

func (deleteSession) Call(ctx context.Context, caller *Caller, db *DB, raw json.RawMessage) (any, string, error) {
	a, session, err := parseControlArgs(raw)
	if err != nil {
		return nil, "", err
	}
	if a.Visibility != "" {
		return nil, "", argErrorf("delete_session takes session_id and an optional request_key")
	}
	id, err := actor(caller, db, session)
	if err != nil {
		return nil, "", err
	}
	c := caller.Controls
	key := a.RequestKey
	if key == "" {
		key = "delete:" + session
	}
	dup := c.deleted(session)
	if !dup {
		dup, err = c.record(caller.Store, id.Author(), controlRecord{SessionID: session, Action: actDelete, RequestKey: recordKey(actDelete, session, "", key)})
		if err != nil {
			return nil, "", err
		}
	}
	data := map[string]any{"session_id": a.SessionID, "deleted": true, "request_key": key, "duplicate": dup}
	return data, "Session " + a.SessionID + " is deleted; no read shows it.", nil
}
