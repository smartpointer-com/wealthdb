-- Presentation: the labels and the web_* views the dashboards read.

-- An account label that says whose account it is, and what kind.
--
-- An account's own name is what the institution calls it and nothing
-- more — a product nickname, a masked card number, an IBAN. Read on a
-- chart, a bar labelled by it alone does not say which institution it
-- belongs to, nor whether the money left a deposit account or a credit
-- card; and product nicknames are generic, so two institutions'
-- accounts can wear the same one. An account with no name would
-- collapse with the other unnamed ones into one "null" bar.
--
-- Two scalar macros, so the rule has ONE home rather than a copy in
-- each view:
--
--   * account_display_name is the fallback: an account with no name of
--     its own is labelled by its id, the way the CLI's `account` column
--     does it.
--   * account_label annotates that with the source and the kind, as
--     `<name> (<source> <kind>)`.
--
-- Name FIRST. A row chart truncates a long label from the end, so
-- putting the identity in front means what gets clipped in a narrow
-- tile is the annotation, not the thing being labelled.
--
-- Both views keep `display_name` and `account_external_id` beside the
-- label, so a consumer that wants the institution's own name, or the
-- id, still reads it off its own column and nothing has to parse the
-- label back apart.
CREATE MACRO account_display_name(nm, ext_id) AS
    COALESCE(NULLIF(TRIM(nm), ''), ext_id);

CREATE MACRO account_label(nm, ext_id, src, kind) AS
    account_display_name(nm, ext_id)
        || ' (' || src || ' ' || COALESCE(kind, 'unknown') || ')';

-- holdings_lines_latest: the same lines as of each source's latest
-- snapshot, the snapshot report_sources_multi dates the source by.
CREATE VIEW holdings_lines_latest AS
    WITH l AS (
        SELECT p.silver_source_id,
               COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
               'position' AS line_kind, p.asset_class,
               p.value_usd, p.value_chf, p.value_eur, p.value_gbp
          FROM serving_positions_latest p
          LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id
               AND a.account_external_id = p.account_external_id
        UNION ALL
        SELECT silver_source_id, tax_wrapper, 'cash', 'cash',
               cash_balance_usd, cash_balance_chf, cash_balance_eur, cash_balance_gbp
          FROM serving_accounts_latest)
    SELECT s.snapshot_at, l.*
      FROM l
      JOIN (SELECT silver_source_id, MAX(snapshot_at) AS snapshot_at
              FROM serving_accounts_latest GROUP BY 1) s USING (silver_source_id);

CREATE VIEW web_sources_latest AS
    SELECT epoch_ms(snapshot_at * 1000) AS snapshot_at,
           silver_source_id, tax_wrapper, asset_class,
           SUM(value_usd) FILTER (WHERE line_kind = 'position') AS positions_value_usd,
           SUM(value_usd) FILTER (WHERE line_kind = 'cash') AS cash_balance_usd,
           SUM(value_usd) AS total_value_usd,
           SUM(value_chf) FILTER (WHERE line_kind = 'position') AS positions_value_chf,
           SUM(value_chf) FILTER (WHERE line_kind = 'cash') AS cash_balance_chf,
           SUM(value_chf) AS total_value_chf,
           SUM(value_eur) FILTER (WHERE line_kind = 'position') AS positions_value_eur,
           SUM(value_eur) FILTER (WHERE line_kind = 'cash') AS cash_balance_eur,
           SUM(value_eur) AS total_value_eur,
           SUM(value_gbp) FILTER (WHERE line_kind = 'position') AS positions_value_gbp,
           SUM(value_gbp) FILTER (WHERE line_kind = 'cash') AS cash_balance_gbp,
           SUM(value_gbp) AS total_value_gbp
      FROM holdings_lines_latest
     GROUP BY 1, 2, 3, 4;

CREATE VIEW web_card_balances_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, 'card') AS account_label,
           currency,
           balance, balance_usd, balance_chf, balance_eur, balance_gbp
      FROM serving_card_balances_history;

