# Share links

Status: proposed on 2026-10-27, not approved. Nothing here is built.

The owner picks a few sessions and gets one URL. Anyone holding the URL can read those sessions, as they were when the link was made, in a browser or with an agent. The recipient installs nothing, holds no key and binds no wallet.

## Goal and done predicate

Goal: one call turns N sessions into one link that a person can scroll and an agent can read.

The feature is done when all of these hold on prod:

1. One `share_link` call with three session ids returns one `https://` URL.
2. The URL opens in a plain browser with no mnemonic install and shows all three sessions.
3. `curl -s <url>.md` returns all three transcripts as markdown.
4. `claude -p "Read <url> and list the three sessions it contains"`, run under a config with no mnemonic server, names all three.
5. After `share_link {"revoke": "<link id>"}`, the next request to every URL form of that link gets 410.
6. A session not in the link is unreachable through any URL form of any link.
7. The box still accepts only UDP 8741 and TCP 22 inbound.

## What the recipient gets

Every URL form shares one token: `https://share.askloyal.com/s/<token>`.

| URL | Returns |
|---|---|
| `/s/<token>` | An HTML page with the title and note, a table of the sessions, and each session rendered below it. Tool calls and tool output sit in collapsed `<details>` blocks. A line at the top tells agents to fetch the `.md` form. |
| `/s/<token>.md` | The whole link as markdown, sessions in order, for agents and `curl`. |
| `/s/<token>/<n>.md` | Session `n` (1-based) alone as markdown. |
| `/s/<token>/<n>.json` | Session `n` as an ATIF trajectory, the same bytes `export atif` writes, with nothing truncated. |

The markdown keeps every user and assistant message whole. A tool call is one line with its name and arguments. A tool result longer than 4 KB is cut, and the cut names the `.json` URL that has it in full. That keeps three long sessions inside what an agent can read in one pass. Whether Claude Code's WebFetch reads a large `.md` in full is unproven. The page therefore tells agents to `curl` the `.md` into a file and read the file.

## What the owner does

From any agent with the mnemonic MCP server:

> Share this session and the two about the QUIC cutover with Vlad, title "QUIC context".

The agent finds the ids with `search_sessions` or `list_sessions`, then calls:

```
share_link {"session_ids": ["session:…", "session:…", "session:…"], "title": "QUIC context", "note": "…", "days": 30}
```

The answer is the URL, the link id, the expiry, and one row per session (harness, project, start time, message count, rendered size). The owner sees what goes out before sending it.

From a terminal:

```
mnemonic share <session-id or Claude Code session uuid>... --title "QUIC context" [--days 30]
mnemonic share --list
mnemonic share --revoke <link id>
```

`mnemonic share` prints the URL and, on macOS, copies it to the clipboard. It calls the same tool over the existing `mnemonic mcp` dial. A bare uuid becomes a `session:` id by the existing encoding in `internal/index/q_common.go`.

`share_link {}` with no arguments lists the caller's live links.

## Lies the current design tells

| Lie | The one fix | What gets deleted |
|---|---|---|
| A reader is a principal with a key, so the recipient must install mnemonic and bind a wallet. | The link is the grant. Its principal is a bearer token, checked by hash. | Nothing at first. Wallet sharing becomes a candidate for deletion (owner call 3). |
| The only door is mutually authenticated QUIC, so a browser can never read anything. | A second listener on loopback, read-only, serving `/s/` and nothing else, published through a Cloudflare Tunnel. Inbound ports do not change. | Nothing. No TLS, autocert or TCP listener comes back. |
| `link` visibility means "shareable by link". Today it means "openable by id by any admitted caller", which the new feature makes a weaker duplicate under the same name. | Share links take the name. | `visLink` and its branches, if prod holds no `link` visibility records (owner call 4). |

## Data shape

A link is one control record, written and revoked like the existing share records:

```
{"action":"link","link_id":"<12 hex>","token_sha256":"<64 hex>","session_ids":["session:…"],
 "title":"…","note":"…","cutoff":"<ts>","expires":"<ts>","author":"candyflipline",
 "ts":"…","request_key":"…","writer":"…"}
{"action":"unlink","link_id":"<12 hex>","ts":"…","request_key":"…","writer":"…"}
```

