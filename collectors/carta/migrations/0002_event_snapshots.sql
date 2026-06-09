-- ============================================================
-- carta silver schema, migration 0002 — event-driven change deltas.
--
-- The first schema modelled one snapshot per bronze dump (snapshot_at =
-- the download time). That makes historical as-of queries wrong: a dump
-- taken today reports holdings "as of today" even though the data is
-- effective earlier (a fund NAV is a quarter-end figure; an exited stake
-- was held for years, then cancelled on its acquisition date). A single
-- download therefore can't answer "what did I hold on 2026-01-01?".
--
-- New model (collector reconstructs the timeline from one dump): the silver
-- stores per-lot CHANGES (deltas), NOT a full-portfolio snapshot per day. A
-- tracked unit — a cap-table security lot (a share certificate / option
-- grant), or a fund interest — gets a row only on a day its state changes;
-- the gold adapter aggregates a company's lots into one position. Events:
-- acquisition, option exercise,
-- exercise-price change, share disposition, acquisition / cancellation, or a
-- new K-1 / capital-account statement (NAV). Same-day changes for a position
-- coalesce to its end-of-day state. `snapshot_at` is the EVENT date (UTC
-- midnight); `dump_runs.snapshot_at` stays the download time (idempotency +
-- provenance only).
--
-- Reading holdings as of day D: for each position take its latest row with
-- snapshot_at <= D, then DISCARD the ones whose latest state is EXITED
-- (position_status='exited'). No full-portfolio snapshot is needed, and an
-- exited position drops out exactly at its disposition date — nothing
-- lingers, nothing has to be re-stored on unrelated positions' event days.
--
-- This migration adds:
--   * securities.market_value — the line's valuation at its snapshot,
--     computed by the collector per the holder's rule (held shares ->
--     quantity x last-exercise-price across any shares; unexercised options
--     -> 0; exited -> 0). Stored here so the silver is self-describing and
--     gold reads a value rather than re-deriving the rule.
--   * securities.position_status — the normalized delta lifecycle marker,
--     'held' | 'exited', that the read model above keys on (it normalizes
--     the source is_canceled / is_expired / is_terminated flags).
--   * capital_events — the reconstructed timeline: one row per (date,
--     entity, event kind). The basis for the snapshot dates above; also a
--     queryable audit of why each delta exists.
-- ============================================================

BEGIN;

-- Per-snapshot valuation of a cap-table line (NULL where unknown / not yet
-- valued). Funds value off fund_metrics.net_asset_value, so this is
-- cap-table-only.
ALTER TABLE securities ADD COLUMN market_value REAL;

-- Delta lifecycle marker: 'held' (a live position at this snapshot) or
-- 'exited' (cancelled / expired / terminated / disposed). The read keeps
-- each position's latest row <= as-of and discards 'exited'.
ALTER TABLE securities ADD COLUMN position_status TEXT;

-- The reconstructed capital-event timeline. event_date is the source date
-- (ISO); snapshot_at is its UTC-midnight coalesced day (matches the
-- securities/fund_metrics snapshot_at it drives).
CREATE TABLE capital_events (
    snapshot_at         INTEGER NOT NULL,                  -- UTC-midnight day of the event
    entity_external_id  INTEGER NOT NULL,
    event_kind          TEXT    NOT NULL,                  -- 'acquired'|'exercise'|'disposition'|'price_change'|'statement'
    event_date          TEXT,                              -- source date string (ISO / MM/DD/YYYY)
    description         TEXT,
    payload             TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (snapshot_at, entity_external_id, event_kind)
);
CREATE INDEX ix_capital_events_entity ON capital_events(entity_external_id);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
