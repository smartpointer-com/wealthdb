-- ============================================================
-- gold schema — the roles of the vocabulary, as data.
--
-- The derived set stamps three tables from the binary's own tables
-- (internal/canonical): spend_categories, whole, and the two kind
-- tables below. The flow macros read a value's or a kind's role there,
-- or through a predicate held to it, rather than name the values.
--
-- spend_categories gains a value's roles (canonical.CategoryRoles):
--
--   leaves_spending,     a line of the value leaves the family's base.
--   leaves_income
--   flow_place           where cash flow places a line of the value
--                        whatever its kind: mortgage, vehicle_delta,
--                        loans, investing. NULL for any other value.
--   flow_class_in        the operating-in class of a receipt, where it
--                        is not other_receipts.
--   flow_class_out       the operating-out class of an outflow, where it
--                        is not consumption; on a crossing, its pool.
--   flow_leaf_detailed   an outflow draws as its own leaf, not under
--                        its primary.
--   fee_view_group       a BANK_FEES value's group in the fee view.
--   projected_income     the income projection projects the type.
--
-- account_kinds holds an account kind's roles (canonical.AccountKindRow)
-- and txn_kind_roles a transaction kind's (canonical.TxKindRow). A side
-- is `any`, `negative` or `positive`: the sign a row of the kind must
-- carry to be in the family's population. NULL is no side.
--
-- The rows arrive with the derived set, which follows this migration
-- and stamps the schema version. Each statement can run twice.
-- ============================================================

ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS leaves_spending    BOOLEAN;
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS leaves_income      BOOLEAN;
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS flow_place         TEXT;
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS flow_class_in      TEXT;
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS flow_class_out     TEXT;
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS flow_leaf_detailed BOOLEAN;
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS fee_view_group     TEXT;
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS projected_income   BOOLEAN;

CREATE TABLE IF NOT EXISTS account_kinds (
    kind              TEXT PRIMARY KEY,
    liability         BOOLEAN NOT NULL,
    holds_cash        BOOLEAN NOT NULL,
    follows_source    BOOLEAN NOT NULL,
    returns_invisible BOOLEAN NOT NULL
);

CREATE TABLE IF NOT EXISTS txn_kind_roles (
    kind          TEXT PRIMARY KEY,
    spending_side TEXT,
    income_side   TEXT,
    matcher_pool  BOOLEAN NOT NULL,
    unsigned      BOOLEAN NOT NULL,
    trade         BOOLEAN NOT NULL,
    income_floor  TEXT,
    spend_floor   TEXT
);
