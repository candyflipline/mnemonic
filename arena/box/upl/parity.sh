#!/bin/sh
# parity.sh BIN — the index parity check (63 public + 63 hidden calls) on the box.
#
# Scores BIN's `index ingest|batch` against the oracle's expected answers in the
# arena59 fixture (expected answers regenerated at 59796bc, no oracle needed),
# clean phase only: -skip incremental,crash,appendcrash. Prints the scorecard's
# verdict and the public and hidden pass/fail counts; exit 0 iff pass and
# 126 calls passed.
#
# The check binary is arena/check built CGO_ENABLED=0 GOOS=linux from
# lane/engine-harness 391e8bb (main 83b0042 has no arena/check): it sits at
# $ARENA_DIR/check-bin. arena/check runs `/usr/bin/time -l` (a BSD flag), so
# the once-per-box shim from arena/box/build.sh is installed when missing.
# Env: BOX (/var/lib/mnu), ARENA_DIR ($BOX/arena), NAME (default
# parity-<sha8 of BIN>). Run under the box lock:
#   flock /var/lib/mnu/box.lock sh parity.sh BIN
set -eu

BIN=${1:?usage: parity.sh BIN}
BOX=${BOX:-/var/lib/mnu}
ARENA_DIR=${ARENA_DIR:-$BOX/arena}
CHECK=$ARENA_DIR/check-bin
FIX=$ARENA_DIR/arena59
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }
[ -x "$CHECK" ] && [ -d "$FIX" ] || { echo "no check-bin or arena59 under $ARENA_DIR" >&2; exit 2; }
SHA8=$(sha256sum "$BIN" | cut -c1-8)
NAME=${NAME:-parity-$SHA8}

if [ ! -e /usr/bin/time.gnu ]; then
	cp /usr/bin/time /usr/bin/time.gnu
	cat > /usr/bin/time <<'SHIM'
#!/bin/sh
# Installed by arena/box/upl/parity.sh (same shim as arena/box/build.sh):
# translate the BSD `-l` arena/check uses into GNU time's max-RSS line.
if [ "$1" = "-l" ]; then
	shift
	exec /usr/bin/time.gnu -f '%M maximum resident set size' "$@"
fi
exec /usr/bin/time.gnu "$@"
SHIM
	chmod +x /usr/bin/time
fi

# file_attention reads the host filesystem (internal/index/q_list_helpers.go:206
# qFindProjectRoot stats <dir>/.git up from each touched path; q_attention_tail.go
# :89 qWorktreeRootsForRepoRoot reads the git dir), so its expected answers hold
# the repo roots of the machine that made them. Without those directories the box
# answers 113 of 126 (the 13 file_attention calls: roots 1 where 2 are expected).
# Create each absolute root the fixture's expected file_attention answers name,
# as an empty <root>/.git directory; idempotent, never touches an existing one.
python3 -I - "$FIX" <<'PYEOF'
import json, os, sys
fix = sys.argv[1]
roots = set()
def walk(o):
    if isinstance(o, dict):
        for k, v in o.items():
            if k in ("root", "worktree_root") and isinstance(v, str):
                roots.add(v)
            elif k == "worktree_roots" and isinstance(v, list):
                roots.update(r for r in v if isinstance(r, str))
            else:
                walk(v)
    elif isinstance(o, list):
        for v in o:
            walk(v)
for s in ("golden", "hidden"):
    tool = {}
    for l in open(os.path.join(fix, s, "calls.jsonl")):
        c = json.loads(l)
        tool[c["id"]] = c["tool"]
    for l in open(os.path.join(fix, s, "expected.jsonl")):
        e = json.loads(l)
        if tool.get(e["id"]) == "file_attention":
            walk(e.get("data"))
made = 0
for r in sorted(roots):
    if r.startswith("/") and not os.path.exists(r + "/.git"):
        os.makedirs(r + "/.git")
        made += 1
print("stub_repo_roots\t%d created of %d named" % (made, len([r for r in roots if r.startswith("/")])))
PYEOF

WRAP=$ARENA_DIR/wrapper-$SHA8
{
	echo '#!/bin/sh'
	echo "exec $(readlink -f "$BIN") index \"\$@\""
} > "$WRAP"
chmod +x "$WRAP"

CARD=$BOX/out/base-$NAME.json
RC=0
"$CHECK" -arena "$FIX" -bin "$WRAP" -name "$NAME" -skip incremental,crash,appendcrash > "$CARD" 2> "$BOX/out/base-$NAME.err" || RC=$?
jq -r '"name\t" + .name,
       "pass\t" + (.pass|tostring),
       "public\t" + ([.public.tools[]|.pass]|add|tostring) + " pass, " + ([.public.tools[]|.fail]|add|tostring) + " fail",
       "hidden\t" + ([.hidden.tools[]|.pass]|add|tostring) + " pass, " + ([.hidden.tools[]|.fail]|add|tostring) + " fail",
       "error\t" + (.error // ""),
       "clean_ingest_wall_ms\t" + (.clean_ingest.wall_ms|tostring)' "$CARD"
TOTAL=$(jq '([.public.tools[]|.pass]|add) + ([.hidden.tools[]|.pass]|add)' "$CARD")
echo "calls_passed	$TOTAL"
[ "$RC" = 0 ] && [ "$TOTAL" = 126 ]
