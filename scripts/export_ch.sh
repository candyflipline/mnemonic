#!/usr/bin/env bash
# Export the retired ClickHouse events to per-month parquet for `mnemonic
# import-ch`: one <out_dir>/events-<YYYYMM>.parquet per month, all 59
# events.tsv columns in order.
#
#	CH_URL=https://host:8443 CH_USER=... CH_PASSWORD=... scripts/export_ch.sh out
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

post() { # post <query> [extra curl args...]
	curl -sS -f -X POST "$url/" -K "$conf" --data-binary "$1" "${@:2}"
}

columns='ingested_at, event_uid, session_id, author, session_date, source_name, harness, inference_provider, source_file, source_inode, source_generation, source_line_no, source_offset, source_ref, record_ts, event_ts, event_kind, actor_kind, payload_type, op_kind, op_status, request_id, trace_id, turn_index, item_id, tool_call_id, parent_tool_call_id, origin_event_id, origin_tool_call_id, tool_name, tool_phase, tool_error, agent_run_id, agent_label, coord_group_id, coord_group_label, is_substream, model, endpoint_kind, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, latency_ms, retry_count, service_tier, content_types, has_reasoning, text_content, text_preview, payload_json, token_usage_json, token_usage_buckets, token_usage_native_units, project_id, repo_rel_path, worktree_root, event_version, cwd'

mkdir -p "$out_dir"
months=$(post "SELECT DISTINCT toYYYYMM(event_ts) FROM $db.events ORDER BY 1 FORMAT TSV")
for month in $months; do
	echo "exporting $db $month -> $out_dir/events-$month.parquet" >&2
	post "SELECT $columns FROM $db.events WHERE toYYYYMM(event_ts) = $month FORMAT Parquet" \
		-o "$out_dir/events-$month.parquet"
done
