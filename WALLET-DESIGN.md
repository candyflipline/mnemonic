# Wallet-pubkey sharing, candidate A

## Problem

A session author shares one session with a Solana wallet address. Whoever holds that wallet reads exactly the shared sessions over QUIC from any machine, and the server config never changes. Option B is decided (FRAME.md): each machine keeps its own device key, a wallet signature binds that key, the binding rides in the client certificate, and the server checks it during the handshake.

Four constraints shape the design. Admission happens in `VerifyPeerCertificate` (`internal/qnet/qnet.go:73`), so live shares must be reachable from the handshake without a second copy. `auth.Identity` members for `DefaultTenant` is the Except set (`internal/auth/auth.go:102-112`), so a wallet placed there would read the whole corpus. Author names must match `^[a-z0-9._-]+$` (`internal/wire/wire.go:15`), and base58 has uppercase. Old builds refuse unknown control actions (`internal/index/controls.go:81`).

### Lies table

| The lie underneath | The one fix | What gets deleted |
|---|---|---|
| "A client is a row in `[[keys]]`": `identify(key string)` can only consult the static table | `qnet.Identify(device, binding)`. qnet passes the opaque extension bytes, and `server.Identify` composes the static keys with `wallet.Verify` and `Controls.Admits` | the key-string callback and `peerKey` (`internal/qnet/key.go:75`) |
| "Every reader's reach is an author set" (`Identity.Members`) | a wallet reader has an empty, non-Except member set. Its reach is the shares, read from Controls per request | nothing; `AuthorSet` stays the tenant model |
| "Zero-value fields mean default" lets an Identity land in DefaultTenant by accident | `auth.WalletReader` is the only constructor that sets `wallet`. Fields stay private, so no caller can build a wallet identity with members or scopes | none |

## Usage (caller's view)

**Owner shares from Claude Code.** The MCP tool is `share_session`.

```
share_session {"session_id": "session:abc…", "wallet": "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"}
→ Session session:abc… is shared with 7xKX…gAsU.   data.wallets = ["7xKX…gAsU"]
share_session {"session_id": "session:abc…", "wallet": "7xKX…gAsU", "revoke": true}
→ Session session:abc… is no longer shared with 7xKX…gAsU.
```

**Recipient binds from a Solana CLI keypair.** This is one step.

```
mnemonic init …                      # writes config.toml with server + server_key, as today
mnemonic wallet bind --keypair ~/.config/solana/id.json --days 90
wallet 7xKX…gAsU bound to this machine until Jan 25, 2027 4:00 PM PST
```

**Recipient binds from Phantom.** Phantom has no file, so the recipient pastes a message and a signature.

```
mnemonic wallet message --wallet 7xKX…gAsU
mnemonic wallet binding v1 wallet=7xKX…gAsU device=Qm3…xw server=pV0…9A expires=2027-01-26T00:00:00Z
# paste into Phantom "Sign message" (or: solana sign-offchain-message '<line>')
mnemonic wallet bind --message '<line>' --signature 5Jx…base58
```

**Recipient connects.** This is unchanged. The `mnemonic mcp` shim loads `~/.mnemonic/key` and `~/.mnemonic/key.wallet` through `qnet.LoadCredential` (`cmd/mnemonic/quic.go:104`). `list_sessions`, `search_sessions` and `open` show only the shared sessions.

**Call sites in the code.**

```go
qs, err := qnet.Listen(cfg.QUIC.Listen, priv, srv.Identify(cfg.Identities(), priv.Public().(ed25519.PublicKey)))   // cmd/mnemonic/server.go:56
cred, err := qnet.LoadCredential(cfg.KeyFile); qnet.Dial(ctx, addr, cred, cfg.ServerKey, qnet.ALPNHTTP)              // cmd/mnemonic/quic.go:105
if w, ok := id.Wallet(); ok { f.shared = c.sharedWith(w) }                                                          // internal/index/scope.go
```

