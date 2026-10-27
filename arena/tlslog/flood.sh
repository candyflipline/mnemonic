#!/bin/sh
# Opens a TCP connection to HOST:PORT and closes it immediately, about RATE
# times a second for SECONDS_RUN seconds (the prod shape: a TLS listener that
# never receives a ClientHello). Prints one JSON line with attempts and
# connected counts. Usage: flood.sh HOST PORT SECONDS_RUN [RATE]
set -u
HOST=$1
PORT=$2
SECONDS_RUN=$3
RATE=${4:-16}
python3 - "$HOST" "$PORT" "$SECONDS_RUN" "$RATE" <<'EOF'
import json, socket, sys, time
host, port, secs, rate = sys.argv[1], int(sys.argv[2]), float(sys.argv[3]), float(sys.argv[4])
interval = 1.0 / rate
attempts = connected = 0
end = time.monotonic() + secs
while time.monotonic() < end:
    t0 = time.monotonic()
    attempts += 1
    try:
        s = socket.create_connection((host, port), timeout=5)
        s.close()
        connected += 1
    except OSError:
        pass
    pause = interval - (time.monotonic() - t0)
    if pause > 0:
        time.sleep(pause)
print(json.dumps({"host": host, "port": port, "attempts": attempts, "connected": connected}))
EOF
