package gold

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"strings"
	"testing"
)

// A mortgage instalment is interest AND principal, and no bank prints
// the share. The split is read off the mortgage's own outstanding
// balance: whatever a period retired was principal, the rest was
// interest.
//
// The fixture is an invented mortgage in an impossible era; the day
// numbers are arbitrary ordinals, not dates anyone holds.

const mortDay = 86400

// seedMortgageBalances writes the lender's own figure for the mortgage
// at each observation. market_value is negative: it is a liability.
func seedMortgageBalances(t *testing.T, db *sql.DB, ctx context.Context, obs map[int]float64) {
	t.Helper()
	seedMortgageBalancesAt(t, db, ctx, 0, obs)
}

// seedMortgageBalancesAt is seedMortgageBalances with each observation
// taken `offset` seconds into its day.
func seedMortgageBalancesAt(t *testing.T, db *sql.DB, ctx context.Context, offset int64, obs map[int]float64) {
	t.Helper()
	for day, mv := range obs {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                                   position_key, asset_class, currency, market_value)
                 VALUES ('cf', ?, 'MORT', 'MORT', 'mortgage', 'USD', ?)`,
			int64(day)*mortDay+offset, mv); err != nil {
			t.Fatalf("seed mortgage balance at day %d: %v", day, err)
		}
	}
}

// seedSourceDays has the source observe something other than the
// mortgage once a day, `offset` seconds into each day from `from`
// through `to`: a custody feed that runs nightly beside it.
func seedSourceDays(t *testing.T, db *sql.DB, ctx context.Context, from, to int, offset int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                               position_key, asset_class, currency, market_value)
             SELECT 'cf', d * ? + ?, 'BROK', 'BROK', 'equity', 'USD', 1000
               FROM range(?, ? + 1) r(d)`,
		int64(mortDay), offset, from, to); err != nil {
		t.Fatalf("seed the source's days %d to %d: %v", from, to, err)
	}
}

// seedMortgagePayment books one instalment against the mortgage.
func seedMortgagePayment(t *testing.T, db *sql.DB, ctx context.Context,
	id string, day int, amount float64, farAccount string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, description)
             VALUES ('cf', ?, ?, 'CASH', 'withdrawal', 'USD', ?, ?)`,
		id, int64(day)*mortDay, amount, id); err != nil {
		t.Fatalf("seed payment %s: %v", id, err)
	}
	var far any
	if farAccount != "" {
		far = farAccount
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                merchant_signature, signature_version, spend_detailed, provenance,
                far_silver_source_id, far_account_external_id, far_class, assigned_at)
             VALUES ('cf', ?, 'sig', 1, 'internal_transfer', 'rule', ?, ?, 'mortgage', 100)`,
		id, map[bool]any{true: "cf", false: nil}[farAccount != ""], far); err != nil {
		t.Fatalf("seed overlay %s: %v", id, err)
	}
}