CREATE VIEW web_income AS
    SELECT epoch_ms(i.occurred_at * 1000) AS occurred_at,
           i.silver_source_id, i.account_external_id,
           account_display_name(i.display_name, i.account_external_id) AS display_name,
           account_label(i.display_name, i.account_external_id,
                         i.silver_source_id, i.account_kind) AS account_label,
           i.account_kind,
           COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
           i.payer_name,
           COALESCE(i.income_primary,  '(uncategorized)') AS income_primary,
           COALESCE(i.income_detailed, '(uncategorized)') AS income_detailed,
           COALESCE(i.income_primary_label, '(uncategorized)') AS income_primary_label,
           COALESCE(i.income_label,         '(uncategorized)') AS income_label,
           i.provider_income_label,
           i.value_usd, i.value_chf, i.value_eur, i.value_gbp
      FROM serving_income_transactions i
      LEFT JOIN accounts a ON a.silver_source_id = i.silver_source_id
           AND a.account_external_id = i.account_external_id;

CREATE VIEW web_spending AS
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, account_kind) AS account_label,
           account_kind,
           merchant_name,
           COALESCE(spend_primary,  '(uncategorized)') AS spend_primary,
           COALESCE(spend_detailed, '(uncategorized)') AS spend_detailed,
           COALESCE(spend_primary_label, '(uncategorized)') AS spend_primary_label,
           COALESCE(spend_label,         '(uncategorized)') AS spend_label,
           provider_spend_label,
           value_usd, value_chf, value_eur, value_gbp
      FROM serving_spending_transactions;

CREATE VIEW web_cashflow AS
    WITH lines AS (
        SELECT t.*,
               CASE WHEN t.section = 'operating_in'  AND (t.grp = '(uncategorized)' OR sg.family = 'both')
                        THEN t.group_label || ' in'
                    WHEN t.section = 'operating_out' AND (t.grp = '(uncategorized)' OR sg.family = 'both')
                        THEN t.group_label || ' out'
                    ELSE t.group_label END AS group_node,
               CASE WHEN t.section = 'operating_in'  AND t.class = '(uncategorized)'
                        THEN t.class_label || ' in'
                    WHEN t.section = 'operating_out' AND t.class = '(uncategorized)'
                        THEN t.class_label || ' out'
                    ELSE t.class_label END AS class_node
          FROM serving_cashflow_transactions t
          LEFT JOIN spend_categories sg ON sg.spend_detailed = t.grp
    )
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, account_kind) AS account_label,
           account_kind, kind,
           section, class, class_label, class_node,
           grp, group_label, group_node,
           name,
           value_usd, value_chf, value_eur, value_gbp
      FROM lines;

