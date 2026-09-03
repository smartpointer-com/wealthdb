-- ============================================================
-- fidelity-web silver, migration 0007 — allow portfolios.kind='daf'.
--
-- The Donor-Advised Fund phase (download.py `daf`; DESIGN.md §12)
-- ingests the Fidelity Charitable Giving Account, which the retail
-- account selector groups under 'Fidelity Charitable® Giving' and the
-- retail phases exclude by id length. Its silver portfolio kind is
-- 'daf'; the gold adapter maps that to account_kind
-- 'donor_advised_fund' + tax_wrapper 'charitable' (DESIGN.md §12.3).
--
-- The kind column carries a CHECK constraint, which SQLite cannot
-- alter in place, so the table is rebuilt with 'daf' added to the
-- allowed set. portfolios has no dependent FK or index, so a
-- create-copy-drop-rename is a clean swap; existing rows are preserved
-- verbatim.
-- ============================================================

CREATE TABLE portfolios_new (
    snapshot_at           INTEGER NOT NULL,
    portfolio_external_id TEXT    NOT NULL,
    kind                  TEXT    NOT NULL
        CHECK (kind IN ('529', 'trust_managed', 'daf', 'other')),
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, portfolio_external_id)
);

INSERT INTO portfolios_new (snapshot_at, portfolio_external_id, kind, payload)
    SELECT snapshot_at, portfolio_external_id, kind, payload FROM portfolios;

DROP TABLE portfolios;
ALTER TABLE portfolios_new RENAME TO portfolios;

-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (7, CAST(strftime('%s', 'now') AS INTEGER));
