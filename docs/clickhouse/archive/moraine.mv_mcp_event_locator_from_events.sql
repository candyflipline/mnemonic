CREATE MATERIALIZED VIEW moraine.mv_mcp_event_locator_from_events TO moraine.mcp_event_locator
(
    `event_uid` String,
    `event_version` UInt64,
    `ingested_at` DateTime64(3),
    `session_id` String,
    `source_name` LowCardinality(String),
    `source_file` String,
    `source_generation` UInt32,
    `source_offset` UInt64,
    `source_line_no` UInt64,
    `sort_time` DateTime64(3),
    `doc_len` UInt32,
    `text_digest` String,
    `payload_phase` String,
    `project_id` String,
    `repo_rel_path` String,
    `worktree_root` String,
    `path_tokens` Array(String),
    `has_codex_mcp` UInt8
)
AS WITH JSONExtractString(payload_json, 'moraine_tool_io', 'input_json') AS tool_input
SELECT
    event_uid,
    event_version,
    ingested_at,
    session_id,
    source_name,
    source_file,
    source_generation,
    source_offset,
    source_line_no,
    ifNull(parseDateTime64BestEffortOrNull(record_ts), toDateTime64('1970-01-01 00:00:00', 3)) AS sort_time,
    toUInt32(length(extractAll(lowerUTF8(text_content), '[a-z0-9_]+'))) AS doc_len,
    hex(SHA256(text_content)) AS text_digest,
    JSONExtractString(payload_json, 'phase') AS payload_phase,
    JSONExtractString(payload_json, 'moraine_tool_io', 'project_id') AS project_id,
    JSONExtractString(payload_json, 'moraine_tool_io', 'repo_rel_path') AS repo_rel_path,
    JSONExtractString(payload_json, 'moraine_tool_io', 'worktree_root') AS worktree_root,
    arrayFilter(path -> (path != ''), arrayDistinct(arrayConcat(extractAll(tool_input, '"(?:file_path|notebook_path|path|target_file|relativeWorkspacePath|relative_workspace_path|filepath|file|filename)"[[:space:]]*:[[:space:]]*"((?:[^"\\\\]|\\\\.)*)"'), extractAll(if(JSONExtractString(tool_input, 'command') != '', JSONExtractString(tool_input, 'command'), JSONExtractString(tool_input, 'cmd')), '(?:^|[[:space:]\'"`=(])((?:/|\\./|\\.\\./)?[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+|[A-Za-z0-9_-]+\\.[A-Za-z0-9_.-]+)(?:[[:space:]\'"`,;|&<>)]|$)'), [JSONExtractString(tool_input, 'file_path'), JSONExtractString(tool_input, 'notebook_path'), JSONExtractString(tool_input, 'path'), JSONExtractString(tool_input, 'target_file'), JSONExtractString(tool_input, 'relativeWorkspacePath'), JSONExtractString(tool_input, 'relative_workspace_path'), JSONExtractString(tool_input, 'filepath'), JSONExtractString(tool_input, 'file'), JSONExtractString(tool_input, 'filename')]))) AS path_tokens,
    toUInt8(positionCaseInsensitiveUTF8(payload_json, 'codex-mcp') > 0) AS has_codex_mcp
FROM moraine.events
WHERE notEmpty(session_id)
