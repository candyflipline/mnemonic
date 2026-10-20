CREATE MATERIALIZED VIEW moraine_personal.mv_search_postings TO moraine_personal.search_postings
(
    `post_version` UInt64,
    `term` String,
    `doc_id` String,
    `session_id` String,
    `source_name` LowCardinality(String),
    `harness` LowCardinality(String),
    `inference_provider` LowCardinality(String),
    `event_class` LowCardinality(String),
    `payload_type` LowCardinality(String),
    `actor_role` LowCardinality(String),
    `name` LowCardinality(String),
    `phase` String,
    `source_ref` String,
    `doc_len` UInt32,
    `tf` UInt16
)
AS SELECT
    d.event_version AS post_version,
    d.term,
    d.event_uid AS doc_id,
    d.session_id,
    d.source_name,
    d.harness,
    d.inference_provider,
    d.event_class,
    d.payload_type,
    d.actor_role,
    d.name,
    d.phase,
    d.source_ref,
    d.doc_len,
    toUInt16(count()) AS tf
FROM
(
    SELECT
        event_version,
        event_uid,
        session_id,
        source_name,
        harness,
        inference_provider,
        event_kind AS event_class,
        payload_type,
        actor_kind AS actor_role,
        tool_name AS name,
        if(tool_phase != '', tool_phase, op_status) AS phase,
        source_ref,
        toUInt32(length(extractAll(lowerUTF8(text_content), '[a-z0-9_]+'))) AS doc_len,
        arrayJoin(extractAll(lowerUTF8(text_content), '[a-z0-9_]+')) AS term
    FROM moraine_personal.events
) AS d
WHERE (d.doc_len > 0) AND ((lengthUTF8(d.term) >= 2) AND (lengthUTF8(d.term) <= 64))
GROUP BY
    d.event_version,
    d.term,
    d.event_uid,
    d.session_id,
    d.source_name,
    d.harness,
    d.inference_provider,
    d.event_class,
    d.payload_type,
    d.actor_role,
    d.name,
    d.phase,
    d.source_ref,
    d.doc_len
