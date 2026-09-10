-- Every account can spend, so the kind gate goes.
--
-- The scope has been widened twice by adding a kind to a list:
-- `cash` and `card` at the start, `brokerage` in migration 0064 once
-- it was clear that a managed account pays its own fees. The list was
-- wrong both times in the same way, and a third entry would only
-- postpone the third time.
--
-- What the kinds actually gate is nothing. An account is a place
-- money sits; whether it can be charged is not a property of its
-- type. Safekeeping depots carry custody prices, private-market
-- custody accounts carry execution fees, loan accounts carry
-- interest, and a cash account attached to a mandate carries that
-- mandate's fees whatever the product is called. None of those kinds
-- is `cash`, `card` or `brokerage`, and a scope built from a list of
-- kinds cannot see any of them.
--
-- So the default becomes `include` and `spend_account_scope` is the
-- ONLY thing that takes an account out. One mechanism instead of
-- two, and the exception is written where exceptions are read
-- rather than compiled into a list nobody looks at.
--
-- This does NOT widen what counts as spending. The population
-- (migration 0041, re-issued 0055) still selects on transaction
-- KIND — `purchase`, `refund`, `reward`, `withdrawal`, `fee`, `tax`,
-- and `interest` only when charged — so a buy, a sell, a dividend, a
-- journal and a corporate action stay out whatever account they sit
-- on. Widening the account set widens the rows by exactly the
-- outflows.
--
-- One consequence is worth naming because it is the design working:
-- a withdrawal out of an account the household also owns the other
-- side of is an internal transfer, and the matcher — whose pool has
-- always spanned every account rather than the scoped ones
-- (migration 0044) — nets the pair out before the base is read. A
-- withdrawal to somewhere gold does not carry stays as spend, which
-- is the honest answer: from the data's point of view the money left.
--
-- `donor_advised_fund` is the kind migration 0064 kept out, on the
-- reading that a grant would double-count giving already booked when
-- the fund was contributed to. That reasoning is unchanged, but it
-- argues about a particular arrangement rather than about a type, so
-- it belongs in `spend_account_scope` where it can be read and
-- argued with.
CREATE OR REPLACE MACRO spend_scoped_accounts() AS TABLE (
    SELECT a.silver_source_id, a.account_external_id, a.account_kind,
           a.display_name, a.nickname, a.account_category,
           a.portfolio_external_id
      FROM accounts a
      LEFT JOIN spend_account_scope s
             ON s.silver_source_id    = a.silver_source_id
            AND s.account_external_id = a.account_external_id
     WHERE COALESCE(s.mode, 'include') = 'include'
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (68, CAST(epoch(now()) AS BIGINT));
