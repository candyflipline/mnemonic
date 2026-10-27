package index

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wallet"
)

type shareSession struct{}

func init() { register(shareSession{}) }

func (shareSession) Name() string { return "share_session" }

type shareArgs struct {
	SessionID  string `json:"session_id"`
	Wallet     string `json:"wallet"`
	Revoke     bool   `json:"revoke"`
	RequestKey string `json:"request_key"`
}

// Call appends a share (or, with revoke, an unshare) of one session with one
// wallet as the caller's control record, under the rule set_visibility
// keeps: only the session's sole author or an admin, never on a deleted
// session. It answers with the wallets the session is shared with after the
// record.
func (shareSession) Call(ctx context.Context, db *DB, raw json.RawMessage) (any, string, error) {
	var a shareArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, "", argErrorf("invalid arguments: %s", err)
	}
	mid, err := parseMcpID(a.SessionID)
	if err != nil {
		return nil, "", err
	}
	if mid.kind != kindSession {
		return nil, "", argErrorf("session_id must be a session id")
	}
	if len(a.RequestKey) > 128 {
		return nil, "", argErrorf("request_key must be at most 128 characters")
	}
	w, err := wallet.ParseAddress(a.Wallet)
	if err != nil {
		return nil, "", argErrorf("wallet must be a base58 Solana address: %s", err)
	}
	session := mid.session
	c := controlsFrom(ctx)
	id, err := actor(ctx, db, c, session)
	if err != nil {
		return nil, "", err
	}
	if c.deleted(session) {
		return nil, "", argErrorf("session not found")
	}
	act := actShare
	if a.Revoke {
		act = actUnshare
	}
	key := a.RequestKey
	if key == "" {
		key = randomKey()
	}
	dup, err := c.record(store.From(ctx), id.Author(), controlRecord{SessionID: session, Action: act, Wallet: w.String(),
		RequestKey: recordKey(act, session, w.String(), key)})
	if err != nil {
		return nil, "", err
	}
	data := map[string]any{"session_id": a.SessionID, "wallet": w.String(), "shared": !a.Revoke, "wallets": c.walletsOf(session), "request_key": key, "duplicate": dup}
	if a.Revoke {
		return data, "Session " + a.SessionID + " is no longer shared with " + w.String() + ".", nil
	}
	return data, "Session " + a.SessionID + " is shared with " + w.String() + ".", nil
}
