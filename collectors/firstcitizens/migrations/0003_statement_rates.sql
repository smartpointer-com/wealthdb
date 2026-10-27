-- ============================================================
-- firstcitizens silver schema, migration 0003 — the interest rate each
-- statement states.
--
--   * statement_rates holds one row per account and statement cycle that
--     states a rate (statement_parser.py): the interest rate on the
--     statement day, and the annual percentage yields that check it. A
--     cycle whose statement states no rate has no row: unknown, not zero.
--     The bank renders a statement anew on every download, so several
--     documents can hold one cycle. The first one parsed writes the row;
--     the source_sha256 names it.
--   * parsed_statements marks each statement document the current parser
--     generation has read, so a load reads only the documents it has not.
--   * parser_generations records that generation. Seeded empty, so the
--     first load after this migration reads every statement in bronze. A
--     moved parser clears both tables and reads them all again.
-- ============================================================

CREATE TABLE statement_rates (
    account_external_id TEXT    NOT NULL,   -- the Q2 account id, as documents
    period_end          INTEGER NOT NULL,   -- the cycle's last day, Unix seconds UTC midnight
    period_start        INTEGER NOT NULL,   -- its first day
    interest_rate       REAL    NOT NULL,   -- percent a year, on the statement day
    apy_earned          REAL,               -- the annual percentage yield the cycle earned
    apy                 REAL,               -- the annual percentage yield on the statement day, where stated
    source_sha256       TEXT    NOT NULL,   -- the document read (documents.sha256)
    payload             TEXT    NOT NULL,   -- {"lines": the text each figure is read from}
    PRIMARY KEY (account_external_id, period_end)
);

CREATE TABLE parsed_statements (
    sha256 TEXT NOT NULL PRIMARY KEY        -- documents.sha256
);

CREATE TABLE parser_generations (
    scope      TEXT    NOT NULL PRIMARY KEY,  -- the pass, 'statements'
    generation TEXT    NOT NULL,              -- collectorkit.srcfp fingerprint
    stamped_at INTEGER NOT NULL               -- Unix seconds UTC
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s','now') AS INTEGER));
