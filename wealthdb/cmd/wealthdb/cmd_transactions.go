package main

import (
	"context"
	"database/sql"
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
	register("transactions", cmdTransactions)
}

// txValueFlags is the set of cmd_transactions flag tokens that
// consume the next arg as their value. Used by reorderFlagsFirst
// so the cmd accepts `transactions 2025 -f csv` and equivalents.
// Boolean flags (`-r`, `--reverse`) are NOT listed here — they
// don't consume a follow-on arg.
var txValueFlags = map[string]bool{
	"-f": true, "--format": true,
	"-C": true, "--columns": true,
	"-x": true, "--currency": true,
	"--fx-mode": true,
}

// reorderFlagsFirst shuffles `args` so that flag tokens (and any
// value tokens they consume per `valueFlags`) precede positional
// args. Preserves the relative order within each group. The
// `--flag=value` form is treated as a single token with no
// look-ahead. `-` alone is a positional (a sentinel for an
// open-ended range bound).
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
		// `--flag=value` is self-contained.
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
	fxMode := fs.String("fx-mode", "historic", "FX rate selection: 'historic' (rate at occurred_at, interpolated) or 'current' (latest available)")
	reverse := fs.Bool("r", false, "reverse-time order (newest first); default is oldest first")
	fs.BoolVar(reverse, "reverse", false, "reverse-time order (newest first); default is oldest first")

	fs.Usage = func() {
		fmt.Fprintln(stderr, transactionsUsage())
	}
	// Go's flag package stops at the first non-flag arg, which
	// would make `transactions 2025 -f csv` interpret `-f csv` as
	// positionals. Reorder so flag tokens float to the front; the
	// positional date args end up at the back where fs.Args()
	// returns them after Parse.
	reordered := reorderFlagsFirst(subargs, txValueFlags)
	if err := fs.Parse(reordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "transactions: bad flags")
	}

	fromEpoch, toEpoch, err := parseDateRangeArgs(fs.Args())
	if err != nil {
		fs.Usage()
		return errs.Newf(2, "transactions: %s", err.Error())
	}

	mode := canonical.FxMode(*fxMode)
	if !mode.Valid() {
		return errs.Newf(2, "transactions: invalid --fx-mode %q (want 'historic' or 'current')", *fxMode)
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

	order := gold.SortAscending
	if *reverse {
		order = gold.SortDescending
	}
	rows, err := gold.TransactionsBetween(ctx, db, fromEpoch, toEpoch, order)
	if err != nil {
		return err
	}
	rendered, err := convertTxAll(ctx, db, rows, outCcy, mode)
	if err != nil {
		return err
	}
	return writeFormatted(stdout, fmtChoice, transactionsTable(rendered, colSet))
}

// renderedTx pairs a raw transaction with its net_amount
// converted to the user's requested output currency. Per-row
// missing rates are tolerated and surface as ConvertedValue=nil
// — matches the positions command's "show holes, don't abort"
// stance.
type renderedTx struct {
	Row            gold.TransactionRow
	ConvertedValue *canonical.Decimal
}

func convertTxAll(ctx context.Context, db *sql.DB, rows []gold.TransactionRow, outCcy string, mode canonical.FxMode) ([]renderedTx, error) {
	out := make([]renderedTx, len(rows))
	anyConverted := false
	anyAttempted := false
	for i, r := range rows {
		out[i].Row = r
		if r.NetAmount == nil {
			continue
		}
		anyAttempted = true
		v, err := canonical.NewDecimalFromString(*r.NetAmount)
		if err != nil {
			continue
		}
		conv, err := gold.ConvertValue(ctx, db, r.OccurredAt, v, r.Currency, outCcy, mode)
		if err != nil {
			continue
		}
		out[i].ConvertedValue = &conv
		anyConverted = true
	}
	if anyAttempted && !anyConverted {
		return out, fmt.Errorf("transactions: no FX rates available to convert to %q (mode=%s)", outCcy, mode)
	}
	return out, nil
}

