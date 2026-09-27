package main

import (
	"flag"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// holdingsFlagSpec parameterises the shared readout-flag registration
// and validation for the holdings subcommands (positions / accounts /
// portfolios / sources / global). The per-command flag help text and
// validation-prefix wording is passed in explicitly per command, not
// derived.
type holdingsFlagSpec struct {
	cmd           string // error-prefix and validation-message command name
	currencyUsage string // -x short-flag help text
	privacyUsage  string // -p / --privacy help text
	withColumns   bool   // register -C / --columns (every view but global)
	withCash      bool   // register --with-cash (positions only)
}

// holdingsFlags holds the registered flag pointers shared across the
// holdings readout subcommands. cols and withCash are nil for the
// commands that don't register them (see holdingsFlagSpec).
type holdingsFlags struct {
	spec     holdingsFlagSpec
	asOf     *string
	format   *string
	cols     *string
	currency *string
	withCash *bool
	privacy  *bool
}

// registerHoldingsFlags registers the shared readout flags on fs in
// the same order the commands used inline, conditionally including
// -C/--columns and --with-cash per spec. Call before fs.Parse.
func registerHoldingsFlags(fs *flag.FlagSet, spec holdingsFlagSpec) *holdingsFlags {
	hf := &holdingsFlags{spec: spec}
	hf.asOf = fs.String("d", "", "as-of date (YYYY-MM-DD; default today UTC)")
	fs.StringVar(hf.asOf, "as-of", "", "as-of date (YYYY-MM-DD; default today UTC)")
	hf.format = fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(hf.format, "format", "table", "output format: table | csv | csv_plain | json")
	if spec.withColumns {
		hf.cols = fs.String("C", "default", "columns: comma-separated names, or 'default' / 'all'")
		fs.StringVar(hf.cols, "columns", "default", "columns: comma-separated names, or 'default' / 'all'")
	}
	hf.currency = fs.String("x", "", spec.currencyUsage)
	fs.StringVar(hf.currency, "currency", "", "output currency (default: config.default_currency)")
	if spec.withCash {
		hf.withCash = fs.Bool("with-cash", false, "also emit one synthetic row per account+currency with non-zero cash")
	}
	hf.privacy = fs.Bool("p", false, spec.privacyUsage)
	fs.BoolVar(hf.privacy, "privacy", false, spec.privacyUsage)
	return hf
}

// holdingsValues is the validated, ready-to-use result of the shared
// readout flags: output format, as-of epoch, loaded config, and
// resolved output currency.
type holdingsValues struct {
	fmtChoice output.Format
	asOfEpoch int64
	cfg       *config.Config
	outCcy    string
}

// resolve validates the parsed flags (output format, as-of date),
// loads the config, and resolves the output currency — prefixing
// errors with the command name passed in the spec. Call after
// fs.Parse.
func (hf *holdingsFlags) resolve(g globalFlags) (holdingsValues, error) {
	spec := hf.spec

	fmtChoice, err := output.Parse(*hf.format)
	if err != nil {
		return holdingsValues{}, errs.Newf(2, "%s: %s", spec.cmd, err.Error())
	}

	asOfEpoch, err := parseAsOf(*hf.asOf, time.Now())
	if err != nil {
		return holdingsValues{}, errs.Newf(2, "%s: %s", spec.cmd, err.Error())
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return holdingsValues{}, err
	}

	outCcy := strings.ToUpper(*hf.currency)
	if outCcy == "" {
		outCcy = cfg.DefaultCurrency
	}
	if len(outCcy) != 3 {
		return holdingsValues{}, errs.Newf(2, "%s: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", spec.cmd, outCcy)
	}

	return holdingsValues{
		fmtChoice: fmtChoice,
		asOfEpoch: asOfEpoch,
		cfg:       cfg,
		outCcy:    outCcy,
	}, nil
}