-- web_exposure: the exposure report's rows for the dashboards, a
-- missing wrapper read as taxable_personal.
CREATE VIEW web_exposure AS
SELECT CAST(epoch_ms(r.as_of * 1000) AS DATE) AS as_of_day,
       r.currency,
       r.silver_source_id,
       COALESCE(r.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       r.asset_class,
       COALESCE(r.sector, 'unknown') AS sector,
       CASE WHEN r.country IS NOT NULL THEN r.country
            WHEN r.asset_class IN ('metal', 'crypto') THEN '(none)' ELSE 'unknown' END AS country,
       co.name AS country_name,
       COALESCE(r.region, CASE WHEN r.asset_class IN ('metal', 'crypto') THEN '(none)' ELSE 'unknown' END)
           AS region,
       COALESCE(r.exposure_currency, 'unknown') AS exposure_currency,
       r.currency_kind,
       CASE WHEN r.asset_class <> 'public_equity' THEN '(none)' ELSE COALESCE(r.size, 'unknown') END
           AS size,
       CASE WHEN r.via_funds THEN 'via funds' ELSE 'direct' END AS held,
       r.how,
       COALESCE(r.situs, 'unknown') AS situs,
       COALESCE(r.situs_basis, 'unknown') AS situs_basis,
       r.value
  FROM report_exposure r
  LEFT JOIN exposure_countries co ON co.code = r.country;

-- The dashboards' views. A holding's labels are its row's in the
-- positions of today; an account's income no holding names (rent, a
-- pension: `income:<type>:<CCY>`) reads its account's. A day is a DATE; a rate
-- prints as `_pct`; money is in the holding's currency with fx_<ccy> to
-- each reporting currency, or a row filter on `currency`.
CREATE VIEW web_yield AS
SELECT CAST(epoch_ms(p.as_of * 1000) AS DATE) AS as_of_day, p.as_of = (SELECT max(as_of) FROM report_yield_positions)
           AS today,
       p.currency, p.silver_source_id, p.account_external_id, p.position_key,
       account_label(a.display_name, p.account_external_id, p.silver_source_id, a.account_kind) AS account_label,
       COALESCE(p.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       COALESCE(p.asset_class, 'unknown') AS asset_class, p.vehicle, p.kind, p.symbol, p.name, p.holding_currency,
       p.quantity, p.price, p.value, p.coupon_pct, p.maturity_date, p.rating, p.stated_pct, p.current_yield_pct,
       p.implied_pct, p.implied_annual_pct, p.realized_pct, p.received_12m, p.mean_value, p.earnings_yield_pct,
       p.dividend_yield_pct, p.payout_pct, p.benchmark_pct, p.gap_pct, p.duration, p.convexity, p.maturity_years,
       p.dv01, p.income_12m, p.income_100bp, p.weighs, p.tags
  FROM report_yield_positions p
  LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id AND a.account_external_id = p.account_external_id;

CREATE VIEW web_yield_curve AS
SELECT CAST(epoch_ms(b.as_of * 1000) AS DATE) AS as_of_day, b.currency, b.silver_source_id,
       COALESCE(p.tax_wrapper, 'taxable_personal') AS tax_wrapper, COALESCE(p.asset_class, 'unknown') AS asset_class,
       b.curve_currency, b.tenor, b.tenor_order,
       -- 'funds', 'bonds', 'cash' or 'liabilities'
       b.held_in, b.dv01
  FROM report_yield_buckets b
  LEFT JOIN report_yield_positions p ON p.as_of = b.as_of AND p.currency = b.currency
       AND p.silver_source_id = b.silver_source_id AND p.account_external_id = b.account_external_id
       AND p.position_key = b.position_key;

-- web_yield_holdings: the attributes of each holding of today's books,
-- once, from the first reporting currency whose book holds it.
CREATE VIEW web_yield_holdings AS
SELECT p.silver_source_id, p.account_external_id, p.position_key,
       account_label(a.display_name, p.account_external_id, p.silver_source_id, a.account_kind) AS account_label,
       COALESCE(p.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       COALESCE(p.asset_class, 'unknown') AS asset_class, p.vehicle, p.kind, p.symbol, p.name, p.quantity, p.price
  FROM report_yield_positions p
  LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id AND a.account_external_id = p.account_external_id
 WHERE p.as_of = (SELECT max(as_of) FROM report_yield_positions)
QUALIFY row_number() OVER (PARTITION BY p.silver_source_id, p.account_external_id, p.position_key
                           ORDER BY list_position(['USD', 'CHF', 'EUR', 'GBP'], p.currency)) = 1;

CREATE VIEW web_yield_parts AS
SELECT x.*, COALESCE(h.account_label,
                     account_label(a.display_name, x.account_external_id, x.silver_source_id, a.account_kind))
           AS account_label,
       COALESCE(h.tax_wrapper, a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       COALESCE(h.asset_class, 'income') AS asset_class, h.vehicle, h.kind, h.symbol, h.name
  FROM report_yield_parts x
  LEFT JOIN web_yield_holdings h ON h.silver_source_id = x.silver_source_id
       AND h.account_external_id = x.account_external_id AND h.position_key = x.position_key
  LEFT JOIN accounts a ON a.silver_source_id = x.silver_source_id AND a.account_external_id = x.account_external_id;

CREATE VIEW web_yield_events AS
SELECT x.*, h.account_label, COALESCE(h.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       COALESCE(h.asset_class, 'unknown') AS asset_class, h.vehicle, h.symbol, h.name
  FROM report_yield_events x
  LEFT JOIN web_yield_holdings h ON h.silver_source_id = x.silver_source_id
       AND h.account_external_id = x.account_external_id AND h.position_key = x.position_key;

CREATE VIEW web_yield_rates AS SELECT currency, month, path, rate_pct FROM report_yield_rates;

CREATE VIEW web_yield_received AS
SELECT x.currency, x.silver_source_id, x.account_external_id, x.position_key, x.day, x.type, x.value, x.withheld,
       COALESCE(h.account_label,
                account_label(a.display_name, x.account_external_id, x.silver_source_id, a.account_kind))
           AS account_label,
       COALESCE(h.tax_wrapper, a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       COALESCE(h.asset_class, CASE WHEN starts_with(x.position_key, 'income:') THEN 'income' ELSE 'unknown' END)
           AS asset_class, h.vehicle, h.symbol, h.name
  FROM report_yield_received x
  LEFT JOIN web_yield_holdings h ON h.silver_source_id = x.silver_source_id
       AND h.account_external_id = x.account_external_id AND h.position_key = x.position_key
  LEFT JOIN accounts a ON a.silver_source_id = x.silver_source_id AND a.account_external_id = x.account_external_id;

CREATE VIEW web_exposure_debt AS
SELECT CAST(epoch_ms(as_of * 1000) AS DATE) AS as_of_day, currency, silver_source_id,
       COALESCE(tax_wrapper, 'taxable_personal') AS tax_wrapper, dimension, bucket, unknown, value
  FROM report_exposure_debt;

CREATE VIEW web_exposure_issuers AS
SELECT CAST(epoch_ms(as_of * 1000) AS DATE) AS as_of_day, currency, rank, issuer, lei, value, share_pct,
       cumulative_share_pct, equity_pct, debt_pct, cash_pct
  FROM report_exposure_issuer_ranks;

-- ============================================================
-- The days the Income dashboard's received figures cover, and the tax withheld at source on them
-- (web/DESIGN.md §6).
--
-- The Income dashboard's cards of the holdings' income put what was
-- received over the Time range picker beside what is expected over the
-- Horizon. A card reads the income received and the tax withheld from
-- two views, and a picker reaches one template tag of a card. So the
-- Time range reaches both through a third view of days:
--
--   * web_income_withheld: the tax withheld at source on the income's
--     accounts, one row per tax line, its value in each reporting
--     currency as a positive amount. These are the lines the income
--     summary's `withheld` memo reads (income_withheld_outccy): every negative
--     `tax` row on those accounts. It is what a source booked, not the
--     withholding table's model.
--   * web_income_days: each day to today from the earliest of the first
--     day income was received (report_yield_received, which holds the
--     income lines of the six types the projection counts,
--     gold.MaterializeYield), the first day tax was
--     withheld, and the first day of last year. Today is the day of the
--     yield report's latest instant. A card keeps the received rows and
--     the withheld tax of the days the picker selects, and counts the
--     months of the selection by those days.
-- ============================================================

CREATE VIEW web_income_withheld AS
SELECT t.silver_source_id, t.account_external_id,
       account_label(a.display_name, t.account_external_id, t.silver_source_id, a.account_kind) AS account_label,
       CAST(epoch_ms(t.occurred_at * 1000) AS DATE) AS day,
       -t.value_usd AS value_usd, -t.value_chf AS value_chf, -t.value_eur AS value_eur, -t.value_gbp AS value_gbp
  FROM serving_transactions t
  JOIN income_scoped_accounts() a
    ON a.silver_source_id = t.silver_source_id AND a.account_external_id = t.account_external_id
 WHERE t.kind = 'tax' AND t.net_amount < 0;

CREATE VIEW web_cashflow_pool AS
SELECT silver_source_id, account_external_id FROM cashflow_pool_accounts();

-- ============================================================
-- The size of a share held directly,
-- for the dashboards' position lists.
--
-- web_positions_history labels a position by symbol, name, asset class
-- and vehicle, and carries no security key. web_position_sizes gives
-- each such label of a public-equity stock its share's size (large,
-- mid, small) from securities, as the exposure leaves take it. A
-- position's security key is the one exposure_holdings reads: the
-- holding alias, else the instrument alias, else the instrument itself,
-- through instrument_keys and exposure_key. A key the look-through
-- expands, as a fund's, is no share and has no size. A label over its
-- whole history may name several securities (a new ISIN after a
-- corporate action): it takes the size its sized securities agree on,
-- and none where they differ. A fund, an option, a private share or any
-- other vehicle has no row.
-- ============================================================

CREATE VIEW web_position_sizes AS
    WITH lines AS (
        SELECT DISTINCT silver_source_id AS src, account_external_id AS acct, position_key,
               instrument_external_id AS iid, symbol, name, asset_class, vehicle
          FROM serving_positions_history
         WHERE vehicle = 'stock' AND asset_class = 'public_equity'),
    keyed AS (
        SELECT l.*, COALESCE(ha.to_key, hd.to_key, ia.k) AS akey
          FROM lines l
          LEFT JOIN holding_aliases ha ON ha.silver_source_id = l.src AND ha.account_external_id = l.acct
               AND ha.from_key = holding_key(l.iid, l.position_key)
          LEFT JOIN holding_aliases hd ON hd.silver_source_id = l.src AND hd.account_external_id = ''
               AND hd.from_key = holding_key(l.iid, l.position_key)
          LEFT JOIN instrument_aliases() ia ON ia.src = l.src AND ia.alias = l.iid),
    secs AS (
        SELECT k.symbol, k.name, k.asset_class, k.vehicle,
               exposure_key(COALESCE(ik1.security_key, ik2.security_key)) AS security_key
          FROM keyed k
          LEFT JOIN instrument_keys ik1 ON ik1.silver_source_id = k.src AND ik1.instrument_external_id = k.akey
          LEFT JOIN instrument_keys ik2 ON ik2.silver_source_id = k.src AND ik2.instrument_external_id = k.iid)
    SELECT x.symbol, x.name, x.asset_class, x.vehicle,
           CASE WHEN COUNT(DISTINCT s.size) = 1 THEN MAX(s.size) END AS size
      FROM secs x
      LEFT JOIN securities s ON s.security_key = x.security_key
           AND s.security_key NOT IN (SELECT fund_ref FROM exposure_fund_keys())
     GROUP BY 1, 2, 3, 4;

CREATE VIEW web_exposure_securities AS
SELECT CAST(epoch_ms(as_of * 1000) AS DATE) AS as_of_day, currency, silver_source_id,
       COALESCE(tax_wrapper, 'taxable_personal') AS tax_wrapper, asset_class, security, name, ticker,
       sector, country, class_weight, sector_weight, country_weight, market_cap, size,
       value, direct_value, via_funds_value, funds
  FROM report_exposure_security_slices;

-- ============================================================
-- The day the Allocation dashboard reads for a chosen day.
--
-- The Allocation cards read the daily holdings history on the As of
-- day. The history ends on the day of the last refresh, so a later day
-- alone would read nothing. web_history_calendar maps each day to the last history
-- day on or before it:
--
--   * as_of_day: every day from the history's first, through the later
--     of its last and tomorrow, UTC whatever the session's time zone.
--     Tomorrow covers a reader whose today is ahead of UTC;
--   * history_day: the last day of serving_global_history on or before
--     it, as the history views render a day.
--
-- A day the history holds maps to itself, a day after it to its last
-- day. A day before the history has no row. A card filters the view on
-- the chosen day and reads its rows of history_day.
-- ============================================================

CREATE VIEW web_history_calendar AS
    WITH h AS (SELECT DISTINCT as_of_day FROM serving_global_history),
    b AS (
        SELECT min(as_of_day) AS lo,
               greatest(max(as_of_day), CAST(epoch(now()) AS BIGINT) // 86400 * 86400 + 86400) AS hi
          FROM h),
    d AS (SELECT unnest(range(lo, hi + 1, 86400)) AS day FROM b WHERE lo IS NOT NULL)
    SELECT epoch_ms(d.day * 1000) AS as_of_day, epoch_ms(h.as_of_day * 1000) AS history_day
      FROM d ASOF JOIN h ON d.day >= h.as_of_day;

CREATE VIEW web_income_days AS
WITH t AS (SELECT CAST(epoch_ms(max(as_of) * 1000) AS DATE) AS today FROM report_yield_positions),
s AS (SELECT least(make_date(year(today) - 1, 1, 1), (SELECT min(day) FROM report_yield_received),
                   (SELECT min(day) FROM web_income_withheld),
                   (SELECT CAST(min(occurred_at) AS DATE) FROM web_income)) AS first, today
        FROM t)
SELECT CAST(d AS DATE) AS day
  FROM (SELECT unnest(generate_series(CAST(first AS TIMESTAMP), CAST(today AS TIMESTAMP), INTERVAL 1 DAY)) AS d
          FROM s);

-- ============================================================
-- A gains row with no asset class reads `unknown` (web/DESIGN.md §6).
--
-- report_gains leaves a position's asset_class NULL where no snapshot
-- names it at either end of the month: a sale within the month of a
-- position no snapshot shows at its start or its end. The Gains
-- dashboard's Asset class picker can pick no NULL, so its classes would
-- not sum to the whole. web_gains reads a missing class as
-- `unknown`, as the exposure and yield views do. report_gains itself
-- keeps the NULL.
-- ============================================================

CREATE VIEW web_gains AS
    SELECT g.currency, g.missing_basis, date_trunc('month', epoch_ms(g.period_start * 1000)) AS period,
           g.silver_source_id, g.account_external_id,
           account_label(a.display_name, g.account_external_id,
                         g.silver_source_id, a.account_kind) AS account,
           a.account_kind, COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
           g.k AS instrument_key, g.symbol, g.name, COALESCE(g.asset_class, 'unknown') AS asset_class, g.vehicle,
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

-- holdings_lines_history: every line of every day's net worth, from
-- the serving histories, the cash arm per currency.
CREATE VIEW holdings_lines_history AS
    SELECT p.as_of_day, p.silver_source_id, p.account_external_id,
           COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
           COALESCE(a.management_style, 'self_directed') AS management_style,
           'position' AS line_kind, p.asset_class, p.vehicle, p.currency, p.symbol, p.name,
           p.value_usd, p.value_chf, p.value_eur, p.value_gbp
      FROM serving_positions_history p
      LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id
           AND a.account_external_id = p.account_external_id
    UNION ALL
    SELECT as_of_day, silver_source_id, account_external_id, tax_wrapper, management_style,
           'cash', 'cash', 'demand_deposit', currency, NULL, NULL,
           balance_usd, balance_chf, balance_eur, balance_gbp
      FROM serving_cash_history;

CREATE VIEW web_sources_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, asset_class,
           SUM(value_usd) FILTER (WHERE line_kind = 'position') AS positions_value_usd,
           SUM(value_usd) FILTER (WHERE line_kind = 'cash') AS cash_balance_usd,
           SUM(value_usd) AS total_value_usd,
           SUM(value_chf) FILTER (WHERE line_kind = 'position') AS positions_value_chf,
           SUM(value_chf) FILTER (WHERE line_kind = 'cash') AS cash_balance_chf,
           SUM(value_chf) AS total_value_chf,
           SUM(value_eur) FILTER (WHERE line_kind = 'position') AS positions_value_eur,
           SUM(value_eur) FILTER (WHERE line_kind = 'cash') AS cash_balance_eur,
           SUM(value_eur) AS total_value_eur,
           SUM(value_gbp) FILTER (WHERE line_kind = 'position') AS positions_value_gbp,
           SUM(value_gbp) FILTER (WHERE line_kind = 'cash') AS cash_balance_gbp,
           SUM(value_gbp) AS total_value_gbp
      FROM holdings_lines_history
     GROUP BY 1, 2, 3, 4;

CREATE VIEW web_asset_classes_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, asset_class,
           SUM(value_usd) AS value_usd, SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur, SUM(value_gbp) AS value_gbp
      FROM holdings_lines_history
     GROUP BY 1, 2, 3, 4;

CREATE VIEW web_vehicles_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, asset_class, vehicle,
           SUM(value_usd) AS value_usd, SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur, SUM(value_gbp) AS value_gbp
      FROM holdings_lines_history
     GROUP BY 1, 2, 3, 4, 5;

CREATE VIEW web_accounts_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, management_style, asset_class,
           SUM(value_usd) AS total_value_usd, SUM(value_chf) AS total_value_chf,
           SUM(value_eur) AS total_value_eur, SUM(value_gbp) AS total_value_gbp
      FROM holdings_lines_history
     GROUP BY 1, 2, 3, 4, 5;

CREATE VIEW web_positions_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper,
           CASE WHEN line_kind = 'cash' THEN to_json([silver_source_id, account_external_id])::VARCHAR END AS line_key,
           symbol,
           CASE WHEN line_kind = 'cash' THEN 'Cash ' || currency ELSE name END AS name,
           asset_class, vehicle, currency,
           SUM(value_usd) AS value_usd, SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur, SUM(value_gbp) AS value_gbp
      FROM holdings_lines_history
     GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9;

CREATE VIEW web_currencies_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, asset_class, currency,
           SUM(value_usd) AS value_usd, SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur, SUM(value_gbp) AS value_gbp
      FROM holdings_lines_history
     WHERE currency IS NOT NULL
       AND COALESCE(value_usd, value_chf, value_eur, value_gbp) IS NOT NULL
     GROUP BY 1, 2, 3, 4, 5;

-- ============================================================
-- The income lines in the pool in the cash flow projection's look-back
-- (docs/CASHFLOW.md §12).
--
-- report_cashflow_plan holds a part, pool_income: the income lines in
-- the pool on their own day (cashflow_pool_spans), the types the income
-- projection projects aside, a monthly average of the same look-back
-- over the months other_income spreads over. Its rows are split as
-- outside_income's are: by the operating-in class of the line's
-- income type and by the account.
--
-- other_income stays the statement's figure, which the cash flow
-- projection reads. The two differ where the statement counts a line
-- as operating income and no income line holds it, such as a
-- reimbursement. With pool_income and outside_income a reader has a
-- run-rate of the income lines on every account.
-- ============================================================

CREATE VIEW web_cashflow_plan AS
SELECT currency, month, month_start, part, class, silver_source_id, account_external_id, value, quality
  FROM report_cashflow_plan;

-- web_benchmarks: the picker's values, None first.
CREATE VIEW web_benchmarks AS
    SELECT benchmark
      FROM (SELECT 'None' AS benchmark, 0 AS rank
            UNION ALL
            SELECT DISTINCT benchmark, 1 FROM benchmark_funds)
     ORDER BY rank, benchmark;

-- web_bridge: the time range lands on month_end, so a picked window
-- takes the whole months that end inside it.
CREATE VIEW web_bridge AS
    SELECT epoch_ms(end_day * 1000) AS month_end, currency, period,
           start_day, end_day, start_net_worth, end_net_worth, net_flow, onboarded,
           debt_repaid, net_saved, gain, quality
      FROM report_bridge;

-- web_marks: the Mark age tile reads marked_pct, one bar per month end,
-- so it takes the USD rows: the figure is the same in every currency.
CREATE VIEW web_marks AS
    SELECT epoch_ms(month_end * 1000) AS month_end, marked_pct
      FROM serving_marks
     WHERE currency = 'USD';

-- ============================================================
-- A portfolio's fee base leaves out its debts (docs/SPENDING.md §7).
--
--   * web_fees: the 'value' line of a portfolio holds no mortgage and
--     no card. Its fee rate is fees over its assets' mean value, the
--     base its return reads, as `wealthdb spending fees` reads it.
-- ============================================================

-- web_fees: what the fee tiles read, at day grain, so a picked window
-- cuts it where it cuts the other Spending tiles.
--   * line 'fee': a spending line of primary BANK_FEES, its value turned
--     to a cost. A refund is negative. fee_group is its detailed group.
--   * line 'value': an account's value on a day. A portfolio's mortgages
--     and cards are left out, as its return leaves them out; an account
--     that is a card or a mortgage keeps its own balance.
--   * line 'return': the entity's return for the month that ends on the
--     day, in `currency`, repeated on each of its accounts so an account
--     picker keeps it.
-- The entity is the account's portfolio, else the account.
CREATE VIEW web_fees AS
    WITH acct AS (
        SELECT a.silver_source_id, a.account_external_id, a.account_kind,
               account_label(a.display_name, a.account_external_id,
                             a.silver_source_id, a.account_kind) AS account_label,
               account_display_name(a.display_name, a.account_external_id) AS display_name,
               NULLIF(a.portfolio_external_id, '') AS portfolio
          FROM accounts a),
    entity AS (
        SELECT c.*,
               to_json([c.silver_source_id, COALESCE(c.portfolio, c.account_external_id)])::VARCHAR AS entity_key,
               CASE WHEN c.portfolio IS NOT NULL THEN 'portfolio' ELSE 'account' END AS entity_kind,
               COALESCE(p.display_name, c.portfolio, c.display_name) AS entity
          FROM acct c
          LEFT JOIN portfolios p ON p.silver_source_id = c.silver_source_id
               AND p.portfolio_external_id = c.portfolio)
    SELECT epoch_ms(t.occurred_at * 1000) AS day, 'fee' AS line,
           e.silver_source_id, e.account_label, e.entity_key, e.entity_kind, e.entity,
           fee_group(t.spend_detailed) AS fee_group, CAST(NULL AS TEXT) AS currency,
           -t.value_usd AS value_usd, -t.value_chf AS value_chf,
           -t.value_eur AS value_eur, -t.value_gbp AS value_gbp,
           CAST(NULL AS DOUBLE) AS twr
      FROM serving_spending_transactions t
      JOIN entity e ON e.silver_source_id = t.silver_source_id
       AND e.account_external_id = t.account_external_id
     WHERE t.spend_primary = 'BANK_FEES'
    UNION ALL
    SELECT epoch_ms(h.as_of_day * 1000), 'value',
           e.silver_source_id, e.account_label, e.entity_key, e.entity_kind, e.entity,
           NULL, NULL,
           h.total_value_usd, h.total_value_chf, h.total_value_eur, h.total_value_gbp,
           NULL
      FROM serving_accounts_history h
      JOIN entity e ON e.silver_source_id = h.silver_source_id
       AND e.account_external_id = h.account_external_id
     WHERE e.entity_kind = 'account' OR e.account_kind NOT IN ('mortgage', 'card')
    UNION ALL
    SELECT epoch_ms(r.end_day * 1000), 'return',
           e.silver_source_id, e.account_label, e.entity_key, e.entity_kind, e.entity,
           NULL, r.currency, NULL, NULL, NULL, NULL, r.twr
      FROM report_returns r
      JOIN entity e ON e.silver_source_id = r.silver_source_id
       AND ((r.grain = 'portfolios' AND r.entity_id = e.portfolio)
         OR (r.grain = 'accounts' AND e.portfolio IS NULL AND r.entity_id = e.account_external_id))
     WHERE r.granularity = 'monthly' AND r.window_from_year = 0 AND NOT r.is_summary;
