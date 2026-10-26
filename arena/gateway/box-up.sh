#!/bin/sh
# box-up.sh: on mnemonic-p1, start one throwaway serve (HTTP 127.0.0.1:28740,
# push 127.0.0.1:28741) and the quick tunnels the spike drives from the Mac.
# Every pid goes to $W/pids (name pid); stop them with box-down.sh, never by
# pattern. Hostnames go to $W/hosts. Spike tooling, not ops.
#   sh box-up.sh            serve + quick tunnel per port (http2 origin on push)
#   MODE=serve sh box-up.sh serve only
set -eu
W=${W:-/var/lib/mnu/spike}
BIN=${BIN:-/var/lib/mnu/spike-bin/mnemonic}
CFD=${CFD:-/var/lib/mnu/spike-bin/cloudflared}
MODE=${MODE:-all}
mkdir -p "$W/data" "$W/cfg" "$W/log"
chmod 700 "$W/cfg"
touch "$W/pids"
if [ ! -s "$W/cfg/server.toml" ]; then
	"$BIN" token new --author spike > "$W/cfg/token.out"
	head -1 "$W/cfg/token.out" > "$W/cfg/token"
	chmod 600 "$W/cfg/token"
	{
		echo "data_dir = \"$W/data\""
		echo 'listen = "127.0.0.1:28740"'
		echo 'push_listen = "127.0.0.1:28741"'
		tail -n +2 "$W/cfg/token.out"
	} > "$W/cfg/server.toml"
fi
setsid nohup systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0 env GOMAXPROCS=4 \
	"$BIN" serve --config "$W/cfg/server.toml" </dev/null > "$W/log/serve.log" 2>&1 &
echo "serve $!" >> "$W/pids"
i=0
until grep -q 'push 127.0.0.1:28741' "$W/log/serve.log"; do
	i=$((i + 1)); [ "$i" -lt 150 ] || { echo "serve not up" >&2; exit 1; }
	sleep 0.2
done
[ "$MODE" = serve ] && exit 0
quick() { # name url extra...
	n=$1; u=$2; shift 2
	setsid nohup "$CFD" tunnel --no-autoupdate --metrics 127.0.0.1:0 --url "$u" "$@" </dev/null > "$W/log/cfd-$n.log" 2>&1 &
	echo "cfd-$n $!" >> "$W/pids"
	i=0
	until h=$(grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' "$W/log/cfd-$n.log" | head -1) && [ -n "$h" ]; do
		i=$((i + 1)); [ "$i" -lt 150 ] || { echo "no hostname for $n" >&2; exit 1; }
		sleep 0.2
	done
	echo "$n $h" >> "$W/hosts"
}
quick push http://127.0.0.1:28741 --http2-origin
quick http http://127.0.0.1:28740
cat "$W/hosts"
