-- ============================================================
-- gold schema, migration 0220 — each private holding's figures at every
-- quarter end, with its asset class, vehicle and status.
--
--   * report_private_markets: derived, like report_bridge. `web
--     refresh` writes the rows of `wealthdb returns private -d` at every
--     quarter end since the first call and at the end of the day of the
--     refresh, every holding and the total, in each reporting currency.
--     as_of is the instant a row reads, and it is part of the key. The
--     rows are rewritten whole on every refresh, so the table is
--     re-created empty, and the next refresh fills it.
--   * paid_in is NULL where the holding states no cost (no_cost).
-- ============================================================

DROP TABLE IF EXISTS report_private_markets;

-- report_private_markets: money rounds to the cent, as the CLI prints
-- it; days and as_of are epoch seconds; the multiples and the IRR are
-- ratios, NULL where the CLI prints a blank. quality is `;`-separated.
-- asset_class, vehicle and status are NULL on the total.
CREATE TABLE report_private_markets (
    computed_at            BIGINT        NOT NULL,
    as_of                  BIGINT        NOT NULL,
    currency               TEXT          NOT NULL,
    is_total               BOOLEAN       NOT NULL,
    silver_source_id       TEXT          NOT NULL,
    account_external_id    TEXT          NOT NULL,
    instrument_external_id TEXT          NOT NULL,
    holding                TEXT          NOT NULL,
    asset_class            TEXT,
    vehicle                TEXT,
    status                 TEXT,
    first_call             BIGINT        NOT NULL,
    paid_in                DECIMAL(28,4),
    distributed            DECIMAL(28,4),
    nav                    DECIMAL(28,4),
    marked_at              BIGINT,
    end_at                 BIGINT        NOT NULL,
    dpi                    DOUBLE,
    tvpi                   DOUBLE,
    irr                    DOUBLE,
    quality                TEXT          NOT NULL,
    PRIMARY KEY (as_of, currency, is_total, silver_source_id, account_external_id, instrument_external_id)
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (220, CAST(epoch(now()) AS BIGINT));