## Shape

**Data first.**
- `wallet.Address [32]byte` is the raw ed25519 key. `String()` is base58, `Author()` is `sol-` plus 64 lowercase hex, which is segment-valid. Config refuses `[[keys]]` authors with that prefix (`internal/config/server_load.go`), so the two can never collide.
- `wallet.Claim{Wallet, Device, Server, Expires}` is what is signed. `wallet.Binding` is a claim plus a signature that verified. Only `Sign` and `Bind` make one, per type-system-discipline.
- The control record gains actions `share` and `unshare` with a `wallet` field holding canonical base58 (`internal/index/controls.go`). There is no grantee list on visibility. Each (session, wallet) pair carries a `stamp{shared, ts, key}` with the same last-writer-wins order as visibility.
- `Controls.byWallet` maps a wallet to its shared sessions. It is derived only in `apply`, under the same lock as `sessions`. It is an index of the one log, not a copy: `apply` is its single writer.

**Module map.**

| Module | Owns | Knows nothing of |
|---|---|---|
| `internal/wallet` (new, stdlib only) | base58, the message bytes, both signature forms, the extension DER, expiry policy, `id.json` | TLS, controls, auth |
| `internal/qnet` | `Credential{Key, Binding}`, the extension OID, carrying opaque bytes in and out of the cert, `key.wallet` beside the key file | what the bytes mean |
| `internal/auth` | `Identity`, `WalletReader(w)` (Read only, no tenant, empty members) | signatures |
| `internal/index` | share records, `Admits`, `sharedWith`, `share_session`, the `SessionFilter.shared` set | TLS |
| `internal/server` | `Identify`, the one admission rule (configured key, else a verified binding with at least one live share) | base58 |

**Handshake to live grants.** There is one source of truth. `server.New` already loads Controls before `qnet.Listen` (`cmd/mnemonic/server.go:45` vs `:56`), so wiring order needs no change. `server.Identify` closes over `s.ctl` and asks `Admits(w)` on every call. qnet calls it in `VerifyPeerCertificate`, where a refusal is a TLS alert, and again in `serve` (`internal/qnet/qnet.go:146`). Both calls read live state.

**Message bytes, exact.** The message is one line of printable ASCII, so `solana sign-offchain-message` picks format 0, the format FRAME proved:

```
mnemonic wallet binding v1 wallet=<base58> device=<base64url 32B> server=<base64url 32B> expires=<YYYY-MM-DDTHH:MM:SSZ>
```

**Signature forms.**
- Raw form: `ed25519.Verify(wallet, msg, sig)`, which is what Phantom signs.
- CLI form: `ed25519.Verify(wallet, "\xffsolana offchain" ‖ 0x00 ‖ 0x00 ‖ u16le(len(msg)) ‖ msg, sig)` (`internal/wallet/wallet.go`, `offchainV0`).
- `Verify` tries both, so no form tag goes on the wire.

**Certificate extension.** It is non-critical, under OID `1.3.6.1.4.1.99999.1.1` (a placeholder). The value is DER `SEQUENCE { wallet OCTET STRING(32), expires INTEGER unix-s, signature OCTET STRING(64) }`. The device and server keys are not carried. The server rebuilds the claim from the key the handshake proved and from its own key, so a binding replayed to another device or server fails the signature check.

**Interface depth.** The server's whole wallet surface is `wallet.Verify(ext, device, server, now) (Address, error)`, one call that hides base58, the DER, the preamble, both forms and the expiry cap. qnet's added surface is one struct and one callback type.

**What it does not do.** It has no on-chain lookup. Wallet readers get no annotations: `f.Author` is false for every author (`internal/index/q_get_trace.go:267`). They get no ingest sources and no push, annotate, control or export with `include_private`.

## How the done predicate is met

