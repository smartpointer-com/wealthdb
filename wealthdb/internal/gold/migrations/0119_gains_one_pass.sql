-- ============================================================
-- gold schema, migration 0119 — the gains readers in one evaluation
-- for several output currencies and readings of a missing cost basis.
--
-- MaterializeGains writes report_gains for every reporting currency
-- under both readings. Run per currency and reading, gains_windows
-- built the buckets, read the positions and the realized lots, and
-- matched the events and findings eight times over. The _all macros
-- below take lists of currencies and readings and do that work once:
-- each line and lot repeats under every currency and reading, carries
-- out_currency and missing_basis, and converts at its own rate. The
-- single-currency macros are those over one-element lists, so every
-- reader keeps one definition.
--
-- Macros only; OR REPLACE keeps this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

-- fx_rates_into: fx_rates_to (0118) for each currency in p_ccys: the
-- rate from every currency gold's figures are in into to_ccy, on every
-- day fx_daily holds any rate. Direct, else through CHF, else through
-- USD.
CREATE OR REPLACE MACRO fx_rates_into(p_ccys) AS TABLE (
    WITH ccys AS (
        SELECT currency AS from_ccy FROM positions
        UNION SELECT currency FROM cash_balances
        UNION SELECT currency FROM realized_lots
        UNION SELECT currency FROM lot_realized
        UNION SELECT currency FROM lots
        UNION SELECT currency FROM lot_anchors),
    grid AS (
        SELECT t.to_ccy, c.from_ccy, d.day
          FROM (SELECT unnest(p_ccys) AS to_ccy) t, ccys c, (SELECT DISTINCT day FROM fx_daily) d)
    SELECT g.to_ccy, g.from_ccy, g.day, COALESCE(x.rate, c1.rate * c2.rate, u1.rate * u2.rate) AS rate
      FROM grid g
      ASOF LEFT JOIN fx_daily x  ON x.from_ccy  = g.from_ccy AND x.to_ccy  = g.to_ccy AND x.day  <= g.day
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = g.from_ccy AND c1.to_ccy = 'CHF'    AND c1.day <= g.day
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = g.to_ccy AND c2.day <= g.day
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = g.from_ccy AND u1.to_ccy = 'USD'    AND u1.day <= g.day
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = g.to_ccy AND u2.day <= g.day
);

CREATE OR REPLACE MACRO fx_rates_to(p_ccy) AS TABLE (
    SELECT from_ccy, day, rate FROM fx_rates_into([p_ccy])
);

