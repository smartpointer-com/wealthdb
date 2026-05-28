package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/config"
	"github.com/ptu/wealthdb/internal/errs"
	"github.com/ptu/wealthdb/internal/gold"
	"github.com/ptu/wealthdb/internal/output"
	"github.com/ptu/wealthdb/internal/pathmode"
)

func init() {
	register("accounts", cmdAccounts)
}

// cmdAccounts is the accounts-grain view of the portfolio. One row
// per registered account with the account's promoted columns plus
// derived aggregates: total non-cash positions value, cash value,
// and grand total, each expressed both in the account's own
// base_currency (when known) and in the user-requested output
// currency. See cmd_positions.go for the parallel positions-grain
// view.
func cmdAccounts(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb accounts", flag.ContinueOnError)
	fs.SetOutput(stderr)

	asOf := fs.String("d", "", "as-of date (YYYY-MM-DD; default today UTC)")
	fs.StringVar(asOf, "as-of", "", "as-of date (YYYY-MM-DD; default today UTC)")
	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	cols := fs.String("C", "default", "columns: comma-separated names, or 'default' / 'all'")
	fs.StringVar(cols, "columns", "default", "columns: comma-separated names, or 'default' / 'all'")
	currency := fs.String("x", "", "output currency for the _<CCY> aggregate columns (default: config.default_currency)")
	fs.StringVar(currency, "currency", "", "output currency (default: config.default_currency)")
	fxMode := fs.String("fx-mode", "historic", "FX rate selection: 'historic' (rate at snapshot time, interpolated) or 'current' (latest available)")
	privacy := fs.Bool("p", false, "redact account IDs / quantities / monetary amounts in the output")
	fs.BoolVar(privacy, "privacy", false, "redact account IDs / quantities / monetary amounts in the output")
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

	mode := canonical.FxMode(*fxMode)
	if !mode.Valid() {
		return errs.Newf(2, "accounts: invalid --fx-mode %q (want 'historic' or 'current')", *fxMode)
	}

	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "accounts: %s", err.Error())
	}

	asOfEpoch, err := parseAsOf(*asOf, time.Now())
	if err != nil {
		return errs.Newf(2, "accounts: %s", err.Error())
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	outCcy := strings.ToUpper(*currency)
	if outCcy == "" {
		outCcy = cfg.DefaultCurrency
	}
	if len(outCcy) != 3 {
		return errs.Newf(2, "accounts: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", outCcy)
	}

	colSet, err := resolveAccountColumns(*cols, outCcy)
	if err != nil {
		return errs.Newf(2, "accounts: %s", err.Error())
	}

	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return errs.Newf(errs.ExitMissingDB,
			"gold database %q does not exist. Run 'wealthdb init' first (requires write access).", cfg.GoldDB)
	}

	openMode := gold.ModeReadWrite
	if dec.Mode == pathmode.ModeReadOnly {
		openMode = gold.ModeReadOnly
	}
	db, err := gold.Open(cfg.GoldDB, openMode)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	rows, err := gold.AccountsAsOf(ctx, db, asOfEpoch, outCcy, mode)
	if err != nil {
		return err
	}

	return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet, *privacy, fmtChoice))
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
		// Defaults applied at render time so users see something
		// useful when the adapter / overrides haven't classified
		// the account. The underlying column stays NULL — gold's
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
		// (the user's -x/--currency choice) so the header carries
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
	return `usage: wealthdb accounts [-d YYYY-MM-DD] [-f FORMAT] [-C COLS] [-x CCY] [--fx-mode MODE] [-p]

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
      --fx-mode MODE       'historic' (default; rate at snapshot time, interpolated) or 'current' (latest rate)
  -p, --privacy            redact account IDs, quantities, and monetary amounts
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
