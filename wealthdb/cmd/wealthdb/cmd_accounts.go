package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// cmdAccounts is the accounts-grain view of the portfolio. One row
// per registered account with the account's promoted columns plus
// derived aggregates: total non-cash positions value, cash value,
// and grand total, each expressed both in the account's own
// base_currency (when known) and in the requested output
// currency. See cmd_positions.go for the parallel positions-grain
// view.
func cmdAccounts(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb holdings accounts", flag.ContinueOnError)
	fs.SetOutput(stderr)

	hf := registerHoldingsFlags(fs, holdingsFlagSpec{
		cmd:           "accounts",
		currencyUsage: "output currency for the _<CCY> aggregate columns (default: config.default_currency)",
		privacyUsage:  "redact account IDs / monetary amounts in the output",
		withColumns:   true,
	})
	fs.Usage = func() {
		fmt.Fprintln(stderr, accountsUsage())
	}
	if err := fs.Parse(splitFusedColumnsFlag(subargs)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "accounts: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "accounts: unexpected positional argument %q", fs.Arg(0))
	}

	hv, err := hf.resolve(g)
	if err != nil {
		return err
	}

	colSet, err := resolveAccountColumns(*hf.cols, hv.outCcy)
	if err != nil {
		return errs.Newf(2, "accounts: %s", err.Error())
	}

	db, err := openGoldForRead(g, hv.cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := gold.AccountsAsOf(ctx, db, hv.asOfEpoch, hv.outCcy)
	if err != nil {
		return err
	}

	return writeFormatted(stdout, hv.fmtChoice, rowsToTable(rows, colSet, *hf.privacy, hv.fmtChoice))
}

// ---- column registry -----------------------------------------------------

func buildAccountColumnRegistry(outCcy string) []columnSpec[gold.AccountRow] {
	suffix := "_" + outCcy
	return []columnSpec[gold.AccountRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return a.SilverSourceID }},
		{Name: "snapshot_date", Align: output.AlignLeft, Extract: func(a gold.AccountRow) string {
			// SnapshotAt is 0 only when the silver source has
			// produced no observations at all — show nothing
			// in that genuinely-unknown case.
			if a.SnapshotAt == 0 {
				return ""
			}
			return formatDate(a.SnapshotAt)
		}},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID, Extract: func(a gold.AccountRow) string {
			if a.DisplayName != nil && *a.DisplayName != "" {
				return *a.DisplayName
			}
			return a.AccountExternalID
		}},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(a gold.AccountRow) string { return a.AccountExternalID }},
		{Name: "account_kind", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return a.AccountKind }},
		// Defaults applied at render time so the rendered cell is
		// never blank when the adapter / overrides haven't
		// classified the account. The underlying column stays NULL — gold's
		// distinguishes "unknown" from "explicitly default" via
		// the database, the CLI surfaces the conventional default.
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
		{Name: "base_currency", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.BaseCurrency) }},
		{Name: "relationship_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.RelationshipID) }},
		{Name: "account_nickname", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.Nickname) }},
		{Name: "account_category", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.AccountCategory) }},
		{Name: "portfolio_external_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.PortfolioExternalID) }},

		// Base-currency aggregates. The header has no _<CCY>
		// suffix because each row's value is in its OWN
		// base_currency — there's no single column-wide currency
		// to advertise. Blank when the account's base_currency is
		// nil.
		{Name: "positions_value", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(a gold.AccountRow) string { return formatCents(a.PositionsValueBase) }},
		{Name: "cash_balance", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(a gold.AccountRow) string { return formatCents(a.CashBalanceBase) }},
		{Name: "total_value", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(a gold.AccountRow) string { return formatCents(a.TotalValueBase) }},

		// Output-currency aggregates. Single column-wide currency
		// (the -x/--currency choice) so the header carries
		// the suffix.
		{Name: "positions_value_outccy", Header: "positions_value" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(a gold.AccountRow) string { return formatCents(a.PositionsValueOutCcy) }},
		{Name: "cash_balance_outccy", Header: "cash_balance" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(a gold.AccountRow) string { return formatCents(a.CashBalanceOutCcy) }},
		{Name: "total_value_outccy", Header: "total_value" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(a gold.AccountRow) string { return formatCents(a.TotalValueOutCcy) }},
	}
}

var defaultAccountColumns = []string{
	"silver_source", "snapshot_date", "account",
	"account_kind", "tax_wrapper", "management_style",
	"base_currency", "positions_value", "cash_balance", "total_value",
	"total_value_outccy",
}

func resolveAccountColumns(flagValue, outCcy string) ([]columnSpec[gold.AccountRow], error) {
	return resolveColumns(flagValue, defaultAccountColumns, buildAccountColumnRegistry(outCcy))
}

func accountsUsage() string {
	registry := buildAccountColumnRegistry("CCY")
	return `usage: wealthdb holdings accounts [-d YYYY-MM-DD] [-f FORMAT] [-C COLS] [-x CCY] [-p]

Print one row per registered account, with derived aggregate
columns rolled up over the account's positions and cash balances.
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
