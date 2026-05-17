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
	register("positions", cmdPositions)
}

func cmdPositions(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb positions", flag.ContinueOnError)
	fs.SetOutput(stderr)

	asOf := fs.String("d", "", "as-of date (YYYY-MM-DD; default today UTC)")
	fs.StringVar(asOf, "as-of", "", "as-of date (YYYY-MM-DD; default today UTC)")
	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	cols := fs.String("C", "default", "columns: comma-separated names, or 'default' / 'all'")
	fs.StringVar(cols, "columns", "default", "columns: comma-separated names, or 'default' / 'all'")
	fs.Usage = func() {
		fmt.Fprintln(stderr, positionsUsage())
	}
	if err := fs.Parse(subargs); err != nil {
		// flag.Parse already printed usage to stderr; suppress
		// further error text for the standard -h case.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "positions: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "positions: unexpected positional argument %q", fs.Arg(0))
	}

	colSet, err := resolveColumns(*cols)
	if err != nil {
		return errs.Newf(2, "positions: %s", err.Error())
	}

	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "positions: %s", err.Error())
	}
	if fmtChoice != output.FormatTable {
		return fmt.Errorf("positions: format %q not implemented yet (see milestone 10)", fmtChoice)
	}

	asOfEpoch, err := parseAsOf(*asOf)
	if err != nil {
		return errs.Newf(2, "positions: %s", err.Error())
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	// Read-only is fine; pick the right mode for what's available.
	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return errs.Newf(errs.ExitMissingDB,
			"gold database %q does not exist. Run 'wealthdb init' first (requires write access).", cfg.GoldDB)
	}

	mode := gold.ModeReadWrite
	if dec.Mode == pathmode.ModeReadOnly {
		mode = gold.ModeReadOnly
	}
	db, err := gold.Open(cfg.GoldDB, mode)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	rows, err := gold.PositionsAsOf(ctx, db, asOfEpoch)
	if err != nil {
		return err
	}

	return output.WriteTable(stdout, positionsTable(rows, colSet))
}

// parseAsOf resolves the -d flag to a Unix-second epoch. Empty
// string means "end of today, UTC".
func parseAsOf(s string) (int64, error) {
	if s == "" {
		now := time.Now().UTC()
		endOfToday := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.UTC)
		return endOfToday.Unix(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, fmt.Errorf("invalid -d value %q: want YYYY-MM-DD", s)
	}
	// End of the given day so a snapshot taken at noon counts as
	// "before today" when the user says -d today's-date.
	t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC)
	return t.Unix(), nil
}

// ---- column registry -----------------------------------------------------

// columnSpec describes one selectable output column for `wealthdb
// positions`. The Extract function pulls the cell value out of a
// gold.PositionRow; nullable fields fall back to a friendly
// alternative (e.g. account → DisplayName or AccountExternalID).
// Align is honoured by table-style formatters; numeric columns
// (quantity, market_value, FX rates as those land) are right-
// aligned so columns of figures line up at the decimal point.
type columnSpec struct {
	Name    string
	Align   output.Alignment
	Extract func(gold.PositionRow) string
}

// allColumns is the registry of every column the user can name
// via --columns. Order here is the order used by --columns all.
var allColumns = []columnSpec{
	{"silver_source", output.AlignLeft, func(r gold.PositionRow) string { return r.SilverSourceID }},
	{"snapshot_date", output.AlignLeft, func(r gold.PositionRow) string { return formatDate(r.SnapshotAt) }},
	{"account", output.AlignLeft, func(r gold.PositionRow) string {
		// Display name preferred (Schwab accountNumber); fall
		// back to the raw external_id when no display name is
		// known (UBS IBAN, Swissquote customer ID).
		if r.DisplayName != nil && *r.DisplayName != "" {
			return *r.DisplayName
		}
		return r.AccountExternalID
	}},
	{"account_id", output.AlignLeft, func(r gold.PositionRow) string { return r.AccountExternalID }},
	{"position_key", output.AlignLeft, func(r gold.PositionRow) string { return r.PositionKey }},
	{"symbol", output.AlignLeft, func(r gold.PositionRow) string { return strOrEmpty(r.Symbol) }},
	{"name", output.AlignLeft, func(r gold.PositionRow) string { return strOrEmpty(r.Name) }},
	{"asset_class", output.AlignLeft, func(r gold.PositionRow) string { return r.AssetClass }},
	{"currency", output.AlignLeft, func(r gold.PositionRow) string { return r.Currency }},
	{"quantity", output.AlignRight, func(r gold.PositionRow) string { return strOrEmpty(r.Quantity) }},
	{"market_value", output.AlignRight, func(r gold.PositionRow) string { return formatCents(r.MarketValue) }},
	{"relationship_id", output.AlignLeft, func(r gold.PositionRow) string { return strOrEmpty(r.RelationshipID) }},
}

