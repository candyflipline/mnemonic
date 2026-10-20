//! `list_sessions`: page-first session listing (page of ids, then one
//! summary pass over those ids, as upstream #677). Owned by lane `list-open`.

use moraine_conversations::{McpSessionListFilter, McpSessionListItem, Page, PageRequest, RepoResult};

pub(crate) fn list_mcp_sessions(
    conn: &duckdb::Connection,
    filter: &McpSessionListFilter,
    page: &PageRequest,
) -> RepoResult<Page<McpSessionListItem>> {
    let _ = (conn, filter, page);
    todo!("lane list-open")
}
