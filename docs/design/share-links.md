# Links

Status: approved by the owner on 2026-10-27 with the calls listed at the end. Being built on `lane/links`. Nothing is deployed.

A link is one URL that grants one thing. There are two kinds:

- A **read link** lets anyone holding it read a set of sessions, as they were when the link was made, in a browser or with an agent. The reader installs nothing.
- An **invite link** lets one machine join the team. `mnemonic join <url>` gives the machine a key the server admits, writes its config and starts its push agent. From then on the machine pushes every session it syncs, and it reads every session of its tenant, like any `[[keys]]` machine.

Links replace wallet sharing and the `link` visibility. Both are deleted.

## Goal and done predicate

Goal: the owner makes either kind of link with one call, and the recipient needs nothing but the URL.

The feature is done when all of these hold on prod:

1. One `share_link` call with three session ids returns one `https://` URL.
2. That URL opens in a plain browser with no mnemonic install and shows all three sessions.
3. `curl -s <url>.md` returns all three transcripts as markdown.
4. `claude -p "Read <url> and list the three sessions it contains"`, run with no mnemonic MCP server configured, names all three.
5. One `share_link {"invite": "<author>"}` call returns one invite URL. On a machine with no `~/.mnemonic`, `mnemonic join <url>` ends with a running push agent, and `mnemonic mcp` answers `list_sessions` with the team's sessions.
6. A session pushed from the joined machine appears in the owner's `list_sessions` under the invited author.
7. Revoking a read link makes the next request to every URL form return 410. Revoking an invite makes the joined machine's next request fail and its next handshake be refused.
8. A session not in a read link is unreachable through any URL form of any link.
9. The box still accepts only UDP 8741 and TCP 22 inbound.
10. `internal/wallet`, the `share_session` tool, `mnemonic wallet` and the `link` visibility are gone, and `grep -ri wallet` over `internal cmd docs README.md` finds nothing.

## Read links

### What the recipient gets

Every URL form shares one token: `https://share.askloyal.com/s/<token>`.

| URL | Returns |
|---|---|
| `/s/<token>` | An HTML page with the title and note, a table of the sessions, and each session rendered below it. Tool calls and tool output sit in collapsed `<details>` blocks. A line at the top tells agents to fetch the `.md` form. |
| `/s/<token>.md` | The whole link as markdown, sessions in order, for agents and `curl`. |
| `/s/<token>/<n>.md` | Session `n` (1-based) alone as markdown. |
| `/s/<token>/<n>.json` | Session `n` as an ATIF trajectory cut at the link's cutoff, with nothing truncated. |

The markdown keeps every user and assistant message whole. A tool call is one line with its name and arguments. A tool result longer than 4 KB is cut, and the cut names the `.json` URL that has it in full. Whether Claude Code's WebFetch reads a large `.md` in full is unproven. The page therefore tells agents to `curl` the `.md` into a file and read the file.

### What the owner does

From any agent with the mnemonic MCP server:

> Share this session and the two about the QUIC cutover, title "QUIC context".

The agent finds the ids with `search_sessions` or `list_sessions`, then calls:

```
share_link {"session_ids": ["session:…", "session:…", "session:…"], "title": "QUIC context", "note": "…", "days": 30}
```

The answer is the URL, the link id, the expiry, and one row per session (harness, project, start time, message count, rendered size). The owner sees what goes out before sending it.

From a terminal:

```
mnemonic share <session id or Claude Code session uuid>... --title "QUIC context" [--days 30]
```

`mnemonic share` prints the URL and, on macOS, copies it to the clipboard. It calls the same tool over the existing `mnemonic mcp` dial. A bare uuid becomes a `session:` id by the encoding in `internal/index/q_common.go`, since Claude Code's raw session id is the record's `sessionId`.

## Invite links

### What the invitee gets

`https://share.askloyal.com/i/<token>` opens a page that names the team's server and the author the machine will push as, and shows two commands:

```
CGO_ENABLED=0 go install github.com/candyflipline/mnemonic/cmd/mnemonic@latest
mnemonic join https://share.askloyal.com/i/<token>
```

`/i/<token>.md` is the same text for an agent to follow.

`mnemonic join <url>`:

1. creates `~/.mnemonic/key` if it is missing;
2. sends `POST /i/<token>` with the device's public key and machine name;
3. gets back the QUIC address, the server key and the author;
4. writes `~/.mnemonic/config.toml` and installs the push agent, as `init` does;
5. prints the `claude mcp add-json` and Codex lines from the README.

The server key arrives over the HTTPS answer, so nobody copies `SERVER_KEY` by hand. That moves trust for the key to the web PKI and DNS of `share.askloyal.com`. Joining again with the same key and the same token returns the same answer, so a retry after a dropped reply is safe.

