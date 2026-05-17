package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

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
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb positions [-d YYYY-MM-DD] [-f table]

Print consolidated positions as of a date. For each silver source,
the latest snapshot ≤ the as-of date is used. Default: today UTC,
table format.

Milestone 6 supports only the 'table' format; csv / csv_plain /
json land in milestone 10.`)
	}
	if err := fs.Parse(subargs); err != nil {
		return errs.Newf(2, "positions: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "positions: unexpected positional argument %q", fs.Arg(0))
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

	t := positionsTable(rows)
	return output.WriteTable(stdout, t)
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

// positionsTable converts a slice of gold.PositionRow into the
// generic output.Table the formatter expects.
func positionsTable(rows []gold.PositionRow) output.Table {
	t := output.Table{
		Columns: []string{
			"silver_source", "snapshot_date", "account", "position_key",
			"asset_class", "currency", "quantity", "market_value",
		},
	}
	for _, r := range rows {
		t.Rows = append(t.Rows, []string{
			r.SilverSourceID,
			formatDate(r.SnapshotAt),
			r.AccountExternalID,
			r.PositionKey,
			r.AssetClass,
			r.Currency,
			strOrEmpty(r.Quantity),
			strOrEmpty(r.MarketValue),
		})
	}
	return t
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

