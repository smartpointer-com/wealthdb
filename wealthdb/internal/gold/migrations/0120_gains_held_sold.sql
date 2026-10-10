-- ============================================================
-- gold schema, migration 0120 — a bucket's gain split by what prices
-- did, for the Gains dashboard.
--
-- gain = realized + unrealized change holds in every bucket, but a sale
-- realizes the whole gain since purchase, most of it built up in
-- earlier buckets, and the same amount leaves the unrealized change: a
-- sale shows as two opposite bars. gains_windows_all, 0119's re-issued
-- here, adds the split that does not:
--
--   held_change_x  the unrealized change with the lots sold in the
--                  bucket left out
--   sold_gain_x    what those lots gained in the bucket: from its start,
--                  or their purchase in it, to the sale
--
-- Summed over rows they add up to realized_x + unrealized_change_x
-- (docs/GAINS.md §2). The split depends on the bucket, so twelve
-- monthly splits do not add up to the year's; their total does.
-- report_gains and web_gains carry both. The CLI keeps realized and
-- unrealized change, which statements reconcile to.
--
-- OR REPLACE and IF NOT EXISTS keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

-- instrument_aliases: the instrument a source keys by its CUSIP (alias)
-- and the ticker it also keys it by (k), where its instruments carry
-- both rows with one symbol: a statement names a security by CUSIP and
-- the trades and snapshots by ticker. The lot pass reads it too
-- (lotAliases).
CREATE OR REPLACE MACRO instrument_aliases() AS TABLE (
    SELECT c.silver_source_id AS src, c.instrument_external_id AS alias, c.symbol AS k
      FROM instruments c
      JOIN instruments t ON t.silver_source_id = c.silver_source_id AND t.instrument_external_id = c.symbol
     WHERE c.cusip = c.instrument_external_id AND c.symbol <> c.instrument_external_id
);

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
               COUNT(*) FILTER (WHERE assumed_zero) AS n_assumed,
               any_value(rate) AS any_rate
          FROM lines GROUP BY ALL),
    side AS (
        SELECT x.instant, x.out_currency, x.missing_basis, x.silver_source_id, x.account_external_id, x.k,
               s0.position_key, s0.symbol, s0.name, s0.asset_class, s0.vehicle, s0.currency, s0.quantity,
               s0.market_value, x.value_x, s0.applies, x.complete,
               CASE WHEN s0.one_ccy THEN x.book_native END AS book_value, x.book_x,
               CASE WHEN s0.one_ccy THEN x.unrealized_native END AS unrealized, x.unrealized_x,
               x.unpriced, s0.stamp, s0.acquisition_date, s0.paid_in, x.fx_missing, s0.open_lots,
               x.n_assumed, s0.rebuilt, CASE WHEN s0.one_ccy THEN x.any_rate END AS rate
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
               COALESCE(e.n_assumed, 0) AS n_assumed, COALESCE(e.rebuilt, FALSE) AS end_rebuilt,
               s.currency AS start_currency, s.rate AS start_rate,
               CASE WHEN s.complete THEN s.unrealized + s.book_value END AS start_clean
          FROM en e FULL JOIN st s ON s.b_from = e.b_from AND s.out_currency = e.out_currency
               AND s.missing_basis = e.missing_basis AND s.silver_source_id = e.silver_source_id
               AND s.account_external_id = e.account_external_id AND s.k = e.k),
    -- Each instrument's clean price per unit at a bucket's start, in a
    -- source that held it then, where every row of it states its
    -- unrealized change: a sold lot's start value is its quantity at
    -- this price.
    prices AS (
        SELECT b_from, out_currency, missing_basis, silver_source_id, k,
               any_value(start_currency) AS currency, any_value(start_rate) AS rate,
               SUM(quantity_start)::DOUBLE AS quantity,
               SUM(start_clean)::DOUBLE / SUM(quantity_start)::DOUBLE AS unit_clean
          FROM pos
         WHERE quantity_start > 0
         GROUP BY ALL
        HAVING bool_and(NOT change_unknown AND start_clean IS NOT NULL AND start_rate IS NOT NULL)
           AND COUNT(DISTINCT start_currency) = 1),
    held AS (
        SELECT DISTINCT silver_source_id, account_external_id,
               holding_key(instrument_external_id, position_key) AS k
          FROM positions),
    sold AS (
        SELECT r.b_from, r.out_currency, r.missing_basis, r.silver_source_id, r.account_external_id, r.k,
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
               COUNT(*) FILTER (WHERE r.document_kind = 'engine') AS n_engine,
               SUM(r.released_x) AS released_x
          FROM (
              -- released_x: what a lot sold in the bucket had gained by
              -- its start, if it was held then (an undated lot counts
              -- as held): its quantity at the start price, less its
              -- cost, at the start's rate. Lots that sell more than the
              -- source held at the start release in proportion. A
              -- document's CUSIP takes the price of the ticker it stands
              -- for (instrument_aliases).
              SELECT y.* EXCLUDE (release, quantity_start),
                     y.release * least(1, y.quantity_start / SUM(y.quantity) FILTER (WHERE y.release IS NOT NULL) OVER (
                         PARTITION BY y.b_from, y.out_currency, y.missing_basis, y.silver_source_id, y.pk)) AS released_x
                FROM (
                  SELECT x.*, bk.b_from, bk.b_to, pr.k AS pk, pr.quantity AS quantity_start,
                         CASE WHEN (x.acquisition_date IS NULL OR x.acquisition_date < CAST(epoch_ms(bk.b_from * 1000) AS DATE))
                                   AND x.book_value IS NOT NULL AND x.currency = pr.currency
                              THEN (x.quantity::DOUBLE * pr.unit_clean - x.book_value::DOUBLE) * pr.rate END AS release
                    FROM (SELECT *, lot_key(instrument_external_id, instrument_hint, description) AS k
                            FROM realized_lots_in_all((SELECT MIN(b_from) FROM bk), p_to, p_ccys, FALSE, p_readings)) x
                    JOIN bk ON x.effective_at BETWEEN bk.b_from AND bk.b_to
                    LEFT JOIN instrument_aliases() al ON al.src = x.silver_source_id AND al.alias = x.k
                    LEFT JOIN prices pr ON pr.b_from = bk.b_from AND pr.out_currency = x.out_currency
                         AND pr.missing_basis = x.missing_basis AND pr.silver_source_id = x.silver_source_id
                         AND pr.k = COALESCE(al.k, x.k)) y) r
          LEFT JOIN held h ON h.silver_source_id = r.silver_source_id AND h.account_external_id = r.account_external_id
               AND h.k = r.k
         GROUP BY r.b_from, r.out_currency, r.missing_basis, r.silver_source_id, r.account_external_id, r.k),
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
          FROM base b, (SELECT unnest(p_ccys) AS out_currency) c, (SELECT unnest(p_readings) AS missing_basis) r),
    split AS (
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
           COALESCE(l.released_x, 0) AS released_x,
           k.out_currency, k.missing_basis
      FROM keys k
      LEFT JOIN pos p ON p.b_from = k.b_from AND p.out_currency = k.out_currency AND p.missing_basis = k.missing_basis
           AND p.silver_source_id = k.silver_source_id AND p.account_external_id = k.account_external_id AND p.k = k.k
      LEFT JOIN sold l ON l.b_from = k.b_from AND l.out_currency = k.out_currency AND l.missing_basis = k.missing_basis
           AND l.silver_source_id = k.silver_source_id AND l.account_external_id = k.account_external_id AND l.k = k.k)
    -- The bucket's gain split by what prices did. held_change_x is the
    -- unrealized change with the lots sold in the bucket left out: their
    -- start gain (released_x) goes back in. sold_gain_x is what those
    -- lots gained in the bucket, from its start (or their purchase in
    -- it) to the sale. A sale's lots and its position can sit on two
    -- rows (a document's CUSIP, a pooled portfolio's wallets), so the
    -- release lands on the lots' row; summed over rows the two add up
    -- to realized_x + unrealized_change_x.
    SELECT s.* EXCLUDE (released_x, out_currency, missing_basis),
           CASE WHEN s.unrealized_change_x IS NOT NULL OR s.released_x <> 0
                THEN COALESCE(s.unrealized_change_x, 0) + s.released_x END AS held_change_x,
           CASE WHEN s.realized_x IS NOT NULL OR s.released_x <> 0
                THEN COALESCE(s.realized_x, 0) - s.released_x END AS sold_gain_x,
           s.out_currency, s.missing_basis
      FROM split s
);

