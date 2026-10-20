//! `open`: one session, one turn, one event. Owned by lane `list-open`.

use moraine_conversations::{McpEventOpen, McpSessionOpen, McpTurnOpen, RepoResult};

pub(crate) fn get_mcp_session(
    conn: &duckdb::Connection,
    session_id: &str,
) -> RepoResult<Option<McpSessionOpen>> {
    let _ = (conn, session_id);
    todo!("lane list-open")
}

pub(crate) fn get_mcp_turn(
    conn: &duckdb::Connection,
    session_id: &str,
    turn_seq: u32,
) -> RepoResult<Option<McpTurnOpen>> {
    let _ = (conn, session_id, turn_seq);
    todo!("lane list-open")
}

pub(crate) fn get_mcp_event(
    conn: &duckdb::Connection,
    event_uid: &str,
) -> RepoResult<Option<McpEventOpen>> {
    let _ = (conn, event_uid);
    todo!("lane list-open")
}
