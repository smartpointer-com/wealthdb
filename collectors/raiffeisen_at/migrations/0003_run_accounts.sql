-- ============================================================
-- raiffeisen_at silver schema, migration 0003 — the accounts each run listed.
--
-- The loader content-dedups the `accounts` roster, so an unchanged account
-- leaves no trace of the runs that still listed it. `run_accounts` records
-- them. `run_windows` (0002) records what a run fetched, not what its roster
-- listed: a failed fetch leaves a listed account out of it. Gold states each
-- account's roster balance at every run that listed it, and ends an account
-- the bank stopped listing (wealthdb/docs/DESIGN.md §10.7).
--
--   * run_accounts holds one row per loaded run and account its
--     accounts.json listed. Written when a run is loaded; runs loaded before
--     this migration are filled in from their bronze dirs on the next load.
-- ============================================================

CREATE TABLE run_accounts (
    snapshot_at         INTEGER NOT NULL,                 -- the run (dump_runs)
    account_external_id TEXT    NOT NULL,                 -- the IBAN
    PRIMARY KEY (snapshot_at, account_external_id)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s','now') AS INTEGER));
