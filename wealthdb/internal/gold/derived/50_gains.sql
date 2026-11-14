-- Gains: lots, cost basis, realized and unrealized gains, and their
-- reports.

-- realized_effective_date: the date a realized lot counts on: its
-- disposal date, else its settlement date, else the last day of its
-- tax year.
CREATE MACRO realized_effective_date(p_disposal, p_settlement, p_tax_year) AS
    COALESCE(p_disposal, p_settlement, make_date(p_tax_year, 12, 31));

CREATE MACRO lot_key(p_instrument, p_hint, p_description) AS
    COALESCE(p_instrument, p_hint, p_description, '');

-- sum_or_null: a + b, reading a missing term as zero, NULL when both
-- are missing.
CREATE MACRO sum_or_null(p_a, p_b) AS
    CASE WHEN p_a IS NULL AND p_b IS NULL THEN NULL ELSE COALESCE(p_a, 0) + COALESCE(p_b, 0) END;

-- realized_lots_all: the realized lots the sources state and the
-- engine's, in one shape. disposal is NULL on a stated row.
CREATE VIEW realized_lots_all AS
    SELECT * FROM realized_lots UNION ALL BY NAME SELECT * FROM lot_realized;

-- stated_basis: whether a position row's book_value is one its source
-- states, not one the engine rebuilt.
CREATE MACRO stated_basis(p_book_value, p_origin) AS
    p_book_value IS NOT NULL AND COALESCE(p_origin, '') <> 'rebuilt';

-- lot_port: what a sale and a stated realized lot must share to count
-- as one account: the account's portfolio, else the account itself.
-- p_pid is portfolio_acct_map()'s pid, '' or NULL outside a portfolio.
CREATE MACRO lot_port(p_pid, p_acct) AS
    CASE WHEN p_pid <> '' THEN 'p:' || p_pid ELSE 'a:' || p_acct END;

-- documented_years: each source, port (lot_port) and tax year with a
-- stated primary realized lot. The instrument does not enter: a
-- document can name a security by an id the trades do not use.
CREATE MACRO documented_years() AS TABLE (
    SELECT DISTINCT r.silver_source_id AS src, lot_port(m.pid, r.account_external_id) AS port, r.tax_year
      FROM realized_lots r
      LEFT JOIN portfolio_acct_map() m ON m.src = r.silver_source_id AND m.acct = r.account_external_id
     WHERE r.is_primary
);

-- documented_txns: the transactions of a documented year
-- (documented_years) on their account or its portfolio. A sale among
-- them is documented: its stated lots are primary and the engine's for
-- it are not, so no sale counts twice. gains_events and the lot pass
-- both read it.
CREATE MACRO documented_txns() AS TABLE (
    SELECT t.silver_source_id AS src, t.transaction_external_id AS txn
      FROM transactions t
      LEFT JOIN portfolio_acct_map() m ON m.src = t.silver_source_id AND m.acct = t.account_external_id
      JOIN documented_years() d ON d.src = t.silver_source_id
       AND d.port = lot_port(m.pid, t.account_external_id)
       AND d.tax_year = year(epoch_ms(t.occurred_at * 1000))
);

-- lot_obs_at: when a snapshot observes, for the lot pass and the check.
-- A snapshot stamped at UTC midnight states its date's closing balance,
-- as the holdings reports read it, so it observes at the day's last
-- second, after the date's trades; any other stamp is a moment.
CREATE MACRO lot_obs_at(p_snap) AS
    CASE WHEN p_snap % 86400 = 0 THEN p_snap + 86399 ELSE p_snap END;

-- gain_needs_cost: a realized lot whose gain is known only with a cost
-- basis it lacks: no stated gain, stated proceeds, no cost.
CREATE MACRO gain_needs_cost(p_gain, p_proceeds, p_book_value) AS
    p_gain IS NULL AND p_proceeds IS NOT NULL AND p_book_value IS NULL;

-- lot_sources: the last lot pass's row of lot_runs for each source it
-- rebuilt, and whether it pooled.
CREATE MACRO lot_sources() AS TABLE (
    SELECT *, grain = 'portfolio' AS pooled
      FROM lot_runs WHERE build_id = (SELECT MAX(build_id) FROM lot_runs)
);

-- lot_key_accounts: each account's key account in the last lot pass:
-- the account, or its portfolio where the source pools. A lot's
-- account_external_id is its key account.
CREATE MACRO lot_key_accounts() AS TABLE (
    SELECT m.src, m.acct,
           CASE WHEN COALESCE(s.pooled, FALSE) AND m.pid <> '' THEN m.pid ELSE m.acct END AS key_account
      FROM portfolio_acct_map() m
      LEFT JOIN lot_sources() s ON s.silver_source_id = m.src
);

-- lots_at: the engine's lots open at each instant in p_instants, with
-- what is left of them: the quantity and cost as opened, less the
-- disposals at or before the instant. A lot's account is its key's: an
-- account, or a pooled source's portfolio. The stated lots of a
-- position are position_lots; report_lots reads both.
CREATE MACRO lots_at(p_instants) AS TABLE (
    WITH b AS (SELECT DISTINCT unnest(p_instants) AS instant),
    o AS (
        SELECT b.instant, l.*
          FROM b JOIN lots l ON l.opened_at <= b.instant AND (l.closed_at IS NULL OR l.closed_at > b.instant)),
    d AS (
        SELECT o.instant, o.lot_id, SUM(x.quantity) AS out_quantity, SUM(x.cost) AS out_cost
          FROM o JOIN lot_disposals x ON x.lot_id = o.lot_id AND x.disposed_at <= o.instant
         GROUP BY ALL)
    SELECT o.instant, o.silver_source_id, o.account_external_id, o.instrument_external_id, o.lot_id,
           o.currency, o.acquisition_date, o.acquired_various, o.origin, o.cost_origin, o.method, o.fees,
           o.quantity - COALESCE(d.out_quantity, 0) AS quantity,
           o.cost - COALESCE(d.out_cost, 0) AS cost
      FROM o LEFT JOIN d ON d.instant = o.instant AND d.lot_id = o.lot_id
     WHERE o.quantity - COALESCE(d.out_quantity, 0) > 0
);

