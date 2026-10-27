package index

import (
	"context"
	"encoding/json"
)

type shareSession struct{}

func init() { register(shareSession{}) }

func (shareSession) Name() string { return "share_session" }

// shareArgs is share_session's input; wallet is parsed at this boundary and
// stored as wallet.Address.String().
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
// record. Sharing what is shared, or revoking what is not, appends nothing
// new in effect: the latest stamp is the same state.
func (shareSession) Call(ctx context.Context, db *DB, raw json.RawMessage) (any, string, error) {
	// TODO:
	//   a := decode raw into shareArgs with DisallowUnknownFields; session id as parseControlArgs does
	//   w, err := wallet.ParseAddress(a.Wallet) -> argErrorf("wallet must be a base58 Solana address")
	//   c := controlsFrom(ctx); id, err := actor(ctx, db, c, session); c.deleted(session) -> not found
	//   act := actShare, or actUnshare when a.Revoke
	//   key := a.RequestKey or randomKey()
	//   dup, err := c.record(store.From(ctx), id.Author(), controlRecord{SessionID: session, Action: act,
	//       Wallet: w.String(), RequestKey: recordKey(act, session, w.String(), key)})
	//   data: session_id, wallet, shared (!a.Revoke), wallets (c.walletsOf(session)), request_key, duplicate
	//   text: "Session <id> is shared with <w>." / "Session <id> is no longer shared with <w>."
	panic("not implemented")
}
