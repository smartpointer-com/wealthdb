-- ============================================================
-- gold schema, migration 0116 — the gains reports, and the cost basis
-- on the positions report.
--
-- No table changes: every object here is a macro over what 0115
-- loads. docs/GAINS.md defines each figure; DESIGN.md §10.13 lists
-- the macros.
--
-- Shared pieces:
--
--   fx_rates_to(ccy), fx_amount   one conversion path into ccy
--   basis_stamp, realized_effective_date, holding_key, lot_key,
--   sum_or_null, gains_quality    the small rules, each written once
--   instrument_labels()           an instrument's symbol and name
--   positions_at(instants)        the position lines as of each instant,
--                                 under the point-in-time rule of
--                                 `wealthdb holdings` (§10.1)
--   gains_position_lines          those lines with clean value,
--                                 unrealized gain, FX and open lots
--   realized_lots_in              the realized lots of a window, with the
--                                 one realized-gain formula and FX
--   gains_events                  the window's sells, in-kind moves and
--                                 corporate actions
--   gains_windows                 one row per bucket, account and
--                                 instrument: what every gains report
--                                 sums, so they reconcile by construction
--
-- Money columns come out as VARCHAR decimals like the other report
-- macros; ratios as DOUBLE. CREATE OR REPLACE keeps this replayable
-- (see gold.Migrate's REPLAY note).
-- ============================================================

-- fx_rates_to: the rate from every currency fx_daily knows into p_ccy,
-- on every day fx_daily holds any rate: direct, else through CHF, else
-- through USD, the order every single-currency report applies. Because
-- the grid holds every day on which any rate changes, an at-or-before
-- join on (currency, day) finds the rates the per-leg joins find.
CREATE OR REPLACE MACRO fx_rates_to(p_ccy) AS TABLE (
    WITH grid AS (
        SELECT c.from_ccy, d.day
          FROM (SELECT DISTINCT from_ccy FROM fx_daily) c,
               (SELECT DISTINCT day FROM fx_daily) d)
    SELECT g.from_ccy, g.day, COALESCE(x.rate, c1.rate * c2.rate, u1.rate * u2.rate) AS rate
      FROM grid g
      ASOF LEFT JOIN fx_daily x  ON x.from_ccy  = g.from_ccy AND x.to_ccy  = p_ccy AND x.day  <= g.day
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = g.from_ccy AND c1.to_ccy = 'CHF' AND c1.day <= g.day
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= g.day
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = g.from_ccy AND u1.to_ccy = 'USD' AND u1.day <= g.day
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= g.day
);

-- fx_amount: p_amt in p_ccy converted into p_tgt at p_rate, a rate
-- from fx_rates_to(p_tgt); the identity when the currencies agree.
CREATE OR REPLACE MACRO fx_amount(p_amt, p_ccy, p_tgt, p_rate) AS
    CASE WHEN p_ccy = p_tgt THEN p_amt::DOUBLE ELSE p_amt::DOUBLE * p_rate END;

-- basis_stamp: a book value's stamp as one token, origin/method/fees.
CREATE OR REPLACE MACRO basis_stamp(p_origin, p_method, p_fees) AS
    p_origin || '/' || p_method || '/' || p_fees;

-- realized_effective_date: the date a realized lot counts on: its
-- disposal date, else its settlement date, else the last day of its
-- tax year.
CREATE OR REPLACE MACRO realized_effective_date(p_disposal, p_settlement, p_tax_year) AS
    COALESCE(p_disposal, p_settlement, make_date(p_tax_year, 12, 31));

-- holding_key, lot_key: what a position line and a realized lot key on
-- in gains_windows. A line keys on its instrument, else its position
-- key; a lot on its instrument, else its instrument hint, else its
-- description, else '' (a lot that names nothing).
CREATE OR REPLACE MACRO holding_key(p_instrument, p_position_key) AS
    COALESCE(p_instrument, p_position_key);
CREATE OR REPLACE MACRO lot_key(p_instrument, p_hint, p_description) AS
    COALESCE(p_instrument, p_hint, p_description, '');

-- sum_or_null: a + b, reading a missing term as zero, NULL when both
-- are missing.
CREATE OR REPLACE MACRO sum_or_null(p_a, p_b) AS
    CASE WHEN p_a IS NULL AND p_b IS NULL THEN NULL ELSE COALESCE(p_a, 0) + COALESCE(p_b, 0) END;

-- gains_quality: the quality column, each flag named in docs/GAINS.md
-- §6, in that order.
CREATE OR REPLACE MACRO gains_quality(p_undocumented, p_without_gain, p_undated, p_unmatched,
        p_in_kind, p_corporate, p_basis_changed, p_unobserved, p_paid_in, p_onboarded, p_fx_missing) AS
    concat_ws(';',
        CASE WHEN p_undocumented > 0 THEN 'sells_without_documents=' || p_undocumented END,
        CASE WHEN p_without_gain > 0 THEN 'lots_without_gain=' || p_without_gain END,
        CASE WHEN p_undated > 0 THEN 'undated_lots=' || p_undated END,
        CASE WHEN p_unmatched > 0 THEN 'unmatched_lots=' || p_unmatched END,
        CASE WHEN p_in_kind > 0 THEN 'in_kind_moves=' || p_in_kind END,
        CASE WHEN p_corporate > 0 THEN 'corporate_actions=' || p_corporate END,
        CASE WHEN p_basis_changed > 0 THEN 'basis_changed=' || p_basis_changed END,
        CASE WHEN p_unobserved > 0 THEN 'accounts_unobserved=' || p_unobserved END,
        CASE WHEN p_paid_in THEN 'paid_in_basis' END,
        CASE WHEN len(p_onboarded) > 0 THEN 'onboarded_in_window=' || array_to_string(p_onboarded, ',') END,
        CASE WHEN p_fx_missing > 0 THEN 'fx_missing=' || p_fx_missing END);

-- instrument_labels: each instrument's symbol (the source's, else the
-- resolved one) and name, keyed as the source keys it.
CREATE OR REPLACE MACRO instrument_labels() AS TABLE (
    WITH k AS (
        SELECT silver_source_id, instrument_external_id FROM instruments
        UNION
        SELECT silver_source_id, lookup_value FROM symbol_resolutions WHERE lookup_kind = 'instrument_external_id')
    SELECT k.silver_source_id, k.instrument_external_id, COALESCE(i.symbol, sri.symbol) AS symbol, i.name
      FROM k
      LEFT JOIN instruments i ON i.silver_source_id = k.silver_source_id
           AND i.instrument_external_id = k.instrument_external_id
      LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = k.silver_source_id
           AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = k.instrument_external_id
);

