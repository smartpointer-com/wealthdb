-- ============================================================
-- carta silver schema, migration 0001 — initial schema.
--
-- Migration discipline: every change lands as a new numbered file in this
-- directory. The loader applies any file whose number exceeds the max
-- applied silver_schema_version in schema_meta. Silver DBs always conform to
-- the latest schema; no backward-compatible drift.
--
-- Storage conventions (shared across wealthdb collectors):
--   * Unix-seconds-UTC integers for ingest timestamps; source date fields
--     (issue_date, vesting_*, document_date, sharing_date) are kept as the
--     source's ISO-8601 / "YYYY-MM-DD" TEXT — the gold adapter parses them.
--   * Stable filter/join columns promoted; the full source object rides in a
--     `payload` TEXT (JSON) column so Carta-side drift is absorbed at silver,
--     never at gold.
--   * Snapshot tables are monotemporal on snapshot_at (PK starts with it, so
--     the implicit B-tree is the as-of index). One row per entity per dump.
--   * Money on the fund-LP side arrives as decimal STRINGS ("100000.00") —
--     stored verbatim as TEXT to avoid float rounding. Cap-table numeric
--     fields (exercise_price, quantities) arrive as JSON numbers — REAL.
--   * documents are content-deduped on content_sha256.
--
-- ------------------------------------------------------------
-- Source shape (see DESIGN.md §3 for the full endpoint map)
-- ------------------------------------------------------------
--   This source is a Carta "individual portfolio" (one individual_id under one
--   firm/organization) holding one or more ENTITIES — each either a
--   cap-table corporation (is_fund_investment=0: shares/options/RSUs/… with
--   strike + vesting) or a fund investment (is_fund_investment=1: an LP
--   capital account). Both families are reached read-only over Carta's
--   internal REST/JSON API and land in one bronze run dir per download.
--
-- ------------------------------------------------------------
-- Gold mapping (informational — implemented in wealthdb, NOT here)
-- ------------------------------------------------------------
--   The gold adapter maps the whole Carta individual portfolio to ONE account
--   (keyed on individual_id), with one position per held company — each
--   company's `securities` rows are that position's lots, aggregated (see
--   DESIGN.md §6 / wealthdb/docs/adapters/carta.md). `entities` carries both
--   individual_id and firm_id so the account can key on either. Vesting has
--   no canonical gold home — `vesting_schedules`/`vesting_events` stay
--   silver-only; every cap-table security_type folds into the private_equity
--   asset_class.
-- ============================================================

PRAGMA foreign_keys = ON;

-- All-or-nothing: executescript() implicitly COMMITs before running, so the
-- transaction must be controlled inside the file.
BEGIN;

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                 -- Unix seconds UTC
);

-- One row per ingested bronze dump. snapshot_at is parsed from the run dir
-- name (YYYYMMDDTHHMMSSZ). The idempotency anchor: the loader skips dumps
-- whose snapshot_at already exists here; --force deletes the silver DB and
-- rebuilds it from all bronze.
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL,                 -- run-ts dirname, relative to bronze root
    individual_id         TEXT,                             -- Carta individual portfolio id
    firm_id               TEXT,                             -- organization / firm id
    dry_run               INTEGER NOT NULL DEFAULT 0,       -- 0/1
    entities_total        INTEGER NOT NULL DEFAULT 0,
    documents_total       INTEGER NOT NULL DEFAULT 0,
    errors_total          INTEGER NOT NULL DEFAULT 0,       -- non-fatal endpoint failures recorded in run.json
    payload               TEXT    NOT NULL                  -- the full bronze run.json verbatim
);


-- ============================================================
-- ENTITIES — the companies + funds held (the "issuers")
-- ============================================================
-- One row per (snapshot, entity). entity_external_id is Carta's
-- corporation_id (the entity id used in the holdings/fund API paths).
-- is_fund_investment is the cap-table-vs-fund discriminator. Cap-table
-- summary fields (held_since/ownership/cash_cost) come from the entity's
-- holdings-dashboard; they are NULL for fund entities.
CREATE TABLE entities (
    snapshot_at         INTEGER NOT NULL,
    entity_external_id  INTEGER NOT NULL,                  -- corporation_id
    individual_id       TEXT    NOT NULL,                  -- owning Carta individual portfolio
    firm_id             TEXT,                              -- organization / firm id
    is_fund_investment  INTEGER NOT NULL,                  -- 0 = cap-table corp, 1 = fund LP
    entity_type         TEXT,                              -- e.g. 'investment'
    legal_name          TEXT,                              -- PII: company / fund legal name
    dba                 TEXT,
    held_since          TEXT,                              -- holdings-dashboard.held_since (ISO)
    ownership           REAL,                              -- holdings-dashboard.ownership
    cash_cost           REAL,                              -- holdings-dashboard.cash_cost
    payload             TEXT    NOT NULL,                  -- meta.json + holdings-dashboard.json
    PRIMARY KEY (snapshot_at, entity_external_id)
);
CREATE INDEX ix_entities_external_id ON entities(entity_external_id);
CREATE INDEX ix_entities_fund        ON entities(is_fund_investment);


