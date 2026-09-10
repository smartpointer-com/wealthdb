-- `card_spend` reads as what it is: card spend nothing has itemised.
--
-- The value's policy is migration 0046's: a bill paid to a card issuer
-- whose card wealthdb does not hold. It stays IN the spending base
-- because it is the only trace of that consumption, and it is replaced
-- by the purchases themselves the day the card is collected.
--
-- Its label, read off the value by the mechanical rule (migration
-- 0058), was "Card spend" — which is true of every card purchase in
-- the product. So on a chart of merchant categories it read as a KIND
-- of spending sitting beside Groceries and Travel, rather than as the
-- placeholder it is, and nothing told a reader that the line stands
-- for purchases nobody has seen.
--
-- "Uncategorized card spend" says both halves. It sits beside
-- '(uncategorized)' without meaning the same thing: that one is a line
-- no tier could place, this one is a bill whose purchases are not in
-- the product at all.
--
-- Label only, at both levels — a delta is its own primary, so the two
-- label columns carry the same string. The value stays `card_spend`:
-- it is the join key, what a rule and a pin write, and what the
-- gauntlet refuses a model. canonical.spendLabelOverrides is the same
-- correction in Go, and TestSpendCategoryLabelsMatchGoTable holds the
-- two together.
UPDATE spend_categories
   SET label         = 'Uncategorized card spend',
       primary_label = 'Uncategorized card spend'
 WHERE spend_detailed = 'card_spend';

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (62, CAST(epoch(now()) AS BIGINT));