-- gains_position_lines_all: gains_position_lines (0118) under every
-- currency in p_ccys and reading in p_readings, which each line names
-- in out_currency and missing_basis.
CREATE OR REPLACE MACRO gains_position_lines_all(p_instants, p_ccys, p_readings) AS TABLE (
    WITH a0 AS (
        SELECT p.* REPLACE (COALESCE(ol.n, p.open_lots, 0) AS open_lots),
               basis_applies(p.asset_class, p.vehicle) AS basis_applies,
               p.market_value - COALESCE(p.accrued_interest, 0) AS clean_value,
               basis_stamp(p.basis_origin, p.basis_method, p.basis_fees) AS stamp
          FROM positions_at(p_instants) p
          LEFT JOIN (SELECT silver_source_id, snapshot_at, account_external_id, position_key, COUNT(*) AS n
                       FROM position_lots GROUP BY ALL) ol
            ON ol.silver_source_id = p.silver_source_id AND ol.snapshot_at = p.snapshot_at
           AND ol.account_external_id = p.account_external_id AND ol.position_key = p.position_key),
    -- One rate per currency, day and output currency, not per line.
    rates AS (
        SELECT d.currency, d.day, d.out_currency, fx.rate
          FROM (SELECT DISTINCT a0.currency, a0.snapshot_at // 86400 AS day, c.out_currency
                  FROM a0, (SELECT unnest(p_ccys) AS out_currency) c) d
          ASOF LEFT JOIN fx_rates_into(p_ccys) fx ON fx.to_ccy = d.out_currency AND fx.from_ccy = d.currency
               AND fx.day <= d.day),
    a AS (
        SELECT a0.* REPLACE (
                   CASE WHEN r.missing_basis = 'zero' AND a0.basis_applies
                        THEN COALESCE(a0.book_value, a0.book_value_known, 0) ELSE a0.book_value END AS book_value),
               r.missing_basis = 'zero' AND basis_missing(a0.asset_class, a0.vehicle, a0.book_value) AS assumed_zero,
               CASE WHEN a0.basis_applies AND a0.book_value IS NULL
                    THEN COALESCE(a0.quantity_without_basis, abs(a0.quantity)) END AS missing_quantity,
               r.missing_basis
          FROM a0, (SELECT unnest(p_readings) AS missing_basis) r),
    l AS (
        SELECT a.*, CASE WHEN a.basis_applies THEN a.clean_value - a.book_value END AS unrealized,
               x.out_currency, x.rate AS fx_rate
          FROM a JOIN rates x ON x.currency = a.currency AND x.day = a.snapshot_at // 86400)
    SELECT l.* EXCLUDE (missing_basis, out_currency, fx_rate),
           CASE WHEN l.currency = l.out_currency THEN 1.0 ELSE l.fx_rate END AS rate,
           fx_amount(l.market_value, l.currency, l.out_currency, l.fx_rate) AS value_x,
           fx_amount(l.book_value, l.currency, l.out_currency, l.fx_rate) AS book_x,
           fx_amount(l.unrealized, l.currency, l.out_currency, l.fx_rate) AS unrealized_x,
           l.currency <> l.out_currency AND l.fx_rate IS NULL AS fx_missing,
           l.out_currency, l.missing_basis
      FROM l
);

CREATE OR REPLACE MACRO gains_position_lines(p_instants, p_ccy, p_missing := 'ignore') AS TABLE (
    SELECT * EXCLUDE (out_currency, missing_basis)
      FROM gains_position_lines_all(p_instants, [p_ccy], [p_missing])
);

-- realized_lots_in_all: realized_lots_in (0118) under every currency in
-- p_ccys and reading in p_readings, named the same way.
CREATE OR REPLACE MACRO realized_lots_in_all(p_from, p_to, p_ccys, p_all, p_readings) AS TABLE (
    WITH d AS (
        SELECT r.* EXCLUDE (payload),
               realized_effective_date(r.disposal_date, r.settlement_date, r.tax_year) AS effective_date
          FROM realized_lots_all r
         WHERE (p_all OR r.is_primary)
           AND CAST(epoch(realized_effective_date(r.disposal_date, r.settlement_date, r.tax_year)) AS BIGINT)
               BETWEEN p_from AND p_to),
    r0 AS (
        SELECT d.* EXCLUDE (effective_date),
               m.missing_basis = 'zero' AND gain_needs_cost(d.realized_gain_loss, d.proceeds, d.book_value) AS assumed_zero,
               d.effective_date, m.missing_basis
          FROM d, (SELECT unnest(p_readings) AS missing_basis) m),
    r AS (
        SELECT r0.* EXCLUDE (effective_date, missing_basis) REPLACE (CASE WHEN r0.assumed_zero THEN 0 ELSE r0.book_value END AS book_value),
               r0.effective_date,
               r0.disposal_date IS NULL AND r0.settlement_date IS NULL AS undated,
               COALESCE(r0.realized_gain_loss,
                        r0.proceeds - CASE WHEN r0.assumed_zero THEN 0 ELSE r0.book_value END
                        + COALESCE(r0.wash_sale_disallowed, 0)) AS gain,
               CASE WHEN r0.realized_gain_loss IS NOT NULL THEN 'stated'
                    WHEN r0.assumed_zero THEN 'assumed_zero'
                    WHEN r0.proceeds IS NOT NULL AND r0.book_value IS NOT NULL THEN 'derived'
                    ELSE 'unknown' END AS gain_origin,
               basis_stamp(r0.basis_origin, r0.basis_method, r0.basis_fees) AS stamp,
               r0.missing_basis
          FROM r0),
    -- One rate per currency, day and output currency, not per lot.
    rates AS (
        SELECT d.currency, d.day, d.out_currency, fx.rate
          FROM (SELECT DISTINCT l.currency, CAST(epoch(l.effective_date) AS BIGINT) // 86400 AS day, c.out_currency
                  FROM d l, (SELECT unnest(p_ccys) AS out_currency) c) d
          ASOF LEFT JOIN fx_rates_into(p_ccys) fx ON fx.to_ccy = d.out_currency AND fx.from_ccy = d.currency
               AND fx.day <= d.day),
    w AS (
        SELECT r.*, CAST(epoch(r.effective_date) AS BIGINT) AS effective_at, x.out_currency, x.rate AS fx_rate
          FROM r JOIN rates x ON x.currency = r.currency AND x.day = CAST(epoch(r.effective_date) AS BIGINT) // 86400)
    SELECT w.* EXCLUDE (missing_basis, out_currency, fx_rate), il.symbol,
           fx_amount(w.proceeds, w.currency, w.out_currency, w.fx_rate) AS proceeds_x,
           fx_amount(w.book_value, w.currency, w.out_currency, w.fx_rate) AS book_x,
           fx_amount(w.gain, w.currency, w.out_currency, w.fx_rate) AS gain_x,
           fx_amount(w.wash_sale_disallowed, w.currency, w.out_currency, w.fx_rate) AS wash_x,
           w.currency <> w.out_currency AND w.fx_rate IS NULL AS fx_missing,
           w.out_currency, w.missing_basis
      FROM w
      LEFT JOIN instrument_labels() il ON il.silver_source_id = w.silver_source_id
           AND il.instrument_external_id = w.instrument_external_id
);

CREATE OR REPLACE MACRO realized_lots_in(p_from, p_to, p_ccy, p_all, p_missing := 'ignore') AS TABLE (
    SELECT * EXCLUDE (out_currency, missing_basis)
      FROM realized_lots_in_all(p_from, p_to, [p_ccy], p_all, [p_missing])
);

-- gains_windows_all: gains_windows (0118) under every currency in
-- p_ccys and reading in p_readings, each row naming its own in
-- out_currency and missing_basis. The buckets, the events, the
-- findings and the accounts a snapshot left out do not depend on
-- either, so they are found once.
CREATE OR REPLACE MACRO gains_windows_all(p_from, p_to, p_period, p_ccys, p_readings) AS TABLE (
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
    lines AS (SELECT * FROM gains_position_lines_all((SELECT l FROM instants), p_ccys, p_readings)),
    -- Presence is the same under every currency and reading.
    lines1 AS (SELECT * FROM lines WHERE out_currency = p_ccys[1] AND missing_basis = p_readings[1]),
    -- What a holding is at an instant, per account and instrument: the
    -- same under every currency and reading, so found once.
    side0 AS (
        SELECT instant, silver_source_id, account_external_id,
               holding_key(instrument_external_id, position_key) AS k,
               min(position_key) AS position_key, any_value(symbol) AS symbol, any_value(name) AS name,
               any_value(asset_class) AS asset_class, any_value(vehicle) AS vehicle,
               -- The native figures need one currency; lines in several
               -- leave them blank and keep the converted ones.
               COUNT(DISTINCT currency) = 1 AS one_ccy,
               CASE WHEN COUNT(DISTINCT currency) = 1 THEN any_value(currency) END AS currency,
               SUM(quantity) AS quantity,
               CASE WHEN COUNT(DISTINCT currency) = 1 THEN SUM(market_value) END AS market_value,
               bool_or(basis_applies) AS applies,
               string_agg(DISTINCT stamp, ',' ORDER BY stamp) AS stamp,
               min(acquisition_date) AS acquisition_date,
               bool_or(basis_method = 'paid_in') AS paid_in,
               SUM(open_lots) AS open_lots,
               bool_or(book_value_known IS NOT NULL) AS rebuilt
          FROM lines1 GROUP BY instant, silver_source_id, account_external_id, k),
    -- What it is worth and cost, per currency and reading.
    sidex AS (
        SELECT instant, out_currency, missing_basis, silver_source_id, account_external_id,
               holding_key(instrument_external_id, position_key) AS k,
               SUM(value_x) AS value_x,
               bool_and(NOT basis_applies OR (book_value IS NOT NULL AND market_value IS NOT NULL)) AS complete,
               SUM(book_value) AS book_native, SUM(book_x) AS book_x,
               SUM(unrealized) AS unrealized_native, SUM(unrealized_x) AS unrealized_x,
               bool_or(fx_missing AND basis_applies) AS unpriced,
               bool_or(fx_missing) AS fx_missing,
               COUNT(*) FILTER (WHERE assumed_zero) AS n_assumed
          FROM lines GROUP BY ALL),
    side AS (
        SELECT x.instant, x.out_currency, x.missing_basis, x.silver_source_id, x.account_external_id, x.k,
               s0.position_key, s0.symbol, s0.name, s0.asset_class, s0.vehicle, s0.currency, s0.quantity,
               s0.market_value, x.value_x, s0.applies, x.complete,
               CASE WHEN s0.one_ccy THEN x.book_native END AS book_value, x.book_x,
               CASE WHEN s0.one_ccy THEN x.unrealized_native END AS unrealized, x.unrealized_x,
               x.unpriced, s0.stamp, s0.acquisition_date, s0.paid_in, x.fx_missing, s0.open_lots,
               x.n_assumed, s0.rebuilt
          FROM sidex x
          JOIN side0 s0 ON s0.instant = x.instant AND s0.silver_source_id = x.silver_source_id
           AND s0.account_external_id = x.account_external_id AND s0.k = x.k),
    en AS (SELECT bk.b_from, s.* FROM side s JOIN bk ON s.instant = bk.b_to),
    st AS (SELECT bk.b_from, s.* FROM side s JOIN bk ON s.instant = bk.b_from - 1),
    pos AS (
        SELECT COALESCE(e.b_from, s.b_from) AS b_from,
               COALESCE(e.out_currency, s.out_currency) AS out_currency,
               COALESCE(e.missing_basis, s.missing_basis) AS missing_basis,
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
               COALESCE(e.fx_missing, FALSE) OR COALESCE(s.fx_missing, FALSE) AS fx_missing,
               COALESCE(e.n_assumed, 0) AS n_assumed, COALESCE(e.rebuilt, FALSE) AS end_rebuilt
          FROM en e FULL JOIN st s ON s.b_from = e.b_from AND s.out_currency = e.out_currency
               AND s.missing_basis = e.missing_basis AND s.silver_source_id = e.silver_source_id
               AND s.account_external_id = e.account_external_id AND s.k = e.k),
    held AS (
        SELECT DISTINCT silver_source_id, account_external_id,
               holding_key(instrument_external_id, position_key) AS k
          FROM positions),
    sold AS (
        SELECT bk.b_from, r.out_currency, r.missing_basis, r.silver_source_id, r.account_external_id, r.k,
               any_value(r.symbol) AS symbol, any_value(r.description) AS description,
               any_value(r.currency) AS currency,
               SUM(r.gain_x) AS realized_x,
               SUM(r.gain_x) FILTER (WHERE r.term = 'short') AS realized_short_x,
               SUM(r.gain_x) FILTER (WHERE r.term = 'long') AS realized_long_x,
               SUM(r.gain_x) FILTER (WHERE r.term IS NULL) AS realized_other_x,
               SUM(r.book_x) FILTER (WHERE r.gain_x IS NOT NULL) AS realized_book_x,
               SUM(r.proceeds_x) AS proceeds_x, SUM(r.wash_x) AS wash_x,
               COUNT(*) AS n_lots,
               COUNT(*) FILTER (WHERE r.gain IS NULL) AS n_without_gain,
               COUNT(*) FILTER (WHERE r.undated) AS n_undated,
               COUNT(*) FILTER (WHERE h.k IS NULL) AS n_unmatched,
               COUNT(*) FILTER (WHERE r.fx_missing) AS n_fx_missing,
               COUNT(*) FILTER (WHERE r.assumed_zero) AS n_assumed,
               COUNT(*) FILTER (WHERE r.document_kind = 'engine') AS n_engine
          FROM (SELECT *, lot_key(instrument_external_id, instrument_hint, description) AS k
                  FROM realized_lots_in_all((SELECT MIN(b_from) FROM bk), p_to, p_ccys, FALSE, p_readings)) r
          JOIN bk ON r.effective_at BETWEEN bk.b_from AND bk.b_to
          LEFT JOIN held h ON h.silver_source_id = r.silver_source_id AND h.account_external_id = r.account_external_id
               AND h.k = r.k
         GROUP BY bk.b_from, r.out_currency, r.missing_basis, r.silver_source_id, r.account_external_id, r.k),
    events AS (
        SELECT bk.b_from, e.silver_source_id, e.account_external_id,
               COALESCE(e.instrument_external_id, '') AS k,
               COUNT(*) FILTER (WHERE e.event = 'sell') AS n_sells,
               COUNT(*) FILTER (WHERE e.event = 'sell' AND NOT e.documented AND NOT e.rebuilt) AS n_undocumented,
               COUNT(*) FILTER (WHERE e.rebuilt) AS n_rebuilt,
               COUNT(*) FILTER (WHERE e.event = 'in_kind') AS n_in_kind,
               COUNT(*) FILTER (WHERE e.event = 'corporate_action') AS n_corporate
          FROM gains_events((SELECT MIN(b_from) FROM bk), p_to) e
          JOIN bk ON e.occurred_at BETWEEN bk.b_from AND bk.b_to
         GROUP BY ALL),
    lot_src AS (SELECT * FROM lot_sources()),
    found AS (
        SELECT bk.b_from, f.silver_source_id, f.account_external_id, f.instrument_external_id AS k,
               COUNT(*) FILTER (WHERE f.finding = 'seed') AS n_seed,
               COUNT(*) FILTER (WHERE f.finding = 'implied') AS n_implied,
               COUNT(*) FILTER (WHERE f.finding = 'blip') AS n_blip,
               COUNT(*) FILTER (WHERE f.finding = 'wash_sale_window') AS n_wash,
               COUNT(*) FILTER (WHERE f.finding = 'fee_unvalued') AS n_fee_unvalued
          FROM lot_findings f
          JOIN lot_src s ON s.silver_source_id = f.silver_source_id AND s.mode = 'fill'
          JOIN bk ON f.found_at BETWEEN bk.b_from AND bk.b_to
         WHERE f.finding IN ('seed', 'implied', 'blip', 'wash_sale_window', 'fee_unvalued')
         GROUP BY ALL),
    -- A seed a stated lot resolved, open at the bucket's end, on the
    -- key's account: the account itself, or its portfolio's pool.
    spliced AS (
        SELECT DISTINCT bk.b_from, l.silver_source_id, l.account_external_id, l.instrument_external_id AS k
          FROM lots l JOIN bk ON l.opened_at <= bk.b_to AND (l.closed_at IS NULL OR l.closed_at > bk.b_to)
         WHERE l.cost_origin = 'resolved'),
    acct_first AS (
        SELECT silver_source_id, account_external_id, MIN(snapshot_at) AS first_at FROM positions GROUP BY ALL),
    acct_at AS (SELECT DISTINCT instant, silver_source_id, account_external_id FROM lines1),
    src_at AS (SELECT DISTINCT instant, silver_source_id FROM lines1),
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
           AND NOT EXISTS (SELECT 1 FROM sold l WHERE l.b_from = bk.b_from
                              AND l.silver_source_id = x.silver_source_id AND l.account_external_id = x.account_external_id)
           AND NOT EXISTS (SELECT 1 FROM events e WHERE e.b_from = bk.b_from
                              AND e.silver_source_id = x.silver_source_id AND e.account_external_id = x.account_external_id)),
    src_first AS (SELECT silver_source_id, MIN(snapshot_at) AS first_at FROM positions GROUP BY 1),
    -- Every currency and reading holds the same keys: the lines and lots
    -- repeat under each. What does not depend on either joins the keys
    -- once (base); the keys then repeat under each.
    keys0 AS (
        SELECT b_from, silver_source_id, account_external_id, k FROM pos
        UNION SELECT b_from, silver_source_id, account_external_id, k FROM sold
        UNION SELECT b_from, silver_source_id, account_external_id, k FROM events
        UNION SELECT b_from, silver_source_id, account_external_id, k FROM found),
    base AS (
        SELECT k.b_from, k.silver_source_id, k.account_external_id, k.k, bk.b_to,
               COALESCE(m.pid, '') AS portfolio_bucket, a.display_name, a.nickname,
               il.symbol AS label_symbol, il.name AS label_name,
               COALESCE(ev.n_sells, 0) AS n_sells, COALESCE(ev.n_undocumented, 0) AS n_undocumented,
               COALESCE(ev.n_in_kind, 0) AS n_in_kind, COALESCE(ev.n_corporate, 0) AS n_corporate,
               COALESCE(ev.n_rebuilt, 0) AS n_rebuilt,
               u.account_external_id IS NOT NULL AS account_unobserved,
               sf.first_at,
               COALESCE(fd.n_seed, 0) AS n_seed, COALESCE(fd.n_implied, 0) AS n_implied,
               COALESCE(fd.n_blip, 0) AS n_blip, COALESCE(fd.n_wash, 0) AS n_wash,
               COALESCE(fd.n_fee_unvalued, 0) AS n_fee_unvalued,
               sp.k IS NOT NULL AS has_spliced, ls.pooled AS src_pooled, ls.dated_by_settlement
          FROM keys0 k
          JOIN bk ON bk.b_from = k.b_from
          LEFT JOIN events ev ON ev.b_from = k.b_from AND ev.silver_source_id = k.silver_source_id
               AND ev.account_external_id = k.account_external_id AND ev.k = k.k
          LEFT JOIN found fd ON fd.b_from = k.b_from AND fd.silver_source_id = k.silver_source_id
               AND fd.account_external_id = k.account_external_id AND fd.k = k.k
          LEFT JOIN unobserved u ON u.b_from = k.b_from AND u.silver_source_id = k.silver_source_id
               AND u.account_external_id = k.account_external_id
          LEFT JOIN portfolio_acct_map() m ON m.src = k.silver_source_id AND m.acct = k.account_external_id
          LEFT JOIN accounts a ON a.silver_source_id = k.silver_source_id AND a.account_external_id = k.account_external_id
          LEFT JOIN src_first sf ON sf.silver_source_id = k.silver_source_id
          LEFT JOIN lot_src ls ON ls.silver_source_id = k.silver_source_id
          LEFT JOIN lot_key_accounts() ka ON ka.src = k.silver_source_id AND ka.acct = k.account_external_id
          LEFT JOIN spliced sp ON sp.b_from = k.b_from AND sp.silver_source_id = k.silver_source_id AND sp.k = k.k
               AND sp.account_external_id = COALESCE(ka.key_account, k.account_external_id)
          LEFT JOIN instrument_labels() il ON il.silver_source_id = k.silver_source_id AND il.instrument_external_id = k.k),
    keys AS (
        SELECT b.*, c.out_currency, r.missing_basis
          FROM base b, (SELECT unnest(p_ccys) AS out_currency) c, (SELECT unnest(p_readings) AS missing_basis) r)
    SELECT k.b_from, k.silver_source_id, k.account_external_id, k.k,
           k.portfolio_bucket, k.display_name, k.nickname,
           p.end_key, p.start_key,
           COALESCE(p.symbol, l.symbol, k.label_symbol) AS symbol, COALESCE(p.name, l.description, k.label_name) AS name,
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
           l.realized_x, l.realized_short_x, l.realized_long_x, l.realized_other_x, l.realized_book_x,
           l.proceeds_x, l.wash_x,
           COALESCE(l.n_lots, 0) AS n_lots, COALESCE(l.n_without_gain, 0) AS n_without_gain,
           COALESCE(l.n_undated, 0) AS n_undated, COALESCE(l.n_unmatched, 0) AS n_unmatched,
           k.n_sells, k.n_undocumented, k.n_in_kind, k.n_corporate,
           k.account_unobserved,
           CASE WHEN COALESCE(p.at_end, FALSE) AND k.first_at BETWEEN k.b_from AND k.b_to
                THEN k.silver_source_id END AS onboarded_source,
           CAST(COALESCE(p.fx_missing, FALSE) AS INTEGER) + COALESCE(l.n_fx_missing, 0) AS n_fx_missing,
           k.n_rebuilt, k.n_seed, k.n_implied, k.n_blip, k.n_wash, k.n_fee_unvalued,
           COALESCE(p.end_rebuilt, FALSE) AND k.has_spliced AS spliced,
           COALESCE(p.end_rebuilt AND k.src_pooled, FALSE) AS pooled,
           COALESCE(l.n_engine > 0 AND k.dated_by_settlement, FALSE) AS settlement,
           COALESCE(p.n_assumed, 0) + COALESCE(l.n_assumed, 0) AS n_assumed_zero,
           k.out_currency, k.missing_basis
      FROM keys k
      LEFT JOIN pos p ON p.b_from = k.b_from AND p.out_currency = k.out_currency AND p.missing_basis = k.missing_basis
           AND p.silver_source_id = k.silver_source_id AND p.account_external_id = k.account_external_id AND p.k = k.k
      LEFT JOIN sold l ON l.b_from = k.b_from AND l.out_currency = k.out_currency AND l.missing_basis = k.missing_basis
           AND l.silver_source_id = k.silver_source_id AND l.account_external_id = k.account_external_id AND l.k = k.k
);

CREATE OR REPLACE MACRO gains_windows(p_from, p_to, p_ccy, p_period, p_missing := 'ignore') AS TABLE (
    SELECT * EXCLUDE (out_currency, missing_basis)
      FROM gains_windows_all(p_from, p_to, p_period, [p_ccy], [p_missing])
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (119, CAST(epoch(now()) AS BIGINT));