1. `share_session` appends `share`. `Admits` admits the wallet, `Sessions` sets `f.shared`, and `Listed` and `Opened` pass shared ids (`internal/index/scope.go`). This feeds list, search, open and export (`internal/export/http.go:106`).
2. The wallet's members are `AuthorSet{}`, so `inTenant` is false for every session (`internal/index/scope.go:64-75`), and only `shared` and `link` open anything.
3. If there is no binding, `Identify` refuses. A different signer, an expired binding or another server key all fail `wallet.Verify`, because the claim is rebuilt from the proven keys. Each refusal is a TLS alert at `internal/qnet/qnet.go:78`.
4. A wallet with zero shares fails `Admits` and is refused in the handshake.
5. `Sessions` re-reads `sharedWith` on every request, so a revoked session drops at the next request. The next connection with no shares left fails `Admits`.
6. Every write path is already closed to a wallet reader. Push (`internal/intake/intake.go:75`) and annotate (`internal/index/q_annotate.go:169`) check scopes, and `WalletReader` grants only Read. Every control tool (`actorOn`, `internal/index/controls.go:268`) reports "session not found" because members are empty.
7. `Verify` checks the raw form and the offchain v0 form. Keypair binding signs the raw form.
8. There is no new dependency. base58 and the DER both use the stdlib (`encoding/asn1`).

## Tradeoffs accepted

- We accept a second signature verification per connection (handshake plus `serve`) in exchange for no per-connection cache that could go stale.
- We accept that an old build fails to boot on a `share` record (`internal/index/controls.go:81`) in exchange for one log. Rolling back past this feature after the first share needs a forward-compatible "ignore unknown action" build deployed first.
- We accept that a share beats `private` (a private session shared with a wallet is visible to it) in exchange for share meaning what the author asked for.
- We accept that a wallet reader sees a shared session with no annotations in exchange for leaving the annotation tenant rule untouched.

## Alternatives considered

- **A grantee list in Identity, built at the handshake.** It is stale after unshare, which breaks item 5, and it is a second copy of the grants.
- **Wallet knowledge in qnet, with qnet verifying and returning an Address.** qnet would learn base58, the preamble and the expiry policy, and the transport would own a domain rule. That is shallow on both sides.
- **A sum type `Reach = Tenant(AuthorSet) | Shares(Address)` replacing `Members()`.** It is more honest, but it touches every `Members()` caller (`internal/export/http.go:95`, `internal/index/controls.go:268`, `internal/index/scope.go:37`). An empty set with private fields gives the same guarantee for two fields.
- **Two tools, `share_session` and `unshare_session`.** `set_visibility` sets precedent for one setter, and one schema means one record path.

## Red-flag screen

| Flag | Result |
|---|---|
| Shallow module | Pass. `wallet.Verify` is one call over five concerns. |
| Information leakage | One leak. The binding file suffix lives in qnet, and `wallet bind` writes it through `qnet.SaveBinding`, so there is still one place. |
| Temporal decomposition | Pass. Parse, verify and expiry are all in `wallet`. |
| Pass-through | Pass. `server.Identify` adds policy. |
| Split ownership | Pass. `byWallet` has one writer, `apply`. |
| Two ways | `Keys.Identify` stays for `server.Identify` only. `qnet.LoadOrCreateKey` stays for the server key, and clients use `LoadCredential`. |
| Importable internals | `wallet.offchainV0` is unexported. |
| Hand-synced list | `share_session` is in `tools_ext.json` and `register`, the same pair every tool has today. |

## Open questions and risks

- Should mnemonic register an IANA enterprise number, replacing placeholder arc 99999? A 2.25 UUID arc does not fit `asn1.ObjectIdentifier` ints.
- Is `MaxLifetime` = 366 days the right cap?
- Should a wallet reader see the session author's own annotations?
- Phantom's raw `signMessage` form is unproven here (FRAME: recall).

## Next implementation step

Implement `internal/wallet` (base58, `Verify`, `Encode`/`Decode`) with one table test pinning the message bytes and a CLI-signed signature from FRAME's scratch keypair.
