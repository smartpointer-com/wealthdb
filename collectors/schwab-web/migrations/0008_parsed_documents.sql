-- ============================================================
-- schwab-web silver schema, migration 0008 — parse markers and
-- statement ownership.
--
-- `parsed_documents` records each logical document that the statement,
-- 1099-B and realized-lot passes have parsed under the current parser
-- generation, one row per (logical_doc_key, document_kind). A pass skips
-- a document it has marked, unless `--reparse`. The marker belongs to the
-- document, not to its rows, so it also closes the gate for:
--   * a document whose parse yields no rows: a statement without
--     transactions, a 1099 without 1099-B lots, a report without
--     realized lots;
--   * a statement whose manifest date differs from the period end it
--     prints, which keys its snapshot rows.
--
-- document_kind is the pass's name for the document: 'statement',
-- 'form_1099b', 'year_end_summary' or 'gain_loss_report'. A moved parser
-- generation (migration 0005) clears the table with the statement
-- tables; the re-parse it forces marks every document again.
--
-- `historical_position_snapshots` and `historical_cash_balances` gain
-- `logical_doc_key`, the statement that wrote the row. Both stay keyed by
-- account and period end. When a second statement prints the same
-- account and period end, the loader leaves the rows of the first in
-- place: a copy with the same content is skipped, and one with other
-- content is skipped with a warning that names both statements.
--
-- Backfill: the owner of an existing row is the document its
-- source_sha256 names, as migration 0004 did for transactions. The
-- markers start empty; a document without one is parsed once more and
-- marked.
-- ============================================================

CREATE TABLE parsed_documents (
    logical_doc_key  TEXT    NOT NULL,   -- account|doc_date|filename, as on the pass's rows
    document_kind    TEXT    NOT NULL,   -- 'statement' | 'form_1099b' | 'year_end_summary' | 'gain_loss_report'
    source_sha256    TEXT    NOT NULL,   -- the bronze copy that was parsed
    PRIMARY KEY (logical_doc_key, document_kind)
);

ALTER TABLE historical_position_snapshots ADD COLUMN logical_doc_key TEXT;
ALTER TABLE historical_cash_balances ADD COLUMN logical_doc_key TEXT;

UPDATE historical_position_snapshots
SET logical_doc_key = (
    SELECT d.account_external_id || '|' || d.doc_date || '|' || d.filename
    FROM documents d
    WHERE d.sha256 = historical_position_snapshots.source_sha256
);

UPDATE historical_cash_balances
SET logical_doc_key = (
    SELECT d.account_external_id || '|' || d.doc_date || '|' || d.filename
    FROM documents d
    WHERE d.sha256 = historical_cash_balances.source_sha256
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (8, CAST(strftime('%s','now') AS INTEGER));