// mortgageShares reads the split back: transaction id → group → amount.
func mortgageShares(t *testing.T, db *sql.DB, ctx context.Context) map[string]map[string]float64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
        SELECT transaction_external_id, grp, CAST(net_amount AS DOUBLE), group_label
          FROM cashflow_lines_base(0, 100000000000)
         WHERE class = 'mortgage'`)
	if err != nil {
		t.Fatalf("read cashflow_lines_base: %v", err)
	}
	defer rows.Close()
	out := map[string]map[string]float64{}
	for rows.Next() {
		var id, grp, label string
		var amt float64
		if err := rows.Scan(&id, &grp, &amt, &label); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// The leaf is what a reader sees; a wrong label is a wrong node.
		// The leaf is the whole of what the diagram says about a
		// mortgage line: which half of an instalment it is, or that it
		// is money borrowed rather than either.
		if want, split := map[string]string{
			"mortgage_interest":     "Mortgage interest",
			"mortgage_amortization": "Mortgage amortization",
			"mortgage_drawdown":     "Mortgage drawdown",
		}[grp]; split && want != label {
			t.Errorf("group %q drew as %q, want %q", grp, label, want)
		}
		if out[id] == nil {
			out[id] = map[string]float64{}
		}
		out[id][grp] = amt
	}
	return out
}

// TestAMortgageInstalmentSplitsByWhatItRetired is the whole rule, in
// the four shapes a mortgage produces.
func TestAMortgageInstalmentSplitsByWhatItRetired(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	// The lender's figure: 8,000 retired over the first interval,
	// 47,000 over the second, and the mortgage still standing at the
	// last observation.
	seedMortgageBalances(t, db, ctx, map[int]float64{
		100: -88000, 200: -80000, 300: -33000,
	})
	// One instalment in the first interval: mixed, and nothing on the
	// row says so.
	seedMortgagePayment(t, db, ctx, "M-MIXED", 100, -9500, "MORT")
	// Two in the second: an extraordinary repayment and the scheduled
	// instalment beside it. The big one takes its face value, the small
	// one takes what principal is left and keeps the rest as interest.
	seedMortgagePayment(t, db, ctx, "M-EXTRA", 200, -44000, "MORT")
	seedMortgagePayment(t, db, ctx, "M-SCHED", 200, -5000, "MORT")
	// One after the last observation, on a mortgage still outstanding:
	// it retired nothing, so it is all interest.
	seedMortgagePayment(t, db, ctx, "M-OPEN", 300, -2200, "MORT")

	got := mortgageShares(t, db, ctx)
	for _, tc := range []struct {
		id                  string
		interest, principal float64
		why                 string
	}{
		{"M-MIXED", -1500, -8000, "the period retired 8,000 of the debt; the rest bought the use of it"},
		{"M-EXTRA", 0, -44000, "an extraordinary repayment retires its own face value"},
		{"M-SCHED", -2000, -3000, "the instalment beside it takes the principal the period has left"},
		{"M-OPEN", -2200, 0, "a period that retired nothing was all interest"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if got[tc.id]["mortgage_interest"] != tc.interest {
				t.Errorf("interest = %v, want %v — %s",
					got[tc.id]["mortgage_interest"], tc.interest, tc.why)
			}
			if got[tc.id]["mortgage_amortization"] != tc.principal {
				t.Errorf("amortization = %v, want %v — %s",
					got[tc.id]["mortgage_amortization"], tc.principal, tc.why)
			}
			// A share of zero is a line that must not be drawn at all:
			// an edge of nothing is still a node on the page.
			if tc.interest == 0 {
				if _, drawn := got[tc.id]["mortgage_interest"]; drawn {
					t.Error("an empty interest share was drawn as a line")
				}
			}
			if tc.principal == 0 {
				if _, drawn := got[tc.id]["mortgage_amortization"]; drawn {
					t.Error("an empty principal share was drawn as a line")
				}
			}
		})
	}
}

// TestAMortgageTheProductDoesNotHoldStaysWhole is the conservative
// direction. With no balance to read there is no share to derive, and
// a guess would either invent a repayment or invent interest. The whole
// instalment draws as interest, which overstates what was consumed
// rather than understating the debt.
func TestAMortgageTheProductDoesNotHoldStaysWhole(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedMortgagePayment(t, db, ctx, "M-UNTRACKED", 100, -6789, "")

	got := mortgageShares(t, db, ctx)
	if got["M-UNTRACKED"]["mortgage_interest"] != -6789 {
		t.Errorf("interest = %v, want the whole instalment",
			got["M-UNTRACKED"]["mortgage_interest"])
	}
	if _, drawn := got["M-UNTRACKED"]["mortgage_amortization"]; drawn {
		t.Error("principal was invented for a mortgage with no balance to read")
	}
}

// TestTheSharesSumToTheInstalment is the property that makes the split
// safe to draw: the statement it feeds still ties to the balances it
// was drawn from, because nothing was created or lost in splitting.
func TestTheSharesSumToTheInstalment(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -88000, 200: -80000, 300: -33000})
	for i, p := range []struct {
		id     string
		day    int
		amount float64
	}{
		{"S-1", 100, -9500}, {"S-2", 200, -44000},
		{"S-3", 200, -5000}, {"S-4", 300, -2200},
	} {
		seedMortgagePayment(t, db, ctx, p.id, p.day, p.amount, "MORT")
		_ = i
	}
	var nodes, lines float64
	if err := db.QueryRowContext(ctx, `
        SELECT (SELECT CAST(COALESCE(SUM(net_amount), 0) AS DOUBLE)
                  FROM cashflow_txn_nodes(0, 100000000000)
                 WHERE class = 'mortgage' AND disposition = 'line'),
               (SELECT CAST(COALESCE(SUM(net_amount), 0) AS DOUBLE)
                  FROM cashflow_lines_base(0, 100000000000)
                 WHERE class = 'mortgage')`).Scan(&nodes, &lines); err != nil {
		t.Fatalf("read totals: %v", err)
	}
	if fmt.Sprintf("%.4f", nodes) != fmt.Sprintf("%.4f", lines) {
		t.Errorf("the split changed the total: nodes %.4f, lines %.4f", nodes, lines)
	}
	// And the node macro still emits ONE row per transaction, which is
	// what report_transactions joins on.
	var n int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM (SELECT transaction_external_id
                                FROM cashflow_txn_nodes(0, 100000000000)
                               WHERE class = 'mortgage'
                               GROUP BY 1 HAVING COUNT(*) > 1)`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d transactions got more than one node row; report_transactions would double-count", n)
	}
}