// parseDateRangeArgs accepts zero, one, or two positional date
// args and produces a [fromEpoch, toEpoch] window.
//
//	(no args)                  → past 30 days (today−30 .. now)
//	YYYY                       → full calendar year
//	YYYY-MM                    → full calendar month
//	YYYY-MM-DD                 → single day
//	YYYY-MM-DD YYYY-MM-DD      → explicit range
//	YYYY-MM-DD -               → open-end (from then to now)
//	- YYYY-MM-DD               → open-start (from epoch to then)
//
// "today" is accepted as an alias for the current UTC date. The
// past-30-days default keeps the no-args output to a useful
// inbox-style window; pass `- today` for all time.
func parseDateRangeArgs(args []string) (int64, int64, error) {
	now := time.Now().UTC()
	endOfNow := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.UTC).Unix()

	switch len(args) {
	case 0:
		thirtyDaysAgo := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -30).Unix()
		return thirtyDaysAgo, endOfNow, nil
	case 1:
		return parseDateShorthand(args[0], endOfNow)
	case 2:
		from, err := parseRangeBound(args[0], false, 0, endOfNow)
		if err != nil {
			return 0, 0, fmt.Errorf("from: %w", err)
		}
		to, err := parseRangeBound(args[1], true, 0, endOfNow)
		if err != nil {
			return 0, 0, fmt.Errorf("to: %w", err)
		}
		if from > to {
			return 0, 0, fmt.Errorf("from > to")
		}
		return from, to, nil
	default:
		return 0, 0, fmt.Errorf("expected 0, 1, or 2 date arguments; got %d", len(args))
	}
}

// parseDateShorthand handles the single-arg forms: YYYY, YYYY-MM,
// YYYY-MM-DD. Returns the [start, end] of the implied window.
func parseDateShorthand(s string, endOfNow int64) (int64, int64, error) {
	if s == "today" {
		now := time.Now().UTC()
		startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
		return startOfToday, endOfNow, nil
	}
	switch len(s) {
	case 4: // YYYY
		t, err := time.Parse("2006", s)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid year %q: %w", s, err)
		}
		from := time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		to := time.Date(t.Year(), 12, 31, 23, 59, 59, 0, time.UTC).Unix()
		return from, to, nil
	case 7: // YYYY-MM
		t, err := time.Parse("2006-01", s)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid month %q: %w", s, err)
		}
		from := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
		// Last day of month: first of next month minus a second.
		next := time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		to := next.Add(-time.Second).Unix()
		return from, to, nil
	case 10: // YYYY-MM-DD
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid date %q: %w", s, err)
		}
		from := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix()
		to := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC).Unix()
		return from, to, nil
	default:
		return 0, 0, fmt.Errorf("invalid date arg %q: want YYYY, YYYY-MM, or YYYY-MM-DD", s)
	}
}

// parseRangeBound parses one of the two positional date args.
// "-" means "open-ended": for the from side it returns the
// epoch-0 sentinel; for the to side it returns endOfNow. "today"
// resolves to today's UTC date.
func parseRangeBound(s string, isTo bool, openFrom, openTo int64) (int64, error) {
	if s == "-" {
		if isTo {
			return openTo, nil
		}
		return openFrom, nil
	}
	if s == "today" {
		now := time.Now().UTC()
		if isTo {
			return time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.UTC).Unix(), nil
		}
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, fmt.Errorf("invalid date %q: want YYYY-MM-DD", s)
	}
	if isTo {
		return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC).Unix(), nil
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix(), nil
}

// ---- column registry -----------------------------------------------------

type txColumnSpec struct {
	Name    string
	Header  string
	Align   output.Alignment
	Extract func(renderedTx) string
}

func (c txColumnSpec) header() string {
	if c.Header != "" {
		return c.Header
	}
	return c.Name
}

