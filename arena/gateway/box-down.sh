#!/bin/sh
# box-down.sh NAME...: kill the named spike processes by the exact pid box-up.sh recorded.
W=${W:-/var/lib/mnu/spike}
for n in "$@"; do
	p=$(awk -v n="$n" '$1==n{print $2}' "$W/pids" | tail -1)
	[ -n "$p" ] || { echo "no pid for $n"; continue; }
	kill "$p" 2>/dev/null && echo "killed $n $p" || echo "$n $p not running"
done