// formatCents renders a decimal-string-as-pointer with exactly
// two fractional digits — the canonical convention for money
// columns. Returns "" for nil so empty cells stay empty (not
// "0.00"). On parse failure falls back to the raw string rather
// than erroring (defensive — the value came from DuckDB and
// should always be valid, but garbage-in shouldn't crash the
// table render).
func formatCents(p *string) string {
	if p == nil {
		return ""
	}
	d, err := canonical.NewDecimalFromString(*p)
	if err != nil {
		return *p
	}
	return d.StringFixed(2)
}

// defaultColumns is what `wealthdb positions` shows when --columns
// isn't passed (or when --columns=default). Tracks user feedback
// from M8 — `account` shows the human-readable identifier and
// `symbol` is included separately from `position_key`.
var defaultColumns = []string{
	"silver_source", "snapshot_date", "account", "symbol",
	"position_key", "asset_class", "currency", "quantity", "market_value",
}

// resolveColumns turns a --columns flag value into an ordered list
// of columnSpec. Supports the special values "default" and "all",
// as well as comma-separated explicit lists. Returns a helpful
// error on unknown column names.
func resolveColumns(flagValue string) ([]columnSpec, error) {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue == "" || flagValue == "default" {
		return columnsByName(defaultColumns)
	}
	if flagValue == "all" {
		out := make([]columnSpec, len(allColumns))
		copy(out, allColumns)
		return out, nil
	}
	names := strings.Split(flagValue, ",")
	for i, n := range names {
		names[i] = strings.TrimSpace(n)
	}
	return columnsByName(names)
}

func columnsByName(names []string) ([]columnSpec, error) {
	index := make(map[string]columnSpec, len(allColumns))
	for _, c := range allColumns {
		index[c.Name] = c
	}
	out := make([]columnSpec, 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		c, ok := index[n]
		if !ok {
			return nil, fmt.Errorf("unknown column %q; available: %s", n, joinColumnNames())
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--columns produced an empty list")
	}
	return out, nil
}

func joinColumnNames() string {
	names := make([]string, len(allColumns))
	for i, c := range allColumns {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// positionsTable converts a slice of gold.PositionRow into the
// generic output.Table the formatter expects, using the caller's
// selected columns. Column alignment is propagated so right-
// aligned numeric columns render with their decimal points lined
// up.
func positionsTable(rows []gold.PositionRow, cols []columnSpec) output.Table {
	t := output.Table{
		Columns: make([]string, len(cols)),
		Aligns:  make([]output.Alignment, len(cols)),
	}
	for i, c := range cols {
		t.Columns[i] = c.Name
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

func positionsUsage() string {
	return `usage: wealthdb positions [-d YYYY-MM-DD] [-f FORMAT] [-C COLS]

Print consolidated positions as of a date. For each silver source,
the latest snapshot ≤ the as-of date is used. Default: today UTC,
table format, default column set.

Flags:
  -d, --as-of YYYY-MM-DD   as-of date (default: today UTC)
  -f, --format FORMAT      output format (default: table; csv / csv_plain / json land in M10)
  -C, --columns COLS       comma-separated column names, or 'default' / 'all'

Available columns:
  ` + joinColumnNames() + `

Default column set:
  ` + strings.Join(defaultColumns, ", ")
}

func formatDate(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02")
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

