//! `ConversationRepository` over the mnemonic cache. The seven methods the
//! MCP tools call delegate to one module each; everything else (monitor and
//! web surfaces) returns `unsupported`. Each module function takes a DuckDB
//! connection to the current cache file and returns the domain type, so the
//! moraine MCP layer renders its output unchanged.

mod attention;
pub mod cache;
mod list;
mod open;
mod search;
mod status;

use async_trait::async_trait;
use moraine_conversations::{
    ConversationRepository, RepoError, RepoResult, AnalyticsRange, AnalyticsSnapshot, Conversation, ConversationDetailOptions, ConversationListFilter, ConversationSearchQuery, ConversationSearchResults, ConversationSummary, FileAttentionQuery, FileAttentionTouch, IngestHeartbeatRead, McpEventOpen, McpSessionListFilter, McpSessionListItem, McpSessionOpen, McpTurnOpen, OpenContext, OpenEventRequest, Page, PageRequest, RepoConfig, SearchEventsQuery, SearchEventsResult, SearchMcpEventsQuery, SearchMcpEventsResult, SessionAnalytics, SessionAnalyticsQuery, SessionEventsQuery, SessionMetadata, SessionMetadataSearchQuery, SessionMetadataSearchResults, StoreDiagnostics, StoreHealth, TablePreview, TablePreviewQuery, TableSummaries, TraceEvent, Turn, TurnListFilter, TurnSummary, WebSearchEvent,
};

use crate::cache::Cache;

pub struct ParquetRepository {
    config: RepoConfig,
    cache: Cache,
}

impl ParquetRepository {
    pub fn new(config: RepoConfig, cache: Cache) -> Self {
        Self { config, cache }
    }

    async fn read<T, F>(&self, query: F) -> RepoResult<T>
    where
        T: Send + 'static,
        F: FnOnce(&duckdb::Connection) -> RepoResult<T> + Send + 'static,
    {
        let conn = self.cache.connection()?;
        tokio::task::spawn_blocking(move || query(&conn))
            .await
            .map_err(|error| RepoError::Internal(format!("mnemonic read task failed: {error}")))?
    }
}

fn unsupported(method: &str) -> RepoError {
    RepoError::Backend(format!("{method} is not supported by the mnemonic store"))
}

#[async_trait]
impl ConversationRepository for ParquetRepository {
    fn config(&self) -> &RepoConfig {
        &self.config
    }
    async fn prewarm_mcp_search_state(&self) -> RepoResult<()> {
        Ok(())
    }
    async fn list_session_analytics( &self, _query: SessionAnalyticsQuery, ) -> RepoResult<Vec<SessionAnalytics>> {
        Err(unsupported("list_session_analytics"))
    }
    async fn analytics_series(&self, _range: AnalyticsRange) -> RepoResult<AnalyticsSnapshot> {
        Err(unsupported("analytics_series"))
    }
    async fn list_web_searches(&self, _limit: u16) -> RepoResult<Vec<WebSearchEvent>> {
        Err(unsupported("list_web_searches"))
    }
    async fn latest_ingest_heartbeat(&self) -> RepoResult<IngestHeartbeatRead> {
        self.read(move |conn| status::latest_ingest_heartbeat(conn)).await
    }
    async fn list_table_summaries(&self) -> RepoResult<TableSummaries> {
        Err(unsupported("list_table_summaries"))
    }
    async fn preview_table(&self, _query: TablePreviewQuery) -> RepoResult<TablePreview> {
        Err(unsupported("preview_table"))
    }
    async fn read_store_health(&self) -> RepoResult<StoreHealth> {
        Err(unsupported("read_store_health"))
    }
    async fn read_store_diagnostics(&self) -> RepoResult<StoreDiagnostics> {
        Err(unsupported("read_store_diagnostics"))
    }
    async fn list_conversations( &self, _filter: ConversationListFilter, _page: PageRequest, ) -> RepoResult<Page<ConversationSummary>> {
        Err(unsupported("list_conversations"))
    }
    async fn get_conversation( &self, _session_id: &str, _opts: ConversationDetailOptions, ) -> RepoResult<Option<Conversation>> {
        Err(unsupported("get_conversation"))
    }
    async fn get_session_metadata(&self, _session_id: &str) -> RepoResult<Option<SessionMetadata>> {
        Err(unsupported("get_session_metadata"))
    }
    async fn get_mcp_session(&self, session_id: &str) -> RepoResult<Option<McpSessionOpen>> {
        let session_id = session_id.to_string();
        self.read(move |conn| open::get_mcp_session(conn, &session_id)).await
    }
    async fn list_mcp_sessions( &self, filter: McpSessionListFilter, page: PageRequest, ) -> RepoResult<Page<McpSessionListItem>> {
        self.read(move |conn| list::list_mcp_sessions(conn, &filter, &page)).await
    }
    async fn list_turns( &self, _session_id: &str, _filter: TurnListFilter, _page: PageRequest, ) -> RepoResult<Page<TurnSummary>> {
        Err(unsupported("list_turns"))
    }
    async fn get_turn(&self, _session_id: &str, _turn_seq: u32) -> RepoResult<Option<Turn>> {
        Err(unsupported("get_turn"))
    }
    async fn get_mcp_turn( &self, session_id: &str, turn_seq: u32, ) -> RepoResult<Option<McpTurnOpen>> {
        let session_id = session_id.to_string();
        self.read(move |conn| open::get_mcp_turn(conn, &session_id, turn_seq)).await
    }
    async fn open_event(&self, _req: OpenEventRequest) -> RepoResult<OpenContext> {
        Err(unsupported("open_event"))
    }
    async fn get_mcp_event(&self, event_uid: &str) -> RepoResult<Option<McpEventOpen>> {
        let event_uid = event_uid.to_string();
        self.read(move |conn| open::get_mcp_event(conn, &event_uid)).await
    }
    async fn list_session_events( &self, _query: SessionEventsQuery, _page: PageRequest, ) -> RepoResult<Page<TraceEvent>> {
        Err(unsupported("list_session_events"))
    }
    async fn search_events(&self, _query: SearchEventsQuery) -> RepoResult<SearchEventsResult> {
        Err(unsupported("search_events"))
    }
    async fn search_mcp_events( &self, query: SearchMcpEventsQuery, ) -> RepoResult<SearchMcpEventsResult> {
        let config = self.config.clone();
        self.read(move |conn| search::search_mcp_events(conn, &config, &query)).await
    }
    async fn search_conversations( &self, _query: ConversationSearchQuery, ) -> RepoResult<ConversationSearchResults> {
        Err(unsupported("search_conversations"))
    }
    async fn search_session_metadata( &self, _query: SessionMetadataSearchQuery, ) -> RepoResult<SessionMetadataSearchResults> {
        Err(unsupported("search_session_metadata"))
    }
    async fn file_attention( &self, query: FileAttentionQuery, ) -> RepoResult<Vec<FileAttentionTouch>> {
        self.read(move |conn| attention::file_attention(conn, &query)).await
    }
}
