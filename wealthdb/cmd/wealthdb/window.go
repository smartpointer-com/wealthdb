package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
)

// windowParser reads a forward view's FROM and TO ("" where absent) at
// now, horizon months its default span: parseForwardWindow,
// parsePlanWindow.
type windowParser func(from, to string, now time.Time, horizon int) (int64, int64, error)

// reportWindow is a report's positional window: the trailing year where
// forward is nil, else forward's reading of FROM [TO]. A refusal prints
// fs's usage and is cmd's usage error.
func reportWindow(fs *flag.FlagSet, cmd string, now time.Time, horizon int, forward windowParser) (int64, int64, error) {
	var from, to int64
	var err error
	switch {
	case forward == nil:
		from, to, err = parseTrailingYearWindow(fs.Args(), now)
	case fs.NArg() > 2:
		err = fmt.Errorf("a window is FROM [TO]")
	default:
		from, to, err = forward(fs.Arg(0), fs.Arg(1), now, horizon)
	}
	if err != nil {
		fs.Usage()
		return 0, 0, errs.Newf(errs.ExitUsage, "%s: %s", cmd, err.Error())
	}
	return from, to, nil
}

// reportPeriods maps the CLI's bucket vocabulary onto the date_trunc
// parts the report macros bucket by. The names are the returns
// family's, extended down to daily / weekly: a spending or income
// report is read at a finer grain than a return, where a quarter is
// the coarsest interesting bucket.
var reportPeriods = map[string]string{
	"daily": "day", "weekly": "week", "monthly": "month",
	"quarterly": "quarter", "annual": "year", "total": "total",
}

// reportPeriodNames is reportPeriods' key set in display order,
// for usage text and error messages.
var reportPeriodNames = []string{"daily", "weekly", "monthly", "quarterly", "annual", "total"}

// periodStart renders the bucket's opening date, empty for the
// `total` bucket, which has no start beyond the window's own.
func periodStart(periodStart *int64) string {
	if periodStart == nil {
		return ""
	}
	return formatDate(*periodStart)
}
