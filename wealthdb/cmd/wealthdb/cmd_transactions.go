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
	privacyHelp := "redact account / tx IDs, statement narratives, quantities, prices, and monetary amounts in the output"
	privacy := fs.Bool("p", false, privacyHelp)
	fs.BoolVar(privacy, "privacy", false, privacyHelp)

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
			},
			// The class follows which of the two the cell holds. A
			// joined instrument name is a public security name and
			// stays legible; the description fallback is a statement
			// narrative and takes the free-text class, exactly as the
			// `description` column below does.
			PrivacyFunc: func(r gold.TransactionRow) PrivacyClass {
				if r.Name != nil && *r.Name != "" {
					return PrivacyNone
				}
				return PrivacyFreeText
			}},
		{Name: "instrument_id", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.InstrumentExternalID) }},
		// The statement narrative, verbatim: a bank line carries a
		// counterparty's name, address and reference text, and gold
		// appends the payer's memo behind the separator. Free-text
		// class, matching the same column on the spending view — the
		// cell masks whole, because there is no safe slice of a
		// narrative to expose.
		{Name: "description", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.Description) }},
		// The spending overlay's verdict on the row (migration 0042).
		// Empty for everything the enrichment pass does not reach:
		// investment rows, and anything outside the spending account
		// scope. The merchant is the store's name for the row's
		// signature, or that signature itself where the store holds
		// none (migration 0054), and is empty on a delta row — an
		// own-account move, capital deployed, a gift — which carries
		// its issuer label or nothing (migrations 0048 and 0052).
		// Free-text class, as on the spending view: the transfer fence
		// gates what may acquire a STORE name, not what this column
		// prints, so a narrative the fence refused surfaces here as
		// its own fold. The spend_* categories and asset_class stay
		// legible as taxonomy.
		{Name: "merchant", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.MerchantName) }},
		{Name: "spend_primary", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.SpendPrimary) }},
		{Name: "spend_detailed", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.SpendDetailed) }},
		// The INCOME overlay's verdict on the same row (migration
		// 0071), and a row can carry both: a deposit the matcher
		// paired is `internal_transfer` on each side, and this is the
		// one surface that shows a transaction from both at once.
		// `payer` takes the free-text class for the reason `merchant`
		// does — on a dividend it is the instrument, but on a deposit
		// it is the store's name for the signature or the fold of the
		// narrative itself, and a column takes the class of the worst
		// thing it can hold.
		{Name: "payer", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.PayerName) }},
		{Name: "income_primary", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.IncomePrimary) }},
		{Name: "income_detailed", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.IncomeDetailed) }},
		// The cheque number of an outgoing paper cheque, for matching a
		// row against the holder's own paper records. It names no third
		// party, so it is not free text — but it is an identifier tied
		// to the holder's own account, which is what PrivacyAccountID
		// covers.
		{Name: "check_no", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.CheckNumber) }},
		// The node the cash flow statement resolved the row to
		// (migration 0081), beside the two families' trios: which
		// section of the statement, which inner node, which leaf. The
		// three are vocabulary and stay legible under -p, as the
		// families' categories do. Blank where the resolution reached
		// no node — a kind with no canonical sign, an unpaired card
		// bill — and on a pool-internal move, which is a movement the
		// statement deliberately does not draw.
		{Name: "cashflow_section", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.CashflowSection) }},
		{Name: "cashflow_class", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.CashflowClass) }},
		{Name: "cashflow_group", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.CashflowGroup) }},
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
		{Name: "account_kind", Align: output.AlignLeft,
			Extract: func(r gold.TransactionRow) string { return strOrEmpty(r.AccountKind) }},
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
from config.default_currency, historic FX (nearest rate
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
  - today                all time (synonym of "- -")
  today                  accepted in any position as a synonym

Flags:
  -r, --reverse            reverse-time order (newest first); default is oldest first
  -f, --format FORMAT      output format: table | csv | csv_plain | json
  -C, --columns COLS       comma-separated column names, 'default', 'all', or
                           a +ADD,...-REMOVE,... delta against the default set
                           (e.g. -C+description-account)
  -x, --currency CCY       output currency for the value column (default: config.default_currency)
  -p, --privacy            redact account / tx IDs, quantities, prices, and monetary amounts;
                           statement narratives (description, and merchant / payer — the
                           store's name or the line's own signature — and the name column
                           where it falls back to one) redact as free text — the cell masks whole
                           (table: visible placeholders; csv: empty cells; json: keys omitted)

Available columns:
  ` + joinColumnNames(registry) + `

  (The 'value' column renders as 'value_<CCY>' in the header,
   reflecting your -x/--currency choice.)

Default column set:
  ` + strings.Join(defaultTransactionColumns, ", ")
}
