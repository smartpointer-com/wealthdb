-- ============================================================
-- gold schema, migration 0215 — each private fund's figures, with its
-- money-weighted return.
--
--   * report_private_markets: derived, like report_bridge. `web
--     refresh` writes the rows of `wealthdb returns private`, every
--     holding and the total, in each reporting currency. The IRR is the
--     returns engine's solver, which no macro can run, so the table
--     holds the rows the CLI prints. web_private serves it.
--
-- IF NOT EXISTS keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

-- report_private_markets: money is in cents, days are epoch seconds,
-- the multiples and the IRR are ratios, NULL where the CLI prints a
-- blank. quality is `;`-separated.
CREATE TABLE IF NOT EXISTS report_private_markets (
    computed_at            BIGINT        NOT NULL,
    currency               TEXT          NOT NULL,
    is_total               BOOLEAN       NOT NULL,
    silver_source_id       TEXT          NOT NULL,
    account_external_id    TEXT          NOT NULL,
    instrument_external_id TEXT          NOT NULL,
    holding                TEXT          NOT NULL,
    first_call             BIGINT        NOT NULL,
    paid_in                DECIMAL(28,4),
    distributed            DECIMAL(28,4),
    nav                    DECIMAL(28,4),
    marked_at              BIGINT,
    end_at                 BIGINT        NOT NULL,
    dpi                    DOUBLE,
    tvpi                   DOUBLE,
    irr                    DOUBLE,
    quality                TEXT          NOT NULL
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (215, CAST(epoch(now()) AS BIGINT));