### What the owner does

```
share_link {"invite": "vlad", "days": 7}
```

The inviter names the author. The invitee never picks it, so an invite cannot be used to push as someone else. Inviting a new author takes the `admin` scope. Any key may invite another machine for its own author. An invite admits one device key, then it is used up. A second machine needs a second invite.

### What a member is

A redeemed invite is a member: a device key with the invite's author, the inviter's tenant and the scopes `read` and `push`. A member is a configured identity in every way that matters. It joins its tenant's member set, reads what that tenant reads, and pushes as its author. `admin` and `annotate` never come from an invite.

Revoking the invite removes the member. `Server.Identify` refuses the key at the next handshake, and `index.Sessions` refuses every request on a connection whose member was removed, which is the rule wallet readers had.

## Listing and revoking

`share_link {}` lists the caller's live links of both kinds, and for each invite the key and machine that redeemed it. `share_link {"revoke": "<link id>"}` revokes either kind. A revoke is final, and a replayed older record cannot revive the link.

## Lies the current design tells

| Lie | The one fix | What gets deleted |
|---|---|---|
| A reader is a principal with a key, so a recipient must install mnemonic and bind a wallet. | A read link is the grant. Its principal is a bearer token, checked by hash. | Wallet sharing: `internal/wallet`, the wallet reader in `internal/auth`, the binding SAN in `internal/qnet`, `share` and `unshare` records, `share_session`, `mnemonic wallet`, `cmd/mnemonic/phantom.html`, `docs/design/wallet-sharing.md` and its README section. |
| Joining the team means the operator pastes a `[[keys]]` block into the server config and restarts the server. | An invite admits a key through the control log, read live by the handshake. | The hand copy of `SERVER_KEY`. `[[keys]]` stays for the owner's machines and static setups. |
| The only door is mutually authenticated QUIC, so a browser can never read anything. | A second listener on loopback, serving `/s/` and `/i/` and nothing else, published through a Cloudflare Tunnel. Inbound ports do not change. | Nothing. No TLS, autocert or TCP listener comes back. |
| `link` visibility means "shareable by link". It means "openable by id by any admitted caller", a weaker duplicate under the same name. | Read links take the name. | `visLink`, its branch in `Opened`, and `"link"` in the `set_visibility` enum in `internal/mcp/tools.json`. |

Prod holds no control records of any kind (checked on the box on 2026-10-27: the raw store has only `claude-code`, `codex` and `prime-agent` harness directories). Deleting `share`, `unshare` and `link` visibility therefore needs no migration.

## Data shape

A link is one control record. Revoking it is one more:

```
{"action":"link","link_id":"<12 hex>","kind":"read","token_sha256":"<64 hex>",
 "session_ids":["session:…"],"title":"…","note":"…","cutoff":"<ts>","expires":"<ts>",
 "author":"candyflipline","tenant":"default","ts":"…","request_key":"…","writer":"…"}
{"action":"link","link_id":"<12 hex>","kind":"invite","token_sha256":"<64 hex>",
 "invite_author":"vlad","expires":"<ts>","author":"candyflipline","tenant":"default", …}
{"action":"join","link_id":"<12 hex>","key":"<base64url ed25519>","machine":"vlad-mbp","ts":"…", …}
{"action":"revoke","link_id":"<12 hex>","ts":"…", …}
```

- **Records name a link, not a session.** `parseControl` accepts these when `session_id` is empty and `link_id` is set, and refuses a `link` record whose kind-specific fields do not match its kind.
- **The token never touches disk.** The server draws 16 random bytes, returns them once as unpadded base64url (22 characters), and stores only the SHA-256. The control log and its backups grant nothing. A lost URL is replaced by a new link.
- **`cutoff` is the share time.** A read link keeps events whose timestamp is at or before it. Lines pushed a few seconds late still appear, because the filter reads event time, not ingest time.
- **`expires`** defaults to 30 days for a read link and 7 for an invite, from 1 to 365. An expired invite stops admitting new joins. A member it already admitted stays until the invite is revoked.
- **Sole author.** Only a session's sole author, or an admin key, may put it in a read link. At render time a session shows only while it is not deleted and the link's author is still its sole author. A foreign line pushed under a linked session id hides the session instead of leaking into the link.
- **One join per invite.** The first `join` record for an invite wins. A later join with a different key is refused, and the same key gets the first answer again.

`Controls` keeps two indexes of the log, written only by `apply` under the existing lock: `byToken map[[32]byte]*link` and `byKey map[string]*member`. The first replaces `byWallet`. The second is what `Server.Identify` and `index.Sessions` read.

## Request paths