func buildTransactionColumnRegistry(outCcy string) []txColumnSpec {
	return []txColumnSpec{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return r.Row.SilverSourceID }},
		{Name: "date", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return formatDate(r.Row.OccurredAt) }},
		{Name: "datetime", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return formatDateTime(r.Row.OccurredAt) }},
		{Name: "account", Align: output.AlignLeft,
			Extract: func(r renderedTx) string {
				if r.Row.DisplayName != nil && *r.Row.DisplayName != "" {
					return *r.Row.DisplayName
				}
				return r.Row.AccountExternalID
			}},
		{Name: "account_id", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return r.Row.AccountExternalID }},
		{Name: "kind", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return r.Row.Kind }},
		{Name: "symbol", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return strOrEmpty(r.Row.Symbol) }},
		{Name: "name", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return strOrEmpty(r.Row.Name) }},
		{Name: "instrument_id", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return strOrEmpty(r.Row.InstrumentExternalID) }},
		{Name: "asset_class", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return strOrEmpty(r.Row.AssetClass) }},
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return r.Row.Currency }},
		{Name: "gross_amount", Align: output.AlignRight,
			Extract: func(r renderedTx) string { return formatCents(r.Row.GrossAmount) }},
		{Name: "net_amount", Align: output.AlignRight,
			Extract: func(r renderedTx) string { return formatCents(r.Row.NetAmount) }},
		{Name: "quantity", Align: output.AlignRight,
			Extract: func(r renderedTx) string { return strOrEmpty(r.Row.Quantity) }},
		{Name: "price", Align: output.AlignRight,
			Extract: func(r renderedTx) string { return strOrEmpty(r.Row.Price) }},
		{Name: "value", Header: "value_" + outCcy, Align: output.AlignRight,
			Extract: func(r renderedTx) string {
				if r.ConvertedValue == nil {
					return ""
				}
				return r.ConvertedValue.StringFixed(2)
			}},
		{Name: "tx_id", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return r.Row.TransactionExternalID }},
		{Name: "relationship_id", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return strOrEmpty(r.Row.RelationshipID) }},
		{Name: "account_nickname", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return strOrEmpty(r.Row.Nickname) }},
		{Name: "account_category", Align: output.AlignLeft,
			Extract: func(r renderedTx) string { return strOrEmpty(r.Row.AccountCategory) }},
	}
}

var defaultTransactionColumns = []string{
	"silver_source", "date", "account", "kind", "symbol",
	"currency", "net_amount", "value",
}

func resolveTransactionColumns(flagValue, outCcy string) ([]txColumnSpec, error) {
	registry := buildTransactionColumnRegistry(outCcy)
	flagValue = strings.TrimSpace(flagValue)
	switch flagValue {
	case "", "default":
		return txColumnsByName(defaultTransactionColumns, registry)
	case "all":
		out := make([]txColumnSpec, len(registry))
		copy(out, registry)
		return out, nil
	}
	names := strings.Split(flagValue, ",")
	for i, n := range names {
		names[i] = strings.TrimSpace(n)
	}
	return txColumnsByName(names, registry)
}

func txColumnsByName(names []string, registry []txColumnSpec) ([]txColumnSpec, error) {
	index := make(map[string]txColumnSpec, len(registry))
	for _, c := range registry {
		index[c.Name] = c
	}
	out := make([]txColumnSpec, 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		c, ok := index[n]
		if !ok {
			return nil, fmt.Errorf("unknown column %q; available: %s", n, joinTxColumnNames(registry))
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--columns produced an empty list")
	}
	return out, nil
}

func joinTxColumnNames(registry []txColumnSpec) string {
	names := make([]string, len(registry))
	for i, c := range registry {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

func transactionsTable(rows []renderedTx, cols []txColumnSpec) output.Table {
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

func transactionsUsage() string {
	registry := buildTransactionColumnRegistry("CCY")
	return `usage: wealthdb transactions [FROM [TO]] [-r] [-f FORMAT] [-C COLS] [-x CCY] [--fx-mode MODE]

Print transactions over a date range. Default: past 30 days,
table format, oldest first, default column set, output currency
from config.default_currency, historic FX mode (rate at
occurred_at).

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
  -C, --columns COLS       comma-separated column names, or 'default' / 'all'
  -x, --currency CCY       output currency for the value column (default: config.default_currency)
      --fx-mode MODE       'historic' (default; rate at occurred_at) or 'current' (latest rate)

Available columns:
  ` + joinTxColumnNames(registry) + `

  (The 'value' column renders as 'value_<CCY>' in the header,
   reflecting your -x/--currency choice.)

Default column set:
  ` + strings.Join(defaultTransactionColumns, ", ")
}
