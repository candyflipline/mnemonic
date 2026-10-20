CREATE MATERIALIZED VIEW moraine.mv_file_attention_project_roots_from_events TO moraine.file_attention_project_roots
(
    `project_id` String,
    `worktree_root` String,
    `observed_version` UInt64
)
AS SELECT
    JSONExtractString(payload_json, 'moraine_tool_io', 'project_id') AS project_id,
    JSONExtractString(payload_json, 'moraine_tool_io', 'worktree_root') AS worktree_root,
    event_version AS observed_version
FROM moraine.events
WHERE startsWith(JSONExtractString(payload_json, 'moraine_tool_io', 'project_id'), 'git:') AND (JSONExtractString(payload_json, 'moraine_tool_io', 'worktree_root') != '')
