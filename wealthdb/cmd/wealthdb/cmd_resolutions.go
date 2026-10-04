package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
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

	rep := resolutionsReport(*sourceFilter)
	open := func() (*sql.DB, error) {
		return openGoldForRead(g, cfg, "gold database %q does not exist. Run 'wealthdb init' first.")
	}
	return writeReport(ctx, rep, "default", "resolutions", open, false, fmtChoice, stdout)
}

// resolutionRow is one symbol_resolutions row as the dump renders it.
type resolutionRow struct {
	Source, LookupKind, LookupValue, Symbol, ModelName string
	ResolvedAt                                         int64
}

// resolutionColumns is the dump's registry. Most of it is instrument
// identifiers and vocabulary, which stay legible under privacy. The
// exception is lookup_value of a by-name row: that value is a
// transaction's statement narrative, the ticker resolved from it, so
// it takes the free-text class the narrative takes everywhere else.
func resolutionColumns() []columnSpec[resolutionRow] {
	return []columnSpec[resolutionRow]{
		{Name: "silver_source", Align: output.AlignLeft, Extract: func(r resolutionRow) string { return r.Source }},
		{Name: "lookup_kind", Align: output.AlignLeft, Extract: func(r resolutionRow) string { return r.LookupKind }},
		{Name: "lookup_value", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			PrivacyFunc: func(r resolutionRow) PrivacyClass {
				if r.LookupKind == "name" {
					return PrivacyFreeText
				}
				return PrivacyNone
			},
			Extract: func(r resolutionRow) string { return r.LookupValue }},
		{Name: "symbol", Align: output.AlignLeft, Extract: func(r resolutionRow) string { return r.Symbol }},
		{Name: "resolved_at", Align: output.AlignLeft, Extract: func(r resolutionRow) string { return formatDate(r.ResolvedAt) }},
		{Name: "model_name", Align: output.AlignLeft, Extract: func(r resolutionRow) string { return r.ModelName }},
	}
}

// resolutionsReport dumps symbol_resolutions, narrowed to one source
// when source is set: the runner the CLI and the MCP server share.
func resolutionsReport(source string) *report {
	registry := resolutionColumns()
	return newReport(registry, columnNames(registry), func(ctx context.Context, db *sql.DB) ([]resolutionRow, error) {
		q := `SELECT silver_source_id, lookup_kind, lookup_value, symbol, resolved_at, model_name
            FROM symbol_resolutions`
		args := []any{}
		if source != "" {
			q += ` WHERE silver_source_id = ?`
			args = append(args, source)
		}
		q += ` ORDER BY silver_source_id, lookup_kind, lookup_value`

		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("resolutions: %w", err)
		}
		defer rows.Close()
		var out []resolutionRow
		for rows.Next() {
			var r resolutionRow
			if err := rows.Scan(&r.Source, &r.LookupKind, &r.LookupValue, &r.Symbol, &r.ResolvedAt, &r.ModelName); err != nil {
				return nil, fmt.Errorf("resolutions scan: %w", err)
			}
			out = append(out, r)
		}
		return out, rows.Err()
	})
}
