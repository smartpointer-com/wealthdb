package main

import (
	"flag"
	"strings"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
)

// holdingsFlagSpec parameterises the shared readout-flag registration
// and validation for the holdings subcommands (positions / accounts /
// portfolios / sources / global). The per-command flag help text and
// validation-prefix wording is passed in explicitly (not derived) so
// each command's user-facing strings stay byte-for-byte what they
// were before the shared helper.
type holdingsFlagSpec struct {
	cmd            string // error-prefix and validation-message command name
	currencyUsage  string // -x short-flag help text
	fxModeUsage    string // --fx-mode help text
	privacyUsage   string // -p / --privacy help text
	fxModeWantHint bool   // append " (want 'historic' or 'current')" to the invalid --fx-mode error
	withColumns    bool   // register -C / --columns (every view but global)
	withCash       bool   // register --with-cash (positions only)
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
	fxMode   *string
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
	hf.fxMode = fs.String("fx-mode", "historic", spec.fxModeUsage)
	if spec.withCash {
		hf.withCash = fs.Bool("with-cash", false, "also emit one synthetic row per account+currency with non-zero cash")
	}
	hf.privacy = fs.Bool("p", false, spec.privacyUsage)
	fs.BoolVar(hf.privacy, "privacy", false, spec.privacyUsage)
	return hf
}

// holdingsValues is the validated, ready-to-use result of the shared
// readout flags: FX mode, output format, as-of epoch, loaded config,
// and resolved output currency.
type holdingsValues struct {
	mode      canonical.FxMode
	fmtChoice output.Format
	asOfEpoch int64
	cfg       *config.Config
	outCcy    string
}

// resolve validates the parsed flags (FX mode, output format, as-of
// date), loads the config, and resolves the output currency — in the
// exact order and with the exact error-prefix strings the commands
// used inline. Call after fs.Parse.
func (hf *holdingsFlags) resolve(g globalFlags) (holdingsValues, error) {
	spec := hf.spec

	mode := canonical.FxMode(*hf.fxMode)
	if !mode.Valid() {
		if spec.fxModeWantHint {
			return holdingsValues{}, errs.Newf(2, "%s: invalid --fx-mode %q (want 'historic' or 'current')", spec.cmd, *hf.fxMode)
		}
		return holdingsValues{}, errs.Newf(2, "%s: invalid --fx-mode %q", spec.cmd, *hf.fxMode)
	}

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
		mode:      mode,
		fmtChoice: fmtChoice,
		asOfEpoch: asOfEpoch,
		cfg:       cfg,
		outCcy:    outCcy,
	}, nil
}
