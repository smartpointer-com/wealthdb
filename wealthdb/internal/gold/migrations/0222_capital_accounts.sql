-- ============================================================
-- gold schema, migration 0222 — private holdings' capital accounts as
-- their sources state them, and the private-markets view's figures
-- that read them.
--
--   * capital_accounts: a private holding's capital account on one day,
--     as a source states it: the commitment, what was called and paid
--     in against it, what remains unfunded, what came back, the value
--     still held, and the source's own multiples. A row is informative:
--     no report counts it as a position or a flow. `returns private`
--     sets it beside what it computes. as_of is the instant the figures
--     stand at. account_external_id is '' where the source names the
--     holding by its instrument alone. A source states one account's
--     capital account per instrument, day and document: source_ref
--     names the document where one source can state one instrument of
--     one account on one day twice, as two portfolios' reports on one
--     fund, and is '' where it states one at most. NULL means the
--     source states nothing for that figure. A reference table
--     (gold.ReferenceTables): keyed by silver_source_id and windowed
--     by as_of, so a load's windowed delete and a reset reach it by
--     those two columns.
--   * report_private_markets gains each holding's commitment and
--     unfunded, in the row's currency, and the multiples and the day of
--     the capital account it reads. The next refresh fills them; the
--     rows before it hold NULL.
--
-- IF NOT EXISTS lets the file run on a database that already has the
-- table.
-- ============================================================

CREATE TABLE IF NOT EXISTS capital_accounts (
    silver_source_id        TEXT           NOT NULL,
    account_external_id     TEXT           NOT NULL DEFAULT '',
    instrument_external_id  TEXT           NOT NULL,
    as_of                   BIGINT         NOT NULL,
    source_ref              TEXT           NOT NULL,
    currency                TEXT           NOT NULL,
    commitment              DECIMAL(28, 4),
    called                  DECIMAL(28, 4),
    paid_in                 DECIMAL(28, 4),
    unfunded                DECIMAL(28, 4),
    distributed             DECIMAL(28, 4),
    nav                     DECIMAL(28, 4),
    tvpi                    DOUBLE,
    dpi                     DOUBLE,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, account_external_id, instrument_external_id, as_of, source_ref)
);

-- report_private_markets: commitment and unfunded round to the cent, as
-- the CLI prints them; the stated multiples are ratios;
-- stated_as_of is epoch seconds. NULL where the CLI prints a blank.
ALTER TABLE report_private_markets ADD COLUMN IF NOT EXISTS commitment DECIMAL(28,4);
ALTER TABLE report_private_markets ADD COLUMN IF NOT EXISTS unfunded DECIMAL(28,4);
ALTER TABLE report_private_markets ADD COLUMN IF NOT EXISTS stated_tvpi DOUBLE;
ALTER TABLE report_private_markets ADD COLUMN IF NOT EXISTS stated_dpi DOUBLE;
ALTER TABLE report_private_markets ADD COLUMN IF NOT EXISTS stated_as_of BIGINT;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (222, CAST(epoch(now()) AS BIGINT));
