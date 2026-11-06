package gold

import (
	"context"
	"database/sql"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// seedFeeLine books one spending line of a BANK_FEES detailed value.
func seedFeeLine(t *testing.T, db *sql.DB, ctx context.Context, src, id, acct, kind, detailed string, at int64, net string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, description)
        VALUES (?, ?, ?, ?, ?, 'USD', CAST(? AS DECIMAL(28,4)), 'FEE')`, src, id, at, acct, kind, net); err != nil {
		t.Fatalf("seed transaction %s: %v", id, err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
        VALUES (?, ?, 'sig-fee', 1, ?, 'manual', 100)`, src, id, detailed); err != nil {
		t.Fatalf("seed enrichment %s: %v", id, err)
	}
}

// seedFeeHousehold holds, over 2024, all invented:
//
//	PF     a portfolio of a brokerage, 100000 all year, and a cash account
//	       of 10000 that pays its fees: 1200 of management, less a 200
//	       refund, and 50 of wire charges
//	PLAN   a retirement account outside any portfolio, 40000, whose
//	       custody fee of 400 no household account pays
//	CHK    a plumbing cash account outside any portfolio, 5000, with an
//	       ATM fee of 10: returns emits no row for it
//	EMPTY  a brokerage worth nothing, with a fee of 30
//	CARD   a card owing 1000, with an annual fee of 95
func seedFeeHousehold(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	seedReturnsSource(t, db, ctx, "fsrc", "schwab")
	start, end := dy(2023, time.December, 31), dy(2024, time.December, 31)
	pf := "PF"
	if _, err := db.ExecContext(ctx, `INSERT INTO portfolios (silver_source_id, portfolio_external_id, display_name,
		first_seen_at, last_seen_at) VALUES ('fsrc', 'PF', 'Mandate', 1, 1)`); err != nil {
		t.Fatalf("seed portfolio: %v", err)
	}
	seedAcct(t, db, ctx, "fsrc", "BROK", canonical.AccountKindBrokerage, &pf, []snap{{start, 100000}, {end, 100000}}, nil)
	seedCashOnlyAcct(t, db, ctx, "fsrc", "PFCASH", canonical.AccountKindCash, []snap{{start, 10000}, {end, 10000}}, nil)
	if _, err := db.ExecContext(ctx, `UPDATE accounts SET portfolio_external_id = 'PF' WHERE account_external_id = 'PFCASH'`); err != nil {
		t.Fatalf("seed portfolio member: %v", err)
	}
	seedAcct(t, db, ctx, "fsrc", "PLAN", canonical.AccountKindBrokerage, nil, []snap{{start, 40000}, {end, 42000}}, nil)
	seedCashOnlyAcct(t, db, ctx, "fsrc", "CHK", canonical.AccountKindCash, []snap{{start, 5000}, {end, 5000}}, nil)
	seedAcct(t, db, ctx, "fsrc", "EMPTY", canonical.AccountKindBrokerage, nil, []snap{{start, 0}, {end, 0}}, nil)
	seedCashOnlyAcct(t, db, ctx, "fsrc", "CARD", canonical.AccountKindCard, []snap{{start, -1000}, {end, -1000}}, nil)

	at := dy(2024, time.March, 15)
	seedFeeLine(t, db, ctx, "fsrc", "F1", "PFCASH", "fee", "BANK_FEES_INVESTMENT_FEES", at, "-1200")
	seedFeeLine(t, db, ctx, "fsrc", "F2", "PFCASH", "refund", "BANK_FEES_INVESTMENT_FEES", at+86400, "200")
	seedFeeLine(t, db, ctx, "fsrc", "F3", "BROK", "fee", "BANK_FEES_OTHER_BANK_FEES", at, "-50")
	seedFeeLine(t, db, ctx, "fsrc", "F4", "PLAN", "fee", "BANK_FEES_INVESTMENT_FEES", at, "-400")
	seedFeeLine(t, db, ctx, "fsrc", "F5", "CHK", "fee", "BANK_FEES_ATM_FEES", at, "-10")
	seedFeeLine(t, db, ctx, "fsrc", "F6", "EMPTY", "fee", "BANK_FEES_LATE_FEES", at, "-30")
	seedFeeLine(t, db, ctx, "fsrc", "F7", "CARD", "fee", "BANK_FEES_OTHER_BANK_FEES", at, "-95")
}

