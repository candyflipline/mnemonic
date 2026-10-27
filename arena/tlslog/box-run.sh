#!/bin/sh
# tlslog box proof: one run on the before binary (c1d2588) and one on the
# after binary (this lane, 8de6ff6). Each serve gets its own network
# namespace, so its config can carry [tls] on 443/8741 while nothing on the
# host's ports is touched; the serve runs under the memory guard (MemoryMax
# 8G, GOMAXPROCS 4) exactly as prod. Kills are by exact pid only (in-ns.sh's
# pid file and trap). Usage: box-run.sh
set -u
BIN=/var/lib/mnu/tlslog-bin
RUN=/var/lib/mnu/tlslog-run
OUT=/var/lib/mnu/out
RESULTS=$OUT/tlslog-results.txt

export PATH=/usr/sbin:/usr/bin:/sbin:/bin:$PATH

# Room guard (STANDING: available >= 10G, fewer than 3 serves box-wide); wait
# for it rather than failing, other lanes' serves come and go.
i=0
while :; do
	avail=$(free -g | awk '/^Mem:/{print $7}')
	serves=$(pgrep -c -f ' serve --config ' || true)
	if [ "${avail:-0}" -ge 10 ] && [ "${serves:-0}" -lt 3 ]; then
		break
	fi
	i=$((i+1))
	if [ "$i" -gt 80 ]; then
		echo "FAIL: precondition waited out (available ${avail}G, serves $serves)" >>"$RESULTS"
		exit 1
	fi
	echo "waiting: available ${avail}G, serves $serves" >>"$RESULTS"
	sleep 30
done

cat >"$RUN/server.toml" <<EOF
data_dir = "$RUN/data"
listen   = "127.0.0.1:443"
push_listen = "127.0.0.1:8741"

[tls]
hosts = ["tlslog.invalid"]

[[tokens]]
author = "tlslog"
sha256 = "0000000000000000000000000000000000000000000000000000000000000000"
EOF

for pair in "c1d2588 $BIN/mnemonic-c1d2588" "8de6ff6 $BIN/mnemonic-8de6ff6"; do
	TAG=${pair%% *}
	BIN_PATH=${pair#* }
	avail=$(free -g | awk '/^Mem:/{print $7}')
	serves=$(pgrep -c -f ' serve --config ' || true)
	if [ "${avail:-0}" -lt 10 ] || [ "${serves:-0}" -ge 3 ]; then
		echo "$TAG FAIL: precondition (available ${avail}G, serves $serves)" >>"$RESULTS"
		exit 1
	fi
	echo "$TAG starting (available ${avail}G, serves $serves)" >>"$RESULTS"
	systemd-run --scope -q -p MemoryMax=8G -p MemorySwapMax=0 \
		env GOMAXPROCS=4 unshare --net sh "$BIN/in-ns.sh" "$BIN_PATH" "$TAG" "$RUN" "$RESULTS" \
		>"$OUT/tlslog-$TAG.log" 2>&1
	echo "$TAG wrapper rc=$?" >>"$RESULTS"
done
echo ALL-DONE >>"$RESULTS"
