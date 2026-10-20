//! `search_sessions`: BM25 (k1, b from `RepoConfig`) over the cache's
//! `postings`, `docs`, `term_stats` and `corpus` tables. Owned by lane `search`.

use moraine_conversations::{RepoConfig, RepoResult, SearchMcpEventsQuery, SearchMcpEventsResult};

pub(crate) fn search_mcp_events(
    conn: &duckdb::Connection,
    config: &RepoConfig,
    query: &SearchMcpEventsQuery,
) -> RepoResult<SearchMcpEventsResult> {
    let _ = (conn, config, query);
    todo!("lane search")
}