// planScans counts the scans of `transactions` in a macro's query plan,
// as the macros expand: one scan per copy of whatever reads the table.
// DuckDB shares identical subplans between copies (the common_subplan
// optimiser), which would hide a copy, so it is off for the count. The
// option and the plan share one connection, because the setting belongs
// to the connection.
func planScans(t *testing.T, db *sql.DB, ctx context.Context, from string) int {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET disabled_optimizers = 'common_subplan'`); err != nil {
		t.Fatalf("disable common_subplan: %v", err)
	}
	rows, err := conn.QueryContext(ctx, `EXPLAIN (FORMAT json) SELECT * FROM `+from)
	if err != nil {
		t.Fatalf("explain %s: %v", from, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var kind, plan string
		if err := rows.Scan(&kind, &plan); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n += strings.Count(plan, `.transactions"`)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain %s: %v", from, err)
	}
	return n
}

// TestTheSplitPlantsNoThirdResolution is the regression for what the
// split must not cost rather than for what it computes.
//
// A table macro is INLINED wherever it is named, so `cashflow_txn_nodes`
// — a dozen joins over every transaction gold holds — lands in the plan
// once per reference. The first version of the split named it four
// times over: once for the lines, once inside a helper that found the
// mortgage payments, and once per arm of a UNION ALL emitting the two
// shares. Row counts stayed small the whole way, so no result set
// showed it; the memory a single query needed tripled, and the
// dashboard — six cards against one shared pool — ran out and failed on
// every card.
//
// Nothing about the arithmetic can catch that, because the arithmetic
// was right. The shape is what has to be pinned. The base names the
// resolution twice, once for the window's lines and once for every
// instalment there has ever been, and must plant no third copy. The
// copies are counted by the scans of `transactions`, which the base
// reads only through the resolution.
func TestTheSplitPlantsNoThirdResolution(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -88000, 200: -80000, 300: -33000})
	seedMortgagePayment(t, db, ctx, "P-1", 100, -9500, "MORT")
	seedMortgagePayment(t, db, ctx, "P-2", 200, -44000, "MORT")

	nodes := planScans(t, db, ctx, "cashflow_txn_nodes(0, 100000000000)")
	base := planScans(t, db, ctx, "cashflow_lines_base(0, 100000000000)")
	if nodes == 0 {
		t.Fatal("the resolution plans no scan of transactions; this guard reads nothing")
	}
	if base > 2*nodes {
		t.Errorf("cashflow_lines_base scans transactions %d times against the resolution's %d: "+
			"the resolution is planted more than twice", base, nodes)
	}
}

// seedFarMortgageRow books a row whose FAR account is the mortgage but
// whose verdict sends it somewhere other than `mortgage` — a payment a
// tier above the matcher re-placed, which keeps the far account it was
// paired on. The split must never read such a row, and must never let
// it spend the interval's principal either.
func seedFarMortgageRow(t *testing.T, db *sql.DB, ctx context.Context,
	id string, day int, amount float64, detailed string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, description)
             VALUES ('cf', ?, ?, 'CASH', 'withdrawal', 'USD', ?, ?)`,
		id, int64(day)*mortDay, amount, id); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                merchant_signature, signature_version, spend_detailed, provenance,
                far_silver_source_id, far_account_external_id, assigned_at)
             VALUES ('cf', ?, 'sig', 1, ?, 'manual', 'cf', 'MORT', 100)`,
		id, detailed); err != nil {
		t.Fatalf("seed overlay %s: %v", id, err)
	}
}

