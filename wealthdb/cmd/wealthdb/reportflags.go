package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// reportFlags are the output flags every report takes: -f, -C, -F, -x
// and -p. The family registers its own options beside them, and each
// family's usage text says what -p redacts there.
type reportFlags struct {
	format, cols, currency *string
	privacy                *bool
	filters                conditionFlags
}

// conditionFlags are the -F conditions, one per flag, in order.
type conditionFlags []string

func (c *conditionFlags) String() string { return strings.Join(*c, " · ") }

func (c *conditionFlags) Set(v string) error {
	*c = append(*c, v)
	return nil
}

// registerReportFlags registers the output flags on fs. A report with
// one fixed shape (holdings global) takes no -C and no -F, and cols
// then reads "default".
func registerReportFlags(fs *flag.FlagSet, withColumns bool) *reportFlags {
	rf := &reportFlags{}
	rf.format = fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(rf.format, "format", "table", "output format: table | csv | csv_plain | json")
	if withColumns {
		rf.cols = fs.String("C", "default", "columns: comma-separated names, 'default', 'all', or a +ADD,-REMOVE delta")
		fs.StringVar(rf.cols, "columns", "default", "columns: comma-separated names, 'default', 'all', or a +ADD,-REMOVE delta")
		fs.Var(&rf.filters, "F", "keep the rows where this condition holds (repeatable; wealthdb help filter)")
		fs.Var(&rf.filters, "filter", "keep the rows where this condition holds (repeatable; wealthdb help filter)")
	} else {
		cols := "default"
		rf.cols = &cols
	}
	rf.currency = fs.String("x", "", "output currency (default: config.default_currency)")
	fs.StringVar(rf.currency, "currency", "", "output currency (default: config.default_currency)")
	rf.privacy = fs.Bool("p", false, "redact identifiers, names and amounts")
	fs.BoolVar(rf.privacy, "privacy", false, "redact identifiers, names and amounts")
	return rf
}

// resolve validates the format, loads the config and settles the output
// currency (-x, else the config's default_currency), prefixing errors
// with the command name. Call after fs.Parse.
func (rf *reportFlags) resolve(g globalFlags, cmd string) (output.Format, *config.Config, string, error) {
	fmtChoice, err := output.Parse(*rf.format)
	if err != nil {
		return "", nil, "", errs.Newf(errs.ExitUsage, "%s: %s", cmd, err.Error())
	}
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return "", nil, "", err
	}
	outCcy := strings.ToUpper(*rf.currency)
	if outCcy == "" {
		outCcy = cfg.DefaultCurrency
	}
	if !config.IsLikelyISO4217(outCcy) {
		return "", nil, "", errs.Newf(errs.ExitUsage, "%s: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", cmd, outCcy)
	}
	return fmtChoice, cfg, outCcy, nil
}

// output is how the report is written: the -C columns, the format, -p,
// and the -F conditions, which bind to the report tool runs for req.
// The tool names the command in a column error ("gains").
func (rf *reportFlags) output(format output.Format, tool string, req request, cfg *config.Config, now time.Time) reportOutput {
	out := reportOutput{cols: *rf.cols, prefix: tool, privacy: *rf.privacy, format: format}
	if len(rf.filters) > 0 {
		out.filter = &cliFilter{conds: rf.filters, tool: tool, req: req, cfg: cfg, now: now}
	}
	return out
}

// routeView splits a report family's `<view> [args]` into the view and
// the args after it. A help request prints the family's usage and
// returns flag.ErrHelp; a missing or unknown view prints it too and is
// a usage error.
func routeView(cmd string, subargs, views []string, usage func() string, stderr io.Writer) (string, []string, error) {
	if len(subargs) == 0 {
		fmt.Fprintln(stderr, usage())
		return "", nil, errs.Newf(errs.ExitUsage, "%s: a view subcommand is required", cmd)
	}
	view := subargs[0]
	switch {
	case view == "-h" || view == "--help" || view == "help":
		fmt.Fprintln(stderr, usage())
		return "", nil, flag.ErrHelp
	case !slices.Contains(views, view):
		fmt.Fprintln(stderr, usage())
		return "", nil, errs.Newf(errs.ExitUsage, "%s: unknown view %q (want %s)", cmd, view, strings.Join(views, " | "))
	}
	return view, subargs[1:], nil
}
