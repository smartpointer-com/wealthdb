package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
)

func init() {
	register("transactions", cmdTransactions)
}

// txValueFlags is the set of cmd_transactions flag tokens that
// consume the next arg as their value. Used by reorderFlagsFirst so
// the cmd accepts `transactions 2025 -f csv` and equivalents.
// Boolean flags (`-r`, `--reverse`) are NOT listed.
var txValueFlags = map[string]bool{
	"-f": true, "--format": true,
	"-C": true, "--columns": true,
	"-x": true, "--currency": true,
}

// reorderFlagsFirst shuffles `args` so that flag tokens (and any
// value tokens they consume per `valueFlags`) precede positional
// args. Preserves relative order within each group. The
// `--flag=value` form is one token with no look-ahead. `-` alone is
// a positional (a sentinel for an open-ended range bound).
func reorderFlagsFirst(args []string, valueFlags map[string]bool) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-" || a == "--" || !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		if valueFlags[a] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func cmdTransactions(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb transactions", flag.ContinueOnError)
	fs.SetOutput(stderr)

	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	cols := fs.String("C", "default", "columns: comma-separated names, or 'default' / 'all'")
	fs.StringVar(cols, "columns", "default", "columns: comma-separated names, or 'default' / 'all'")
	currency := fs.String("x", "", "output currency for the value column (default: config.default_currency)")
	fs.StringVar(currency, "currency", "", "output currency (default: config.default_currency)")
	reverse := fs.Bool("r", false, "reverse-time order (newest first); default is oldest first")
	fs.BoolVar(reverse, "reverse", false, "reverse-time order (newest first); default is oldest first")
	privacy := fs.Bool("p", false, "redact account / tx IDs, quantities, prices, and monetary amounts in the output")
	fs.BoolVar(privacy, "privacy", false, "redact account / tx IDs, quantities, prices, and monetary amounts in the output")

	fs.Usage = func() {
		fmt.Fprintln(stderr, transactionsUsage())
	}
	// Go's flag package stops at the first non-flag arg, which would
	// make `transactions 2025 -f csv` treat `-f csv` as positionals.
	// Reorder so flag tokens float to the front; the positional date
	// args end up at the back where fs.Args() returns them.
	reordered := reorderFlagsFirst(splitFusedColumnsFlag(subargs), txValueFlags)
	if err := fs.Parse(reordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "transactions: bad flags")
	}

	fromEpoch, toEpoch, err := parseDateRange(fs.Args(), time.Now())
	if err != nil {
		fs.Usage()
		return errs.Newf(2, "transactions: %s", err.Error())
	}

	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "transactions: %s", err.Error())
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
		return errs.Newf(2, "transactions: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", outCcy)
	}

	colSet, err := resolveTransactionColumns(*cols, outCcy)
	if err != nil {
		return errs.Newf(2, "transactions: %s", err.Error())
	}

	db, err := openGoldForRead(g, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	order := gold.SortAscending
	if *reverse {
		order = gold.SortDescending
	}
	rows, err := gold.TransactionsBetween(ctx, db, fromEpoch, toEpoch, outCcy, order)
	if err != nil {
		return err
	}
	return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet, *privacy, fmtChoice))
}

// ---- column registry -----------------------------------------------------

func buildTransactionColumnRegistry(outCcy string) []columnSpec[gold.TransactionRow] {
	return []columnSpec[gold.TransactionRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return r.SilverSourceID }},
		{Name: "date", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return formatDate(r.OccurredAt) }},
		{Name: "datetime", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return formatDateTime(r.OccurredAt) }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.TransactionRow) string {
				if r.DisplayName != nil && *r.DisplayName != "" {
					return *r.DisplayName
				}
				return r.AccountExternalID
			}},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.TransactionRow) string { return r.AccountExternalID }},
		{Name: "kind", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return r.Kind }},
		{Name: "symbol", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.Symbol) }},
		{Name: "name", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string {
				// Prefer the joined instruments.name; fall back to the
				// adapter-supplied transactions.description (Schwab
				// dividends, UBS web cash_movement captions) so
				// instrument-related rows still surface a label.
				if r.Name != nil && *r.Name != "" {
					return *r.Name
				}
				return strOrEmpty(r.Description)
			}},
		{Name: "instrument_id", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.InstrumentExternalID) }},
		{Name: "description", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.Description) }},
		{Name: "asset_class", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.AssetClass) }},
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return r.Currency }},
		{Name: "gross_amount", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.TransactionRow) string { return formatCents(r.GrossAmount) }},
		{Name: "net_amount", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.TransactionRow) string { return formatCents(r.NetAmount) }},
		{Name: "quantity", Align: output.AlignRight, Privacy: PrivacyQuantity,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.Quantity) }},
		{Name: "price", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.Price) }},
		{Name: "value", Header: "value_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.TransactionRow) string { return formatCents(r.ValueOutCcy) }},
		{Name: "tx_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.TransactionRow) string { return r.TransactionExternalID }},
		{Name: "relationship_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.RelationshipID) }},
		{Name: "account_nickname", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.Nickname) }},
		{Name: "account_category", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.AccountCategory) }},
	}
}

var defaultTransactionColumns = []string{
	"silver_source", "date", "account", "kind", "symbol",
	"instrument_id", "currency", "net_amount", "value",
}

func resolveTransactionColumns(flagValue, outCcy string) ([]columnSpec[gold.TransactionRow], error) {
	return resolveColumns(flagValue, defaultTransactionColumns, buildTransactionColumnRegistry(outCcy))
}

func transactionsUsage() string {
	registry := buildTransactionColumnRegistry("CCY")
	return `usage: wealthdb transactions [FROM [TO]] [-r] [-f FORMAT] [-C COLS] [-x CCY] [-p]

Print transactions over a date range. Default: past 30 days,
table format, oldest first, default column set, output currency
from config.default_currency, historic FX mode (nearest rate
at-or-before occurred_at).

Date arguments (positional, optional; may appear before or after
flags):
  (no args)              past 30 days
  YYYY                   full calendar year
  YYYY-MM                full calendar month
  YYYY-MM-DD             single day
  YYYY-MM-DD YYYY-MM-DD  explicit range, inclusive
  YYYY-MM-DD -           open end (FROM date to today)
  - YYYY-MM-DD           open start (epoch to TO date)
  - today                all time (synonym of "- today")
  today                  accepted in any position as a synonym

Flags:
  -r, --reverse            reverse-time order (newest first); default is oldest first
  -f, --format FORMAT      output format: table | csv | csv_plain | json
  -C, --columns COLS       comma-separated column names, 'default', 'all', or
                           a +ADD,...-REMOVE,... delta against the default set
                           (e.g. -C+description-account)
  -x, --currency CCY       output currency for the value column (default: config.default_currency)
  -p, --privacy            redact account / tx IDs, quantities, prices, and monetary amounts
                           (table: visible placeholders; csv: empty cells; json: keys omitted)

Available columns:
  ` + joinColumnNames(registry) + `

  (The 'value' column renders as 'value_<CCY>' in the header,
   reflecting your -x/--currency choice.)

Default column set:
  ` + strings.Join(defaultTransactionColumns, ", ")
}
