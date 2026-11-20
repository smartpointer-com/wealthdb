package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// checkPrivateWindow refuses a window: the view reads every leg since a
// holding's first call, to the latest snapshot. today is the end of the
// current day, the default window's end.
func checkPrivateWindow(fromEpoch, toEpoch, today int64) error {
	if fromEpoch > 0 || toEpoch < today {
		return errors.New("private reads every leg since the first call, to the latest snapshot: it takes no window")
	}
	return nil
}

// returnsPrivateReport is the private-markets view, the runner the CLI and
// the MCP server share.
func returnsPrivateReport(req request) *report {
	kindOf, loadKinds := sourceKinds()
	fetch := func(ctx context.Context, db *sql.DB) ([]gold.PrivateRow, error) {
		return gold.RunPrivate(ctx, db, req.currency)
	}
	return withLoads(newReport(buildPrivateColumnRegistry(req.currency, kindOf), defaultPrivateColumns, fetch),
		loadKinds)
}

var defaultPrivateColumns = []string{"silver_source", "account", "holding", "first_call", "paid_in",
	"distributed", "nav", "marked_at", "dpi", "tvpi", "irr_pct", "quality"}

// buildPrivateColumnRegistry is the private view's registry. Its account
// columns read as `returns accounts` reads its entity, and its holding
// columns as `holdings positions` reads a holding: -p hides the money
// and the account, never what is held.
func buildPrivateColumnRegistry(outCcy string, kindOf func(string) string) []columnSpec[gold.PrivateRow] {
	ref := func(r gold.PrivateRow) accountRef {
		return accountRef{source: r.SilverSourceID, name: r.DisplayName, nickname: r.Nickname, id: r.AccountExternalID}
	}
	holding := func(r gold.PrivateRow) (string, string) { return r.SilverSourceID, r.InstrumentID }
	return []columnSpec[gold.PrivateRow]{
		strCol("silver_source", func(r gold.PrivateRow) string { return r.SilverSourceID }),
		accountCol(kindOf, ref),
		accountIDCol("account_id", kindOf, ref),
		holdingNameCol(strCol("holding", func(r gold.PrivateRow) string { return r.Holding }), kindOf, holding),
		holdingIDCol(strCol("holding_id", func(r gold.PrivateRow) string { return r.InstrumentID }), kindOf, holding),
		dateCol("first_call", func(r gold.PrivateRow) int64 { return r.FirstCall }),
		outCcyCol("paid_in", outCcy, func(r gold.PrivateRow) *string { return r.PaidIn }),
		outCcyCol("distributed", outCcy, func(r gold.PrivateRow) *string { return r.Distributed }),
		outCcyCol("nav", outCcy, func(r gold.PrivateRow) *string { return r.NAV }),
		datePtrCol("marked_at", func(r gold.PrivateRow) *int64 { return r.MarkedAt }),
		dateCol("end_date", func(r gold.PrivateRow) int64 { return r.EndAt }),
		decimalCol("dpi", func(r gold.PrivateRow) *float64 { return r.DPI }),
		decimalCol("tvpi", func(r gold.PrivateRow) *float64 { return r.TVPI }),
		ratioOrBlankCol("irr_pct", "", func(r gold.PrivateRow) *float64 { return r.IRR }),
		qualityCol(func(r gold.PrivateRow) string { return strings.Join(r.Quality, ";") }),
	}
}

func returnsPrivateUsage() string {
	return `usage: wealthdb returns private [-f FORMAT] [-C COLS] [-F COND]... [-x CCY] [-p]

What each private fund or SPV took, what it gave back and what it is
marked at, since its first call, to the latest snapshot. One row per
holding, then the total.

A private holding is an instrument in one account that a contribution
line names: a capital call, or an SPV subscription. A private company's
shares bought directly are a buy, and founder stock has no cash leg, so
neither is one. Their gains are in 'wealthdb gains'. A holding with no
money in, out or marked, such as a call refunded whole, is left out.

  paid_in      the calls, net of refunds
  distributed  what came back: distributions, sales, and exits in kind
               at the value their transfer states. An exit in other
               shares counts for the holding whose holding_id its
               ledger row's holding column names
  nav          the holding's value at the latest snapshot; 0 once it
               has left the holdings, blank where it never had a value
  marked_at    the day of that value's mark, or of its last line's once
               it has left. Marks are old by nature
  dpi          distributed / paid_in
  tvpi         (distributed + nav) / paid_in
  irr_pct      the money-weighted return since the first call, a yearly
               rate: the returns engine's, on every leg and the NAV
  end_date     the day irr_pct ends: the later of the NAV's snapshot and
               the last leg

Each leg converts at its own day's rate, and the NAV at its snapshot's,
so every currency reads at historic FX. A holding's money rounds to the
cent, and the total adds the cents. The total's dpi and tvpi pool the
holdings: the sums' ratios, not an average of the rows' ratios. Its
irr_pct pools the legs of every holding listed, and its marked_at is
the oldest mark. A holding with no value leaves the total's nav, tvpi
and irr_pct blank. The total is over every holding: a filter keeps or
drops it as a row, never recomputes it. The total also counts each exit
in kind out of a listed holding's account that names no holding, unless
the account bought the shares or took them in.

What the figures miss:
  - A call after the last mark sits outside the NAV until the next mark,
    so tvpi reads low; a distribution after it can read high.
  - Commitments and unfunded capital are not in gold.
  - The view carries no volatility or other risk figure: marks are too
    old and sparse for one.

The quality column says what a row misses:
  no_mark                  no value was ever recorded: nav, tvpi and
                           irr_pct are blank
  exited                   the holding has left the holdings: nav is 0
  at_cost                  the source carries the NAV at cost, before
                           any valuation: tvpi reads 1 and irr_pct 0
  call_after_mark          a call came after the mark
  distribution_after_mark  a distribution or exit came after the mark
  exit_unvalued=N          N exits in kind state no value
  unattributed_exit=N      on the total: N exits in kind name no
                           holding; the total counts them, no row does
  unpriced=N               N legs or lines have no rate to the output
                           currency: the sums leave them out, and a
                           leg with none leaves irr_pct blank
and the returns engine's mwr_* tags on irr_pct. On the total each tag
counts the holdings it holds on; exit_unvalued and unpriced also count
the exits that name no holding.

Flags:
  -f, --format      table | csv | csv_plain | json
  -C, --columns     comma-separated names, 'default', 'all', or a +ADD,-REMOVE delta
  -F, --filter COND keep the rows where COND holds; repeatable;
                    wealthdb help filter lists the operators
  -x, --currency    output currency (default: config.default_currency)
  -p, --privacy     redact the amounts and the accounts; the holdings'
                    names, the multiples and irr_pct stay visible

--method, --period, --annualize, --netting, --inception, --benchmark and
a window do not apply.

Available columns:
  ` + joinColumnNames(buildPrivateColumnRegistry("CCY", nil)) + `

Default column set:
  ` + strings.Join(defaultPrivateColumns, ", ")
}
