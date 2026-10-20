#!/usr/bin/env bash
#	CH_URL=https://host:8443 CH_USER=... CH_PASSWORD=... CH_DB=... scripts/export_ch.sh out
#
# CH_USER and CH_PASSWORD reach curl through a 0600 temp config and are never
# on argv or stdout. Query text goes in the POST body.
set -euo pipefail

out_dir=${1:?usage: export_ch.sh <out_dir>}
url=${CH_URL:?CH_URL is required}
user=${CH_USER:?CH_USER is required}
password=${CH_PASSWORD:?CH_PASSWORD is required}
db=${CH_DB:?set CH_DB to the ClickHouse database}

conf=$(mktemp)
trap 'rm -f "$conf"' EXIT
printf 'user = "%s:%s"\n' "$user" "$password" >"$conf"

post() {
	curl -sS -f -X POST "$url/" -K "$conf" --data-binary "$1" "${@:2}"
}

columns=$(cut -f1 "$(dirname "$0")/../docs/clickhouse/events.tsv" | paste -sd, - | sed 's/,/, /g')

mkdir -p "$out_dir"
months=$(post "SELECT DISTINCT toYYYYMM(event_ts) FROM $db.events ORDER BY 1 FORMAT TSV")
for month in $months; do
	echo "exporting $db $month -> $out_dir/events-$month.parquet" >&2
	post "SELECT $columns FROM $db.events WHERE toYYYYMM(event_ts) = $month FORMAT Parquet" \
		-o "$out_dir/events-$month.parquet"
done