-- gains_quality: the quality column, each flag named in docs/GAINS.md
-- §6, in that order.
CREATE MACRO gains_quality(p_undocumented, p_rebuilt, p_without_gain, p_undated, p_unmatched,
        p_in_kind, p_corporate, p_basis_changed, p_unobserved, p_paid_in, p_onboarded,
        p_seeds, p_implied, p_blips, p_spliced, p_pooled, p_settlement, p_wash, p_fee_unvalued,
        p_assumed_zero, p_fx_missing) AS
    concat_ws(';',
        CASE WHEN p_undocumented > 0 THEN 'sells_without_documents=' || p_undocumented END,
        CASE WHEN p_rebuilt > 0 THEN 'sells_rebuilt=' || p_rebuilt END,
        CASE WHEN p_without_gain > 0 THEN 'lots_without_gain=' || p_without_gain END,
        CASE WHEN p_undated > 0 THEN 'undated_lots=' || p_undated END,
        CASE WHEN p_unmatched > 0 THEN 'unmatched_lots=' || p_unmatched END,
        CASE WHEN p_in_kind > 0 THEN 'in_kind_moves=' || p_in_kind END,
        CASE WHEN p_corporate > 0 THEN 'corporate_actions=' || p_corporate END,
        CASE WHEN p_basis_changed > 0 THEN 'basis_changed=' || p_basis_changed END,
        CASE WHEN p_unobserved > 0 THEN 'accounts_unobserved=' || p_unobserved END,
        CASE WHEN p_paid_in THEN 'paid_in_basis' END,
        CASE WHEN len(p_onboarded) > 0 THEN 'onboarded_in_window=' || array_to_string(p_onboarded, ',') END,
        CASE WHEN p_seeds > 0 THEN 'seed_lots=' || p_seeds END,
        CASE WHEN p_implied > 0 THEN 'implied_disposals=' || p_implied END,
        CASE WHEN p_blips > 0 THEN 'snapshot_blips=' || p_blips END,
        CASE WHEN p_spliced THEN 'spliced' END,
        CASE WHEN p_pooled THEN 'pooled' END,
        CASE WHEN p_settlement THEN 'dated_by_settlement' END,
        CASE WHEN p_wash > 0 THEN 'wash_sales_not_applied=' || p_wash END,
        CASE WHEN p_fee_unvalued > 0 THEN 'fee_unvalued=' || p_fee_unvalued END,
        CASE WHEN p_assumed_zero > 0 THEN 'basis_assumed_zero=' || p_assumed_zero END,
        CASE WHEN p_fx_missing > 0 THEN 'fx_missing=' || p_fx_missing END);

