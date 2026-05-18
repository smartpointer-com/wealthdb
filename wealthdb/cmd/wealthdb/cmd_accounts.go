package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

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
	fs.Usage = func() {
		fmt.Fprintln(stderr, accountsUsage())
	}
	if err := fs.Parse(subargs); err != nil {
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

	asOfEpoch, err := parseAsOf(*asOf)
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

	return writeFormatted(stdout, fmtChoice, accountsTable(rows, colSet))
}

// ---- column registry -----------------------------------------------------

type accountColumnSpec struct {
	Name    string
	Header  string // empty ⇒ same as Name
	Align   output.Alignment
	Extract func(gold.AccountRow) string
}

func (c accountColumnSpec) header() string {
	if c.Header != "" {
		return c.Header
	}
	return c.Name
}

func buildAccountColumnRegistry(outCcy string) []accountColumnSpec {
	suffix := "_" + outCcy
	return []accountColumnSpec{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return a.SilverSourceID }},
		{Name: "snapshot_date", Align: output.AlignLeft, Extract: func(a gold.AccountRow) string {
			if a.SnapshotAt == 0 {
				return ""
			}
			return formatDate(a.SnapshotAt)
		}},
		{Name: "account", Align: output.AlignLeft, Extract: func(a gold.AccountRow) string {
			if a.DisplayName != nil && *a.DisplayName != "" {
				return *a.DisplayName
			}
			return a.AccountExternalID
		}},
		{Name: "account_id", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return a.AccountExternalID }},
		{Name: "account_kind", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return a.AccountKind }},
		{Name: "base_currency", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.BaseCurrency) }},
		{Name: "relationship_id", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.RelationshipID) }},
		{Name: "account_nickname", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.Nickname) }},
		{Name: "account_category", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.AccountCategory) }},
		{Name: "portfolio_external_id", Align: output.AlignLeft,
			Extract: func(a gold.AccountRow) string { return strOrEmpty(a.PortfolioExternalID) }},

		// Base-currency aggregates. The header has no _<CCY>
		// suffix because each row's value is in its OWN
		// base_currency — there's no single column-wide currency
		// to advertise. Blank when the account's base_currency is
		// nil.
		{Name: "positions_value", Align: output.AlignRight,
			Extract: func(a gold.AccountRow) string { return formatCents(a.PositionsValueBase) }},
		{Name: "cash_balance", Align: output.AlignRight,
			Extract: func(a gold.AccountRow) string { return formatCents(a.CashBalanceBase) }},
		{Name: "total_value", Align: output.AlignRight,
			Extract: func(a gold.AccountRow) string { return formatCents(a.TotalValueBase) }},

		// Output-currency aggregates. Single column-wide currency
		// (the user's -x/--currency choice) so the header carries
		// the suffix.
		{Name: "positions_value_outccy", Header: "positions_value" + suffix, Align: output.AlignRight,
			Extract: func(a gold.AccountRow) string { return formatCents(a.PositionsValueOutCcy) }},
		{Name: "cash_balance_outccy", Header: "cash_balance" + suffix, Align: output.AlignRight,
			Extract: func(a gold.AccountRow) string { return formatCents(a.CashBalanceOutCcy) }},
		{Name: "total_value_outccy", Header: "total_value" + suffix, Align: output.AlignRight,
			Extract: func(a gold.AccountRow) string { return formatCents(a.TotalValueOutCcy) }},
	}
}

var defaultAccountColumns = []string{
	"silver_source", "snapshot_date", "account", "base_currency",
	"positions_value", "cash_balance", "total_value",
	"total_value_outccy",
}

func resolveAccountColumns(flagValue, outCcy string) ([]accountColumnSpec, error) {
	registry := buildAccountColumnRegistry(outCcy)
	flagValue = strings.TrimSpace(flagValue)
	switch flagValue {
	case "", "default":
		return accountColumnsByName(defaultAccountColumns, registry)
	case "all":
		out := make([]accountColumnSpec, len(registry))
		copy(out, registry)
		return out, nil
	}
	names := strings.Split(flagValue, ",")
	for i, n := range names {
		names[i] = strings.TrimSpace(n)
	}
	return accountColumnsByName(names, registry)
}

func accountColumnsByName(names []string, registry []accountColumnSpec) ([]accountColumnSpec, error) {
	index := make(map[string]accountColumnSpec, len(registry))
	for _, c := range registry {
		index[c.Name] = c
	}
	out := make([]accountColumnSpec, 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		c, ok := index[n]
		if !ok {
			return nil, fmt.Errorf("unknown column %q; available: %s", n, joinAccountColumnNames(registry))
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--columns produced an empty list")
	}
	return out, nil
}

func joinAccountColumnNames(registry []accountColumnSpec) string {
	names := make([]string, len(registry))
	for i, c := range registry {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

func accountsTable(rows []gold.AccountRow, cols []accountColumnSpec) output.Table {
	t := output.Table{
		Columns: make([]string, len(cols)),
		Aligns:  make([]output.Alignment, len(cols)),
	}
	for i, c := range cols {
		t.Columns[i] = c.header()
		t.Aligns[i] = c.Align
	}
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = c.Extract(r)
		}
		t.Rows = append(t.Rows, cells)
	}
	return t
}

func accountsUsage() string {
	registry := buildAccountColumnRegistry("CCY")
	return `usage: wealthdb accounts [-d YYYY-MM-DD] [-f FORMAT] [-C COLS] [-x CCY] [--fx-mode MODE]

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
  -C, --columns COLS       comma-separated column names, or 'default' / 'all'
  -x, --currency CCY       output currency for the _<CCY> aggregate columns (default: config.default_currency)
      --fx-mode MODE       'historic' (default; rate at snapshot time, interpolated) or 'current' (latest rate)

Available columns:
  ` + joinAccountColumnNames(registry) + `

  (The 'positions_value_outccy', 'cash_balance_outccy', and
   'total_value_outccy' columns render as 'positions_value_<CCY>',
   'cash_balance_<CCY>', and 'total_value_<CCY>' in the header,
   reflecting your -x/--currency choice.)

Default column set:
  ` + strings.Join(defaultAccountColumns, ", ")
}