-- ============================================================
-- SECURITIES — cap-table holdings (one row per security line)
-- ============================================================
-- From the {rows:[…]} of each per-corporation security-type endpoint.
-- security_type is the source list name (option / share / rsu / rsa /
-- warrant / convertible / sar / piu / equity-grant). Fields vary by type;
-- the common + decision-relevant ones are promoted (NULL where a type omits
-- them), the full row is in payload.
CREATE TABLE securities (
    snapshot_at         INTEGER NOT NULL,
    entity_external_id  INTEGER NOT NULL,
    security_type       TEXT    NOT NULL,                  -- 'option'|'share'|'rsu'|'rsa'|'warrant'|'convertible'|'sar'|'piu'|'equity_grant'
    security_external_id INTEGER NOT NULL,                 -- row.id
    label               TEXT,                              -- row.label (e.g. certificate/grant label)
    issuable_type       TEXT,                              -- row.issuable_type
    stock_type          TEXT,                              -- row.stock_type (e.g. ISO/NSO/common/preferred)
    status              TEXT,                              -- row.status
    issue_date          TEXT,                              -- row.issue_date (ISO)
    currency            TEXT,                              -- row.currency (ISO 4217)
    quantity            REAL,                              -- row.quantity
    exercise_price      REAL,                              -- row.exercise_price (the STRIKE; options/sar)
    cost                REAL,                              -- row.cost (acquisition cost; shares)
    exercised           REAL,                              -- row.exercised
    vested              REAL,                              -- row.vested
    exercisable         REAL,                              -- row.exercisable
    has_vesting         INTEGER,                           -- row.has_vesting (0/1)
    is_canceled         INTEGER,
    is_expired          INTEGER,
    is_terminated       INTEGER,
    is_fully_exercised  INTEGER,
    fund_name           TEXT,                              -- row.fund_name (the holding's plan/fund label)
    payload             TEXT    NOT NULL,                  -- the full row object
    PRIMARY KEY (snapshot_at, entity_external_id, security_type, security_external_id)
);
CREATE INDEX ix_securities_entity ON securities(entity_external_id);
CREATE INDEX ix_securities_type   ON securities(security_type);


-- ============================================================
-- VESTING — the first vesting concept in wealthdb (silver-only for now)
-- ============================================================
-- Per option/RSU grant that has_vesting, from the vesting-data endpoint.
-- vesting_schedules carries the grant-level summary; vesting_events the
-- dated event series (the actual schedule). grant_external_id == the
-- security_external_id of the corresponding option/grant row.
CREATE TABLE vesting_schedules (
    snapshot_at              INTEGER NOT NULL,
    entity_external_id       INTEGER NOT NULL,
    grant_external_id        INTEGER NOT NULL,             -- option_grant id
    label                    TEXT,
    so_type                  TEXT,                         -- ISO/NSO/...
    has_iso_nso_split        INTEGER,                      -- 0/1
    vesting_type             TEXT,                         -- vesting_manager.vesting_template.vesting_type
    vesting_start_date       TEXT,
    vesting_end_date         TEXT,
    vested_shares_quantity   REAL,
    net_total_shares_quantity REAL,
    payload                  TEXT    NOT NULL,             -- the full vesting-data object (minus event_data)
    PRIMARY KEY (snapshot_at, entity_external_id, grant_external_id)
);

-- One row per (snapshot, grant, event). seq preserves source order;
-- vest_date + cumulative make the schedule queryable.
CREATE TABLE vesting_events (
    snapshot_at         INTEGER NOT NULL,
    grant_external_id   INTEGER NOT NULL,
    seq                 INTEGER NOT NULL,                  -- index within vesting_event_data[]
    entity_external_id  INTEGER NOT NULL,
    vest_date           TEXT,                              -- event.date (ISO)
    amount              REAL,                              -- event.amount (units vesting this event)
    cumulative          REAL,                              -- event.cumulative
    has_vested          INTEGER,                           -- 0/1
    vesting_type        TEXT,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, grant_external_id, seq)
);
CREATE INDEX ix_vesting_events_grant ON vesting_events(grant_external_id);


