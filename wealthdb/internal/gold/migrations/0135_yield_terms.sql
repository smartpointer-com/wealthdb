-- ============================================================
-- gold schema, migration 0135 — what a holding's own terms state: a
-- bond's coupon, maturity, frequency, call and rating, and an account's
-- stated interest rates (docs/DESIGN.md §7.6). The yield pass
-- (gold.RebuildYield) rewrites the two derived tables at the end of
-- every load and reload:
--
--   * instrument_terms: what the sources state of each bond-like
--     instrument's terms, each field with where it comes from;
--   * account_rates: one row per account, currency, side and tier of a
--     rate a source or a ledger record states.
--
-- The declarations ledger gains four kinds of record (rate, rating,
-- dividend, terms; docs/EXPOSURE.md §9), replaced whole at every load
-- (gold.SyncExposure) like its dimensions. instrument_terms_at lays the
-- terms and rating records in force on a day over instrument_terms. The
-- yield config tables of 0134 are seeded with the built-in conventions,
-- cycles and horizon, so a gold that has not loaded since the upgrade
-- reads the defaults.
--
-- OR REPLACE, IF NOT EXISTS and seeds into empty tables keep this
-- replayable (see gold.Migrate's REPLAY note).
-- ============================================================

-- instrument_terms: what the sources state of a bond-like instrument's
-- terms (vehicle bond, mortgage, loan, convertible_note). coupon_pct is
-- percent a year; frequency the coupon payments a year (0 a discount
-- instrument). coupon_kind is fixed, floating, inflation_linked or zero.
-- call_date is the first day the issuer may redeem early at a stated
-- price; make_whole a call that pays the holder for the coupons it
-- forgoes, and so shortens nothing. rating is a credit bucket (AAA to B,
-- unrated); in_default an agency's grade of default. Each *_origin says
-- where the field comes from: payload (the source's own record of the
-- instrument), description (its name), openfigi (OpenFIGI's ticker) or
-- convention (the market's, by yield_conventions).
CREATE TABLE IF NOT EXISTS instrument_terms (
    silver_source_id        TEXT     NOT NULL,
    instrument_external_id  TEXT     NOT NULL,
    vehicle                 TEXT,
    coupon_pct              DOUBLE,
    coupon_origin           TEXT,
    maturity                DATE,
    maturity_origin         TEXT,
    frequency               INTEGER,
    frequency_origin        TEXT,
    coupon_kind             TEXT,
    coupon_kind_origin      TEXT,
    day_count               TEXT,
    day_count_origin        TEXT,
    call_date               DATE,
    call_origin             TEXT,
    make_whole              BOOLEAN  NOT NULL DEFAULT FALSE,
    rating                  TEXT,
    rating_origin           TEXT,
    in_default              BOOLEAN  NOT NULL DEFAULT FALSE,
    PRIMARY KEY (silver_source_id, instrument_external_id)
);

-- account_rates: an interest rate an account earns (side credit) or pays
-- (side debit) on its balance in one currency. tier counts from 1; up_to
-- is the balance a tier runs to, NULL for the last. valid_from is the
-- day the rate holds from; term_end a term deposit's end. day_count is
-- as the source names it. origin is stated (the source's payload, the
-- key in field) or declared (a ledger record, its line in line_no).
-- observed_at is the snapshot the rate was read at, NULL where the
-- source states it once for the account.
CREATE TABLE IF NOT EXISTS account_rates (
    silver_source_id     TEXT     NOT NULL,
    account_external_id  TEXT     NOT NULL,
    currency             TEXT,
    side                 TEXT     NOT NULL,
    tier                 INTEGER  NOT NULL,
    rate_pct             DOUBLE   NOT NULL,
    up_to                DOUBLE,
    valid_from           DATE,
    term_end             DATE,
    day_count            TEXT,
    origin               TEXT     NOT NULL,
    field                TEXT,
    line_no              INTEGER,
    observed_at          BIGINT
);

-- The ledger's yield records, one row each, keyed by its line. A rate
-- names an account; the others a holding. as_of is the UTC midnight of
-- the document's day, NULL for a record that is simply true.
CREATE TABLE IF NOT EXISTS yield_declared_rates (
    line_no              INTEGER  NOT NULL PRIMARY KEY,
    silver_source_id     TEXT     NOT NULL,
    account_external_id  TEXT     NOT NULL,
    as_of                BIGINT,
    currency             TEXT,
    rate_pct             DOUBLE   NOT NULL,
    until                DATE,
    source               JSON,
    note                 TEXT
);

CREATE TABLE IF NOT EXISTS yield_declared_ratings (
    line_no                 INTEGER  NOT NULL PRIMARY KEY,
    silver_source_id        TEXT     NOT NULL,
    instrument_external_id  TEXT     NOT NULL,
    as_of                   BIGINT,
    rating                  TEXT     NOT NULL,
    agency                  TEXT,
    source                  JSON,
    note                    TEXT
);

-- amount is the dividend per share of one payment, in currency (NULL:
-- the holding's own); frequency the payments a year.
CREATE TABLE IF NOT EXISTS yield_declared_dividends (
    line_no                 INTEGER  NOT NULL PRIMARY KEY,
    silver_source_id        TEXT     NOT NULL,
    instrument_external_id  TEXT     NOT NULL,
    as_of                   BIGINT,
    amount                  DOUBLE   NOT NULL,
    frequency               INTEGER  NOT NULL,
    currency                TEXT,
    source                  JSON,
    note                    TEXT
);

CREATE TABLE IF NOT EXISTS yield_declared_terms (
    line_no                 INTEGER  NOT NULL PRIMARY KEY,
    silver_source_id        TEXT     NOT NULL,
    instrument_external_id  TEXT     NOT NULL,
    as_of                   BIGINT,
    coupon_pct              DOUBLE,
    frequency               INTEGER,
    maturity                DATE,
    call_date               DATE,
    fixed_until             DATE,
    reset_date              DATE,
    coupon_kind             TEXT,
    day_count               TEXT,
    duration                DOUBLE,
    yield_pct               DOUBLE,
    source                  JSON,
    note                    TEXT
);

-- The built-in conventions, cycles and horizon (canonical, yield.go),
-- seeded where no load has stamped the yield block yet.
INSERT INTO yield_conventions (market, frequency, day_count, origin)
SELECT * FROM (VALUES ('CHF', 1, '30/360', 'builtin'), ('EUR', 1, '30/360', 'builtin'),
                      ('USD', 2, '30/360', 'builtin'), ('USD:bill', 0, 'ACT/360', 'builtin'),
                      ('USD:government', 2, 'ACT/ACT', 'builtin'))
 WHERE NOT EXISTS (SELECT 1 FROM yield_conventions);

INSERT INTO yield_cycles (sector, cycle, origin)
SELECT * FROM (VALUES ('energy', 'sensitive', 'builtin'), ('materials', 'cyclical', 'builtin'),
                      ('industrials', 'sensitive', 'builtin'), ('consumer_cyclical', 'cyclical', 'builtin'),
                      ('consumer_staples', 'defensive', 'builtin'), ('health_care', 'defensive', 'builtin'),
                      ('financials', 'cyclical', 'builtin'), ('technology', 'sensitive', 'builtin'),
                      ('communication', 'sensitive', 'builtin'), ('utilities', 'defensive', 'builtin'),
                      ('real_estate', 'cyclical', 'builtin'))
 WHERE NOT EXISTS (SELECT 1 FROM yield_cycles);

INSERT INTO yield_settings (horizon_months)
SELECT 12 WHERE NOT EXISTS (SELECT 1 FROM yield_settings);

-- instrument_terms_at: each instrument's terms on a day: instrument_terms
-- with the ledger's terms and rating records in force on it laid over,
-- field by field, origin declared. The record in force is the latest
-- dated on or before the day, else the one without a date; a newer
-- record replaces an older one whole. A record names an instrument no
-- source states terms for too, a fund's factsheet figures among them
-- (duration, yield_pct). A bond with a coupon and no kind stated is fixed
-- by convention, or zero at a coupon of 0. fixed_until and reset_date are
-- a record's alone; terms_as_of is its day.
CREATE OR REPLACE MACRO instrument_terms_at(p_asof) AS TABLE (
    WITH d AS (
        SELECT * FROM yield_declared_terms
         WHERE as_of IS NULL OR as_of <= p_asof
       QUALIFY row_number() OVER (PARTITION BY silver_source_id, instrument_external_id
                                  ORDER BY as_of DESC NULLS LAST, line_no DESC) = 1),
    r AS (
        SELECT * FROM yield_declared_ratings
         WHERE as_of IS NULL OR as_of <= p_asof
       QUALIFY row_number() OVER (PARTITION BY silver_source_id, instrument_external_id
                                  ORDER BY as_of DESC NULLS LAST, line_no DESC) = 1),
    k AS (
        SELECT silver_source_id, instrument_external_id FROM instrument_terms
        UNION SELECT silver_source_id, instrument_external_id FROM d
        UNION SELECT silver_source_id, instrument_external_id FROM r),
    m AS (
        SELECT k.silver_source_id, k.instrument_external_id, t.vehicle,
               COALESCE(d.coupon_pct, t.coupon_pct) AS coupon_pct,
               CASE WHEN d.coupon_pct IS NOT NULL THEN 'declared' ELSE t.coupon_origin END AS coupon_origin,
               COALESCE(d.maturity, t.maturity) AS maturity,
               CASE WHEN d.maturity IS NOT NULL THEN 'declared' ELSE t.maturity_origin END AS maturity_origin,
               COALESCE(d.frequency, t.frequency) AS frequency,
               CASE WHEN d.frequency IS NOT NULL THEN 'declared' ELSE t.frequency_origin END AS frequency_origin,
               d.coupon_kind AS declared_kind, t.coupon_kind AS source_kind, t.coupon_kind_origin AS source_kind_origin,
               COALESCE(d.day_count, t.day_count) AS day_count,
               CASE WHEN d.day_count IS NOT NULL THEN 'declared' ELSE t.day_count_origin END AS day_count_origin,
               COALESCE(d.call_date, t.call_date) AS call_date,
               CASE WHEN d.call_date IS NOT NULL THEN 'declared' ELSE t.call_origin END AS call_origin,
               COALESCE(t.make_whole, FALSE) AND d.call_date IS NULL AS make_whole,
               COALESCE(r.rating, t.rating) AS rating,
               CASE WHEN r.rating IS NOT NULL THEN 'declared' ELSE t.rating_origin END AS rating_origin,
               COALESCE(t.in_default, FALSE) AS in_default,
               d.fixed_until, d.reset_date, d.duration, d.yield_pct, COALESCE(d.as_of, r.as_of) AS terms_as_of
          FROM k
          LEFT JOIN instrument_terms t USING (silver_source_id, instrument_external_id)
          LEFT JOIN d USING (silver_source_id, instrument_external_id)
          LEFT JOIN r USING (silver_source_id, instrument_external_id))
    SELECT m.* EXCLUDE (declared_kind, source_kind, source_kind_origin),
           CASE WHEN declared_kind IS NOT NULL THEN declared_kind
                WHEN source_kind_origin IS DISTINCT FROM 'convention' AND source_kind IS NOT NULL THEN source_kind
                WHEN vehicle = 'bond' AND coupon_pct = 0 THEN 'zero'
                WHEN vehicle = 'bond' AND coupon_pct > 0 THEN 'fixed' END AS coupon_kind,
           CASE WHEN declared_kind IS NOT NULL THEN 'declared'
                WHEN source_kind_origin IS DISTINCT FROM 'convention' AND source_kind IS NOT NULL THEN source_kind_origin
                WHEN vehicle = 'bond' AND coupon_pct IS NOT NULL THEN 'convention' END AS coupon_kind_origin
      FROM m
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (135, CAST(epoch(now()) AS BIGINT));
