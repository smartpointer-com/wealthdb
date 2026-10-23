package main

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// portfolioNamePrivacy picks the right redaction class for the
// portfolio column's display value, which mixes different
// conventions across sources:
//
//   - the sentinel row for a source's orphan accounts renders the
//     literal "(no portfolio)". It is a structural marker with no
//     row content behind it, so it stays legible for every source.
//   - a source whose names may be typed (typedAccountLabel) shows
//     the customer's words: the cointracking adapter emits the
//     free-form CT account name as display_name, and a manual or
//     declared portfolio is named, or keyed, by hand. These can be
//     anything — one word or several — so they print as a pseudonym
//     whatever their shape, the id fallback too (portfolioCol). A
//     tracker's `cu_<user>` id names its user and prints as one too
//     (typedIDCol).
//   - UBS / other Swiss-source adapters emit bank-assigned
//     portfolio category labels (Savings / Brokerage / …).
//     These pass through under the PrivacyAccountID heuristic
//     because their info content is taxonomy, not identifier.
//
// kindOf resolves a silver_source_id to its silver_kind — the
// config-side id is free-form, so matching the id string instead
// would silently lose redaction for a source named anything but
// the literal adapter name.
func portfolioNamePrivacy(kindOf func(string) string) func(gold.PortfolioRow) PrivacyClass {
	return func(r gold.PortfolioRow) PrivacyClass {
		if r.PortfolioExternalID == "" {
			return PrivacyNone
		}
		if typedAccountLabel(kindOf, r.SilverSourceID) {
			return PrivacyPseudonym
		}
		return PrivacyAccountID
	}
}

// ---- column registry -----------------------------------------------------

// buildPortfolioColumnRegistry is the portfolios view: one row per
// portfolio plus one sentinel row per source for the accounts it did
// not group, so that Σ total_value_<CCY> equals the accounts view's.
func buildPortfolioColumnRegistry(outCcy string, kindOf func(string) string) []columnSpec[gold.PortfolioRow] {
	return []columnSpec[gold.PortfolioRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return r.SilverSourceID }},
		dateCol("snapshot_date", func(r gold.PortfolioRow) int64 { return r.SnapshotAt }),
		// Sentinel rows render as "(no portfolio)" so they stand out
		// at a glance.
		portfolioCol("portfolio", kindOf, func(r gold.PortfolioRow) (string, string, *string) {
			return r.SilverSourceID, r.PortfolioExternalID, r.DisplayName
		}),
		typedIDCol("portfolio_id", kindOf, func(r gold.PortfolioRow) (string, string) {
			return r.SilverSourceID, r.PortfolioExternalID
		}),
		textCol("base_currency", func(r gold.PortfolioRow) *string { return r.BaseCurrency }),
		{Name: "relationship_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.RelationshipID) }},
		nicknameCol("portfolio_nickname", func(r gold.PortfolioRow) *string { return r.Nickname }),
		// Portfolio-level taxonomy rollups. Strict semantics in
		// the gold layer: tax_wrapper non-nil only when every
		// component account agrees AND none is unclassified;
		// management_style non-nil only when every non-overlay
		// component agrees (and none is unclassified). Blank
		// cells genuinely mean "ambiguous / mixed / unknown" —
		// not the same as the accounts table's render-time
		// default fallback, where blank would silently show
		// taxable_personal / self_directed.
		textCol("tax_wrapper", func(r gold.PortfolioRow) *string { return r.TaxWrapper }),
		textCol("management_style", func(r gold.PortfolioRow) *string { return r.ManagementStyle }),

		moneyCol("positions_value", "", func(r gold.PortfolioRow) *string { return r.PositionsValueBase }),
		moneyCol("cash_balance", "", func(r gold.PortfolioRow) *string { return r.CashBalanceBase }),
		moneyCol("total_value", "", func(r gold.PortfolioRow) *string { return r.TotalValueBase }),

		outCcyTwin("positions_value", outCcy, func(r gold.PortfolioRow) *string { return r.PositionsValueOutCcy }),
		outCcyTwin("cash_balance", outCcy, func(r gold.PortfolioRow) *string { return r.CashBalanceOutCcy }),
		outCcyTwin("total_value", outCcy, func(r gold.PortfolioRow) *string { return r.TotalValueOutCcy }),
	}
}

var defaultPortfolioColumns = []string{
	"silver_source", "snapshot_date", "portfolio",
	"tax_wrapper", "management_style", "base_currency",
	"positions_value", "cash_balance", "total_value",
	"total_value_outccy",
}

func portfoliosUsage() string {
	registry := buildPortfolioColumnRegistry("CCY", func(string) string { return "" })
	return `usage: wealthdb holdings portfolios [-d YYYY-MM-DD] [-f FORMAT] [-C COLS] [-x CCY] [-p]

Print one row per portfolio (a grouping of accounts the source
manages together) plus one row per source for the accounts it did
not group. Sum of total_value_<CCY> across all rows equals the same
sum from 'wealthdb holdings accounts', which equals 'wealthdb
holdings positions --with-cash'.

Flags:
  -d, --as-of YYYY-MM-DD   as-of date (default: today UTC)
  -f, --format FORMAT      output format (table | csv | csv_plain | json)
  -C, --columns COLS       comma-separated column names, 'default', 'all', or
                           a +ADD,...-REMOVE,... delta against the default set
                           (e.g. -C+relationship_id-cash_balance)
  -x, --currency CCY       output currency for the _<CCY> aggregate columns (default: config.default_currency)
  -p, --privacy            redact portfolio / account IDs and monetary amounts; every
                           name a person typed prints as a pseudonym, such as manual-1,
                           or as a redacted ID
                           (table: visible placeholders; csv: empty cells; json: keys omitted)

Available columns:
  ` + joinColumnNames(registry) + `

  ('positions_value_outccy', 'cash_balance_outccy',
   'total_value_outccy' render as positions_value_<CCY> etc.)

Default column set:
  ` + strings.Join(defaultPortfolioColumns, ", ")
}
