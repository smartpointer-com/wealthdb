package main

import (
	"flag"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// reportFlags are the output flags every windowed report family takes:
// -f, -C, -x and -p. The family registers its own options beside them.
type reportFlags struct {
	format, cols, currency *string
	privacy                *bool
}

// registerReportFlags registers the output flags on fs; privacyUsage is
// the family's own account of what -p redacts.
func registerReportFlags(fs *flag.FlagSet, privacyUsage string) *reportFlags {
	rf := &reportFlags{}
	rf.format = fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(rf.format, "format", "table", "output format: table | csv | csv_plain | json")
	rf.cols = fs.String("C", "default", "columns: comma-separated names, or 'default' / 'all'")
	fs.StringVar(rf.cols, "columns", "default", "columns: comma-separated names, or 'default' / 'all'")
	rf.currency = fs.String("x", "", "output currency (default: config.default_currency)")
	fs.StringVar(rf.currency, "currency", "", "output currency (default: config.default_currency)")
	rf.privacy = fs.Bool("p", false, privacyUsage)
	fs.BoolVar(rf.privacy, "privacy", false, privacyUsage)
	return rf
}

// resolve validates the format, loads the config and settles the output
// currency, prefixing errors with the command name. Call after fs.Parse.
func (rf *reportFlags) resolve(g globalFlags, cmd string) (output.Format, *config.Config, string, error) {
	fmtChoice, err := output.Parse(*rf.format)
	if err != nil {
		return "", nil, "", errs.Newf(2, "%s: %s", cmd, err.Error())
	}
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return "", nil, "", err
	}
	outCcy := strings.ToUpper(*rf.currency)
	if outCcy == "" {
		outCcy = cfg.DefaultCurrency
	}
	if len(outCcy) != 3 {
		return "", nil, "", errs.Newf(2, "%s: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", cmd, outCcy)
	}
	return fmtChoice, cfg, outCcy, nil
}
