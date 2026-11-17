-- Yield: the inputs and figures the yield reports read.

-- fund_risk_signed: fund_risk in the yield report's sign, positive where
-- the fund loses value as rates rise, as a long bond fund does on the
-- whole. A filer's convention is one per filing (a fund on its day, from
-- one source), so the filing flips whole where its dv01 sums below zero,
-- else its dv100 where it states no dv01: every figure, dv100 and the
-- credit-spread figures with dv01, its rows keeping their signs relative
-- to each other. A fund that states a negative duration reads it in that
-- sign instead (yield.fundRisk).
CREATE VIEW fund_risk_signed AS
SELECT silver_source_id, fund_ref, as_of, currency, tenor, dv01 * flip AS dv01, dv100 * flip AS dv100,
       credit_ig_dv01 * flip AS credit_ig_dv01, credit_hy_dv01 * flip AS credit_hy_dv01, payload
  FROM (SELECT *, CASE WHEN COALESCE(SUM(dv01) OVER filing, SUM(dv100) OVER filing) < 0 THEN -1 ELSE 1 END AS flip
          FROM fund_risk
        WINDOW filing AS (PARTITION BY silver_source_id, fund_ref, as_of));

-- yield_realized_pct: the realized yield of a group of report_yield_positions
-- rows, pooled as yield.summarize pools a row's: their income over the sum
-- of their mean daily values, over the rows that weigh, else over every
-- row (docs/YIELD.md §4); none where that sum is zero. Nothing received
-- over a debt reads 0, not -0. The daily amounts are not here, so a
-- group's thin rule, which reads its pooled days, does not apply: the
-- figure stands where the engine's row is thin, and the quality card shows
-- the share tagged thin.
CREATE MACRO yield_realized_pct(received, mean, weighs) AS
    CASE WHEN bool_or(weighs)
         THEN 100 * sum(received) FILTER (WHERE weighs) / NULLIF(sum(mean) FILTER (WHERE weighs), 0)
         ELSE 100 * sum(received) / NULLIF(sum(mean), 0) END + 0.0;

-- yield_rate: the value-weighted average of rate x over a group's rows
-- that state it, weighed as yield.summarize weighs one: over the rows
-- that weigh (no liability, no line of no positive value), else over
-- every row where none weighs (docs/YIELD.md §5). None where the value
-- that states it sums to zero.
CREATE MACRO yield_rate(x, value, weighs) AS
    CASE WHEN bool_or(weighs)
         THEN sum(value * x) FILTER (WHERE weighs AND x IS NOT NULL)
              / NULLIF(sum(value) FILTER (WHERE weighs AND x IS NOT NULL), 0)
         ELSE sum(value * x) FILTER (WHERE x IS NOT NULL) / NULLIF(sum(value) FILTER (WHERE x IS NOT NULL), 0) END;

-- yield_earning_value: the value a group's income yield divides by,
-- weighed as its rates are (yield_rate): the rows that weigh, else every
-- row.
CREATE MACRO yield_earning_value(value, weighs) AS
    CASE WHEN bool_or(weighs) THEN sum(value) FILTER (WHERE weighs) ELSE sum(value) END;

-- yield_part_income: a projection part's income (report_yield_parts) in
-- its holding's currency under rate path `path` and a parallel shift of
-- `shift` basis points, given the month's benchmark rate in percent
-- (yield.Part.Income): the fixed part, the floating principal at the
-- rate plus its spread and the shift, and a rolled holding's income at
-- its rate on the path plus the shift. Each floating rate floors at zero.
CREATE MACRO yield_part_income(fixed, principal, spread, rate, roll_constant, roll_forward, roll_principal, path,
                               shift) AS
    fixed + principal * GREATEST(0, rate + spread + (shift) / 100.0) / 1200
        + CASE WHEN roll_principal = 0 THEN CASE WHEN path = 'forward' THEN roll_forward ELSE roll_constant END
               ELSE roll_principal * GREATEST(0, (CASE WHEN path = 'forward' THEN roll_forward ELSE roll_constant END)
                                                 * 1200 / roll_principal + (shift) / 100.0) / 1200 END;

-- ---- yield_holdings, hash-joined ----------------------------------------

-- yield_holdings: the exposure holdings with their amounts, each
-- holding's kind in the keys it joins on. A cash holding joins positions on a NULL snapshot, and a
-- position joins the cash on a NULL key, so neither matches. In a LEFT
-- JOIN, DuckDB hashes only on equalities between both sides. A condition
-- on the holding alone would make each read loop over every position.
CREATE MACRO yield_holdings(p_asof, p_ccy) AS TABLE (
    SELECT h.*, COALESCE(p.market_value, c.amount)::DOUBLE AS amount, p.quantity::DOUBLE AS quantity,
           p.accrued_interest::DOUBLE AS accrued, p.acquisition_date
      FROM exposure_holdings(p_asof, p_ccy) h
      LEFT JOIN positions p ON p.silver_source_id = h.src AND p.account_external_id = h.acct
           AND p.position_key = h.position_key AND p.snapshot_at = CASE WHEN NOT h.is_cash THEN h.snap END
      LEFT JOIN cash_chosen(p_asof) c ON c.silver_source_id = h.src AND c.account_external_id = h.acct
           AND 'cash:' || c.currency = CASE WHEN h.is_cash THEN h.position_key END
);