// TestTheSplitDoesNotDependOnTheWindowAsked is the defect that reached
// production: the payments competing for an interval's principal were
// clipped to the REPORT RANGE, so an instalment was split against
// whichever of its siblings the reader happened to be looking at. The
// same row reported one split over a year and another over the month
// containing it — and the narrow window reported MORE principal than
// the interval ever retired, because the payment that should have taken
// it was outside the range.
//
// An interval's allocation is a property of the interval. The question
// asked of it cannot change the answer.
func TestTheSplitDoesNotDependOnTheWindowAsked(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -88000, 200: -80000, 300: -33000})
	// Two instalments in one interval: the big one takes the principal.
	seedMortgagePayment(t, db, ctx, "W-BIG", 120, -6000, "MORT")
	seedMortgagePayment(t, db, ctx, "W-SMALL", 150, -5000, "MORT")

	read := func(from, to int) map[string]float64 {
		rows, err := db.QueryContext(ctx, `
            SELECT transaction_external_id, grp, CAST(net_amount AS DOUBLE)
              FROM cashflow_lines_base(?, ?) WHERE class = 'mortgage'`,
			int64(from)*mortDay, int64(to)*mortDay)
		if err != nil {
			t.Fatalf("lines_base(%d,%d): %v", from, to, err)
		}
		defer rows.Close()
		out := map[string]float64{}
		for rows.Next() {
			var id, grp string
			var amt float64
			if err := rows.Scan(&id, &grp, &amt); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[id+"/"+grp] = amt
		}
		return out
	}
	whole := read(0, 1000)
	// A window holding only the SMALL instalment. Under the defect it
	// took the whole interval's principal, the big one being invisible.
	narrow := read(140, 160)
	for k, v := range narrow {
		if whole[k] != v {
			t.Errorf("%s = %v over the narrow window, %v over the whole: "+
				"the split moved with the question asked", k, v, whole[k])
		}
	}
	if len(narrow) == 0 {
		t.Fatal("the narrow window returned nothing; the test reads nothing")
	}
}

// TestADrawdownIsNotSplitAndTakesNoPrincipal pins the direction. A
// mortgage line runs either way — docs/CASHFLOW.md §4, a drawdown is an
// inflow — and only an outflow retires debt. Ranked on absolute size,
// money BORROWED could outrank the repayments, take the principal they
// had retired, and draw itself as a positive interest edge on the wrong
// side of the diagram.
func TestADrawdownIsNotSplitAndTakesNoPrincipal(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -88000, 200: -80000, 300: -33000})
	// A drawdown larger than every repayment beside it.
	seedMortgagePayment(t, db, ctx, "D-DRAW", 110, 40000, "MORT")
	seedMortgagePayment(t, db, ctx, "D-REPAY", 120, -6000, "MORT")

	got := mortgageShares(t, db, ctx)
	if _, split := got["D-DRAW"]["mortgage_amortization"]; split {
		t.Error("a drawdown was drawn as amortization: it borrows debt, it does not retire it")
	}
	if _, split := got["D-DRAW"]["mortgage_interest"]; split {
		t.Error("a drawdown was drawn as interest")
	}
	// And it did not eat the repayment's principal: the interval retired
	// 8,000, which the repayment is capped at its own 6,000 of.
	if got["D-REPAY"]["mortgage_amortization"] != -6000 {
		t.Errorf("the repayment kept %v of principal, want -6000: the drawdown took it",
			got["D-REPAY"]["mortgage_amortization"])
	}
}