-- positions_at: the position lines as of each instant in p_instants, one
-- row per (instant, line). Each source contributes its latest snapshot
-- at or before the instant.
CREATE OR REPLACE MACRO positions_at(p_instants) AS TABLE (
    WITH b AS (SELECT DISTINCT unnest(p_instants) AS instant),
    snaps AS (SELECT DISTINCT silver_source_id, snapshot_at FROM positions),
    pick AS (
        SELECT b.instant, s.silver_source_id, MAX(s.snapshot_at) AS snapshot_at
          FROM b JOIN snaps s ON s.snapshot_at <= b.instant
         GROUP BY b.instant, s.silver_source_id)
    SELECT pk.instant, p.silver_source_id, p.snapshot_at, p.account_external_id,
           a.display_name, a.relationship_id, a.nickname, a.account_category,
           p.position_key, p.instrument_external_id, il.symbol, il.name,
           p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value,
           p.book_value, p.accrued_interest, p.acquisition_date,
           p.basis_origin, p.basis_method, p.basis_fees
      FROM positions p
      JOIN pick pk ON pk.silver_source_id = p.silver_source_id AND pk.snapshot_at = p.snapshot_at
      LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id AND a.account_external_id = p.account_external_id
      LEFT JOIN instrument_labels() il ON il.silver_source_id = p.silver_source_id
           AND il.instrument_external_id = p.instrument_external_id
);

