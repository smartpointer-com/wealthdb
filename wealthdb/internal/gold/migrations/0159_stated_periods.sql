-- ============================================================
-- gold schema, migration 0159 — stated periods.
--
-- stated_periods: an account's summary of one period as a document
-- states it, beside the facts gold derives from positions and the
-- ledger: the values at the period's two ends, the net additions, the
-- gain, the income, the fees and the accruals. The month-end workbook
-- the svb collector reads is one such document. A row is informative:
-- no report counts it as a position or a flow. `returns stated` sets
-- it against what the returns engine computes for the same account and
-- period.
--
-- as_of is the UTC midnight of the period's last day, period_start its
-- first. net_additions is signed (money in positive, out negative);
-- fees is a positive amount, as stated. NULL means the document states
-- nothing for that figure. source_sha256 names the stating document,
-- so two documents' statements of one period stay apart.
--
-- A reference table (gold.ReferenceTables): keyed by silver_source_id
-- and windowed by as_of, so a load's windowed delete and a reset reach
-- it by those two columns.
--
-- IF NOT EXISTS keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS stated_periods (
    silver_source_id     TEXT           NOT NULL,
    account_external_id  TEXT           NOT NULL,
    as_of                BIGINT         NOT NULL,
    period_start         BIGINT         NOT NULL,
    currency             TEXT           NOT NULL,
    beginning_value      DECIMAL(28, 4),
    ending_value         DECIMAL(28, 4),
    accrual              DECIMAL(28, 4),
    net_additions        DECIMAL(28, 4),
    net_gain             DECIMAL(28, 4),
    income               DECIMAL(28, 4),
    fees                 DECIMAL(28, 4),
    source_sha256        TEXT           NOT NULL,
    payload              JSON,
    PRIMARY KEY (silver_source_id, account_external_id, as_of, source_sha256)
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (159, CAST(epoch(now()) AS BIGINT));
