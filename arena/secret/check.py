#!/usr/bin/env python3
"""Planted-secret check: no tool of either mode ever returns a planted secret.

usage: python3 -I arena/secret/check.py --bin BIN --out DIR [--mode local|server|both]

BIN is a `-tags server` build. DIR must not exist. The fixture is the arena59
raw copy (RAW, default /Users/user/loyal/mnemonic-harness/engine/arena59/raw):
a few of its sources are decompressed to harness-native paths under a fresh
HOME, two small plant files are written next to them (one claude-code, one
codex, each a clone of a fixture line with the text replaced), and one more
secret is appended to each after the first ingest. No corpus data is kept
anywhere but DIR.

Server mode: `mnemonic serve` on 127.0.0.1 ports fed by `mnemonic push` from
that HOME. Local mode: `mnemonic local` on that HOME. Per mode the script
calls all ten tools, with probes that carry the planted text in their
arguments or aim at it (search, open, get_trace, query text_content / lower()
/ like on the secret and on its prefix and tail, annotate and set_visibility
and delete_session with the secret as text/label/request_key), then counts
raw-secret occurrences in every response body. A hit is any response holding
a secret, its lowercase, or a 12+ byte substring of it, after the call's own
string arguments are cut out of the response (a tool echoing what the caller
just sent is the caller's text, not a stored secret). Exit 1 on any hit.
Run it under nice -n 20 (children inherit it). The table printed at the end is the result; secrets are never printed.
"""
import argparse, glob, json, os, random, re, subprocess, sys, time, urllib.request, uuid

RAW = "/Users/user/loyal/mnemonic-harness/engine/arena59/raw"
ROOTS = {("claude-code", "claude"): ".claude/projects/", ("codex", "codex"): ".codex/sessions/"}
DAY = "2026-10-20"
WIN = ("2026-10-01T00:00:00Z", "2026-10-31T00:00:00Z")
WIDE = ("2025-01-01T00:00:00Z", "2026-12-31T00:00:00Z")
rng = random.Random(59)


def rand(alpha, n):
    return "".join(rng.choice(alpha) for _ in range(n))