func feeParams(ccy, period string) ReturnParams {
	return ReturnParams{FromEpoch: bucketAt(2024, time.January, 1), ToEpoch: bucketAt(2025, time.January, 1) - 1,
		OutCcy: ccy, Period: period}
}

func runFees(t *testing.T, db *sql.DB, ctx context.Context, p ReturnParams) map[string]FeeRow {
	t.Helper()
	rows, err := RunFees(ctx, db, p)
	if err != nil {
		t.Fatalf("RunFees: %v", err)
	}
	out := map[string]FeeRow{}
	for _, r := range rows {
		out[r.EntityID] = r
	}
	return out
}

func feeRowHas(r FeeRow, tag string) bool {
	for _, q := range r.Quality {
		if q == tag {
			return true
		}
	}
	return false
}

func TestFeesByEntity(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedFeeHousehold(t, db, ctx)
	rows := runFees(t, db, ctx, feeParams("USD", "total"))

	// The portfolio: both accounts' lines, the refund netted, in two groups.
	pf, ok := rows["PF"]
	if !ok {
		t.Fatalf("no portfolio row: %+v", rows)
	}
	if pf.EntityKind != "portfolio" || pf.EntityLabel != "Mandate" {
		t.Errorf("entity = %s %s", pf.EntityKind, pf.EntityLabel)
	}
	if cents(t, pf.Fees) != 105000 || cents(t, pf.FeesInvestment) != 100000 || cents(t, pf.FeesBank) != 5000 {
		t.Errorf("fees %s, investment %s, bank %s", *pf.Fees, *pf.FeesInvestment, *pf.FeesBank)
	}
	if cents(t, pf.MeanValue) != 11000000 {
		t.Errorf("mean value %s, want 110000", *pf.MeanValue)
	}
	// 2024 has 366 days: 1050 over 110000, annualized on 365.
	want := 1050.0 / 110000 * 365 / 366
	if pf.FeeRate == nil || math.Abs(*pf.FeeRate-want) > 1e-9 {
		t.Errorf("rate %v, want %v", pf.FeeRate, want)
	}
	if pf.TWR == nil {
		t.Errorf("the portfolio has no return: %v", pf.Quality)
	}

	// A vehicle's fee line counts, on an account outside a portfolio.
	if plan := rows["PLAN"]; plan.EntityKind != "account" || cents(t, plan.Fees) != 40000 || plan.TWR == nil {
		t.Errorf("plan = %+v", plan)
	}

	// A plumbing account has a rate and no return.
	chk := rows["CHK"]
	if chk.FeeRate == nil || chk.TWR != nil || !feeRowHas(chk, "no_return") {
		t.Errorf("plumbing = %+v", chk)
	}
	if cents(t, chk.FeesATM) != 1000 {
		t.Errorf("atm %s", *chk.FeesATM)
	}

	// A mean value of zero has no rate, and a fee in `other`.
	empty := rows["EMPTY"]
	if empty.FeeRate != nil || !feeRowHas(empty, "no_mean_value") {
		t.Errorf("empty = %+v", empty)
	}
	if cents(t, empty.FeesOther) != 3000 {
		t.Errorf("other %s", *empty.FeesOther)
	}

	// A card owes: its mean value is below zero, so it has no rate either.
	card := rows["CARD"]
	if card.FeeRate != nil || cents(t, card.MeanValue) != -100000 || !feeRowHas(card, "no_mean_value") {
		t.Errorf("card = %+v", card)
	}
}

