-- ============================================================
-- gold schema, migration 0155 — the inputs a materialize covered.
--
-- web-materialize records, per scope, the inputs it built from. A
-- later run whose row matches does no work: the tables from the
-- recorded run are still the ones those inputs produce.
--
--   * scope: which materialize the row covers.
--   * load_key: the loads, the freshness marks, enrichment and the
--     account scopes, the categorize verdicts, the symbol
--     resolutions, the lot-run log and the config-stamped tables, as
--     text.
--   * config_hash: a hash of the config file and of each file beside
--     it the materialize reads.
--   * engine_version: the engine that wrote the tables.
--   * as_of: the end of the UTC day the tables were built through.
--
-- The skip check reads the table.
-- ============================================================

CREATE TABLE IF NOT EXISTS materialize_inputs (
    scope           VARCHAR PRIMARY KEY,
    load_key        VARCHAR NOT NULL,
    config_hash     VARCHAR NOT NULL,
    engine_version  VARCHAR NOT NULL,
    as_of           BIGINT  NOT NULL
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (155, CAST(epoch(now()) AS BIGINT));