// TestOnlyAMortgageLineSpendsThePrincipal is the hole in the previous
// migration's own safety argument. It drew the payments from the raw
// tables — every row whose FAR account is a mortgage — and argued that
// was safe because the share is only ever READ on a line the resolution
// classed as mortgage. That is an argument about reading. A widened row
// still ranked in the allocation and still SPENT the interval's
// decline, and the share it took was then dropped, turning real
// amortisation into interest with nothing to show for it.
func TestOnlyAMortgageLineSpendsThePrincipal(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -88000, 200: -80000, 300: -33000})
	// Bigger than the instalment, paired to the same mortgage, and
	// placed by a tier that sends it to financing's OTHER class.
	seedFarMortgageRow(t, db, ctx, "S-PINNED", 110, -7500, "debt_repayment")
	seedMortgagePayment(t, db, ctx, "S-REAL", 120, -6000, "MORT")

	got := mortgageShares(t, db, ctx)
	// The interval retired 8,000 and the instalment is 6,000 of it.
	if got["S-REAL"]["mortgage_amortization"] != -6000 {
		t.Errorf("the instalment kept %v of principal, want -6000: a row "+
			"nothing reads spent the interval's decline",
			got["S-REAL"]["mortgage_amortization"])
	}
	if _, drawn := got["S-PINNED"]; drawn {
		t.Error("a row the resolution did not class as mortgage was drawn as one")
	}
}

// TestAClosedMortgageRetiresWhatWasLeft covers the arm that decides
// whether a mortgage's LAST interval retired the rest of the loan. The
// question is whether the mortgage's last observation ENDS, by the
// ending every reader takes — not whether a newer MORTGAGE snapshot
// exists, which is the same date as the mortgage's own last observation
// wherever a source tracks a single one, leaving the arm unable to fire
// and the final repayment drawn as interest.
func TestAClosedMortgageRetiresWhatWasLeft(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	// The only mortgage, observed every 100 days with a custody account
	// and last seen at day 200 owing 40,000...
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -50000, 200: -40000})
	// ...while the source goes on reporting the custody account alone,
	// two of the mortgage's cadences on: its run re-covers the mortgage's
	// company, longer after it than the mortgage's own pace explains.
	for _, day := range []int{100, 200, 400} {
		seedSourceDays(t, db, ctx, day, day, 0)
	}
	seedMortgagePayment(t, db, ctx, "C-CLOSE", 250, -41000, "MORT")

	got := mortgageShares(t, db, ctx)
	if got["C-CLOSE"]["mortgage_amortization"] != -40000 {
		t.Errorf("the closing retired %v, want -40000 — the whole of what was left",
			got["C-CLOSE"]["mortgage_amortization"])
	}
	if got["C-CLOSE"]["mortgage_interest"] != -1000 {
		t.Errorf("the closing's interest = %v, want -1000",
			got["C-CLOSE"]["mortgage_interest"])
	}
}

// The mortgage the end tests read: reported monthly by its lender, its
// balance falling 1,000 a month, last seen at day 190.
var monthlyMortgage = map[int]float64{100: -90000, 130: -89000, 160: -88000, 190: -87000}

// TestAMortgageEndsAfterOneAndAHalfCadences is a mortgage beside a
// faster feed. Its lender reports it monthly, in the run of a custody
// feed that runs nightly in the same source. Each night the source
// re-covers the mortgage's company without it, which is the mortgage's
// pace and not its closing: it stands until the source runs more than
// one and a half of its cadences past its last observation. Standing,
// its last interval retires nothing, and the instalment there is all
// interest. Ended, the interval retires what was left. How the shared
// ending reads a cadence is TestTheCadenceIsTheMedianOfTheKeysDays's.
func TestAMortgageEndsAfterOneAndAHalfCadences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		last   int // the day of the source's newest nightly snapshot
		repaid bool
	}{
		{"a month on, before its next observation loads", 221, false},
		{"one and a half cadences on", 235, false},
		{"past one and a half cadences", 236, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := openMigrated(t)
			seedResolutionFixture(t, db, ctx)
			seedMortgageBalances(t, db, ctx, monthlyMortgage)
			seedSourceDays(t, db, ctx, 100, tc.last, 0)
			seedMortgagePayment(t, db, ctx, "K-OPEN", 195, -1500, "MORT")

			want := map[string]float64{"mortgage_interest": -1500}
			if tc.repaid {
				want = map[string]float64{"mortgage_amortization": -1500}
			}
			if got := mortgageShares(t, db, ctx)["K-OPEN"]; !maps.Equal(got, want) {
				t.Errorf("the last interval's instalment splits as %v, want %v (repaid: %v)",
					got, want, tc.repaid)
			}
		})
	}
}