// The mean runs over the days the entity held a value, not the window's, and
// the row says it saw part of the window.
func TestFeeMeanValueCountsHeldDays(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "fsrc", "schwab")
	start := dy(2024, time.July, 1)
	seedAcct(t, db, ctx, "fsrc", "EARLY", canonical.AccountKindBrokerage, nil, []snap{{dy(2023, time.December, 31), 500}}, nil)
	seedAcct(t, db, ctx, "fsrc", "LATE", canonical.AccountKindBrokerage, nil, []snap{{start, 1000}, {dy(2024, time.December, 31), 1000}}, nil)
	seedFeeLine(t, db, ctx, "fsrc", "F1", "LATE", "fee", "BANK_FEES_INVESTMENT_FEES", dy(2024, time.September, 2), "-10")
	r := runFees(t, db, ctx, feeParams("USD", "total"))["LATE"]
	if cents(t, r.MeanValue) != 100000 {
		t.Errorf("mean %s, want 1000 over the days held", *r.MeanValue)
	}
	if !feeRowHas(r, "part_window_value") {
		t.Errorf("quality %v", r.Quality)
	}
}

// The rows sum to the spending report's Bank fees, in every currency and
// bucket size, and a row's groups to its fees.
func TestFeesSumToTheSpendingReport(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedFeeHousehold(t, db, ctx)
	seedFX(t, db, dy(2023, time.December, 1), "USD", "CHF", "0.90")
	for _, ccy := range []string{"USD", "CHF"} {
		p := feeParams(ccy, "total")
		cats, err := SpendingCategories(ctx, db, p.FromEpoch, p.ToEpoch, ccy, "total", "primary")
		if err != nil {
			t.Fatalf("SpendingCategories: %v", err)
		}
		var report int64
		for _, c := range cats {
			if c.Category == "BANK_FEES" {
				report = cents(t, c.NetSpend)
			}
		}
		for _, period := range []string{"total", "annual", "quarterly", "monthly"} {
			rows, err := RunFees(ctx, db, feeParams(ccy, period))
			if err != nil {
				t.Fatalf("RunFees: %v", err)
			}
			var sum int64
			for _, r := range rows {
				sum += cents(t, r.Fees)
				if g := cents(t, r.FeesInvestment) + cents(t, r.FeesBank) + cents(t, r.FeesATM) + cents(t, r.FeesOther); absCents(g-cents(t, r.Fees)) > 2 {
					t.Errorf("%s: groups %d, fees %s", r.EntityID, g, *r.Fees)
				}
			}
			if absCents(sum-report) > 5 {
				t.Errorf("%s %s: fee rows %d, spending report %d", ccy, period, sum, report)
			}
		}
	}
}

// The row's return chains the months of `returns accounts --period monthly`
// that end in the window, the figure the fee tiles read.
func TestFeeReturnChainsTheMonths(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedFeeHousehold(t, db, ctx)
	p := feeParams("USD", "total")
	rows := runFees(t, db, ctx, p)
	ret, err := RunReturns(ctx, db, ReturnParams{Level: "accounts", ToEpoch: p.ToEpoch, OutCcy: "USD",
		Method: "twr", Period: "monthly", Annualize: "auto", Netting: true, Inception: "full"})
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	want, n := 1.0, 0
	from, to := EpochDay(p.FromEpoch), EpochDay(p.ToEpoch)
	for _, r := range ret {
		if r.EntityID == "PLAN" && !r.IsSummary && r.EndDay >= from && r.EndDay <= to && r.TWR != nil {
			want *= 1 + *r.TWR
			n++
		}
	}
	if n == 0 {
		t.Fatalf("no months: %+v", ret)
	}
	if got := rows["PLAN"].TWR; got == nil || math.Abs(*got-(want-1)) > 1e-12 {
		t.Errorf("twr %v, chained %v", got, want-1)
	}
}

