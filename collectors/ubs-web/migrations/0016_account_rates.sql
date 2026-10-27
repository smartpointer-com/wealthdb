-- ============================================================
-- ubs-web silver schema, migration 0016 — the cash accounts' interest
-- conditions.
--
-- The `rates` pass (rates.py, DESIGN.md §3.11) saves each cash
-- account's conditions as the account API states them, or as the
-- legacy account page does for an account the API does not answer for.
-- One row per (snapshot, account, side, tier):
--
--   side         'credit' (the balance earns the rate) or 'debit' (it
--                pays it).
--   tier         1, 2, … per side, in the order the source lists them.
--                A tiered rate has one row per tier.
--   rate_pct     percent a year, as stated.
--   upper_limit  the balance the tier runs to; NULL for none, which is
--                the last tier and a flat rate's only one.
--   valid_from   'YYYY-MM-DD', the day the condition holds from.
--   surface      'api' or 'legacy', the surface it was read from.
--
-- `snapshot_at` is the dump's download instant, as on every snapshot
-- table. An account captured with no condition has no row: it states
-- no rate, which is not a rate of zero.
-- ============================================================

CREATE TABLE account_rates (
    snapshot_at          INTEGER NOT NULL,   -- Unix seconds UTC
    account_external_id  TEXT    NOT NULL,   -- IBAN no-spaces upper
    side                 TEXT    NOT NULL
        CHECK (side IN ('credit', 'debit')),
    tier                 INTEGER NOT NULL CHECK (tier >= 1),
    rate_pct             REAL    NOT NULL,
    upper_limit          REAL,
    valid_from           TEXT,
    surface              TEXT    NOT NULL
        CHECK (surface IN ('api', 'legacy')),
    payload              TEXT    NOT NULL,   -- the condition as read
    PRIMARY KEY (snapshot_at, account_external_id, side, tier)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (16, CAST(strftime('%s','now') AS INTEGER));