-- realized_lots_in_all: realized_lots_in under every currency in
-- p_ccys and reading in p_readings, named the same way.
CREATE MACRO realized_lots_in_all(p_from, p_to, p_ccys, p_all, p_readings) AS TABLE (
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

CREATE MACRO realized_lots_in(p_from, p_to, p_ccy, p_all, p_missing := 'ignore') AS TABLE (
    SELECT * EXCLUDE (out_currency, missing_basis)
      FROM realized_lots_in_all(p_from, p_to, [p_ccy], p_all, [p_missing])
);

-- report_gains_realized: the realized lots of the window under
-- p_missing, with what each lot realized: `disposal` is sell, fee, spend, tender, cash_in_lieu or
-- expiry.
CREATE MACRO report_gains_realized(p_from, p_to, p_ccy, p_all, p_missing := 'ignore') AS TABLE (
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
           r.realized_lot_external_id, r.source_document,
           COALESCE(r.disposal, 'sell') AS disposal
      FROM realized_lots_in(p_from, p_to, p_ccy, p_all, p_missing := p_missing) r
      LEFT JOIN accounts a ON a.silver_source_id = r.silver_source_id AND a.account_external_id = r.account_external_id
     ORDER BY r.effective_at, r.silver_source_id, r.account_external_id, r.realized_lot_external_id
);

-- gains_events: the sales of the window and how each is covered,
-- counting a sale the engine realized against a stated basis as
-- documented.
CREATE MACRO gains_events(p_from, p_to) AS TABLE (
    WITH docs AS (SELECT * FROM documented_txns()),
    rebuilt AS (
        SELECT DISTINCT silver_source_id AS src, disposal_transaction_external_id AS txn
          FROM lot_disposals WHERE realized_lot_external_id IS NOT NULL),
    stated AS (
        SELECT DISTINCT r.silver_source_id AS src, r.transaction_external_id AS txn
          FROM lot_realized r
          JOIN lot_sources() s ON s.silver_source_id = r.silver_source_id AND s.mode = 'stated'),
    e AS (
        SELECT t.silver_source_id, t.account_external_id, t.instrument_external_id, t.occurred_at,
               t.transaction_external_id,
               CASE t.kind WHEN 'sell' THEN 'sell' WHEN 'corporate_action' THEN 'corporate_action'
                    ELSE 'in_kind' END AS event,
               t.kind = 'sell' AND (d.txn IS NOT NULL OR st.txn IS NOT NULL) AS documented
          FROM transactions t
          LEFT JOIN docs d ON d.src = t.silver_source_id AND d.txn = t.transaction_external_id
          LEFT JOIN stated st ON st.src = t.silver_source_id AND st.txn = t.transaction_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to
           AND (t.kind = 'sell'
                OR (t.kind = 'corporate_action' AND t.instrument_external_id IS NOT NULL)
                OR (t.kind IN ('transfer_in', 'transfer_out', 'journal')
                    AND t.instrument_external_id IS NOT NULL AND t.quantity IS NOT NULL)))
    SELECT e.silver_source_id, e.account_external_id, e.instrument_external_id, e.occurred_at, e.event,
           e.documented, e.event = 'sell' AND NOT e.documented AND rb.txn IS NOT NULL AS rebuilt
      FROM e
      LEFT JOIN rebuilt rb ON rb.src = e.silver_source_id AND rb.txn = e.transaction_external_id
);

-- report_gains_coverage: what the documents and the engine cover. A sale the
-- engine rebuilt is covered; `sells_rebuilt` counts those, and
-- `no_realized` means sales neither a document nor the engine covers.
-- The engine's columns:
--   positions_rebuilt  positions at the window's end whose cost basis
--                      the engine wrote, in whole or in part
--   seed_lots          the seed lots (opened without a cost) the
--                      engine opened in the window
--   lot_mode           the source's mode in the last lot pass
--   lot_methods        the methods that rebuilt the account's lots
CREATE MACRO report_gains_coverage(p_from, p_to, p_ccy) AS TABLE (
    WITH pos AS (
        SELECT silver_source_id, account_external_id,
               COUNT(*) AS n_positions,
               COUNT(book_value) AS n_with_basis,
               COUNT(*) FILTER (WHERE book_value_known IS NOT NULL) AS n_rebuilt,
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
               COUNT(*) FILTER (WHERE NOT documented AND NOT rebuilt) AS n_undocumented,
               COUNT(*) FILTER (WHERE rebuilt) AS n_rebuilt
          FROM gains_events(p_from, p_to) WHERE event = 'sell' GROUP BY ALL),
    sold AS (
        SELECT silver_source_id, account_external_id, COUNT(*) AS n_lots
          FROM realized_lots_all
         WHERE is_primary
           AND CAST(epoch(realized_effective_date(disposal_date, settlement_date, tax_year)) AS BIGINT)
               BETWEEN p_from AND p_to
         GROUP BY ALL),
    docs AS (
        SELECT silver_source_id, account_external_id,
               string_agg(DISTINCT document_kind, ',' ORDER BY document_kind) AS documents
          FROM realized_lots_all
         WHERE tax_year BETWEEN year(epoch_ms(CAST(p_from AS BIGINT) * 1000)) AND year(epoch_ms(CAST(p_to AS BIGINT) * 1000))
         GROUP BY ALL),
    seeds AS (
        SELECT silver_source_id, account_external_id, COUNT(*) AS n_seeds
          FROM lot_findings WHERE finding = 'seed' AND found_at BETWEEN p_from AND p_to GROUP BY ALL),
    methods AS (
        SELECT DISTINCT ka.src AS silver_source_id, ka.acct AS account_external_id, l.method
          FROM (SELECT DISTINCT silver_source_id, account_external_id, method FROM lots) l
          JOIN lot_key_accounts() ka ON ka.src = l.silver_source_id AND ka.key_account = l.account_external_id),
    keys AS (
        SELECT silver_source_id, account_external_id FROM pos
        UNION SELECT silver_source_id, account_external_id FROM sells
        UNION SELECT silver_source_id, account_external_id FROM sold),
    m AS (
        SELECT k.silver_source_id, k.account_external_id, a.display_name, a.nickname, account_wrapper(a.tax_wrapper) AS tax_wrapper,
               COALESCE(p.n_positions, 0) AS n_positions, p.n_with_basis, p.value_x, p.value_with_basis_x,
               p.coverage, p.stamps, COALESCE(p.open_lots, 0) AS open_lots,
               COALESCE(s.n_sells, 0) AS sells, COALESCE(s.n_undocumented, 0) AS n_undocumented,
               COALESCE(s.n_rebuilt, 0) AS sells_rebuilt,
               COALESCE(l.n_lots, 0) AS realized_lots, d.documents,
               COALESCE(p.n_rebuilt, 0) AS positions_rebuilt, COALESCE(sd.n_seeds, 0) AS seed_lots,
               ls.mode AS lot_mode,
               (SELECT string_agg(DISTINCT x.method, ',' ORDER BY x.method) FROM methods x
                 WHERE x.silver_source_id = k.silver_source_id AND x.account_external_id = k.account_external_id) AS lot_methods
          FROM keys k
          LEFT JOIN pos p USING (silver_source_id, account_external_id)
          LEFT JOIN sells s USING (silver_source_id, account_external_id)
          LEFT JOIN sold l USING (silver_source_id, account_external_id)
          LEFT JOIN docs d USING (silver_source_id, account_external_id)
          LEFT JOIN seeds sd USING (silver_source_id, account_external_id)
          LEFT JOIN lot_sources() ls USING (silver_source_id)
          LEFT JOIN accounts a USING (silver_source_id, account_external_id))
    SELECT silver_source_id, account_external_id, display_name, nickname, tax_wrapper,
           CAST(value_x::DECIMAL(28,4) AS VARCHAR) AS value,
           CAST(value_with_basis_x::DECIMAL(28,4) AS VARCHAR) AS value_with_basis,
           coverage AS basis_coverage, stamps AS basis_stamps, CAST(open_lots AS BIGINT) AS open_lots,
           sells, realized_lots, documents,
           CASE WHEN n_positions > 0 AND n_with_basis = 0 THEN 'no_basis'
                WHEN n_undocumented > 0 AND (n_positions = 0 OR coverage >= 0.99) THEN 'no_realized'
                WHEN n_undocumented = 0 AND (n_positions = 0 OR coverage >= 0.99) THEN 'ok'
                ELSE 'partial' END AS verdict,
           sells_rebuilt, positions_rebuilt, seed_lots, lot_mode, lot_methods
      FROM m
     ORDER BY silver_source_id, account_external_id
);

-- report_lots: the lots held at p_asof, with deterministic labels and
-- a rate that belongs to the currency it reports.
CREATE MACRO report_lots(p_asof, p_ccy, p_missing := 'ignore') AS TABLE (
    WITH lines AS (SELECT * FROM gains_position_lines([p_asof], p_ccy)),
    stated AS (
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
          JOIN lines p
            ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snapshot_at
           AND p.account_external_id = l.account_external_id AND p.position_key = l.position_key),
    keyed AS (
        SELECT p.*, COALESCE(ka.key_account, p.account_external_id) AS key_account
          FROM lines p
          LEFT JOIN lot_key_accounts() ka ON ka.src = p.silver_source_id AND ka.acct = p.account_external_id
         WHERE p.book_value_known IS NOT NULL),
    keys AS (
        SELECT silver_source_id, key_account, holding_key(instrument_external_id, position_key) AS k,
               max(snapshot_at) AS snapshot_at, min(position_key) AS position_key,
               min(symbol) AS symbol, min(name) AS name, min(currency) AS currency,
               arg_min(rate, currency) AS rate, bool_and(basis_applies) AS basis_applies,
               CASE WHEN count(DISTINCT account_external_id) = 1 THEN any_value(display_name) END AS display_name,
               CASE WHEN count(DISTINCT account_external_id) = 1 THEN any_value(nickname) END AS nickname,
               SUM(quantity)::DOUBLE AS quantity, SUM(market_value)::DOUBLE AS position_value,
               SUM(COALESCE(accrued_interest, 0))::DOUBLE AS position_accrued
          FROM keyed GROUP BY ALL),
    engine AS (
        SELECT k.silver_source_id, k.snapshot_at, k.key_account AS account_external_id,
               COALESCE(k.display_name, pb.display_name) AS display_name, k.nickname,
               k.position_key, k.symbol, k.name, k.currency, k.rate, k.basis_applies, l.lot_id AS lot_key,
               l.acquisition_date, CAST(NULL AS VARCHAR) AS term, CAST(NULL AS BOOLEAN) AS covered,
               l.quantity, l.cost AS book_value,
               CASE WHEN l.cost IS NOT NULL THEN 'rebuilt' END AS basis_origin,
               CAST(NULL AS VARCHAR) AS source_document,
               l.quantity::DOUBLE / NULLIF(k.quantity, 0) AS share,
               CASE WHEN k.position_value IS NOT NULL AND k.quantity <> 0 THEN 'pro_rata' END AS value_origin,
               CAST(NULL AS DOUBLE) AS stated_value, k.position_value, k.position_accrued
          FROM lots_at((SELECT list(DISTINCT lot_obs_at(snapshot_at)) FROM keys)) l
          JOIN keys k ON k.silver_source_id = l.silver_source_id AND k.key_account = l.account_external_id
           AND k.k = l.instrument_external_id AND l.instant = lot_obs_at(k.snapshot_at)
          LEFT JOIN portfolio_buckets() pb ON pb.src = k.silver_source_id AND pb.pid = k.key_account
         WHERE NOT EXISTS (SELECT 1 FROM stated s WHERE s.silver_source_id = k.silver_source_id
                              AND s.account_external_id = k.key_account AND s.position_key = k.position_key)),
    v AS (
        SELECT * REPLACE (CASE WHEN p_missing = 'zero' AND basis_applies THEN COALESCE(book_value, 0)
                               ELSE book_value END AS book_value)
          FROM (SELECT * FROM stated UNION ALL BY NAME SELECT * FROM engine)),
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
           COALESCE(term, CASE WHEN acquisition_date IS NOT NULL THEN
                CASE WHEN CAST(epoch_ms(CAST(p_asof AS BIGINT) * 1000) AS DATE) > acquisition_date + INTERVAL 1 YEAR
                     THEN 'long' ELSE 'short' END END) AS term,
           covered, CAST(quantity AS VARCHAR) AS quantity, currency,
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

-- report_gains_check: the positions check at each account's
-- active snapshot at the window's end, where that falls in the window.
CREATE MACRO report_gains_check(p_from, p_to, p_ccy) AS TABLE (
    WITH r AS (
        SELECT x.*, lot_key(x.instrument_external_id, x.instrument_hint, x.description) AS k
          FROM realized_lots_in(p_from, p_to, p_ccy, TRUE) x),
    eng AS (
        SELECT silver_source_id, account_external_id, k, tax_year,
               COUNT(*) AS n, SUM(quantity) AS qty, SUM(book_x) AS cost_x, SUM(gain_x) AS gain_x,
               COUNT(*) FILTER (WHERE book_x IS NULL) AS n_uncosted
          FROM r WHERE document_kind = 'engine' AND NOT is_primary GROUP BY ALL),
    sta AS (
        SELECT silver_source_id, account_external_id, k, tax_year,
               SUM(quantity) AS qty, SUM(book_x) AS cost_x, SUM(gain_x) AS gain_x
          FROM r WHERE document_kind <> 'engine' AND is_primary GROUP BY ALL),
    realized AS (
        SELECT 'realized' AS check_kind, e.silver_source_id, e.account_external_id,
               CAST(e.tax_year AS VARCHAR) AS period,
               SUM(e.n) AS items, SUM(e.qty) AS quantity,
               SUM(e.cost_x) AS engine_x, SUM(s.cost_x) AS stated_x,
               SUM(e.gain_x) AS engine_gain_x, SUM(s.gain_x) AS stated_gain_x
          FROM eng e JOIN sta s ON s.silver_source_id = e.silver_source_id
           AND s.account_external_id = e.account_external_id AND s.k = e.k AND s.tax_year = e.tax_year
         WHERE e.n_uncosted = 0 AND abs(e.qty - s.qty) <= 0.005 * abs(s.qty)
         GROUP BY ALL),
    -- A pooled source states no basis of a wallet's own, so only
    -- account keys are compared.
    last_snap AS (
        SELECT a.src AS silver_source_id, a.acct AS account_external_id, a.snap AS snapshot_at,
               lot_obs_at(a.snap) AS instant
          FROM pos_active_at([p_to]) a JOIN lot_sources() s ON s.silver_source_id = a.src AND NOT s.pooled
         WHERE a.snap >= p_from),
    stated_pos AS (
        SELECT p.silver_source_id, p.account_external_id, holding_key(p.instrument_external_id, p.position_key) AS k,
               any_value(p.currency) AS currency, ls.snapshot_at, ls.instant,
               SUM(p.quantity) AS qty, SUM(p.book_value) AS cost
          FROM positions p JOIN last_snap ls ON ls.silver_source_id = p.silver_source_id
           AND ls.account_external_id = p.account_external_id AND ls.snapshot_at = p.snapshot_at
         WHERE stated_basis(p.book_value, p.basis_origin) AND basis_applies(p.asset_class, p.vehicle)
         GROUP BY ALL),
    open_book AS (
        SELECT instant, silver_source_id, account_external_id, instrument_external_id AS k,
               SUM(quantity) AS qty, SUM(cost) AS cost, bool_and(cost IS NOT NULL) AS costed
          FROM lots_at((SELECT list(DISTINCT instant) FROM last_snap)) GROUP BY ALL),
    positions_check AS (
        SELECT 'positions' AS check_kind, s.silver_source_id, s.account_external_id, NULL AS period,
               COUNT(*) AS items, SUM(s.qty) AS quantity,
               SUM(fx_amount(b.cost, s.currency, p_ccy, fx.rate)) AS engine_x,
               SUM(fx_amount(s.cost, s.currency, p_ccy, fx.rate)) AS stated_x,
               CAST(NULL AS DOUBLE) AS engine_gain_x, CAST(NULL AS DOUBLE) AS stated_gain_x
          FROM stated_pos s
          JOIN open_book b ON b.instant = s.instant AND b.silver_source_id = s.silver_source_id
           AND b.account_external_id = s.account_external_id AND b.k = s.k
          ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = s.currency AND fx.day <= (s.snapshot_at // 86400)
         WHERE b.costed AND abs(b.qty - s.qty) <= 0.005 * abs(s.qty)
         GROUP BY ALL),
    anchors AS (
        SELECT 'anchors' AS check_kind, a.silver_source_id, a.account_external_id,
               NULL AS period, COUNT(*) AS items, SUM(a.matched_quantity) AS quantity,
               SUM(fx_amount(a.book_cost, a.currency, p_ccy, fx.rate)) AS engine_x,
               SUM(fx_amount(a.stated_cost, a.currency, p_ccy, fx.rate)) AS stated_x,
               CAST(NULL AS DOUBLE) AS engine_gain_x, CAST(NULL AS DOUBLE) AS stated_gain_x
          FROM lot_anchors a
          ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = a.currency AND fx.day <= (a.anchored_at // 86400)
         WHERE a.anchored_at BETWEEN p_from AND p_to AND NOT a.kept
         GROUP BY ALL),
    -- What each move carried, from its own disposals: the receiving
    -- lots of one move can be many (one per lot, or an average pool
    -- reopened per merge), so they only say where it went.
    moves AS (
        SELECT d.disposal_transaction_external_id AS txn, g.silver_source_id AS from_src,
               g.instrument_external_id AS inst, MIN(d.disposed_at) AS at,
               SUM(d.quantity) AS qty, SUM(d.cost) AS cost, bool_and(d.cost IS NOT NULL) AS costed
          FROM lot_disposals d JOIN lots g ON g.lot_id = d.lot_id
         WHERE d.kind = 'move' AND d.disposed_at BETWEEN p_from AND p_to
         GROUP BY ALL),
    moved AS (
        SELECT n.silver_source_id, n.account_external_id, n.instrument_external_id, n.currency,
               MIN(mv.at) AS at, SUM(mv.qty) AS qty, SUM(mv.cost) AS cost, bool_and(mv.costed) AS costed
          FROM moves mv
          JOIN (SELECT DISTINCT silver_source_id, account_external_id, instrument_external_id, currency,
                       origin_transaction_external_id
                  FROM lots WHERE origin = 'transfer_in') n
            ON n.origin_transaction_external_id = mv.txn AND n.silver_source_id <> mv.from_src
           AND n.instrument_external_id = mv.inst
         GROUP BY ALL),
    first_stated AS (
        SELECT mv.silver_source_id, mv.account_external_id, mv.instrument_external_id,
               arg_min(p.book_value::DOUBLE / NULLIF(p.quantity::DOUBLE, 0), p.snapshot_at) AS unit_cost
          FROM moved mv
          JOIN positions p ON p.silver_source_id = mv.silver_source_id AND p.account_external_id = mv.account_external_id
           AND p.instrument_external_id = mv.instrument_external_id AND p.snapshot_at >= mv.at
           AND stated_basis(p.book_value, p.basis_origin)
         GROUP BY ALL),
    handover AS (
        SELECT 'handover' AS check_kind, mv.silver_source_id, mv.account_external_id, NULL AS period,
               COUNT(*) AS items, SUM(mv.qty) AS quantity,
               SUM(fx_amount(mv.cost, mv.currency, p_ccy, fx.rate)) FILTER (WHERE mv.costed AND fs.unit_cost IS NOT NULL) AS engine_x,
               SUM(fx_amount(fs.unit_cost * mv.qty, mv.currency, p_ccy, fx.rate)) FILTER (WHERE mv.costed AND fs.unit_cost IS NOT NULL) AS stated_x,
               CAST(NULL AS DOUBLE) AS engine_gain_x, CAST(NULL AS DOUBLE) AS stated_gain_x
          FROM moved mv
          LEFT JOIN first_stated fs USING (silver_source_id, account_external_id, instrument_external_id)
          ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = mv.currency AND fx.day <= (mv.at // 86400)
         GROUP BY ALL),
    allc AS (
        SELECT * FROM realized UNION ALL BY NAME SELECT * FROM positions_check
        UNION ALL BY NAME SELECT * FROM anchors UNION ALL BY NAME SELECT * FROM handover)
    SELECT c.check_kind AS "check", c.silver_source_id, c.account_external_id, a.display_name, a.nickname,
           c.period, CAST(c.items AS BIGINT) AS items, CAST(c.quantity AS VARCHAR) AS quantity,
           CAST(c.engine_x::DECIMAL(28,4) AS VARCHAR) AS engine_cost,
           CAST(c.stated_x::DECIMAL(28,4) AS VARCHAR) AS stated_cost,
           CAST((c.engine_x - c.stated_x)::DECIMAL(28,4) AS VARCHAR) AS cost_delta,
           CAST(c.engine_gain_x::DECIMAL(28,4) AS VARCHAR) AS engine_gain,
           CAST(c.stated_gain_x::DECIMAL(28,4) AS VARCHAR) AS stated_gain,
           CAST((c.engine_gain_x - c.stated_gain_x)::DECIMAL(28,4) AS VARCHAR) AS gain_delta
      FROM allc c
      LEFT JOIN accounts a ON a.silver_source_id = c.silver_source_id AND a.account_external_id = c.account_external_id
     ORDER BY CASE c.check_kind WHEN 'realized' THEN 1 WHEN 'positions' THEN 2 WHEN 'anchors' THEN 3 ELSE 4 END,
              c.silver_source_id, c.account_external_id, c.period
);

-- ============================================================
-- A bucket's gain split by what prices did, for the Gains dashboard.
--
-- gain = realized + unrealized change holds in every bucket, but a sale
-- realizes the whole gain since purchase, most of it built up in
-- earlier buckets, and the same amount leaves the unrealized change: a
-- sale would show as two opposite bars. gains_windows_all adds the
-- split that does not:
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
-- The split and the sums do not depend on the order rows arrive in.
-- A sold lot's start value is its quantity at the instrument's start
-- price, converted at the start's rate. Where a source holds the
-- instrument in several accounts, each account's snapshot is converted
-- at the rate of its own day, so the price takes the rate of the
-- newest snapshot at or before the start: any one of the rates would
-- let held_change_x and sold_gain_x move between two materializations
-- of the same gold. The sums over a holding's sold lots (realized_x and
-- its parts, the cost basis, proceeds, wash sales, released_x) add
-- their values in ascending order, so their last bits are the same on
-- every run.
-- ============================================================

-- gains_windows_all: per bucket of the window, each holding's gains in
-- every currency and reading, with the start rate of the newest
-- observation and the lots' sums in a fixed order.
CREATE MACRO gains_windows_all(p_from, p_to, p_period, p_ccys, p_readings) AS TABLE (
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
    aliases AS (SELECT * FROM holding_aliases()),
    -- A holding a source keys two ways reads under one key (aliases).
    lines AS (
        SELECT l.* REPLACE (COALESCE(ha.to_key, l.instrument_external_id) AS instrument_external_id,
                            COALESCE(ha.to_key, l.position_key) AS position_key)
          FROM gains_position_lines_all((SELECT l FROM instants), p_ccys, p_readings) l
          LEFT JOIN aliases ha ON ha.silver_source_id = l.silver_source_id
               AND (ha.account_external_id = '' OR ha.account_external_id = l.account_external_id)
               AND ha.from_key = holding_key(l.instrument_external_id, l.position_key)),
    -- Presence is the same under every currency and reading.
    lines1 AS (SELECT * FROM lines WHERE out_currency = p_ccys[1] AND missing_basis = p_readings[1]),
    -- What a holding is at an instant, per account and instrument: the
    -- same under every currency and reading, so found once.
    side0 AS (
        SELECT instant, silver_source_id, account_external_id,
               holding_key(instrument_external_id, position_key) AS k,
               min(position_key) AS position_key, min(symbol) AS symbol, min(name) AS name,
               min(asset_class) AS asset_class, min(vehicle) AS vehicle,
               -- The account's snapshot the instant reads.
               max(snapshot_at) AS observed_at,
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
               x.n_assumed, s0.rebuilt, CASE WHEN s0.one_ccy THEN x.any_rate END AS rate,
               s0.observed_at
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
               -- A cost basis the zero reading assumes on one side and
               -- the source states on the other is a change of basis,
               -- not of price, as it is under the ignore reading.
               (s.k IS NOT NULL AND NOT s.complete) OR (e.k IS NOT NULL AND NOT e.complete)
                   OR (s.k IS NOT NULL AND e.k IS NOT NULL AND (s.n_assumed > 0) <> (e.n_assumed > 0)) AS change_unknown,
               (s.k IS NOT NULL AND s.applies AND s.complete) OR (e.k IS NOT NULL AND e.applies AND e.complete) AS one_side_known,
               e.k IS NOT NULL AS at_end, COALESCE(e.applies, FALSE) AS end_applies,
               COALESCE(e.applies AND NOT e.complete, FALSE) AS end_without_basis,
               COALESCE(e.unpriced, FALSE) AS end_unpriced,
               e.stamp, COALESCE(e.acquisition_date, s.acquisition_date) AS acquisition_date,
               COALESCE(e.open_lots, 0) AS open_lots,
               COALESCE(e.paid_in, FALSE) OR COALESCE(s.paid_in, FALSE) AS paid_in,
               COALESCE(e.fx_missing, FALSE) OR COALESCE(s.fx_missing, FALSE) AS fx_missing,
               COALESCE(e.n_assumed, 0) AS n_assumed, COALESCE(e.rebuilt, FALSE) AS end_rebuilt,
               s.currency AS start_currency, s.rate AS start_rate, s.observed_at AS start_observed_at,
               CASE WHEN s.complete THEN s.unrealized + s.book_value END AS start_clean,
               CASE WHEN s.complete THEN s.book_value END AS start_book,
               CASE WHEN s.complete THEN s.unrealized END AS start_unrealized
          FROM en e FULL JOIN st s ON s.b_from = e.b_from AND s.out_currency = e.out_currency
               AND s.missing_basis = e.missing_basis AND s.silver_source_id = e.silver_source_id
               AND s.account_external_id = e.account_external_id AND s.k = e.k),
    -- Each instrument's clean price per unit at a bucket's start, in a
    -- source that held it then, where every row of it states its
    -- unrealized change: a sold lot's start value is its quantity at
    -- this price. A holding valued by amount, with no quantity, gives
    -- its start gain and cost basis instead: a lot's share is its
    -- basis over the holding's. Accounts observed on different days
    -- convert at different rates; the price takes the rate of the
    -- newest observation, the one nearest the bucket's start. Two
    -- observations of one day share a rate.
    prices AS (
        SELECT b_from, out_currency, missing_basis, silver_source_id, k,
               any_value(start_currency) AS currency, arg_max(start_rate, start_observed_at) AS rate,
               SUM(quantity_start)::DOUBLE AS quantity,
               CASE WHEN bool_and(quantity_start > 0)
                    THEN SUM(start_clean)::DOUBLE / SUM(quantity_start)::DOUBLE END AS unit_clean,
               SUM(start_book)::DOUBLE AS book, SUM(start_unrealized)::DOUBLE AS unrealized
          FROM pos
         WHERE quantity_start > 0 OR quantity_start IS NULL
         GROUP BY ALL
        HAVING bool_and(NOT change_unknown AND start_clean IS NOT NULL AND start_rate IS NOT NULL)
           AND COUNT(DISTINCT start_currency) = 1),
    held AS (
        SELECT DISTINCT p.silver_source_id, p.account_external_id,
               COALESCE(ha.to_key, holding_key(p.instrument_external_id, p.position_key)) AS k
          FROM positions p
          LEFT JOIN aliases ha ON ha.silver_source_id = p.silver_source_id
               AND (ha.account_external_id = '' OR ha.account_external_id = p.account_external_id)
               AND ha.from_key = holding_key(p.instrument_external_id, p.position_key)),
    -- A holding's lots reach the sums in no fixed order. Floats added
    -- in another order can differ in the last bits. So each sum adds
    -- its values in ascending order.
    sold AS (
        SELECT r.b_from, r.out_currency, r.missing_basis, r.silver_source_id, r.account_external_id, r.k,
               min(r.symbol) AS symbol, min(r.description) AS description,
               min(r.currency) AS currency,
               SUM(r.gain_x ORDER BY r.gain_x) AS realized_x,
               SUM(r.gain_x ORDER BY r.gain_x) FILTER (WHERE r.term = 'short') AS realized_short_x,
               SUM(r.gain_x ORDER BY r.gain_x) FILTER (WHERE r.term = 'long') AS realized_long_x,
               SUM(r.gain_x ORDER BY r.gain_x) FILTER (WHERE r.term IS NULL) AS realized_other_x,
               SUM(r.book_x ORDER BY r.book_x) FILTER (WHERE r.gain_x IS NOT NULL) AS realized_book_x,
               SUM(r.proceeds_x ORDER BY r.proceeds_x) AS proceeds_x, SUM(r.wash_x ORDER BY r.wash_x) AS wash_x,
               COUNT(*) AS n_lots,
               COUNT(*) FILTER (WHERE r.gain IS NULL) AS n_without_gain,
               COUNT(*) FILTER (WHERE r.undated) AS n_undated,
               COUNT(*) FILTER (WHERE h.k IS NULL) AS n_unmatched,
               COUNT(*) FILTER (WHERE r.fx_missing) AS n_fx_missing,
               COUNT(*) FILTER (WHERE r.assumed_zero) AS n_assumed,
               COUNT(*) FILTER (WHERE r.document_kind = 'engine') AS n_engine,
               SUM(r.released_x ORDER BY r.released_x) AS released_x
          FROM (
              -- released_x: what a lot sold in the bucket had gained by
              -- its start, if it was held then (an undated lot counts
              -- as held): its quantity at the start price, less its
              -- cost, at the start price's rate; on a holding with no
              -- quantity, the holding's start gain by the lot's share of
              -- its basis. Lots that sell more than the source held at
              -- the start release in proportion. A document's CUSIP
              -- takes the price of the ticker it stands for
              -- (instrument_aliases).
              SELECT y.* EXCLUDE (release, quantity_start, book_start, by_unit),
                     y.release * least(1, CASE WHEN y.by_unit
                         THEN y.quantity_start / SUM(y.quantity) FILTER (WHERE y.release IS NOT NULL) OVER w
                         ELSE y.book_start / SUM(y.book_value) FILTER (WHERE y.release IS NOT NULL) OVER w END) AS released_x
                FROM (
                  SELECT x.*, bk.b_from, bk.b_to, pr.k AS pk, pr.quantity AS quantity_start, pr.book AS book_start,
                         pr.unit_clean IS NOT NULL AS by_unit,
                         CASE WHEN NOT (x.acquisition_date IS NULL OR x.acquisition_date < CAST(epoch_ms(bk.b_from * 1000) AS DATE))
                                   OR x.book_value IS NULL OR x.currency <> pr.currency THEN NULL
                              WHEN pr.unit_clean IS NOT NULL
                              THEN (x.quantity::DOUBLE * pr.unit_clean - x.book_value::DOUBLE) * pr.rate
                              WHEN pr.book > 0
                              THEN pr.unrealized * (x.book_value::DOUBLE / pr.book) * pr.rate END AS release
                    FROM (SELECT x0.* REPLACE (COALESCE(ha.to_key, x0.k) AS k)
                            FROM (SELECT *, lot_key(instrument_external_id, instrument_hint, description) AS k
                                    FROM realized_lots_in_all((SELECT MIN(b_from) FROM bk), p_to, p_ccys, FALSE, p_readings)) x0
                            LEFT JOIN aliases ha ON ha.silver_source_id = x0.silver_source_id
                                 AND (ha.account_external_id = '' OR ha.account_external_id = x0.account_external_id)
                                 AND ha.from_key = x0.k) x
                    JOIN bk ON x.effective_at BETWEEN bk.b_from AND bk.b_to
                    LEFT JOIN instrument_aliases() al ON al.src = x.silver_source_id AND al.alias = x.k
                    LEFT JOIN prices pr ON pr.b_from = bk.b_from AND pr.out_currency = x.out_currency
                         AND pr.missing_basis = x.missing_basis AND pr.silver_source_id = x.silver_source_id
                         AND pr.k = COALESCE(al.k, x.k)) y
                WINDOW w AS (PARTITION BY y.b_from, y.out_currency, y.missing_basis, y.silver_source_id, y.pk)) r
          LEFT JOIN held h ON h.silver_source_id = r.silver_source_id AND h.account_external_id = r.account_external_id
               AND h.k = r.k
         GROUP BY r.b_from, r.out_currency, r.missing_basis, r.silver_source_id, r.account_external_id, r.k),
    events AS (
        SELECT bk.b_from, e.silver_source_id, e.account_external_id,
               COALESCE(ha.to_key, e.instrument_external_id, '') AS k,
               COUNT(*) FILTER (WHERE e.event = 'sell') AS n_sells,
               COUNT(*) FILTER (WHERE e.event = 'sell' AND NOT e.documented AND NOT e.rebuilt) AS n_undocumented,
               COUNT(*) FILTER (WHERE e.rebuilt) AS n_rebuilt,
               COUNT(*) FILTER (WHERE e.event = 'in_kind') AS n_in_kind,
               COUNT(*) FILTER (WHERE e.event = 'corporate_action') AS n_corporate
          FROM gains_events((SELECT MIN(b_from) FROM bk), p_to) e
          JOIN bk ON e.occurred_at BETWEEN bk.b_from AND bk.b_to
          LEFT JOIN aliases ha ON ha.silver_source_id = e.silver_source_id
               AND (ha.account_external_id = '' OR ha.account_external_id = e.account_external_id)
               AND ha.from_key = e.instrument_external_id
         GROUP BY ALL),
    lot_src AS (SELECT * FROM lot_sources()),
    found AS (
        SELECT bk.b_from, f.silver_source_id, f.account_external_id, COALESCE(ha.to_key, f.instrument_external_id) AS k,
               COUNT(*) FILTER (WHERE f.finding = 'seed') AS n_seed,
               COUNT(*) FILTER (WHERE f.finding = 'implied') AS n_implied,
               COUNT(*) FILTER (WHERE f.finding = 'blip') AS n_blip,
               COUNT(*) FILTER (WHERE f.finding = 'wash_sale_window') AS n_wash,
               COUNT(*) FILTER (WHERE f.finding = 'fee_unvalued') AS n_fee_unvalued
          FROM lot_findings f
          JOIN lot_src s ON s.silver_source_id = f.silver_source_id AND s.mode = 'fill'
          JOIN bk ON f.found_at BETWEEN bk.b_from AND bk.b_to
          LEFT JOIN aliases ha ON ha.silver_source_id = f.silver_source_id
               AND (ha.account_external_id = '' OR ha.account_external_id = f.account_external_id)
               AND ha.from_key = f.instrument_external_id
         WHERE f.finding IN ('seed', 'implied', 'blip', 'wash_sale_window', 'fee_unvalued')
         GROUP BY ALL),
    -- A seed a stated lot resolved, open at the bucket's end, on the
    -- key's account: the account itself, or its portfolio's pool.
    spliced AS (
        SELECT DISTINCT bk.b_from, l.silver_source_id, l.account_external_id,
               COALESCE(ha.to_key, l.instrument_external_id) AS k
          FROM lots l JOIN bk ON l.opened_at <= bk.b_to AND (l.closed_at IS NULL OR l.closed_at > bk.b_to)
          LEFT JOIN aliases ha ON ha.silver_source_id = l.silver_source_id
               AND (ha.account_external_id = '' OR ha.account_external_id = l.account_external_id)
               AND ha.from_key = l.instrument_external_id
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

CREATE MACRO gains_windows(p_from, p_to, p_ccy, p_period, p_missing := 'ignore') AS TABLE (
    SELECT * EXCLUDE (out_currency, missing_basis)
      FROM gains_windows_all(p_from, p_to, p_period, [p_ccy], [p_missing])
);

-- report_gains_buckets: the gains per bucket and grain under
-- p_missing, with the engine's flags.
CREATE MACRO report_gains_buckets(p_from, p_to, p_ccy, p_period, p_grain, p_missing := 'ignore') AS TABLE (
    WITH g AS (
        SELECT w.*,
               CASE WHEN p_grain = 'all' THEN '' ELSE silver_source_id END AS g_src,
               CASE WHEN p_grain = 'portfolios' THEN portfolio_bucket ELSE '' END AS g_pid,
               CASE WHEN p_grain = 'accounts' THEN account_external_id ELSE '' END AS g_acct
          FROM gains_windows(p_from, p_to, p_ccy, p_period, p_missing := p_missing) w),
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
               SUM(n_undocumented) AS n_undocumented, SUM(n_rebuilt) AS n_rebuilt,
               SUM(n_without_gain) AS n_without_gain,
               SUM(n_undated) AS n_undated, SUM(n_in_kind) AS n_in_kind, SUM(n_corporate) AS n_corporate,
               COUNT(*) FILTER (WHERE basis_changed) AS n_basis_changed,
               COUNT(DISTINCT silver_source_id || '|' || account_external_id) FILTER (WHERE account_unobserved) AS n_unobserved,
               bool_or(paid_in) AS paid_in,
               list_sort(list_distinct(list(onboarded_source) FILTER (WHERE onboarded_source IS NOT NULL))) AS onboarded,
               SUM(n_seed) AS n_seed, SUM(n_implied) AS n_implied, SUM(n_blip) AS n_blip,
               bool_or(spliced) AS spliced, bool_or(pooled) AS pooled, bool_or(settlement) AS settlement,
               SUM(n_wash) AS n_wash, SUM(n_fee_unvalued) AS n_fee_unvalued,
               SUM(n_assumed_zero) AS n_assumed_zero,
               SUM(n_fx_missing) AS n_fx_missing
          FROM g GROUP BY b_from, g_src, g_pid, g_acct)
    SELECT CASE WHEN p_period = 'total' THEN NULL ELSE m.b_from END AS period_start,
           NULLIF(m.g_src, '') AS silver_source_id,
           CASE WHEN p_grain = 'portfolios' THEN m.g_pid END AS portfolio_external_id,
           pb.display_name AS portfolio_display_name,
           NULLIF(m.g_acct, '') AS account_external_id,
           a.display_name, a.nickname, a.account_kind, account_wrapper(a.tax_wrapper) AS tax_wrapper, a.account_category, a.relationship_id,
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
           gains_quality(m.n_undocumented, m.n_rebuilt, m.n_without_gain, m.n_undated, 0, m.n_in_kind,
                         m.n_corporate, m.n_basis_changed, m.n_unobserved, m.paid_in, m.onboarded,
                         m.n_seed, m.n_implied, m.n_blip, m.spliced, m.pooled, m.settlement, m.n_wash,
                         m.n_fee_unvalued, m.n_assumed_zero, m.n_fx_missing) AS quality
      FROM m
      LEFT JOIN portfolio_buckets() pb ON p_grain = 'portfolios' AND pb.src = m.g_src AND pb.pid = m.g_pid
      LEFT JOIN accounts a ON p_grain = 'accounts' AND a.silver_source_id = m.g_src AND a.account_external_id = m.g_acct
     ORDER BY m.b_from, m.g_src, m.g_pid, m.g_acct
);

-- report_gains_positions: the gains per holding of the window under
-- p_missing, with the engine's flags.
CREATE MACRO report_gains_positions(p_from, p_to, p_ccy, p_missing := 'ignore') AS TABLE (
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
           gains_quality(n_undocumented, n_rebuilt, n_without_gain, n_undated, n_unmatched, n_in_kind,
                         n_corporate, CAST(basis_changed AS INTEGER), CAST(account_unobserved AS INTEGER), paid_in,
                         CASE WHEN onboarded_source IS NULL THEN [] ELSE [onboarded_source] END,
                         n_seed, n_implied, n_blip, spliced, pooled, settlement, n_wash, n_fee_unvalued,
                         n_assumed_zero, n_fx_missing) AS quality
      FROM gains_windows(p_from, p_to, p_ccy, 'total', p_missing := p_missing)
     ORDER BY silver_source_id, account_external_id, COALESCE(end_key, start_key, k)
);