func TestChainMonths(t *testing.T) {
	t.Parallel()
	r := func(end int64, twr *float64, q ...string) ReturnRow {
		return ReturnRow{EndDay: end, TWR: twr, Quality: q}
	}
	months := []ReturnRow{r(10, f64(0.5)), r(20, f64(0.1), "stale_snapshot"), r(30, nil, "dietz_degenerate"),
		r(40, f64(-1)), r(50, f64(1))}
	got, tags := chainMonths(months, feeBucket{start: 15, end: 45})
	if got == nil || math.Abs(*got-0.1) > 1e-12 {
		t.Errorf("chain %v", got)
	}
	if !slices.Equal(tags, []string{"stale_snapshot", "dietz_degenerate"}) {
		t.Errorf("tags %v", tags)
	}
	if got, _ := chainMonths(months, feeBucket{start: 60, end: 70}); got != nil {
		t.Errorf("a window with no month: %v", *got)
	}
}

func TestFeeBucketsClipToTheWindow(t *testing.T) {
	t.Parallel()
	day := func(y int, m time.Month, d int) int64 { return EpochDay(bucketAt(y, m, d)) }
	got := feeBuckets(day(2024, time.February, 10), day(2024, time.April, 2), "monthly")
	want := []feeBucket{{"2024-02", day(2024, time.February, 10), day(2024, time.February, 29)},
		{"2024-03", day(2024, time.March, 1), day(2024, time.March, 31)},
		{"2024-04", day(2024, time.April, 1), day(2024, time.April, 2)}}
	if len(got) != len(want) {
		t.Fatalf("buckets = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if q := feeBuckets(day(2024, time.November, 1), day(2025, time.February, 1), "quarterly"); len(q) != 2 ||
		q[0].label != "2024-Q4" || q[1].label != "2025-Q1" {
		t.Errorf("quarters = %+v", q)
	}
	if a := feeBuckets(day(2024, time.November, 1), day(2025, time.February, 1), "annual"); len(a) != 2 || a[1].label != "2025" {
		t.Errorf("years = %+v", a)
	}
}

// What web_fees serves sums to the CLI's figures, for the tiles to read.
func TestWebFeesHoldsTheCLIsFigures(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedFeeHousehold(t, db, ctx)
	mustMaterializeServing(t, db, ctx)
	p := feeParams("USD", "total")
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: p.ToEpoch, ComputedAt: 1}); err != nil {
		t.Fatalf("MaterializeReturns: %v", err)
	}
	rows := runFees(t, db, ctx, p)
	window := `day >= TIMESTAMP '2024-01-01' AND day < TIMESTAMP '2025-01-01'`
	var fees, sum float64
	var days int64
	if err := db.QueryRowContext(ctx, `
		SELECT sum(value_usd) FILTER (WHERE line = 'fee')::DOUBLE,
		       sum(value_usd) FILTER (WHERE line = 'value')::DOUBLE,
		       count(DISTINCT day) FILTER (WHERE line = 'value')
		  FROM web_fees WHERE entity = 'Mandate' AND `+window).Scan(&fees, &sum, &days); err != nil {
		t.Fatalf("web_fees: %v", err)
	}
	pf := rows["PF"]
	if toCents(fees) != cents(t, pf.Fees) {
		t.Errorf("web_fees fees %v, CLI %s", fees, *pf.Fees)
	}
	if toCents(sum/float64(days)) != cents(t, pf.MeanValue) {
		t.Errorf("web_fees mean %v, CLI %s", sum/float64(days), *pf.MeanValue)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM web_fees WHERE line = 'return' AND entity = 'Mandate'`).Scan(&n); err != nil {
		t.Fatalf("web_fees returns: %v", err)
	}
	if n == 0 {
		t.Errorf("web_fees holds no monthly return for the portfolio")
	}
}
