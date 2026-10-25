-- ============================================================
-- firstcitizens silver schema, migration 0002 — the accounts each run listed.
--
-- The loader content-dedups the `accounts` roster, so an unchanged account
-- leaves no trace of the runs that still listed it. `run_accounts` records
-- them. Gold states each account's roster balance at every run that listed
-- it, and ends an account the bank stopped listing
-- (wealthdb/docs/DESIGN.md §10.7).
--
--   * run_accounts holds one row per loaded run and account its
--     accounts.json listed. Written when a run is loaded; runs loaded before
--     this migration are filled in from their bronze dirs on the next load.
-- ============================================================

CREATE TABLE run_accounts (
    snapshot_at         INTEGER NOT NULL,                 -- the run (dump_runs)
    account_external_id TEXT    NOT NULL,                 -- the Q2 account id
    PRIMARY KEY (snapshot_at, account_external_id)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s','now') AS INTEGER));
