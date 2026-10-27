# Sharing a session with a Solana wallet

A session's author can share that session with a Solana wallet address. Whoever controls the wallet can then read the shared sessions, and nothing else, from any machine. That machine's device key carries a binding the wallet signed. The server config does not change.

## Shape

| Package | Owns |
|---|---|
| `internal/wallet` | base58, the binding message, both signature forms, the DER envelope, expiry policy, Solana `id.json` files. Standard library only. |
| `internal/qnet` | carrying opaque binding bytes in the client certificate and out of it, and the `key.wallet` file beside the key. It never reads the bytes. |
| `internal/auth` | `Identity`, one sealed principal: a configured key or a wallet reader. |
| `internal/index` | `share` and `unshare` control records, `Admits`, the wallet's `SessionFilter`, and the `share_session` tool. |
| `internal/server` | `Server.Identify`, the one admission rule. |

### The wallet reader

`auth.Identity` holds one unexported principal. `auth.Keys` and `auth.Local` make the configured kind. `auth.WalletReader(wallet.Binding)` makes the wallet kind, and only `wallet.Sign`, `wallet.Bind` and `wallet.Verify` make a nonzero `Binding`. A wallet reader has:

- no tenant;
- the empty inclusive member set, so no author is a member, and the default tenant's "every author not named elsewhere" set can never reach it;
- `Read` and no other scope;
- author `sol-` followed by the 64 lowercase hex digits of its key, which is a legal author segment. `LoadServer` refuses a `[[keys]]` author with the `sol-` prefix, so the two kinds never share a name;
- the binding's expiry. `index.Sessions` checks it on every request.

The zero `Identity` grants nothing.

## Binding

### Message

The wallet signs one line of printable ASCII with no trailing newline:

```
mnemonic wallet binding v1 wallet=<base58> device=<base64url> server=<base64url> expires=<YYYY-MM-DDTHH:MM:SSZ>
```

`device` and `server` are the unpadded base64url ed25519 keys, as `mnemonic key` prints them. `expires` is UTC, whole seconds. Here is a real line from the end-to-end run:

```
mnemonic wallet binding v1 wallet=CZr22EmASNjdeikMj9S7ZCRSWT2Xb92Bh8QZt5GMzio device=Wohkq_lDvr-bKmk7On1-exxfqvOoKG4JMIVkKDE0JdQ server=b-UuB5JoMh5SEJvu4fHodiGFE1OJsyFHIA5GMzZuIgQ expires=2026-11-26T19:50:03Z
```

The line is one row of printable ASCII, so `solana sign-offchain-message` signs it with offchain format 0. A message containing a line feed makes solana-cli 4.2.0 pick format 1. `wallet.ParseMessage` accepts only lines that `Claim.Message` would write itself.

### Signature forms

`wallet.Bind` accepts a 64-byte signature that verifies under the wallet key over either of these:

- the raw message bytes. Phantom's `signMessage` and `mnemonic wallet bind --keypair` sign this form.
- `"\xffsolana offchain"`, then version byte `0x00`, then format byte `0x00`, then the message length as a u16 little-endian, then the message. `solana sign-offchain-message` signs this form, Ledger included.

No tag on the wire says which form was used. Both forms bind the same message, and both are tried.

### Envelope

The certificate carries the wallet, the expiry and the signature as DER:

```
Binding ::= SEQUENCE { wallet OCTET STRING (32), expires INTEGER (unix seconds), signature OCTET STRING (64) }
```

The envelope never carries the device key or the server key. The server rebuilds the claim from the key the TLS handshake proved and from its own key. A binding copied to another device, or sent to another server, therefore fails the signature check. `wallet.Verify` also refuses DER that would not re-encode to the same bytes, and any bytes after the sequence.

The client's self-signed certificate carries the envelope as one subjectAltName URI:

```
mnemonic:binding-v1:<unpadded base64url of the DER>
```

This is a tag, not a URL anyone fetches. Go marks the SAN extension critical, because the certificate subject is empty. `qnet` fails the handshake when a certificate carries two such URIs, or one whose base64url text is not canonical. The client stores the DER beside its key, at `~/.mnemonic/key.wallet` (mode 0600). `qnet.LoadCredential` reads it on every dial.

### Lifetime

`mnemonic wallet` defaults to 30 days (`--days`). The server refuses any binding whose expiry is more than 366 days past the moment it checks.