ALNUM = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
SECRETS = {
    "gh1": "ghp_" + rand(ALNUM, 36),
    "aws1": "AKIA" + rand("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 16),
    "pem1": "-----BEGIN PRIVATE KEY-----\n" + "\n".join(rand(ALNUM + "+/", 64) for _ in range(3)) + "\n-----END PRIVATE KEY-----",
    "gh2": "ghp_" + rand(ALNUM, 36),
    "aws2": "AKIA" + rand("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 16),
}
PEM_BODY = SECRETS["pem1"].split("\n")[1]
NEEDLES = {}
for k, v in SECRETS.items():
    NEEDLES[k] = [v, v.lower()]
    if k.startswith("pem"):
        NEEDLES[k] += [l for l in v.split("\n")[1:-1]]
    else:
        NEEDLES[k] += [v[4:], v[4:].lower(), v[-12:], v[-12:].lower()]
FLAT = sorted({n for ns in NEEDLES.values() for n in ns if len(n) >= 12})
MARK1, MARK2 = "fxmarkerone", "fxmarkertwo"


def sh(cmd, **kw):
    return subprocess.run(cmd, check=True, **kw)


def fixture_home(home, nclaude, ncodex):
    mans = []
    for h, n in (("claude-code", nclaude), ("codex", ncodex)):
        ms = []
        for p in sorted(glob.glob(f"{RAW}/author=*/machine=*/harness={h}/*.json")):
            d = json.load(open(p))
            root = ROOTS.get((d["harness"], d["source_name"]))
            if root and 3000 < d["size"] < 400000:
                ms.append((p, d, root))
        mans += ms[:n]
    n = 0
    for p, d, root in mans:
        t = os.path.join(home, root, "fx%d" % n, os.path.basename(d["path"]))
        os.makedirs(os.path.dirname(t), exist_ok=True)
        with open(p[:-5] + ".jsonl.zst", "rb") as f, open(t, "wb") as o:
            sh(["zstd", "-d", "-c"], stdin=f, stdout=o, stderr=subprocess.DEVNULL)
        n += 1
    return len(mans)


def first_line(home, root, pred):
    for p in sorted(glob.glob(os.path.join(home, root, "**", "*.jsonl"), recursive=True)):
        with open(p) as f:
            for l in f:
                try:
                    j = json.loads(l)
                except ValueError:
                    continue
                if pred(j):
                    return j
    sys.exit("fixture has no line for " + root)


def ts(h, m, s=0):
    return "%sT%02d:%02d:%02d.000Z" % (DAY, h, m, s)


def claude_user(tpl, sid, text, when):
    j = json.loads(json.dumps(tpl))
    j["sessionId"], j["uuid"], j["timestamp"], j["parentUuid"] = sid, str(uuid.uuid4()), when, None
    j["message"] = {"role": "user", "content": text}
    return j


def plant(home):
    ctpl = first_line(home, ".claude/projects", lambda j: j.get("type") == "user" and isinstance(j.get("message", {}).get("content"), str))
    sid = str(uuid.uuid4())
    cpath = os.path.join(home, ".claude/projects/fxplant/%s.jsonl" % sid)
    os.makedirs(os.path.dirname(cpath), exist_ok=True)
    body = "%s planted token %s and key %s then\n%s\nend of plant" % (MARK1, SECRETS["gh1"], SECRETS["aws1"], SECRETS["pem1"])
    with open(cpath, "w") as f:
        f.write(json.dumps(claude_user(ctpl, sid, body, ts(12, 0))) + "\n")
    meta = first_line(home, ".codex/sessions", lambda j: j.get("type") == "session_meta")
    msg = first_line(home, ".codex/sessions", lambda j: j.get("type") == "response_item" and j.get("payload", {}).get("type") == "message" and j["payload"].get("role") == "user")
    xid = str(uuid.uuid4())
    meta = json.loads(json.dumps(meta))
    meta["timestamp"], meta["payload"]["id"] = ts(12, 5), xid
    meta["payload"]["timestamp"] = ts(12, 5)
    xpath = os.path.join(home, ".codex/sessions/2026/10/20/rollout-2026-10-20T12-05-00-%s.jsonl" % xid)
    os.makedirs(os.path.dirname(xpath), exist_ok=True)
    msg = json.loads(json.dumps(msg))
    msg["timestamp"] = ts(12, 6)
    msg["payload"]["content"] = [{"type": "input_text", "text": "%s codex token %s key %s\n%s\nend" % (MARK1, SECRETS["gh1"], SECRETS["aws1"], SECRETS["pem1"])}]
    with open(xpath, "w") as f:
        f.write(json.dumps(meta) + "\n" + json.dumps(msg) + "\n")
    return dict(ctpl=ctpl, csid=sid, cpath=cpath, mtpl=msg, xpath=xpath)


def append_plant(p):
    with open(p["cpath"], "a") as f:
        f.write(json.dumps(claude_user(p["ctpl"], p["csid"], "%s appended token %s key %s" % (MARK2, SECRETS["gh2"], SECRETS["aws2"]), ts(13, 0))) + "\n")
    m = json.loads(json.dumps(p["mtpl"]))
    m["timestamp"] = ts(13, 1)
    m["payload"]["content"] = [{"type": "input_text", "text": "%s appended token %s key %s" % (MARK2, SECRETS["gh2"], SECRETS["aws2"])}]
    with open(p["xpath"], "a") as f:
        f.write(json.dumps(m) + "\n")


class Mcp:
    def __init__(self, url, token, resp_dir):
        self.url, self.token, self.dir, self.n = url, token, resp_dir, 0
        os.makedirs(resp_dir, exist_ok=True)
        self.log = []

    def call(self, tool, args, err_ok=False):
        self.n += 1
        body = json.dumps({"jsonrpc": "2.0", "id": self.n, "method": "tools/call", "params": {"name": tool, "arguments": args}}).encode()
        req = urllib.request.Request(self.url, body, {"Content-Type": "application/json", "Accept": "application/json", "MCP-Protocol-Version": "2025-06-18", "Authorization": "Bearer " + self.token}, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=90) as r:
                out = r.read().decode()
        except urllib.error.HTTPError as e:
            out = e.read().decode()
        open(os.path.join(self.dir, "%03d-%s.json" % (self.n, tool)), "w").write(out)
        rest = out
        for v in sorted((x for x in args.values() if isinstance(x, str) and len(x) >= 12), key=len, reverse=True):
            for e in (v, json.dumps(v)[1:-1], json.dumps(v, ensure_ascii=False)[1:-1]):
                rest = rest.replace(e, "")
        hits = [n for n in FLAT if n in rest]
        try:
            j = json.loads(out)
        except ValueError:
            j = {}
        sc = (j.get("result") or {}).get("structuredContent") or {}
        failed = bool(j.get("error") or sc.get("error") or (j.get("result") or {}).get("isError") or not j)
        self.log.append((tool, hits, out, failed and not err_ok, out.count("[REDACTED:")))
        if not j:
            return {}
        return (j.get("result") or {}).get("structuredContent", {}).get("data") or j


def ids(v, out):
    if isinstance(v, str):
        if v.startswith("session:"):
            out.add(v)
    elif isinstance(v, dict):
        [ids(x, out) for x in v.values()]
    elif isinstance(v, list):
        [ids(x, out) for x in v]
    return out


def wait_for(fn, what, secs=180):
    t0 = time.time()
    while time.time() - t0 < secs:
        if fn():
            return time.time() - t0
        time.sleep(1)
    sys.exit("timeout waiting for " + what)


def probes(m, tag):
    """Every tool, arguments carrying or aiming at the planted text."""
    g, a, pem = SECRETS["gh1" if tag == "first" else "gh2"], SECRETS["aws1" if tag == "first" else "aws2"], SECRETS["pem1"]
    mark = MARK1 if tag == "first" else MARK2
    s = m.call("search_sessions", {"query": mark, "n_hits": 20})
    planted = sorted(ids(s, set()))
    for q in (g, a, g.lower(), g[:8], g[4:20], "BEGIN PRIVATE KEY", PEM_BODY, a[:8]):
        m.call("search_sessions", {"query": q, "n_hits": 20})
    ls = m.call("list_sessions", {"limit": 25, "start_datetime": WIDE[0], "end_datetime": WIDE[1]})
    sess = sorted(set(planted) | ids(ls, set()))
    for sid in sess:
        cur = None
        for _ in range(8):
            o = m.call("open", {"id": sid, "limit": 25, **({"cursor": cur} if cur else {})})
            cur = (o.get("next_cursor") or o.get("cursor")) if isinstance(o, dict) else None
            if not cur:
                break
        raw = sid.split(":", 1)[1] if ":" in sid else sid
        m.call("get_trace", {"session_id": sid, "include_annotations": True})
    for path in (g, a, "/tmp/" + g, "/tmp/x.go"):
        m.call("file_attention", {"path": path, "granularity": "events", "start_datetime": WIDE[0], "end_datetime": WIDE[1]})
        m.call("file_attention", {"path": path, "granularity": "sessions", "start_datetime": WIDE[0], "end_datetime": WIDE[1]})
    m.call("get_ingest_status", {})
    qs = [
        "from: events | filter: text_content like '%%%s%%' | select: session_id, text_content | limit: 20" % g,
        "from: events | filter: lower(text_content) like '%%%s%%' | select: session_id, lower(text_content) as t | limit: 20" % g.lower(),
        "from: events | filter: text_content like '%%%s%%' | select: session_id, text_content | limit: 20" % g[:8],
        "from: events | filter: lower(text_content) like '%%%s%%' | select: session_id, text_content | limit: 20" % g[:8].lower(),
        "from: events | filter: text_content ilike '%%%s%%' | select: session_id, text_content | limit: 20" % a[:8],
        "from: events | filter: lower(text_content) like '%%%s%%' | select: session_id, text_content | limit: 20" % a.lower(),
        "from: events | filter: text_content like '%%BEGIN PRIVATE KEY%%' | select: session_id, text_content | limit: 20",
        "from: events | filter: text_content like '%%%s%%' | select: session_id, text_content | limit: 20" % PEM_BODY[:16],
        "from: events | filter: text_content match '%s' | select: session_id, text_content | limit: 20" % mark,
        "from: events | filter: text_content like '%%%s%%' | select: session_id, text_content, payload_json | limit: 20" % mark,
        "from: events | filter: payload_json like '%%%s%%' | select: session_id, payload_json | limit: 20" % g[:8],
        "from: events | filter: lower(text_content) like '%redacted:github-token%' | select: session_id, text_content | limit: 20",
    ]
    for q in qs:
        m.call("query", {"query": q, "from": WIN[0], "to": WIN[1]})
    return planted


def tools_write(m, sess):
    """annotate, set_visibility, delete_session with the secret as free text."""
    s0 = sess[0]
    m.call("annotate", {"session_id": s0, "name": "fx-note", "text": SECRETS["gh1"], "source": "human"})
    m.call("annotate", {"session_id": s0, "name": "fx-label", "label": SECRETS["aws1"], "source": "scorer"})
    m.call("annotate", {"session_id": s0, "name": "fx-pem", "text": SECRETS["pem1"], "source": "human"})
    time.sleep(4)
    m.call("get_trace", {"session_id": s0, "include_annotations": True})
    m.call("query", {"query": "from: annotations | select: annotation_id, name, value_text, value_label | limit: 20", "from": WIN[0], "to": WIN[1]})
    m.call("query", {"query": "from: annotations | filter: lower(value_text) like '%%%s%%' | select: name, value_text | limit: 20" % SECRETS["gh1"][:8].lower(), "from": WIN[0], "to": WIN[1]})
    m.call("set_visibility", {"session_id": s0, "visibility": "private", "request_key": SECRETS["aws1"]})
    m.call("list_sessions", {"limit": 25, "start_datetime": WIDE[0], "end_datetime": WIDE[1]})
    m.call("search_sessions", {"query": SECRETS["gh1"][:12], "n_hits": 20})
    m.call("delete_session", {"session_id": sess[-1], "request_key": SECRETS["gh1"]})
    m.call("open", {"id": sess[-1]}, err_ok=True)
    m.call("list_sessions", {"limit": 25, "start_datetime": WIDE[0], "end_datetime": WIDE[1]})


def listening(port):
    import socket
    with socket.socket() as c:
        return c.connect_ex(("127.0.0.1", port)) == 0


def start(cmd, env, log):
    f = open(log, "w")
    p = subprocess.Popen(cmd, env=env, stdout=f, stderr=subprocess.STDOUT)
    return p


def run_mode(mode, binp, out, nclaude, ncodex, port):
    d = os.path.join(out, mode)
    home = os.path.join(d, "home")
    os.makedirs(home)
    nsrc = fixture_home(home, nclaude, ncodex)
    pl = plant(home)
    env = dict(os.environ, HOME=home, GOMAXPROCS="4")
    procs = []
    try:
        if mode == "local":
            srv = start([binp, "local", "-data-dir", d + "/data", "-listen", "127.0.0.1:%d" % port], env, d + "/server.log")
            procs.append(srv)
            m = Mcp("http://127.0.0.1:%d/mcp" % port, "local", d + "/resp")
            push = lambda: None
        else:
            tok = subprocess.run([binp, "token", "new", "--author", "fx"], env=env, capture_output=True, text=True, check=True).stdout.split("\n")
            os.makedirs(d + "/cfg")
            open(d + "/cfg/token", "w").write(tok[0] + "\n")
            os.chmod(d + "/cfg/token", 0o600)
            open(d + "/cfg/server.toml", "w").write('data_dir = "%s/data"\nlisten = "127.0.0.1:%d"\npush_listen = "127.0.0.1:%d"\n%s\n' % (d, port, port + 1, "\n".join(tok[1:]).rstrip() + '\nscopes = ["read", "push", "annotate", "admin"]'))
            open(d + "/cfg/client.toml", "w").write('server = "http://127.0.0.1:%d"\ntoken_file = "%s/cfg/token"\nmachine = "fxmac"\n' % (port + 1, d))
            os.makedirs(d + "/data")
            srv = start([binp, "serve", "--config", d + "/cfg/server.toml"], env, d + "/server.log")
            procs.append(srv)
            m = Mcp("http://127.0.0.1:%d/mcp" % port, tok[0], d + "/resp")
            push = lambda: sh(["nice", "-n", "20", binp, "push", "--config", d + "/cfg/client.toml"], env=env, stdout=open(d + "/push.log", "a"), stderr=subprocess.STDOUT)
        wait_for(lambda: listening(port) and (mode == "local" or listening(port + 1)), "server listening", 60)
        push()
        t1 = wait_for(lambda: ids(m.call("search_sessions", {"query": MARK1, "n_hits": 20}), set()), "first ingest")
        m.log.clear()
        sess = probes(m, "first")
        assert len(sess) >= 2, "planted sessions not found: %d" % len(sess)
        tools_write(m, sess)
        append_plant(pl)
        push()
        t2 = wait_for(lambda: ids(m.call("search_sessions", {"query": MARK2, "n_hits": 20}), set()), "appended ingest")
        probes(m, "second")
    finally:
        for p in procs:
            p.terminate()
        for p in procs:
            try:
                p.wait(30)
            except subprocess.TimeoutExpired:
                p.kill()
    return nsrc, t1, t2, m.log


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bin", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--mode", default="both", choices=["local", "server", "both"])
    ap.add_argument("--claude", type=int, default=6)
    ap.add_argument("--codex", type=int, default=3)
    ap.add_argument("--port", type=int, default=7791)
    a = ap.parse_args()
    if os.path.exists(a.out):
        sys.exit(a.out + " exists")
    os.makedirs(a.out)
    modes = ["server", "local"] if a.mode == "both" else [a.mode]
    tools = ["search_sessions", "set_visibility", "file_attention", "delete_session", "get_trace", "annotate", "get_ingest_status", "open", "list_sessions", "query"]
    bad = 0
    print("mode\ttool\tprobes\thits\tnote")
    for i, mode in enumerate(modes):
        nsrc, t1, t2, log = run_mode(mode, a.bin, a.out, a.claude, a.codex, a.port + 10 * i)
        for t in tools:
            rows = [r for r in log if r[0] == t]
            hit = sum(1 for r in rows if r[1])
            errs = sum(1 for r in rows if r[3])
            bad += hit + errs
            print("%s\t%s\t%d\t%d\t%s%s" % (mode, t, len(rows), hit, "errored=%d" % errs if errs else "", "" if rows else "\tNOT CALLED"))
            if not rows:
                bad += 1
            for r in rows:
                if r[1]:
                    print("  HIT %s needles=%d resp=%s" % (t, len(r[1]), r[2][:0]))
        print("%s\tcontrol\tmasks in responses (github/aws/pem): %s\t(sources %d, first ingest %.0fs, appended %.0fs)" % (mode, "/".join(str(sum(r[2].count("[REDACTED:" + k + "]") for r in log)) for k in ("github-token", "aws-access-token", "pem-private-key")), nsrc, t1, t2))
    print("TOTAL HITS", bad)
    sys.exit(1 if bad else 0)


main()
