-- ============================================================
-- raiffeisen_at silver schema, migration 0002 — supplied transaction listings.
--
-- Transaction listings the bank supplies on request reach years past the
-- live history's rolling ~36-month window. load.py stitches them in beside
-- the live rows (../DESIGN.md §I); this migration lets silver tell the two
-- apart and remember what each download covered.
--
--   * transactions.source gains a second value, 'supplied_listing', next to
--     0001's 'history' (the column is unconstrained, so that needs no DDL).
--     A supplied row's txn_id is "doc_" + a hash of the posting's structural
--     columns — never the parsed text, never the statement print date — so
--     every extract of one account gives a posting the same id. Its payload
--     is the listing's own vocabulary (butag, herk, txt, valuta, lines, …,
--     doc_sha256, and live_seen on a day the live history saw part of), not
--     a kontoumsaetze row.
--   * daily_balances gains the same `source` column. The live loader keeps
--     writing it with INSERT OR REPLACE and without naming the column, so a
--     live balance lands as 'history' and takes over a day a listing held.
--     The stitch finds and replaces its own rows by 'supplied_listing'.
--   * documents gains a doc_kind, 'transaction_listing': one row per listing
--     in supplied/ that binds to an account, whose payload records its
--     coverage, whether it stitched (status + reason), and the day ranges it
--     speaks for.
--   * run_windows records, per loaded run and account, the days the download
--     covered: 'transactions' from the first day it certifies (its
--     `--lookback` window, floored by the server's history floor, or the day
--     after the oldest day a cut-short walk returned) and 'balances' over the
--     kontostaende series; last_day is the run's own, partial day. Written
--     when a run is loaded; runs loaded before this migration are filled in
--     from their bronze dirs on the next load.
--
-- The supplied layer is re-derived on every load and replaces the stored one
-- when it differs, so none of its rows is append-only.
-- ============================================================

ALTER TABLE daily_balances ADD COLUMN source TEXT NOT NULL DEFAULT 'history';

CREATE TABLE run_windows (
    snapshot_at         INTEGER NOT NULL,                 -- the run (dump_runs)
    account_external_id TEXT    NOT NULL,                 -- the IBAN
    kind                TEXT    NOT NULL,                 -- 'transactions' | 'balances'
    first_day           INTEGER NOT NULL,                 -- Unix seconds UTC at midnight
    last_day            INTEGER NOT NULL,                 -- the run's own (partial) day
    PRIMARY KEY (snapshot_at, account_external_id, kind)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s','now') AS INTEGER));
