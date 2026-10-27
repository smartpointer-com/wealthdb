-- ============================================================
-- gold schema, migration 0164 — declarations a source parses from a
-- document, beside the declarations ledger (docs/EXPOSURE.md §9).
--
--   * exposure_reports, a reference table: one row per holding and
--     date for which a source parses a split from a document, such as a
--     private-markets fund's quarterly report. dimensions holds each
--     dimension's split as JSON, value to share in percent, each
--     dimension summing to 100. A reference table (gold.ReferenceTables):
--     keyed by silver_source_id and windowed by as_of, so a load's
--     windowed delete and a reset reach it by those two columns.
--   * exposure_declarations gains origin: 'ledger' for a record of the
--     declarations ledger, 'parsed' for one gold.SyncExposure builds
--     from exposure_reports. A parsed record has no ledger line, so its
--     line_no is negative.
--
-- Macros: exposure_declarations_at reads the latest dated record on or
-- before the instant, and on an equal date the ledger's. A parsed
-- record therefore never outranks a ledger record of the same or a
-- later date. The other macros of 0129 stand.
--
-- OR REPLACE and IF NOT EXISTS keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS exposure_reports (
    silver_source_id        TEXT    NOT NULL,
    instrument_external_id  TEXT    NOT NULL,
    as_of                   BIGINT  NOT NULL,
    dimensions              JSON    NOT NULL,
    source                  JSON,
    note                    TEXT,
    PRIMARY KEY (silver_source_id, instrument_external_id, as_of)
);

ALTER TABLE exposure_declarations ADD COLUMN IF NOT EXISTS origin TEXT DEFAULT 'ledger';

-- exposure_declarations_at: per holding, the record that describes its
-- exposure at the instant: the latest dated on or before it that states
-- an economic dimension, the ledger's on an equal date, else one without
-- a date. A record that states only a situs is no record here. A
-- declaration has no age limit: a target does not go stale the way a
-- fund's roster does.
CREATE OR REPLACE MACRO exposure_declarations_at(p_asof) AS TABLE (
    SELECT d.*
      FROM exposure_declarations d
     WHERE d.dimensions <> '' AND (d.as_of IS NULL OR d.as_of <= p_asof)
   QUALIFY ROW_NUMBER() OVER (PARTITION BY d.silver_source_id, d.instrument_external_id
                              ORDER BY d.as_of DESC NULLS LAST, d.origin = 'ledger' DESC) = 1
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (164, CAST(epoch(now()) AS BIGINT));
