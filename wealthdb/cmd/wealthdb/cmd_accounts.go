package main

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// ---- column registry -----------------------------------------------------

// buildAccountColumnRegistry is the accounts view: one row per account
// with its taxonomy and its positions, cash and total value, each in
// the account's own base_currency and in the output currency.
func buildAccountColumnRegistry(outCcy string) []columnSpec[gold.AccountRow] {
	return []columnSpec[gold.AccountRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return a.SilverSourceID }},
		// SnapshotAt is 0 only when the silver source has produced no
		// observations at all, which shows as a blank.
		dateCol("snapshot_date", func(a gold.AccountRow) int64 { return a.SnapshotAt }),
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(a gold.AccountRow) string { return accountLabel(a.DisplayName, a.AccountExternalID) }},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(a gold.AccountRow) string { return a.AccountExternalID }},
		{Name: "account_kind", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return a.AccountKind }},
		// Defaults applied at render time, so the cell is never blank
		// when neither the adapter nor an override classified the
		// account. The column itself stays NULL, so gold still tells
		// "unknown" from "explicitly the default".
		{Name: "tax_wrapper", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string {
				if a.TaxWrapper != nil {
					return *a.TaxWrapper
				}
				return "taxable_personal"
			}},
		{Name: "management_style", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string {
				if a.ManagementStyle != nil {
					return *a.ManagementStyle
				}
				return "self_directed"
			}},
		textCol("base_currency", func(a gold.AccountRow) *string { return a.BaseCurrency }),
		{Name: "relationship_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.RelationshipID) }},
		textCol("account_nickname", func(a gold.AccountRow) *string { return a.Nickname }),
		textCol("account_category", func(a gold.AccountRow) *string { return a.AccountCategory }),
		{Name: "portfolio_external_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.PortfolioExternalID) }},

		// Base-currency aggregates. The header has no _<CCY> suffix
		// because each row's value is in its OWN base_currency: there
		// is no single column-wide currency to advertise. Blank when
		// the account's base_currency is nil.
		moneyCol("positions_value", "", func(a gold.AccountRow) *string { return a.PositionsValueBase }),
		moneyCol("cash_balance", "", func(a gold.AccountRow) *string { return a.CashBalanceBase }),
		moneyCol("total_value", "", func(a gold.AccountRow) *string { return a.TotalValueBase }),

		// Output-currency aggregates, one column-wide currency (the
		// -x/--currency choice), so the header carries the suffix.
		outCcyTwin("positions_value", outCcy, func(a gold.AccountRow) *string { return a.PositionsValueOutCcy }),
		outCcyTwin("cash_balance", outCcy, func(a gold.AccountRow) *string { return a.CashBalanceOutCcy }),
		outCcyTwin("total_value", outCcy, func(a gold.AccountRow) *string { return a.TotalValueOutCcy }),
	}
}

var defaultAccountColumns = []string{
	"silver_source", "snapshot_date", "account",
	"account_kind", "tax_wrapper", "management_style",
	"base_currency", "positions_value", "cash_balance", "total_value",
	"total_value_outccy",
}

func accountsUsage() string {
	registry := buildAccountColumnRegistry("CCY")
	return `usage: wealthdb holdings accounts [-d YYYY-MM-DD] [-f FORMAT] [-C COLS] [-x CCY] [-p]

Print one row per account: its source, its kind (brokerage, cash,
card, mortgage, custody, …), its tax wrapper (taxable, roth_ira,
401k, pillar_3a, …), and its positions, cash and total value as of
the date. A loan or a card balance is negative. Property, loans and
private holdings recorded by hand are accounts like any other.
Base-currency aggregates (positions_value, cash_balance,
total_value) are blank for accounts with no base_currency. The
matching _<CCY> aggregates use the -x/--currency choice and stay
populated whenever at least one underlying line resolves an FX
path to that currency.

Flags:
  -d, --as-of YYYY-MM-DD   as-of date (default: today UTC)
  -f, --format FORMAT      output format (table | csv | csv_plain | json)
  -C, --columns COLS       comma-separated column names, 'default', 'all', or
                           a +ADD,...-REMOVE,... delta against the default set
                           (e.g. -C+account_id-cash_balance)
  -x, --currency CCY       output currency for the _<CCY> aggregate columns (default: config.default_currency)
  -p, --privacy            redact account IDs and monetary amounts
                           (table: visible placeholders; csv: empty cells; json: keys omitted)

Available columns:
  ` + joinColumnNames(registry) + `

  (The 'positions_value_outccy', 'cash_balance_outccy', and
   'total_value_outccy' columns render as 'positions_value_<CCY>',
   'cash_balance_<CCY>', and 'total_value_<CCY>' in the header,
   reflecting your -x/--currency choice.)

Default column set:
  ` + strings.Join(defaultAccountColumns, ", ")
}
