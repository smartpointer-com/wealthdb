-- The Gains dashboard: gains_windows' monthly rows, materialized per
-- reporting currency, and the view Metabase reads them through.
--
-- gains_windows: re-issued from 0116 verbatim but for one column.
-- realized_book_x is the cost basis of the bucket's primary lots whose
-- gain is known, at the lots' dates: the denominator of a realized
-- percent. A lot with a gain but no cost basis adds to realized_x and
-- not to it.

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
               SUM(r.book_x) FILTER (WHERE r.gain_x IS NOT NULL) AS realized_book_x,
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
           l.realized_x, l.realized_short_x, l.realized_long_x, l.realized_other_x, l.realized_book_x,
           l.proceeds_x, l.wash_x,
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

-- report_gains: gains_windows(0, now, CCY, 'month') for every reporting
-- currency, in the output currency. `wealthdb web-materialize`
-- (MaterializeGains, gains_materialize.go) rewrites it whole at every
-- web refresh, as it does report_returns: one dashboard open fires a
-- dozen tiles, and each would otherwise compute the whole history.
--
-- Contract: each currency's rows are gains_windows' rows verbatim, less
-- the figures in the holding's own currency, so the rows summed per
-- month equal `wealthdb gains summary --period monthly -x CCY` over the
-- same months.
CREATE TABLE IF NOT EXISTS report_gains (
    computed_at         BIGINT  NOT NULL,  -- epoch seconds, one per run
    currency            TEXT    NOT NULL,  -- the output currency
    period_start        BIGINT  NOT NULL,  -- the month's first second (gains_windows' b_from)
    silver_source_id    TEXT    NOT NULL,
    account_external_id TEXT    NOT NULL,
    k                   TEXT    NOT NULL,  -- the instrument key the row's inputs meet on
    symbol              TEXT,
    name                TEXT,
    asset_class         TEXT,
    vehicle             TEXT,
    book_x              DOUBLE,            -- at the month's end
    value_x             DOUBLE,
    unrealized_start_x  DOUBLE,
    unrealized_end_x    DOUBLE,
    unrealized_change_x DOUBLE,
    realized_x          DOUBLE,
    realized_short_x    DOUBLE,
    realized_long_x     DOUBLE,
    realized_other_x    DOUBLE,
    realized_book_x     DOUBLE,
    proceeds_x          DOUBLE,
    wash_x              DOUBLE,
    at_end              BOOLEAN NOT NULL,
    end_applies         BOOLEAN NOT NULL,
    end_without_basis   BOOLEAN NOT NULL,
    end_unpriced        BOOLEAN NOT NULL,
    basis_changed       BOOLEAN NOT NULL,
    paid_in             BOOLEAN NOT NULL,
    account_unobserved  BOOLEAN NOT NULL,
    onboarded_source    TEXT,
    n_lots              BIGINT  NOT NULL,
    n_without_gain      BIGINT  NOT NULL,
    n_undated           BIGINT  NOT NULL,
    n_sells             BIGINT  NOT NULL,
    n_undocumented      BIGINT  NOT NULL,
    n_in_kind           BIGINT  NOT NULL,
    n_corporate         BIGINT  NOT NULL,
    n_fx_missing        BIGINT  NOT NULL
);

-- web_gains: report_gains for the Gains dashboard. `period` is the
-- month as a zone-free timestamp (the 0049 rule); the first bucket
-- opens at the first data, so it is truncated to its month. The account
-- is labelled through the shared helpers (0063), and the account's kind
-- and tax wrapper join in for the pickers. The figures take the names
-- the gains command prints.
CREATE OR REPLACE VIEW web_gains AS
    SELECT g.currency, date_trunc('month', epoch_ms(g.period_start * 1000)) AS period,
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
           g.n_corporate AS corporate_actions, g.n_fx_missing AS fx_missing
      FROM report_gains g
      LEFT JOIN accounts a ON a.silver_source_id = g.silver_source_id
           AND a.account_external_id = g.account_external_id;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (117, CAST(epoch(now()) AS BIGINT));
