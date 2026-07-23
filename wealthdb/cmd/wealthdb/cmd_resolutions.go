package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
)

func init() {
	register("resolutions", cmdResolutions)
}

// cmdResolutions dumps the symbol_resolutions table for inspection
// — the LLM-derived and manual-override (config-synced) ticker
// fallbacks the read path COALESCEs in. Read-only. Lets the
// model's output be inspected against external research to decide
// which entries (if any) need a manual override in wealthdb.cfg.
func cmdResolutions(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb resolutions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	sourceFilter := fs.String("s", "", "filter to a specific silver_source_id")
	fs.StringVar(sourceFilter, "source", "", "filter to a specific silver_source_id")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb resolutions [-s SOURCE] [-f FORMAT]

Dump every row in the symbol_resolutions table — the LLM-derived
and manual-override ticker fallbacks the read path uses for rows
the silver adapters couldn't resolve themselves.

Flags:
  -s, --source ID         filter to one silver_source_id
  -f, --format FORMAT     table | csv | csv_plain | json (default: table)

The model_name column tells you each row's provenance:
  - 'manual-override'        — from cfg.symbol_resolution.overrides
  - anything else            — LLM model that emitted the row`)
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "resolutions: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "resolutions: unexpected positional argument %q", fs.Arg(0))
	}

	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "resolutions: %s", err.Error())
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	db, err := openGoldForRead(g, cfg, "gold database %q does not exist. Run 'wealthdb init' first.")
	if err != nil {
		return err
	}
	defer db.Close()

	q := `SELECT silver_source_id, lookup_kind, lookup_value, symbol, resolved_at, model_name
            FROM symbol_resolutions`
	args := []any{}
	if *sourceFilter != "" {
		q += ` WHERE silver_source_id = ?`
		args = append(args, *sourceFilter)
	}
	q += ` ORDER BY silver_source_id, lookup_kind, lookup_value`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("resolutions: %w", err)
	}
	defer rows.Close()

	t := output.Table{
		Columns: []string{"silver_source", "lookup_kind", "lookup_value", "symbol", "resolved_at", "model_name"},
		Aligns: []output.Alignment{
			output.AlignLeft, output.AlignLeft, output.AlignLeft,
			output.AlignLeft, output.AlignLeft, output.AlignLeft,
		},
	}
	for rows.Next() {
		var src, kind, value, symbol, modelName string
		var resolvedAt int64
		if err := rows.Scan(&src, &kind, &value, &symbol, &resolvedAt, &modelName); err != nil {
			return fmt.Errorf("resolutions scan: %w", err)
		}
		t.Rows = append(t.Rows, []string{src, kind, value, symbol, formatDate(resolvedAt), modelName})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return writeFormatted(stdout, fmtChoice, t)
}