-- gains_position_lines: positions_at with the figures every gains
-- reader takes from a line.
--   clean_value    market value less the accrued income it carries
--                  (DESIGN.md §7.1)
--   basis_applies  false for a line no cost basis describes: cash held
--                  as a position, a mortgage, and an FX forward, whose
--                  value is itself the gain
--   unrealized     clean value less the cost basis, NULL where the
--                  source states none or no cost basis applies
--   rate           the rate into p_ccy at the snapshot day (1 in p_ccy
--                  itself); every *_x amount converts at it, so an
--                  unrealized gain carries no FX between its two legs
--   open_lots      the line's open lots
CREATE OR REPLACE MACRO gains_position_lines(p_instants, p_ccy) AS TABLE (
    WITH a AS (
        SELECT p.*,
               p.asset_class <> 'cash' AND COALESCE(p.vehicle, '') NOT IN ('mortgage', 'forward') AS basis_applies,
               p.market_value - COALESCE(p.accrued_interest, 0) AS clean_value,
               basis_stamp(p.basis_origin, p.basis_method, p.basis_fees) AS stamp
          FROM positions_at(p_instants) p),
    l AS (
        SELECT a.*, CASE WHEN a.basis_applies THEN a.clean_value - a.book_value END AS unrealized
          FROM a),
    ol AS (
        SELECT silver_source_id, snapshot_at, account_external_id, position_key, COUNT(*) AS n
          FROM position_lots GROUP BY ALL)
    SELECT l.*,
           CASE WHEN l.currency = p_ccy THEN 1.0 ELSE fx.rate END AS rate,
           fx_amount(l.market_value, l.currency, p_ccy, fx.rate) AS value_x,
           fx_amount(l.book_value, l.currency, p_ccy, fx.rate) AS book_x,
           fx_amount(l.unrealized, l.currency, p_ccy, fx.rate) AS unrealized_x,
           l.currency <> p_ccy AND fx.rate IS NULL AS fx_missing,
           COALESCE(ol.n, 0) AS open_lots
      FROM l
      LEFT JOIN ol ON ol.silver_source_id = l.silver_source_id AND ol.snapshot_at = l.snapshot_at
           AND ol.account_external_id = l.account_external_id AND ol.position_key = l.position_key
      ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = l.currency AND fx.day <= (l.snapshot_at // 86400)
);

-- report_positions: every position line as of p_asof, value in p_ccy,
-- with its cost basis, clean value and unrealized gain.
CREATE OR REPLACE MACRO report_positions(p_asof, p_ccy) AS TABLE (
    SELECT silver_source_id, snapshot_at, account_external_id, display_name,
           relationship_id, nickname, account_category, position_key,
           instrument_external_id, symbol, name, asset_class, vehicle, currency,
           CAST(quantity AS VARCHAR) AS quantity, CAST(market_value AS VARCHAR) AS market_value,
           CAST(value_x::DECIMAL(28,4) AS VARCHAR) AS value_outccy,
           CAST(book_value AS VARCHAR) AS book_value,
           CAST(book_x::DECIMAL(28,4) AS VARCHAR) AS book_value_outccy,
           CAST(accrued_interest AS VARCHAR) AS accrued_interest,
           CAST(clean_value AS VARCHAR) AS clean_value,
           CAST(unrealized AS VARCHAR) AS unrealized_gain,
           CAST(unrealized_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_gain_outccy,
           unrealized::DOUBLE / NULLIF(book_value::DOUBLE, 0) AS unrealized_ratio,
           stamp AS basis_stamp,
           CAST(acquisition_date AS VARCHAR) AS acquisition_date
      FROM gains_position_lines([p_asof], p_ccy)
     ORDER BY silver_source_id, account_external_id, position_key
);

-- report_cash: one line per account and currency with a non-zero cash
-- balance, in report_positions' shape; cash has no cost basis, so those
-- columns are NULL.
CREATE OR REPLACE MACRO report_cash(p_asof, p_ccy) AS TABLE (
    SELECT cc.silver_source_id, cc.snapshot_at, cc.account_external_id, a.display_name,
           a.relationship_id, a.nickname, a.account_category,
           'cash:' || cc.currency AS position_key, CAST(NULL AS VARCHAR) AS instrument_external_id,
           cc.currency AS symbol, 'Cash ' || cc.currency AS name,
           'cash' AS asset_class, 'demand_deposit' AS vehicle, cc.currency,
           CAST(NULL AS VARCHAR) AS quantity, CAST(cc.amount AS VARCHAR) AS market_value,
           CAST(fx_amount(cc.amount, cc.currency, p_ccy, fx.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy,
           CAST(NULL AS VARCHAR) AS book_value, CAST(NULL AS VARCHAR) AS book_value_outccy,
           CAST(NULL AS VARCHAR) AS accrued_interest, CAST(NULL AS VARCHAR) AS clean_value,
           CAST(NULL AS VARCHAR) AS unrealized_gain, CAST(NULL AS VARCHAR) AS unrealized_gain_outccy,
           CAST(NULL AS DOUBLE) AS unrealized_ratio, CAST(NULL AS VARCHAR) AS basis_stamp,
           CAST(NULL AS VARCHAR) AS acquisition_date
      FROM cash_chosen(p_asof) cc
      LEFT JOIN accounts a ON cc.silver_source_id = a.silver_source_id AND cc.account_external_id = a.account_external_id
      ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = cc.currency AND fx.day <= (cc.snapshot_at // 86400)
     ORDER BY cc.silver_source_id, cc.account_external_id, cc.currency
);

-- realized_lots_in: the realized lots whose effective date
-- (realized_effective_date; `undated` when only the tax year places it)
-- falls in [p_from, p_to]: the primary set, or every copy with p_all.
-- The gain is the document's, else proceeds − cost basis + wash sale
-- disallowed, else NULL; this is the one place that formula lives.
-- Amounts convert at the effective date's rate.
CREATE OR REPLACE MACRO realized_lots_in(p_from, p_to, p_ccy, p_all) AS TABLE (
    WITH r AS (
        SELECT r.* EXCLUDE (payload),
               realized_effective_date(r.disposal_date, r.settlement_date, r.tax_year) AS effective_date,
               r.disposal_date IS NULL AND r.settlement_date IS NULL AS undated,
               COALESCE(r.realized_gain_loss,
                        r.proceeds - r.book_value + COALESCE(r.wash_sale_disallowed, 0)) AS gain,
               CASE WHEN r.realized_gain_loss IS NOT NULL THEN 'stated'
                    WHEN r.proceeds IS NOT NULL AND r.book_value IS NOT NULL THEN 'derived'
                    ELSE 'unknown' END AS gain_origin,
               basis_stamp(r.basis_origin, r.basis_method, r.basis_fees) AS stamp
          FROM realized_lots r
         WHERE p_all OR r.is_primary),
    w AS (SELECT *, CAST(epoch(effective_date) AS BIGINT) AS effective_at FROM r)
    SELECT w.*, il.symbol,
           fx_amount(w.proceeds, w.currency, p_ccy, fx.rate) AS proceeds_x,
           fx_amount(w.book_value, w.currency, p_ccy, fx.rate) AS book_x,
           fx_amount(w.gain, w.currency, p_ccy, fx.rate) AS gain_x,
           fx_amount(w.wash_sale_disallowed, w.currency, p_ccy, fx.rate) AS wash_x,
           w.currency <> p_ccy AND fx.rate IS NULL AS fx_missing
      FROM w
      LEFT JOIN instrument_labels() il ON il.silver_source_id = w.silver_source_id
           AND il.instrument_external_id = w.instrument_external_id
      ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = w.currency AND fx.day <= (w.effective_at // 86400)
     WHERE w.effective_at BETWEEN p_from AND p_to
);

-- gains_events: the transactions in [p_from, p_to] that move a holding
-- other than by a purchase, one row each, `event` saying which:
--   sell              a sale. It is documented when a primary realized
--                     lot of its calendar year sits on its account or on
--                     another account of the same registered portfolio:
--                     a bank can book the sale on the portfolio's cash
--                     account and print the lots against its custody
--                     account.
--   in_kind           a security moved in or out (a transfer or journal
--                     naming an instrument and a quantity): the holding
--                     arrives or leaves with its whole unrealized gain.
--   corporate_action  a merger, split or spin-off: one holding becomes
--                     another with no realized lot.
CREATE OR REPLACE MACRO gains_events(p_from, p_to) AS TABLE (
    WITH m AS (SELECT * FROM portfolio_acct_map()),
    docs AS (
        SELECT DISTINCT r.silver_source_id AS src, r.account_external_id AS acct, rm.pid, r.tax_year
          FROM realized_lots r LEFT JOIN m rm ON rm.src = r.silver_source_id AND rm.acct = r.account_external_id
         WHERE r.is_primary)
    SELECT t.silver_source_id, t.account_external_id, t.instrument_external_id, t.occurred_at,
           CASE t.kind WHEN 'sell' THEN 'sell' WHEN 'corporate_action' THEN 'corporate_action'
                ELSE 'in_kind' END AS event,
           t.kind = 'sell' AND EXISTS (
               SELECT 1 FROM docs d
                WHERE d.src = t.silver_source_id
                  AND d.tax_year = year(epoch_ms(t.occurred_at * 1000))
                  AND (d.acct = t.account_external_id OR (tm.pid <> '' AND d.pid = tm.pid))) AS documented
      FROM transactions t
      LEFT JOIN m tm ON tm.src = t.silver_source_id AND tm.acct = t.account_external_id
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND (t.kind = 'sell'
            OR (t.kind = 'corporate_action' AND t.instrument_external_id IS NOT NULL)
            OR (t.kind IN ('transfer_in', 'transfer_out', 'journal')
                AND t.instrument_external_id IS NOT NULL AND t.quantity IS NOT NULL))
);

-- report_gains_realized: the realized lots of the window, one row per
-- lot, oldest first.
CREATE OR REPLACE MACRO report_gains_realized(p_from, p_to, p_ccy, p_all) AS TABLE (
    SELECT r.silver_source_id, r.account_external_id, a.display_name, a.nickname,
           CAST(r.effective_date AS VARCHAR) AS effective_date, r.undated,
           r.instrument_external_id, r.symbol, r.description,
           CAST(r.quantity AS VARCHAR) AS quantity,
           CAST(r.acquisition_date AS VARCHAR) AS acquisition_date, r.acquired_various,
           date_diff('day', r.acquisition_date, r.effective_date) AS held_days,
           r.term, r.covered, r.form_8949_box, r.currency,
           CAST(r.proceeds AS VARCHAR) AS proceeds, CAST(r.book_value AS VARCHAR) AS book_value,
           CAST(r.gain AS VARCHAR) AS gain,
           CAST(r.wash_sale_disallowed AS VARCHAR) AS wash_sale_disallowed,
           CAST(r.accrued_market_discount AS VARCHAR) AS accrued_market_discount,
           r.gain_origin,
           CAST(r.proceeds_x::DECIMAL(28,4) AS VARCHAR) AS proceeds_outccy,
           CAST(r.book_x::DECIMAL(28,4) AS VARCHAR) AS book_value_outccy,
           CAST(r.gain_x::DECIMAL(28,4) AS VARCHAR) AS gain_outccy,
           r.document_kind, r.tax_year, r.is_primary, r.stamp AS basis_stamp,
           r.realized_lot_external_id, r.source_document
      FROM realized_lots_in(p_from, p_to, p_ccy, p_all) r
      LEFT JOIN accounts a ON a.silver_source_id = r.silver_source_id AND a.account_external_id = r.account_external_id
     ORDER BY r.effective_at, r.silver_source_id, r.account_external_id, r.realized_lot_external_id
);

-- report_lots: the open lots of the positions held at p_asof. A lot's
-- value is the one its source states, else its position's value pro
-- rata by quantity. Either way the unrealized gain leaves out the lot's
-- share of the position's accrued income, so the lots' gains add up to
-- the position's. A lot of a line no cost basis describes has no
-- unrealized gain.
CREATE OR REPLACE MACRO report_lots(p_asof, p_ccy) AS TABLE (
    WITH v AS (
        SELECT p.silver_source_id, p.snapshot_at, p.account_external_id, p.display_name, p.nickname,
               p.position_key, p.symbol, p.name, p.currency, p.rate, p.basis_applies, l.lot_key,
               l.acquisition_date, l.term, l.covered, l.quantity, l.book_value, l.basis_origin,
               l.source_document,
               l.quantity::DOUBLE / NULLIF(p.quantity::DOUBLE, 0) AS share,
               CASE WHEN l.market_value IS NOT NULL THEN 'stated'
                    WHEN p.market_value IS NOT NULL AND p.quantity <> 0 THEN 'pro_rata' END AS value_origin,
               l.market_value::DOUBLE AS stated_value,
               p.market_value::DOUBLE AS position_value,
               COALESCE(p.accrued_interest::DOUBLE, 0) AS position_accrued
          FROM position_lots l
          JOIN gains_position_lines([p_asof], p_ccy) p
            ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snapshot_at
           AND p.account_external_id = l.account_external_id AND p.position_key = l.position_key),
    u AS (
        SELECT v.*,
               COALESCE(stated_value, position_value * share) AS lot_value,
               CASE WHEN basis_applies
                    THEN COALESCE(stated_value, position_value * share) - position_accrued * share
                         - book_value::DOUBLE END AS unrealized
          FROM v)
    SELECT silver_source_id, snapshot_at, account_external_id, display_name, nickname,
           position_key, symbol, name, lot_key,
           CAST(acquisition_date AS VARCHAR) AS acquisition_date,
           date_diff('day', acquisition_date, CAST(epoch_ms(CAST(p_asof AS BIGINT) * 1000) AS DATE)) AS held_days,
           term, covered, CAST(quantity AS VARCHAR) AS quantity, currency,
           CAST(book_value AS VARCHAR) AS book_value,
           CAST(lot_value::DECIMAL(28,4) AS VARCHAR) AS market_value, value_origin,
           CAST(unrealized::DECIMAL(28,4) AS VARCHAR) AS unrealized_gain,
           unrealized / NULLIF(book_value::DOUBLE, 0) AS unrealized_ratio,
           CAST((book_value::DOUBLE * rate)::DECIMAL(28,4) AS VARCHAR) AS book_value_outccy,
           CAST((lot_value * rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy,
           CAST((unrealized * rate)::DECIMAL(28,4) AS VARCHAR) AS unrealized_gain_outccy,
           basis_origin, source_document
      FROM u
     ORDER BY silver_source_id, account_external_id, position_key, acquisition_date, lot_key
);

-- gains_windows: one row per period bucket of [p_from, p_to], account
-- and instrument: the holding at the bucket's two boundaries, the lots
-- realized in it and the events that moved it. Every gains report sums
-- these rows, so the grains and the positions view reconcile by
-- construction.
--
-- A bucket's start is the holdings as of the second before it opens,
-- its end the holdings as of its last second. The window opens no
-- earlier than the first snapshot, realized lot or event gold holds, so
-- an open start does not produce empty buckets back to 1970.
--
-- A position line keys on holding_key, a realized lot on lot_key, an
-- event on its instrument; rows meet where they name the same one. Per
-- key:
--   unrealized_change_x  end − start, each side the sum of its lines,
--                        each side converted at its own date, so it
--                        includes the exchange-rate change on a gain
--                        carried through the bucket. Unknown (NULL)
--                        when a side holds a line a cost basis applies
--                        to but the source states no basis or no value
--                        for.
--   basis_changed        that unknown change where the other side has
--                        a full basis: the source began or stopped
--                        stating one inside the bucket.
--   unmatched            lots whose key the account never held in any
--                        snapshot: the document and the holdings share
--                        no identity. A round trip inside the bucket is
--                        not one when any snapshot holds it.
--   account_unobserved   the account has lines at one boundary only,
--                        while its source has a snapshot at the other
--                        and the account had lines by then, and no lot
--                        or event in the bucket explains it: the
--                        snapshot left it out, or it closed without a
--                        closing row.
CREATE OR REPLACE MACRO gains_windows(p_from, p_to, p_ccy, p_period) AS TABLE (
    WITH first_data AS (
        SELECT LEAST((SELECT MIN(snapshot_at) FROM positions),
                     (SELECT MIN(CAST(epoch(realized_effective_date(disposal_date, settlement_date, tax_year)) AS BIGINT))
                        FROM realized_lots),
                     (SELECT MIN(occurred_at) FROM transactions
                       WHERE kind IN ('sell', 'transfer_in', 'transfer_out', 'journal', 'corporate_action'))) AS f),
    w AS (SELECT CAST(GREATEST(p_from, COALESCE(f, p_from)) AS BIGINT) AS f0,
                 CASE WHEN p_period = 'total' THEN 'year' ELSE p_period END AS unit
            FROM first_data),
    starts AS (
        SELECT f0 AS b_from FROM w
        UNION
        SELECT CAST(epoch(x) AS BIGINT)
          FROM (SELECT f0, unnest(generate_series(date_trunc(unit, epoch_ms(f0 * 1000)),
                                                   epoch_ms(CAST(p_to AS BIGINT) * 1000),
                                                   CAST('1 ' || unit AS INTERVAL))) AS x FROM w)
         WHERE p_period <> 'total' AND CAST(epoch(x) AS BIGINT) > f0),
    bk AS (
        SELECT b_from, COALESCE(LEAD(b_from) OVER (ORDER BY b_from) - 1, p_to) AS b_to
          FROM starts WHERE b_from <= p_to),
    instants AS (SELECT list(DISTINCT t) AS l FROM (SELECT b_from - 1 AS t FROM bk UNION SELECT b_to FROM bk)),
    lines AS (SELECT * FROM gains_position_lines((SELECT l FROM instants), p_ccy)),
    -- One row per instant, account and instrument.
    side AS (
        SELECT instant, silver_source_id, account_external_id,
               holding_key(instrument_external_id, position_key) AS k,
               min(position_key) AS position_key, any_value(symbol) AS symbol, any_value(name) AS name,
               any_value(asset_class) AS asset_class, any_value(vehicle) AS vehicle,
               -- The native figures need one currency; lines in several
               -- leave them blank and keep the converted ones.
               CASE WHEN COUNT(DISTINCT currency) = 1 THEN any_value(currency) END AS currency,
               SUM(quantity) AS quantity,
               CASE WHEN COUNT(DISTINCT currency) = 1 THEN SUM(market_value) END AS market_value,
               SUM(value_x) AS value_x,
               bool_or(basis_applies) AS applies,
               bool_and(NOT basis_applies OR (book_value IS NOT NULL AND market_value IS NOT NULL)) AS complete,
               CASE WHEN COUNT(DISTINCT currency) = 1 THEN SUM(book_value) END AS book_value,
               SUM(book_x) AS book_x,
               CASE WHEN COUNT(DISTINCT currency) = 1 THEN SUM(unrealized) END AS unrealized,
               SUM(unrealized_x) AS unrealized_x,
               bool_or(fx_missing AND basis_applies) AS unpriced,
               string_agg(DISTINCT stamp, ',' ORDER BY stamp) AS stamp,
               min(acquisition_date) AS acquisition_date,
               bool_or(basis_method = 'paid_in') AS paid_in,
               bool_or(fx_missing) AS fx_missing,
               SUM(open_lots) AS open_lots
          FROM lines GROUP BY instant, silver_source_id, account_external_id, k),
    en AS (SELECT bk.b_from, s.* FROM side s JOIN bk ON s.instant = bk.b_to),
    st AS (SELECT bk.b_from, s.* FROM side s JOIN bk ON s.instant = bk.b_from - 1),
    pos AS (
        SELECT COALESCE(e.b_from, s.b_from) AS b_from,
               COALESCE(e.silver_source_id, s.silver_source_id) AS silver_source_id,
               COALESCE(e.account_external_id, s.account_external_id) AS account_external_id,
               COALESCE(e.k, s.k) AS k,
               e.position_key AS end_key, s.position_key AS start_key,
               COALESCE(e.symbol, s.symbol) AS symbol, COALESCE(e.name, s.name) AS name,
               COALESCE(e.asset_class, s.asset_class) AS asset_class,
               COALESCE(e.vehicle, s.vehicle) AS vehicle, COALESCE(e.currency, s.currency) AS currency,
               s.quantity AS quantity_start, e.quantity AS quantity_end,
               CASE WHEN e.complete THEN e.book_value END AS book_value,
               CASE WHEN e.complete THEN e.book_x END AS book_x,
               e.market_value, e.value_x,
               CASE WHEN e.complete THEN e.unrealized END AS unrealized_end,
               CASE WHEN s.complete THEN s.unrealized_x END AS unrealized_start_x,
               CASE WHEN e.complete THEN e.unrealized_x END AS unrealized_end_x,
               (s.k IS NOT NULL AND NOT s.complete) OR (e.k IS NOT NULL AND NOT e.complete) AS change_unknown,
               (s.k IS NOT NULL AND s.applies AND s.complete) OR (e.k IS NOT NULL AND e.applies AND e.complete) AS one_side_known,
               e.k IS NOT NULL AS at_end, COALESCE(e.applies, FALSE) AS end_applies,
               COALESCE(e.applies AND NOT e.complete, FALSE) AS end_without_basis,
               COALESCE(e.unpriced, FALSE) AS end_unpriced,
               e.stamp, COALESCE(e.acquisition_date, s.acquisition_date) AS acquisition_date,
               COALESCE(e.open_lots, 0) AS open_lots,
               COALESCE(e.paid_in, FALSE) OR COALESCE(s.paid_in, FALSE) AS paid_in,
               COALESCE(e.fx_missing, FALSE) OR COALESCE(s.fx_missing, FALSE) AS fx_missing
          FROM en e FULL JOIN st s ON s.b_from = e.b_from AND s.silver_source_id = e.silver_source_id
               AND s.account_external_id = e.account_external_id AND s.k = e.k),
    held AS (
        SELECT DISTINCT silver_source_id, account_external_id,
               holding_key(instrument_external_id, position_key) AS k
          FROM positions),
    lots AS (
        SELECT bk.b_from, r.silver_source_id, r.account_external_id, r.k,
               any_value(r.symbol) AS symbol, any_value(r.description) AS description,
               any_value(r.currency) AS currency,
               SUM(r.gain_x) AS realized_x,
               SUM(r.gain_x) FILTER (WHERE r.term = 'short') AS realized_short_x,
               SUM(r.gain_x) FILTER (WHERE r.term = 'long') AS realized_long_x,
               SUM(r.gain_x) FILTER (WHERE r.term IS NULL) AS realized_other_x,
               SUM(r.proceeds_x) AS proceeds_x, SUM(r.wash_x) AS wash_x,
               COUNT(*) AS n_lots,
               COUNT(*) FILTER (WHERE r.gain IS NULL) AS n_without_gain,
               COUNT(*) FILTER (WHERE r.undated) AS n_undated,
               COUNT(*) FILTER (WHERE h.k IS NULL) AS n_unmatched,
               COUNT(*) FILTER (WHERE r.fx_missing) AS n_fx_missing
          FROM (SELECT *, lot_key(instrument_external_id, instrument_hint, description) AS k
                  FROM realized_lots_in((SELECT MIN(b_from) FROM bk), p_to, p_ccy, FALSE)) r
          JOIN bk ON r.effective_at BETWEEN bk.b_from AND bk.b_to
          LEFT JOIN held h ON h.silver_source_id = r.silver_source_id AND h.account_external_id = r.account_external_id
               AND h.k = r.k
         GROUP BY bk.b_from, r.silver_source_id, r.account_external_id, r.k),
    events AS (
        SELECT bk.b_from, e.silver_source_id, e.account_external_id,
               COALESCE(e.instrument_external_id, '') AS k,
               COUNT(*) FILTER (WHERE e.event = 'sell') AS n_sells,
               COUNT(*) FILTER (WHERE e.event = 'sell' AND NOT e.documented) AS n_undocumented,
               COUNT(*) FILTER (WHERE e.event = 'in_kind') AS n_in_kind,
               COUNT(*) FILTER (WHERE e.event = 'corporate_action') AS n_corporate
          FROM gains_events((SELECT MIN(b_from) FROM bk), p_to) e
          JOIN bk ON e.occurred_at BETWEEN bk.b_from AND bk.b_to
         GROUP BY ALL),
    acct_first AS (
        SELECT silver_source_id, account_external_id, MIN(snapshot_at) AS first_at FROM positions GROUP BY ALL),
    acct_at AS (SELECT DISTINCT instant, silver_source_id, account_external_id FROM lines),
    src_at AS (SELECT DISTINCT instant, silver_source_id FROM lines),
    unobserved AS (
        SELECT DISTINCT bk.b_from, x.silver_source_id, x.account_external_id
          FROM bk
          JOIN acct_at x ON x.instant = bk.b_from - 1 OR x.instant = bk.b_to
          JOIN src_at o ON o.silver_source_id = x.silver_source_id
               AND o.instant = CASE WHEN x.instant = bk.b_to THEN bk.b_from - 1 ELSE bk.b_to END
          JOIN acct_first af ON af.silver_source_id = x.silver_source_id AND af.account_external_id = x.account_external_id
          LEFT JOIN acct_at y ON y.silver_source_id = x.silver_source_id AND y.account_external_id = x.account_external_id
               AND y.instant = o.instant
         WHERE y.account_external_id IS NULL AND af.first_at <= o.instant
           AND NOT EXISTS (SELECT 1 FROM lots l WHERE l.b_from = bk.b_from
                              AND l.silver_source_id = x.silver_source_id AND l.account_external_id = x.account_external_id)
           AND NOT EXISTS (SELECT 1 FROM events e WHERE e.b_from = bk.b_from
                              AND e.silver_source_id = x.silver_source_id AND e.account_external_id = x.account_external_id)),
    src_first AS (SELECT silver_source_id, MIN(snapshot_at) AS first_at FROM positions GROUP BY 1),
    keys AS (
        SELECT b_from, silver_source_id, account_external_id, k FROM pos
        UNION SELECT b_from, silver_source_id, account_external_id, k FROM lots
        UNION SELECT b_from, silver_source_id, account_external_id, k FROM events)
    SELECT k.b_from, k.silver_source_id, k.account_external_id, k.k,
           COALESCE(m.pid, '') AS portfolio_bucket, a.display_name, a.nickname,
           p.end_key, p.start_key,
           COALESCE(p.symbol, l.symbol, il.symbol) AS symbol, COALESCE(p.name, l.description, il.name) AS name,
           p.asset_class, p.vehicle, COALESCE(p.currency, l.currency) AS currency,
           p.quantity_start, p.quantity_end, p.book_value, p.book_x, p.market_value, p.value_x,
           p.unrealized_end, p.unrealized_start_x, p.unrealized_end_x,
           CASE WHEN NOT COALESCE(p.change_unknown, FALSE)
                THEN sum_or_null(p.unrealized_end_x, -p.unrealized_start_x) END AS unrealized_change_x,
           COALESCE(p.change_unknown AND p.one_side_known, FALSE) AS basis_changed,
           COALESCE(p.at_end, FALSE) AS at_end, COALESCE(p.end_applies, FALSE) AS end_applies,
           COALESCE(p.end_without_basis, FALSE) AS end_without_basis,
           COALESCE(p.end_unpriced, FALSE) AS end_unpriced,
           p.stamp, p.acquisition_date, COALESCE(p.open_lots, 0) AS open_lots,
           COALESCE(p.paid_in, FALSE) AS paid_in,
           l.realized_x, l.realized_short_x, l.realized_long_x, l.realized_other_x, l.proceeds_x, l.wash_x,
           COALESCE(l.n_lots, 0) AS n_lots, COALESCE(l.n_without_gain, 0) AS n_without_gain,
           COALESCE(l.n_undated, 0) AS n_undated, COALESCE(l.n_unmatched, 0) AS n_unmatched,
           COALESCE(ev.n_sells, 0) AS n_sells, COALESCE(ev.n_undocumented, 0) AS n_undocumented,
           COALESCE(ev.n_in_kind, 0) AS n_in_kind, COALESCE(ev.n_corporate, 0) AS n_corporate,
           u.account_external_id IS NOT NULL AS account_unobserved,
           CASE WHEN COALESCE(p.at_end, FALSE) AND sf.first_at BETWEEN k.b_from AND bk.b_to
                THEN k.silver_source_id END AS onboarded_source,
           CAST(COALESCE(p.fx_missing, FALSE) AS INTEGER) + COALESCE(l.n_fx_missing, 0) AS n_fx_missing
      FROM keys k
      JOIN bk ON bk.b_from = k.b_from
      LEFT JOIN pos p ON p.b_from = k.b_from AND p.silver_source_id = k.silver_source_id
           AND p.account_external_id = k.account_external_id AND p.k = k.k
      LEFT JOIN lots l ON l.b_from = k.b_from AND l.silver_source_id = k.silver_source_id
           AND l.account_external_id = k.account_external_id AND l.k = k.k
      LEFT JOIN events ev ON ev.b_from = k.b_from AND ev.silver_source_id = k.silver_source_id
           AND ev.account_external_id = k.account_external_id AND ev.k = k.k
      LEFT JOIN unobserved u ON u.b_from = k.b_from AND u.silver_source_id = k.silver_source_id
           AND u.account_external_id = k.account_external_id
      LEFT JOIN portfolio_acct_map() m ON m.src = k.silver_source_id AND m.acct = k.account_external_id
      LEFT JOIN accounts a ON a.silver_source_id = k.silver_source_id AND a.account_external_id = k.account_external_id
      LEFT JOIN src_first sf ON sf.silver_source_id = k.silver_source_id
      LEFT JOIN instrument_labels() il ON il.silver_source_id = k.silver_source_id AND il.instrument_external_id = k.k
);

-- report_gains_buckets: realized and unrealized gains per period bucket
-- of [p_from, p_to], at one grain: 'all' (one row per bucket),
-- 'sources', 'portfolios' (the holdings buckets, '' for a source's
-- accounts outside any registered portfolio) or 'accounts'. Each grain
-- groups gains_windows' rows its own way.
CREATE OR REPLACE MACRO report_gains_buckets(p_from, p_to, p_ccy, p_period, p_grain) AS TABLE (
    WITH g AS (
        SELECT w.*,
               CASE WHEN p_grain = 'all' THEN '' ELSE silver_source_id END AS g_src,
               CASE WHEN p_grain = 'portfolios' THEN portfolio_bucket ELSE '' END AS g_pid,
               CASE WHEN p_grain = 'accounts' THEN account_external_id ELSE '' END AS g_acct
          FROM gains_windows(p_from, p_to, p_ccy, p_period) w),
    m AS (
        SELECT b_from, g_src, g_pid, g_acct,
               SUM(realized_x) AS realized_x, SUM(realized_short_x) AS realized_short_x,
               SUM(realized_long_x) AS realized_long_x, SUM(realized_other_x) AS realized_other_x,
               SUM(unrealized_start_x) AS unrealized_start_x, SUM(unrealized_end_x) AS unrealized_end_x,
               SUM(unrealized_change_x) AS unrealized_change_x,
               SUM(proceeds_x) AS proceeds_x, SUM(wash_x) AS wash_x,
               SUM(n_lots) AS n_lots, SUM(n_sells) AS n_sells,
               COUNT(*) FILTER (WHERE at_end) AS n_positions,
               COUNT(*) FILTER (WHERE end_without_basis) AS n_without_basis,
               SUM(abs(value_x)) FILTER (WHERE at_end AND end_applies) AS abs_value_x,
               SUM(abs(value_x)) FILTER (WHERE at_end AND end_applies AND NOT end_without_basis) AS abs_basis_value_x,
               bool_or(end_unpriced) AS unpriced,
               SUM(n_undocumented) AS n_undocumented, SUM(n_without_gain) AS n_without_gain,
               SUM(n_undated) AS n_undated, SUM(n_in_kind) AS n_in_kind, SUM(n_corporate) AS n_corporate,
               COUNT(*) FILTER (WHERE basis_changed) AS n_basis_changed,
               COUNT(DISTINCT silver_source_id || '|' || account_external_id) FILTER (WHERE account_unobserved) AS n_unobserved,
               bool_or(paid_in) AS paid_in,
               list_sort(list_distinct(list(onboarded_source) FILTER (WHERE onboarded_source IS NOT NULL))) AS onboarded,
               SUM(n_fx_missing) AS n_fx_missing
          FROM g GROUP BY b_from, g_src, g_pid, g_acct)
    SELECT CASE WHEN p_period = 'total' THEN NULL ELSE m.b_from END AS period_start,
           NULLIF(m.g_src, '') AS silver_source_id,
           CASE WHEN p_grain = 'portfolios' THEN m.g_pid END AS portfolio_external_id,
           pb.display_name AS portfolio_display_name,
           NULLIF(m.g_acct, '') AS account_external_id,
           a.display_name, a.nickname, a.account_kind, a.tax_wrapper, a.account_category, a.relationship_id,
           CAST(m.realized_x::DECIMAL(28,4) AS VARCHAR) AS realized,
           CAST(m.realized_short_x::DECIMAL(28,4) AS VARCHAR) AS realized_short,
           CAST(m.realized_long_x::DECIMAL(28,4) AS VARCHAR) AS realized_long,
           CAST(m.realized_other_x::DECIMAL(28,4) AS VARCHAR) AS realized_other,
           CAST(m.unrealized_start_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_start,
           CAST(m.unrealized_end_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_end,
           CAST(m.unrealized_change_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_change,
           CAST(sum_or_null(m.realized_x, m.unrealized_change_x)::DECIMAL(28,4) AS VARCHAR) AS gain,
           CAST(m.proceeds_x::DECIMAL(28,4) AS VARCHAR) AS proceeds,
           CAST(m.wash_x::DECIMAL(28,4) AS VARCHAR) AS wash_disallowed,
           CAST(m.n_lots AS BIGINT) AS realized_lots, CAST(m.n_sells AS BIGINT) AS sells,
           m.n_positions AS positions, m.n_without_basis AS positions_without_basis,
           CASE WHEN NOT m.unpriced THEN m.abs_basis_value_x / NULLIF(m.abs_value_x, 0) END AS basis_coverage,
           gains_quality(m.n_undocumented, m.n_without_gain, m.n_undated, 0, m.n_in_kind, m.n_corporate,
                         m.n_basis_changed, m.n_unobserved, m.paid_in, m.onboarded, m.n_fx_missing) AS quality
      FROM m
      LEFT JOIN portfolio_buckets() pb ON p_grain = 'portfolios' AND pb.src = m.g_src AND pb.pid = m.g_pid
      LEFT JOIN accounts a ON p_grain = 'accounts' AND a.silver_source_id = m.g_src AND a.account_external_id = m.g_acct
     ORDER BY m.b_from, m.g_src, m.g_pid, m.g_acct
);

-- report_gains_positions: gains_windows' rows for the whole window, one
-- per account and instrument. `lot_key` names a row the lots or events
-- alone make, where no position line names the instrument at either
-- end.
CREATE OR REPLACE MACRO report_gains_positions(p_from, p_to, p_ccy) AS TABLE (
    SELECT silver_source_id, account_external_id, display_name, nickname,
           COALESCE(end_key, start_key) AS position_key,
           CASE WHEN end_key IS NULL AND start_key IS NULL THEN NULLIF(k, '') END AS lot_key,
           symbol, name, asset_class, vehicle, currency,
           CAST(quantity_start AS VARCHAR) AS quantity_start, CAST(quantity_end AS VARCHAR) AS quantity_end,
           CAST(book_value AS VARCHAR) AS book_value,
           CAST(book_x::DECIMAL(28,4) AS VARCHAR) AS book_value_outccy,
           CAST(market_value AS VARCHAR) AS market_value,
           CAST(value_x::DECIMAL(28,4) AS VARCHAR) AS value_outccy,
           CAST(unrealized_start_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_start,
           CAST(unrealized_end_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_end,
           CAST(unrealized_change_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_change,
           CAST(realized_x::DECIMAL(28,4) AS VARCHAR) AS realized,
           CAST(sum_or_null(realized_x, unrealized_change_x)::DECIMAL(28,4) AS VARCHAR) AS gain,
           unrealized_end::DOUBLE / NULLIF(book_value::DOUBLE, 0) AS unrealized_ratio,
           stamp AS basis_stamp,
           CAST(acquisition_date AS VARCHAR) AS acquisition_date,
           open_lots,
           gains_quality(n_undocumented, n_without_gain, n_undated, n_unmatched, n_in_kind, n_corporate,
                         CAST(basis_changed AS INTEGER), CAST(account_unobserved AS INTEGER), paid_in,
                         CASE WHEN onboarded_source IS NULL THEN [] ELSE [onboarded_source] END,
                         n_fx_missing) AS quality
      FROM gains_windows(p_from, p_to, p_ccy, 'total')
     ORDER BY silver_source_id, account_external_id, COALESCE(end_key, start_key, k)
);

-- report_gains_coverage: per account, how far the window's gains can be
-- trusted. One row per account holding positions a cost basis can
-- describe at the window's end (gains_position_lines' basis_applies),
-- or with sells or primary realized lots inside it. A position with no
-- rate into p_ccy leaves the share unmeasured (NULL). The verdict:
-- `no_basis` an account holding positions, none with a cost basis;
-- `no_realized` sells no document covers, on an account whose
-- positions are covered; `ok` positions at least 99% covered by value
-- and every sell documented; `partial` anything between.
CREATE OR REPLACE MACRO report_gains_coverage(p_from, p_to, p_ccy) AS TABLE (
    WITH pos AS (
        SELECT silver_source_id, account_external_id,
               COUNT(*) AS n_positions,
               COUNT(book_value) AS n_with_basis,
               SUM(value_x) AS value_x,
               SUM(value_x) FILTER (WHERE book_value IS NOT NULL) AS value_with_basis_x,
               CASE WHEN COUNT(*) FILTER (WHERE value_x IS NULL) = 0
                    THEN SUM(abs(value_x)) FILTER (WHERE book_value IS NOT NULL) / NULLIF(SUM(abs(value_x)), 0)
               END AS coverage,
               string_agg(DISTINCT stamp, ',' ORDER BY stamp) AS stamps,
               SUM(open_lots) AS open_lots
          FROM gains_position_lines([p_to], p_ccy) WHERE basis_applies GROUP BY ALL),
    sells AS (
        SELECT silver_source_id, account_external_id, COUNT(*) AS n_sells,
               COUNT(*) FILTER (WHERE NOT documented) AS n_undocumented
          FROM gains_events(p_from, p_to) WHERE event = 'sell' GROUP BY ALL),
    lots AS (
        SELECT silver_source_id, account_external_id, COUNT(*) AS n_lots
          FROM realized_lots_in(p_from, p_to, p_ccy, FALSE) GROUP BY ALL),
    docs AS (
        SELECT silver_source_id, account_external_id,
               string_agg(DISTINCT document_kind, ',' ORDER BY document_kind) AS documents
          FROM realized_lots
         WHERE tax_year BETWEEN year(epoch_ms(CAST(p_from AS BIGINT) * 1000)) AND year(epoch_ms(CAST(p_to AS BIGINT) * 1000))
         GROUP BY ALL),
    keys AS (
        SELECT silver_source_id, account_external_id FROM pos
        UNION SELECT silver_source_id, account_external_id FROM sells
        UNION SELECT silver_source_id, account_external_id FROM lots),
    m AS (
        SELECT k.silver_source_id, k.account_external_id, a.display_name, a.nickname, a.tax_wrapper,
               COALESCE(p.n_positions, 0) AS n_positions, p.n_with_basis, p.value_x, p.value_with_basis_x,
               p.coverage, p.stamps, COALESCE(p.open_lots, 0) AS open_lots,
               COALESCE(s.n_sells, 0) AS sells, COALESCE(s.n_undocumented, 0) AS n_undocumented,
               COALESCE(l.n_lots, 0) AS realized_lots, d.documents
          FROM keys k
          LEFT JOIN pos p USING (silver_source_id, account_external_id)
          LEFT JOIN sells s USING (silver_source_id, account_external_id)
          LEFT JOIN lots l USING (silver_source_id, account_external_id)
          LEFT JOIN docs d USING (silver_source_id, account_external_id)
          LEFT JOIN accounts a USING (silver_source_id, account_external_id))
    SELECT silver_source_id, account_external_id, display_name, nickname, tax_wrapper,
           CAST(value_x::DECIMAL(28,4) AS VARCHAR) AS value,
           CAST(value_with_basis_x::DECIMAL(28,4) AS VARCHAR) AS value_with_basis,
           coverage AS basis_coverage, stamps AS basis_stamps, CAST(open_lots AS BIGINT) AS open_lots,
           sells, realized_lots, documents,
           CASE WHEN n_positions > 0 AND n_with_basis = 0 THEN 'no_basis'
                WHEN n_undocumented > 0 AND (n_positions = 0 OR coverage >= 0.99) THEN 'no_realized'
                WHEN n_undocumented = 0 AND (n_positions = 0 OR coverage >= 0.99) THEN 'ok'
                ELSE 'partial' END AS verdict
      FROM m
     ORDER BY silver_source_id, account_external_id
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (116, CAST(epoch(now()) AS BIGINT));