## Admission

`Server.Identify(keys, serverKey)` runs twice per QUIC connection. The first run is in `VerifyPeerCertificate`, where a refusal is a TLS alert. The second run comes when the connection is served. Both runs read live state.

1. If the device key is in `[[keys]]`, the client gets that configured Identity, and any binding it carries is ignored.
2. Otherwise, the certificate must carry a binding that `wallet.Verify` accepts for this device key, this server key and the current time, and `Controls.Admits(wallet)` must be true, meaning some session that is not deleted is shared with that wallet now. The client then gets `auth.WalletReader(binding)`.
3. Everything else is refused. The client sees `server refused our key <key>: CRYPTO_ERROR 0x12a (remote): tls: bad certificate`.

## Shares

### Records

Shares live in the session control log, next to visibility and delete records:

```
{"session_id":"…","action":"share","wallet":"<canonical base58>","ts":"…","request_key":"…","writer":"…"}
{"session_id":"…","action":"unshare","wallet":"<canonical base58>","ts":"…","request_key":"…","writer":"…"}
```

`parseControl` accepts a share record only when `wallet` re-encodes to the same base58 text and `visibility` is empty.

### Order

Each (session, wallet) pair keeps its latest decision. Decisions compare by `ts`, then by `request_key`, the same rule visibility uses. Under the write lock, a new decision gets `ts = max(now, previous ts + 1 ns)` for its pair, so a wall clock that steps back cannot make the new decision lose to an older one. An unshare stays as a stamp, so replaying an older share cannot revive it. A delete dominates: a deleted session is never listed or opened, and it does not count toward `Admits`.

`Controls.byWallet` maps each wallet to the sessions it is currently shared with. Only `apply` writes it, under the same lock as the per-session stamps. It is an index of the one log, not a second copy of the grants.

### Who may share

`share_session {session_id, wallet, revoke?, request_key?}` follows the `set_visibility` rule. Only the session's sole author or an admin key may share, and no one may share a deleted session. The answer lists the wallets the session is shared with after the call. A retry with the same `request_key` appends nothing.

### What a wallet reader sees

- `list_sessions`, `search_sessions`, `query` and export list exactly the sessions shared with the wallet that are not deleted.
- `open` and `get_trace` also open a `link` session by id, as for any admitted caller.
- A share beats `private`: a private session shared with a wallet is visible to that wallet.
- Annotations do not show, because no author is in the reader's tenant.
- `push` fails with `key lacks scope push`, and `annotate` fails with `token lacks scope annotate`. `set_visibility`, `delete_session` and `share_session` answer `session not found`, because the reader's member set is empty.
- After an unshare, the reader's next request on its open connection no longer sees the session. If the wallet has no shares left, its next connection is refused.
- Once the binding expires, every read on an open connection fails with `the wallet binding of <wallet> expired at <time>; bind again with mnemonic wallet bind`. New connections are refused.

## Client commands

```
mnemonic wallet bind --keypair <id.json> [--days 30]
mnemonic wallet bind --phantom [--days 30]
mnemonic wallet message --wallet <base58> [--days 30]
mnemonic wallet bind --message '<line>' --signature <base58|base64>
mnemonic wallet show
```

Every form reads `--config` (default `~/.mnemonic/config.toml`) for the key file and `server_key`. `--keypair` signs the raw form and keeps no wallet secret. `--phantom` serves one embedded page on a loopback port under a random path. The page connects `window.phantom.solana` (or `window.solana`), shows the exact message, and calls `signMessage` on the message bytes. It never signs a transaction. The page posts the address and signature back, and the command verifies them, stores the binding and exits. The paste path refuses a line that names another device or server, or that has already expired. Every path verifies the signature before it stores anything.

## Rollback

A build older than this feature refuses unknown control actions and fails to boot (`controls …: control record action "share"`). Before the first `share_session` call, rolling back is a binary swap. After it, roll back only to a build that reads share records, or remove the share records from the control log first.

## Verification

The end-to-end run started the real `serve` and client binaries over QUIC on loopback. It checked share, list, search and open, invisibility of other sessions, the five handshake refusals, revoke on a live connection, the refused writes, the keypair, CLI-paste and Phantom-page binding paths, and expiry on a live connection. A real Phantom click was not exercised. The page was driven by a script that signed the raw form with a throwaway key.
