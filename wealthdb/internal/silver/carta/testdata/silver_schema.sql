CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                 -- Unix seconds UTC
);
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
    payload             TEXT    NOT NULL, market_value REAL, position_status TEXT,                  -- the full row object
    PRIMARY KEY (snapshot_at, entity_external_id, security_type, security_external_id)
);
CREATE INDEX ix_securities_entity ON securities(entity_external_id);
CREATE INDEX ix_securities_type   ON securities(security_type);
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