```
browser or agent
  -> Cloudflare, TLS for share.askloyal.com
  -> cloudflared on the box, dialing out, no inbound port
  -> mnemonic serve, [share] listener on 127.0.0.1:8742
  -> GET /s/{token}[.md] | /s/{token}/{n}.{md,json} | GET /i/{token}[.md] | POST /i/{token}
  -> sha256(token) -> Controls.byToken -> live, unexpired, not revoked
```

```
member machine -> QUIC 8741 -> Server.Identify(device key)
  -> [[keys]] entry?      configured identity
  -> Controls.byKey hit?  member identity (invite author, inviter's tenant, read+push)
  -> otherwise            refused
```

- **Boundary.** The public handler is the only code that reads a token. It parses it, looks up the link and hands the renderer a typed view (sessions, cutoff, title). The renderer never sees a request or a token. A missing, malformed or unknown token gets 404. A revoked or expired one gets 410. Neither body says which.
- **Members join the tenant's member set live.** `auth.NewKeys` computes member sets once from `[[keys]]`. Identities must now be computed from `[[keys]]` plus the controls' members at each `Identify`, so a member added after boot is in its tenant's set and its tenant mates can read its sessions.
- **Package placement.** `internal/index` owns the records, both indexes, `Controls.Link` and the `share_link` tool. `internal/export` gains the markdown and HTML renderers, both walking the ATIF `Step` sequence that `newTrajectory` builds, so the formats cannot drift. `internal/server` gains `Server.Public()`, the `/s/` and `/i/` mux, and a new `Identify`. `cmd/mnemonic` gains `share` and `join`. `internal/config` gains `[share] listen`, `base_url` and `quic_addr`.
- **No new Go module.** `html/template` and `crypto/sha256` are stdlib. The tunnel is a separate binary and service on the box, not a `go.mod` entry.

## Response headers

Every `/s/` and `/i/` response carries:

```
Cache-Control: no-store
X-Robots-Tag: noindex, nofollow
Referrer-Policy: no-referrer
Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'
```

`no-store` keeps Cloudflare and browsers from serving a revoked link. `no-referrer` matters most. Transcripts are full of URLs, and a click on one must not send the token to that site. The pages have no script and no external fetch.

## Risks the owner accepts by shipping this

- Anyone a read link reaches can read it. That includes forwards, chat unfurl bots, and the provider of any agent the recipient points at it. Expiry and revoke bound the damage. Nothing prevents it.
- Anyone an unused invite reaches can join as its author. One use and a 7-day default bound it. The owner sees the joined key and machine in `share_link {}` and can revoke.
- Push redaction was tuned for a server only the team reads. A read link is the first path to readers outside the team. The `share_link` answer lists sizes so the owner can open the link and check it before sending.
- The server key does not protect links. Losing the box loses the links and the members. Rotating the server key does not revoke them.

## Build order

Each unit ends in a check on the real thing.

1. **Subtract.** Delete wallet sharing and `link` visibility. Check: gates green and `grep -ri wallet internal cmd` empty.
2. **Records and tool.** `link`, `join` and `revoke` records, both indexes, `share_link` create, list and revoke. Check: a scratch test creates, lists, revokes, joins and replays the log.
3. **Render.** Markdown and HTML over ATIF steps, with the cutoff and the 4 KB cut. Runs in parallel with units 1 and 2.
4. **Admission.** Live member sets and the new `Identify`. Check: a scratch test over real QUIC on loopback admits a joined key, then refuses it after revoke on both the open connection and a new handshake.
5. **Public listener and CLI.** `[share]` config, `Server.Public()`, `mnemonic share`, `mnemonic join`. Check: loopback end to end for every done-predicate item except 9, with a fresh `HOME` for the joining machine.
6. **Tunnel and deploy, on the owner's go.** cloudflared as a systemd unit, a CNAME for `share.askloyal.com`, `admin` added to the owner's key in `server.toml`, deploy. Check: the done predicate on prod, item 9 with `ss -lntup` on the box and an outside port scan.

One review runs over the whole branch before deploy, not per unit.

## Owner calls

Decided on 2026-10-27:

1. Read links are snapshots at share time.
2. Wallet sharing is deleted. Links cover sharing threads and inviting people into the team.
3. `link` visibility is deleted, and read links take the name.
4. Build now. Deploy waits for the owner's go.

Defaults taken in this design, which the owner can change:

5. The host name is `share.askloyal.com`. The zone is already on Cloudflare.
6. Read links default to 30 days and invites to 7, both from 1 to 365.
7. An invite admits one machine. Inviting a new author takes `admin`, so the owner's key gains `admin` at deploy.
8. Members get `read` and `push`, never `annotate` or `admin`.
