-- ============================================================
-- carta silver schema, migration 0003 — the dated cash-flow ledger.
--
-- Carta exposes holdings (cap-table lots, fund NAV) but NOT the cash
-- mechanics: an option exercise is paid from an external bank, a fund
-- capital call is wired straight into the SPV/fund, and exit / distribution
-- proceeds leave to an external account. The Carta "account" is therefore a
-- SENTINEL for the opaque managed accounts (Carta custody + the fund
-- managers' books) — we never see a real cash balance.
--
-- This table records the dated cash EVENTS as positive magnitudes; `kind`
-- carries the direction + nature. The gold adapter (see DESIGN.md
-- §6 / wealthdb/docs/adapters/carta.md) projects each row as a balanced
-- DOUBLE-ENTRY pair on a sentinel funding account, mirroring equityzen:
--   exercise      -> deposit (+) + buy          (-)   shares acquired
--   capital_call  -> deposit (+) + contribution (-)   capital into a fund
--   exit          -> sell    (+) + withdrawal   (-)   shares realized
--   distribution  -> distribution (+) + withdrawal (-) fund cash returned
-- so the sentinel's derived balance is always exactly 0 (a pass-through
-- clearing account). A $0 exit (no recorded proceeds) emits the $0 sell leg
-- and omits the meaningless $0 withdrawal. The buy / sell legs carry the
-- share lot + price; the pure-cash legs do not.
-- ============================================================

CREATE TABLE cash_flows (
    cash_flow_external_id TEXT    NOT NULL PRIMARY KEY,  -- synthesized: '<kind>:<entity>[:<date>|:<lot>]'
    entity_external_id    TEXT    NOT NULL,              -- the Carta entity the flow concerns (position link)
    snapshot_at           INTEGER NOT NULL,              -- download run that computed it
    kind                  TEXT    NOT NULL,              -- 'exercise' | 'exit' | 'capital_call' | 'distribution'
    flow_date             TEXT,                          -- event date (source ISO / YYYY-MM-DD)
    amount                REAL,                           -- positive magnitude (USD); kind carries direction
    shares                REAL,                           -- shares acquired/realized (exercise/exit); NULL for fund flows
    price_per_share       REAL,                           -- per-share price (exercise = strike; exit = proceeds/share); NULL for fund flows
    currency              TEXT    NOT NULL DEFAULT 'USD',
    description           TEXT,                           -- generic event label (no PII)
    payload               TEXT    NOT NULL
);
CREATE INDEX idx_cash_flows_entity ON cash_flows(entity_external_id);
CREATE INDEX idx_cash_flows_date   ON cash_flows(flow_date);

-- Migration-complete marker — must be the LAST statement.
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s', 'now') AS INTEGER));
