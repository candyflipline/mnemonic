CREATE VIEW search_term_stats (`term` String, `docs` UInt64) AS SELECT term, toUInt64(count()) AS docs FROM search_postings FINAL GROUP BY term