-- ============================================================
-- FUND METRICS — the LP capital account (fund entities)
-- ============================================================
-- From the fund-admin partner metrics. One row per (snapshot, fund entity).
-- Money fields are decimal STRINGS in the source — kept as TEXT verbatim.
CREATE TABLE fund_metrics (
    snapshot_at               INTEGER NOT NULL,
    entity_external_id        INTEGER NOT NULL,            -- the fund entity id
    fund_external_id          TEXT,                        -- partner.fund_id
    fund_uuid                 TEXT,                        -- partner.fund_uuid
    currency                  TEXT,                        -- partner.fund_currency
    vintage_year              INTEGER,                     -- metrics.vintage_year
    commitment                TEXT,                        -- metrics.commitment (decimal string)
    called_capital            TEXT,                        -- metrics.called_capital
    capital_contributed       TEXT,                        -- metrics.capital_contributed
    capital_contributed_paid  TEXT,                        -- metrics.capital_contributed_paid
    distributions             TEXT,                        -- metrics.distributions
    net_asset_value           TEXT,                        -- metrics.net_asset_value (NAV)
    capital_call_liabilities  TEXT,                        -- metrics.capital_call_liabilities
    prepaid_capital_contribution TEXT,                     -- metrics.prepaid_capital_contribution
    sharing_date              TEXT,                        -- the as-of/sharing date
    payload                   TEXT    NOT NULL,            -- the full partner-metrics object
    PRIMARY KEY (snapshot_at, entity_external_id)
);


-- ============================================================
-- CAP CALLS — active LP capital calls (forward-compatible; empty observed)
-- ============================================================
-- The individual_lp_active_cap_calls endpoint returned [] in the first run
-- (no active calls). Schema present so a future call lands without a
-- migration. Historical calls/distributions are captured as documents.
CREATE TABLE cap_calls (
    snapshot_at         INTEGER NOT NULL,
    entity_external_id  INTEGER NOT NULL,
    call_external_id    TEXT    NOT NULL,                  -- source id; synthesized if absent
    due_date            TEXT,
    amount              TEXT,                              -- decimal string
    currency            TEXT,
    status              TEXT,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, entity_external_id, call_external_id)
);


-- ============================================================
-- DOCUMENTS — the PDF archive index (content-deduped)
-- ============================================================
-- One row per distinct PDF content (K-1, 1042-S, capital-account statement,
-- quarterly financials, capital-call / distribution notice). The PDF
-- binaries stay under <bronze-root>/<run-ts>/documents/doc_<id>.pdf; this
-- table indexes them. Same content fetched in multiple dumps collapses to
-- one row; first_seen_at = earliest dump, last_seen_at advances.
-- bronze_path is RELATIVE to the bronze root (host vs container agnostic).
CREATE TABLE documents (
    content_sha256        TEXT    NOT NULL PRIMARY KEY,
    doc_id                INTEGER UNIQUE,                  -- index row id
    uuid                  TEXT,
    document_name         TEXT    NOT NULL,                -- PII: may embed person/fund name
    document_type         TEXT,                            -- source label (e.g. 'Tax - Schedule K-1')
    document_date         TEXT,                            -- ISO / 'YYYY-MM-DD'
    fund_id               TEXT,
    fund_name             TEXT,
    firm_id               TEXT,
    firm_name             TEXT,
    capital_account_name  TEXT,                            -- PII
    stakeholder_name      TEXT,                            -- PII
    file_size             INTEGER NOT NULL,
    bronze_path           TEXT    NOT NULL,                -- '<run-ts>/documents/doc_<id>.pdf' relative to bronze root
    first_seen_at         INTEGER NOT NULL,
    last_seen_at          INTEGER NOT NULL,
    payload               TEXT    NOT NULL                 -- the index row verbatim
);
CREATE INDEX ix_documents_doc_id ON documents(doc_id);
CREATE INDEX ix_documents_type   ON documents(document_type);
CREATE INDEX ix_documents_fund   ON documents(fund_id);


-- Migration-complete marker (second-to-last; COMMIT closes the txn).
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