ALTER TABLE report_gains ADD COLUMN IF NOT EXISTS held_change_x DOUBLE;
ALTER TABLE report_gains ADD COLUMN IF NOT EXISTS sold_gain_x   DOUBLE;

-- web_gains: 0118's, with the split.
CREATE OR REPLACE VIEW web_gains AS
    SELECT g.currency, g.missing_basis, date_trunc('month', epoch_ms(g.period_start * 1000)) AS period,
           g.silver_source_id, g.account_external_id,
           account_label(a.display_name, g.account_external_id,
                         g.silver_source_id, a.account_kind) AS account,
           a.account_kind, a.tax_wrapper,
           g.k AS instrument_key, g.symbol, g.name, g.asset_class, g.vehicle,
           g.book_x AS cost_basis, g.value_x AS value,
           g.unrealized_start_x AS unrealized_start, g.unrealized_end_x AS unrealized_end,
           g.unrealized_change_x AS unrealized_change,
           g.realized_x AS realized, g.realized_short_x AS realized_short,
           g.realized_long_x AS realized_long, g.realized_other_x AS realized_other,
           g.realized_book_x AS realized_cost_basis,
           g.proceeds_x AS proceeds, g.wash_x AS wash_disallowed,
           g.at_end, g.end_applies, g.end_without_basis, g.end_unpriced,
           g.basis_changed, g.paid_in, g.account_unobserved, g.onboarded_source,
           g.n_lots AS realized_lots, g.n_without_gain AS lots_without_gain,
           g.n_undated AS undated_lots, g.n_sells AS sells,
           g.n_undocumented AS sells_without_documents, g.n_in_kind AS in_kind_moves,
           g.n_corporate AS corporate_actions, g.n_fx_missing AS fx_missing,
           g.n_rebuilt AS sells_rebuilt, g.n_seed AS seed_lots, g.n_implied AS implied_disposals,
           g.n_blip AS snapshot_blips, g.n_wash AS wash_sales_not_applied, g.n_fee_unvalued AS fee_unvalued,
           g.n_assumed_zero AS basis_assumed_zero, g.spliced, g.pooled, g.settlement AS dated_by_settlement,
           g.held_change_x AS held_change, g.sold_gain_x AS sold_gain
      FROM report_gains g
      LEFT JOIN accounts a ON a.silver_source_id = g.silver_source_id
           AND a.account_external_id = g.account_external_id;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (120, CAST(epoch(now()) AS BIGINT));
