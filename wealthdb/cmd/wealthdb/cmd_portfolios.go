package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
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
//   - the cointracking adapter emits the free-form CT account name
//     as display_name. These are user-chosen and can be anything —
//     one word or several — so they take the free-text class and
//     mask whole. The name is the row label, so the redacted rows
//     of one source look alike; `-C +portfolio_id` puts a
//     distinguishable (account-id-redacted) key back on the row.
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
		if kindOf(r.SilverSourceID) == "cointracking" {
			return PrivacyFreeText
		}
		return PrivacyAccountID
	}
}

// cmdPortfolios is the portfolio-grain rollup. One row per
// registered portfolio plus one sentinel row per silver_source
// that aggregates orphan accounts (no portfolio). The invariant
// is: sum(portfolios.total_value_<CCY>) == sum(accounts.total_value_<CCY>)
// == positions --with-cash total.
func cmdPortfolios(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb holdings portfolios", flag.ContinueOnError)
	fs.SetOutput(stderr)

	hf := registerHoldingsFlags(fs, holdingsFlagSpec{
		cmd:           "portfolios",
		currencyUsage: "output currency for the _<CCY> aggregate columns (default: config.default_currency)",
		privacyUsage:  "redact portfolio / account IDs and monetary amounts in the output",
		withColumns:   true,
	})
	fs.Usage = func() {
		fmt.Fprintln(stderr, portfoliosUsage())
	}
	if err := fs.Parse(splitFusedColumnsFlag(subargs)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "portfolios: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "portfolios: unexpected positional argument %q", fs.Arg(0))
	}

	hv, err := hf.resolve(g)
	if err != nil {
		return err
	}

	rep := holdingsReport(request{view: "portfolios", currency: hv.outCcy, asOf: hv.asOfEpoch})
	open := func() (*sql.DB, error) { return openGoldForRead(g, hv.cfg) }
	return writeReport(ctx, rep, *hf.cols, "portfolios", open, *hf.privacy, hv.fmtChoice, stdout)
}

// ---- column registry -----------------------------------------------------

func buildPortfolioColumnRegistry(outCcy string, kindOf func(string) string) []columnSpec[gold.PortfolioRow] {
	suffix := "_" + outCcy
	return []columnSpec[gold.PortfolioRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return r.SilverSourceID }},
		{Name: "snapshot_date", Align: output.AlignLeft, Extract: func(r gold.PortfolioRow) string {
			if r.SnapshotAt == 0 {
				return ""
			}
			return formatDate(r.SnapshotAt)
		}},
		{Name: "portfolio", Align: output.AlignLeft,
			Privacy:     PrivacyAccountID,
			PrivacyFunc: portfolioNamePrivacy(kindOf),
			Extract: func(r gold.PortfolioRow) string {
				// Sentinel rows render as "(no portfolio)" so they
				// stand out at a glance; real portfolios show their
				// display_name if present, else their external_id.
				if r.PortfolioExternalID == "" {
					return "(no portfolio)"
				}
				if r.DisplayName != nil && *r.DisplayName != "" {
					return *r.DisplayName
				}
				return r.PortfolioExternalID
			}},
		{Name: "portfolio_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.PortfolioRow) string { return r.PortfolioExternalID }},
		{Name: "base_currency", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.BaseCurrency) }},
		{Name: "relationship_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.RelationshipID) }},
		{Name: "portfolio_nickname", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.Nickname) }},
		// Portfolio-level taxonomy rollups. Strict semantics in
		// the gold layer: tax_wrapper non-nil only when every
		// component account agrees AND none is unclassified;
		// management_style non-nil only when every non-overlay
		// component agrees (and none is unclassified). Blank
		// cells genuinely mean "ambiguous / mixed / unknown" —
		// not the same as the accounts table's render-time
		// default fallback, where blank would silently show
		// taxable_personal / self_directed.
		{Name: "tax_wrapper", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.TaxWrapper) }},
		{Name: "management_style", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.ManagementStyle) }},

		{Name: "positions_value", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.PositionsValueBase) }},
		{Name: "cash_balance", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.CashBalanceBase) }},
		{Name: "total_value", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.TotalValueBase) }},

		{Name: "positions_value_outccy", Header: "positions_value" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.PositionsValueOutCcy) }},
		{Name: "cash_balance_outccy", Header: "cash_balance" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.CashBalanceOutCcy) }},
		{Name: "total_value_outccy", Header: "total_value" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.TotalValueOutCcy) }},
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
  -p, --privacy            redact portfolio / account IDs and monetary amounts
                           (table: visible placeholders; csv: empty cells; json: keys omitted)

Available columns:
  ` + joinColumnNames(registry) + `

  ('positions_value_outccy', 'cash_balance_outccy',
   'total_value_outccy' render as positions_value_<CCY> etc.)

Default column set:
  ` + strings.Join(defaultPortfolioColumns, ", ")
}