- **One record per link.** The existing records key on one session. A link is a set of sessions, so `parseControl` gains a case where `session_id` is empty and `link_id` is set. One record per link means revoke is one record and the session order the owner chose is kept.
- **The token never touches disk.** The server draws 16 random bytes, returns them once as unpadded base64url (22 characters), and stores only the SHA-256. The control log and its backups are not an authority. A lost URL is replaced by a new link, and the old one stays valid until it expires or is revoked.
- **`cutoff` is the share time.** The render keeps events whose timestamp is at or before it. The link shows what the owner shared, and messages typed after sharing never leak. Lines pushed a few seconds late still appear, because the filter reads event time, not ingest time.
- **`expires`** defaults to 30 days, from 1 to 365, the same policy as wallet bindings.
- **`author`** is the creator. Only a session's sole author, or an admin key, may put it in a link, the same rule as `share_session`. At render time a session shows only while it is not deleted and the creator is still its sole author. A foreign line pushed under a linked session id hides that session instead of leaking into the link.

`Controls` keeps `byToken map[[32]byte]*link`, written only by `apply` under the existing lock. It is an index of the log, the same way `byWallet` is.

## Request path

```
browser or agent
  -> Cloudflare (TLS for share.askloyal.com)
  -> cloudflared on the box, dialing out, no inbound port
  -> mnemonic serve, [share] listener on 127.0.0.1:8742
  -> GET /s/{token}[.md] | /s/{token}/{n}.{md,json}
  -> sha256(token) -> Controls.byToken -> live, unexpired, not unlinked?
  -> for each session id: not deleted, sole author == link author?
  -> render from ix.Snapshot(), events with ts <= cutoff
```

- **Boundary.** The public handler is the only code that reads a token. It parses the token, looks up the link and hands the renderer a typed `Link` (sessions, cutoff, title). The renderer never sees a request or a token. A missing, malformed or unknown token gets 404. A revoked or expired one gets 410. Neither body says which.
- **Package placement.** `internal/index` owns the records, `byToken`, `Controls.Link(hash)` and the `share_link` tool. `internal/export` gains `markdown.go` and `html.go`, both walking the ATIF `Step` sequence that `newTrajectory` already builds, so the three formats cannot drift. `internal/server` gains `Server.Public()`, the `/s/` mux. `cmd/mnemonic` gains `share`. `internal/config` gains `[share] listen` and `base_url`.
- **No new Go module.** `html/template` and `crypto/sha256` are stdlib. The tunnel is a separate binary and service on the box, not a `go.mod` entry.

## Response headers

Every `/s/` response carries:

```
Cache-Control: no-store
X-Robots-Tag: noindex, nofollow
Referrer-Policy: no-referrer
Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'
```

`no-store` keeps Cloudflare and browsers from serving a revoked link. `no-referrer` matters most. Transcripts are full of URLs, and a click on one must not send the token to that site. The page has no script and no external fetch.

## Risks the owner accepts by shipping this

- Anyone the link reaches can read it. That includes forwards, chat unfurl bots, and the provider of any agent the recipient points at it. Expiry and revoke bound the damage. Nothing prevents it.
- The content is what push already redacted, and push redaction was tuned for a server only the team reads. A link is the first path to readers outside the team. The `share_link` answer lists sizes so the owner can open the link and check it before sending.
- The server key does not protect links. Losing the box loses the links. Rotating the server key does not revoke them.

## Build order

Each unit ends in a check on the real thing.

1. **Records and tool.** `link` and `unlink` records, `byToken`, `share_link` create, list and revoke. Check: a scratch test creates, lists, revokes and replays the log, and an old build refuses the new action (rollback note as in wallet sharing).
2. **Render.** `markdown.go` and `html.go` over ATIF steps, with the cutoff and the 4 KB cut. Check: render three real sessions from a local `mnemonic local` index, open the HTML, and read the markdown.
3. **Public listener.** `[share] listen`, `Server.Public()`, status codes and headers. Check: loopback `curl` for every row of the URL table plus revoked, expired, unknown token, deleted session and foreign author.
4. **CLI.** `mnemonic share`. Check: it prints a URL that `curl` resolves on loopback.
5. **Tunnel and deploy (owner's go).** cloudflared as a systemd unit, a CNAME for `share.askloyal.com`, deploy. Check: done-predicate items 1 to 7 on prod, item 7 with `ss -lntup` on the box and an outside port scan.

One review runs over the whole branch before deploy, not per unit.

## Owner calls

1. **Snapshot or live.** Proposed: snapshot at share time. Live would follow a running session but also publish anything typed after sharing.
2. **Host name.** Proposed: `share.askloyal.com`. The zone is already on Cloudflare.
3. **Wallet sharing.** It shipped on 2026-10-27, and the deploy notes record no share made on prod. Share links cover "give someone these threads" with no install. Wallet sharing still uniquely gives a recipient's agent `search_sessions` across what was shared. Recommendation: keep it until links ship, then delete it if nobody has used it.
4. **`link` visibility.** Recommendation: delete it and give the name to share links, after checking prod's control log for `"visibility":"link"` records.
5. **Default expiry.** Proposed: 30 days, from 1 to 365.