// TestAMortgageObservedOnceTakesAMonthAsItsCadence is a mortgage with no
// gap to read. Observed once, it takes a statement's month as its
// cadence: its source running on the next day leaves it standing, and
// past one and a half months it reads as repaid, its last interval
// retiring the whole of what was left.
func TestAMortgageObservedOnceTakesAMonthAsItsCadence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		srcDay int
		want   map[string]float64
	}{
		{"the next day", 101, map[string]float64{"mortgage_interest": -41000}},
		{"at one and a half months", 145, map[string]float64{"mortgage_interest": -41000}},
		{"past one and a half months", 146,
			map[string]float64{"mortgage_amortization": -40000, "mortgage_interest": -1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := openMigrated(t)
			seedResolutionFixture(t, db, ctx)
			seedMortgageBalances(t, db, ctx, map[int]float64{100: -40000})
			seedSourceDays(t, db, ctx, 100, 100, 0)
			seedSourceDays(t, db, ctx, tc.srcDay, tc.srcDay, 0)
			seedMortgagePayment(t, db, ctx, "O-CLOSE", 100, -41000, "MORT")
			if got := mortgageShares(t, db, ctx)["O-CLOSE"]; !maps.Equal(got, tc.want) {
				t.Errorf("the instalment splits as %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAMortgageInTheSourcesNewestSnapshotStands is the day grain. A
// mortgage observed in its source's newest snapshot is as current as
// the source is: read instant against instant, it stands, though its
// day began hours before that snapshot. Both shapes stand: one with a
// cadence, and one observed once.
func TestAMortgageInTheSourcesNewestSnapshotStands(t *testing.T) {
	const run = 2 * 3600 // the nightly run lands at 02:00
	for _, tc := range []struct {
		name string
		obs  map[int]float64
	}{
		{"observed monthly", monthlyMortgage},
		{"observed once", map[int]float64{190: -87000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := openMigrated(t)
			seedResolutionFixture(t, db, ctx)
			seedMortgageBalancesAt(t, db, ctx, run, tc.obs)
			seedSourceDays(t, db, ctx, 100, 190, run)
			seedMortgagePayment(t, db, ctx, "G-OPEN", 192, -1500, "MORT")

			want := map[string]float64{"mortgage_interest": -1500}
			if got := mortgageShares(t, db, ctx)["G-OPEN"]; !maps.Equal(got, want) {
				t.Errorf("the instalment after the newest snapshot splits as %v, want %v: "+
					"a mortgage observed in it is still standing", got, want)
			}
		})
	}
}

// TestMigration0143DDLIsRerunnable holds the mortgage's end to the
// replay bar. Replaying 0093, 0096 or 0143 puts an older end rule back,
// so the rerun replays every later re-issue after it, as Migrate would.
// 0144 re-issues cashflow_lines_base on the shared ending; its replay is
// held here, on a mortgage book.
// The monthly mortgage, reported at 02:00 in a run of its own, has no
// company, while the feed at 03:00 runs two cadences past it: 0143's rule
// reads it as repaid, the shared ending as standing, after each replay.
func TestMigration0143DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedMortgageBalancesAt(t, db, ctx, 2*3600, monthlyMortgage)
	seedSourceDays(t, db, ctx, 100, 250, 3*3600)
	seedMortgagePayment(t, db, ctx, "Q-OPEN", 195, -1500, "MORT")

	want := map[string]float64{"mortgage_interest": -1500}
	for _, file := range []string{
		"0144_account_end_cadence.sql",
		"0143_mortgage_end_cadence.sql",
		"0096_cashflow_mortgage_drawdown_leaf.sql",
		"0093_cashflow_mortgage_split_fixes.sql",
	} {
		rerunMigrationDDL(t, db, ctx, file)
		if got := mortgageShares(t, db, ctx)["Q-OPEN"]; !maps.Equal(got, want) {
			t.Errorf("after replaying %s the open instalment splits as %v, want %v", file, got, want)
		}
	}
}

// TestAMortgageInARunOfItsOwnWaitsForTheHorizon: a mortgage its lender
// reports in a run of its own, at 02:00 beside a custody feed at 03:00,
// has no company for a later run to re-cover. Only the horizon ends it:
// it stands past one and a half cadences and at 300 days, and reads as
// repaid once its source runs more than hist_carry_days() past it.
func TestAMortgageInARunOfItsOwnWaitsForTheHorizon(t *testing.T) {
	for _, c := range []struct {
		last   int // the day of the source's newest nightly snapshot
		repaid bool
	}{{236, false}, {490, false}, {555, false}, {556, true}} {
		t.Run(fmt.Sprint(c.last), func(t *testing.T) {
			db, ctx := openMigrated(t)
			seedResolutionFixture(t, db, ctx)
			seedMortgageBalancesAt(t, db, ctx, 2*3600, monthlyMortgage)
			seedSourceDays(t, db, ctx, 100, c.last, 3*3600)
			seedMortgagePayment(t, db, ctx, "H-OPEN", 195, -1500, "MORT")
			want := map[string]float64{"mortgage_interest": -1500}
			if c.repaid {
				want = map[string]float64{"mortgage_amortization": -1500}
			}
			if got := mortgageShares(t, db, ctx)["H-OPEN"]; !maps.Equal(got, want) {
				t.Errorf("the source runs to day %d: the instalment splits as %v, want %v", c.last, got, want)
			}
		})
	}
}

// TestADrawdownIsDrawnAndTheHubStillConserves is the defect a class
// with BOTH shapes produces. The Sankey's leaf stage is the self-edge
// rule, and the split relabels the group on an OUTFLOW only, so a
// drawdown kept `mortgage` for its group and fell out of the leaves.
// Its class then had leaves, which took the class out of the atoms in
// its own right too — and a class's hub edge is the sum of its leaves,
// so the drawdown was in neither. The residual cash node went on
// counting it, so the diagram did not merely lose the money: it read
// as a household that saved what it had in fact borrowed.
//
// Both orientations, because which way the class nets decides where
// the drawdown hangs and only one of the two was ever exercised.
func TestADrawdownIsDrawnAndTheHubStillConserves(t *testing.T) {
	for _, tc := range []struct {
		name            string
		draw, repay     float64
		wantResidual    float64
		wantDrawdownVia string
	}{
		// Borrowed more than repaid: the class nets INTO the household,
		// the drawdown runs with it and draws through it, and the two
		// repayment leaves detach onto the other side.
		{"the class nets the way the drawdown runs", 40000, -9500, 30500, "Mortgage"},
		// Repaid more than borrowed: the class nets OUT, so the
		// drawdown runs against it and attaches to the hub itself.
		{"the class nets against the drawdown", 4000, -9500, -5500, "Household"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := openMigrated(t)
			seedResolutionFixture(t, db, ctx)
			if _, err := db.ExecContext(ctx, `
                INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
                     VALUES ('cf', 0, 'USD', 'USD', 1.0)`); err != nil {
				t.Fatalf("seed fx: %v", err)
			}
			// The interval retired 8,000, so the repayment splits.
			seedMortgageBalances(t, db, ctx, map[int]float64{100: -88000, 200: -80000})
			seedMortgagePayment(t, db, ctx, "H-REPAY", 100, tc.repay, "MORT")
			seedMortgagePayment(t, db, ctx, "H-DRAW", 110, tc.draw, "MORT")

			rows, err := CashflowSankey(ctx, db, 0, 100000000000, "USD", "group", "whole")
			if err != nil {
				t.Fatalf("CashflowSankey: %v", err)
			}
			var intoHub, outOfHub float64
			edge := map[string]float64{}
			for _, r := range rows {
				v := mustFloat(t, r.Value, "value")
				switch {
				case r.Target == "Household":
					intoHub += v
				case r.Source == "Household":
					outOfHub += v
				}
				edge[r.Source+"->"+r.Target] = v
			}
			if math.Abs(intoHub-outOfHub) > 0.005 {
				t.Errorf("%.2f reaches the hub and %.2f leaves it; edges: %v",
					intoHub, outOfHub, edge)
			}
			// The money borrowed is on the page, through whichever node
			// the class's own direction puts it behind.
			if got := edge["Mortgage drawdown->"+tc.wantDrawdownVia]; math.Abs(got-tc.draw) > 0.005 {
				t.Errorf("the drawdown draws into %q as %.2f, want %.2f; edges: %v",
					tc.wantDrawdownVia, got, tc.draw, edge)
			}
			// And the residual says what the household actually kept.
			residual := edge["Cash savings->Household"] - edge["Household->Cash savings"]
			if math.Abs(residual+tc.wantResidual) > 0.005 {
				t.Errorf("the residual is %.2f, want %.2f; edges: %v",
					-residual, tc.wantResidual, edge)
			}
		})
	}
}

// TestNoFinancingGroupRepeatsItsClass is the invariant behind the fix
// rather than the one case that broke it. A section with a leaf stage
// draws a row through a leaf, and a row whose group repeats its class
// has no leaf to be drawn through — it is dropped from the leaves by
// the self-edge rule and from the atoms by its class having others, so
// it reaches the diagram nowhere while the residual keeps counting it.
func TestNoFinancingGroupRepeatsItsClass(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -88000, 200: -80000})
	seedMortgagePayment(t, db, ctx, "I-REPAY", 100, -9500, "MORT")
	seedMortgagePayment(t, db, ctx, "I-DRAW", 110, 40000, "MORT")
	seedMortgagePayment(t, db, ctx, "I-ZERO", 120, 0, "MORT")

	// Both spellings of the rule: gold's Sankey compares the node IDS
	// and the dashboard's compares the LABELS, so a pair that collides
	// in either one is dropped on that surface alone.
	rows, err := db.QueryContext(ctx, `
        SELECT section, class, grp, class = grp AS same_id, COUNT(*)
          FROM cashflow_lines_base(0, 100000000000)
         WHERE section IN ('operating_in', 'operating_out', 'financing')
           AND class <> '(uncategorized)'
           AND (class = grp OR class_label = group_label)
         GROUP BY 1, 2, 3, 4`)
	if err != nil {
		t.Fatalf("read cashflow_lines_base: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var section, class, grp string
		var sameID bool
		var n int
		if err := rows.Scan(&section, &class, &grp, &sameID, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("%d row(s) in %s.%s group as %q, repeating the class's %s: "+
			"a leaf-stage section draws them nowhere",
			n, section, class, grp, map[bool]string{true: "id", false: "label"}[sameID])
	}
}

// TestEveryMortgageRowSurvivesItsSplit is the macro's own invariant,
// asked of every shape at once: a row's emitted shares sum to the row,
// and no row is emitted as nothing. A mortgage outflow that retired no
// principal and was itself under half a cent satisfied neither share's
// admission test and vanished — small, but the one way a split could
// fail to conserve.
func TestEveryMortgageRowSurvivesItsSplit(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -88000, 200: -80000, 300: -33000})
	for _, tc := range []struct {
		id     string
		day    int
		amount float64
	}{
		{"V-MIXED", 100, -9500},  // splits into both shares
		{"V-WHOLE", 200, -47000}, // the interval's whole decline
		{"V-OPEN", 300, -2200},   // retired nothing: all interest
		{"V-DUST", 300, -0.004},  // retired nothing and under half a cent
		{"V-DRAW", 110, 40000},   // money borrowed
		{"V-NIL", 120, 0},        // a zero-amount mortgage row
	} {
		seedMortgagePayment(t, db, ctx, tc.id, tc.day, tc.amount, "MORT")
	}

	shares := mortgageShares(t, db, ctx)
	for _, tc := range []struct {
		id   string
		want float64
	}{
		{"V-MIXED", -9500}, {"V-WHOLE", -47000}, {"V-OPEN", -2200},
		{"V-DUST", -0.004}, {"V-DRAW", 40000}, {"V-NIL", 0},
	} {
		lines, ok := shares[tc.id]
		if !ok {
			t.Errorf("%s was emitted as no line at all", tc.id)
			continue
		}
		var sum float64
		for _, v := range lines {
			sum += v
		}
		if math.Abs(sum-tc.want) > 0.0005 {
			t.Errorf("%s: the shares sum to %v, want %v (%v)", tc.id, sum, tc.want, lines)
		}
	}
}
