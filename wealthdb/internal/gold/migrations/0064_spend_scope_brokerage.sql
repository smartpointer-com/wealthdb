-- A brokerage account can be a chequing account.
--
-- The scope defaulted to `cash` and `card` on the reading that a
-- brokerage account holds investments and a deposit account spends.
-- That is not how the products are sold. Schwab One is the clearest
-- case — one account, a brokerage with a debit card and bill pay
-- attached — and it is not alone; a managed account pays its own
-- management fees, its own withholding, and wires cash out to
-- whoever the holder tells it to. Every one of those is an outflow,
-- and a scope that cannot see them reports a household that never
-- pays its manager.
--
-- What this does NOT do is admit the investing. The population
-- (migration 0041, re-issued 0055) selects on transaction KIND —
-- `purchase`, `refund`, `reward`, `withdrawal`, `fee`, `tax`, and
-- interest only when it is charged — so a buy, a sell, a dividend, a
-- journal and a corporate action stay outside it whatever account
-- they sit on. Widening the account set therefore widens the rows by
-- exactly the outflows, which is the whole intent.
--
-- Two consequences worth naming, because both are the design working
-- rather than a surprise:
--
--   * a brokerage's cash withdrawals are usually money moving to the
--     holder's OWN bank. Those now enter the population from the
--     sending side too, and the internal-transfer matcher — whose
--     pool has always spanned every account, not just the scoped
--     ones (migration 0044) — gets first refusal and nets the pair
--     out. A withdrawal to somewhere the product does not track
--     stays as spend, which is correct.
--   * a fee on a brokerage account is two different animals: the
--     account's own management fee, and a security-level
--     pass-through such as an ADR depositary charge, booked per
--     position. Both are real money out and both belong in the base;
--     what separates them is the narrative, and the rule tier reads
--     it (internal/spending/rules.go).
--
-- `donor_advised_fund` is deliberately NOT added. A grant out of one
-- is charitable giving, but the money left the household when it was
-- contributed — counting the grant too would count it twice. An
-- account that should be in either way is what `spend_account_scope`
-- is for; this changes only the default.
CREATE OR REPLACE MACRO spend_scoped_accounts() AS TABLE (
    SELECT a.silver_source_id, a.account_external_id, a.account_kind,
           a.display_name, a.nickname, a.account_category,
           a.portfolio_external_id
      FROM accounts a
      LEFT JOIN spend_account_scope s
             ON s.silver_source_id    = a.silver_source_id
            AND s.account_external_id = a.account_external_id
     WHERE COALESCE(s.mode,
                    CASE WHEN a.account_kind IN ('cash', 'card', 'brokerage')
                         THEN 'include' ELSE 'exclude' END) = 'include'
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (64, CAST(epoch(now()) AS BIGINT));
