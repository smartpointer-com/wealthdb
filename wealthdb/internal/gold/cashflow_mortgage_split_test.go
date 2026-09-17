package gold

import (
	"context"
	"database/sql"
	"fmt"
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
	for day, mv := range obs {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                                   position_key, asset_class, currency, market_value)
                 VALUES ('cf', ?, 'MORT', 'MORT', 'mortgage', 'USD', ?)`,
			int64(day)*mortDay, mv); err != nil {
			t.Fatalf("seed mortgage balance at day %d: %v", day, err)
		}
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
		// A line that does not split keeps the class's own leaf, which
		// the Sankey attaches to the hub rather than drawing below it.
		if want, split := map[string]string{
			"mortgage_interest":     "Mortgage interest",
			"mortgage_amortization": "Mortgage amortization",
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
		id                 string
		interest, principal float64
		why                string
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

// planHashJoins counts the hash joins in a macro's query plan, which is
// how many times the planner planted its inputs.
func planHashJoins(t *testing.T, db *sql.DB, ctx context.Context, from string) int {
	t.Helper()
	rows, err := db.QueryContext(ctx, `EXPLAIN SELECT * FROM `+from)
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
		n += strings.Count(plan, "HASH_JOIN")
	}
	return n
}

// TestTheSplitDoesNotPlantTheResolutionTwice is the regression for what
// the split first cost rather than for what it computes.
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
// was right. The shape is what has to be pinned: the base may add a
// modest amount of work of its own on top of the resolution, and must
// not plant a second copy of it.
func TestTheSplitDoesNotPlantTheResolutionTwice(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -88000, 200: -80000, 300: -33000})
	seedMortgagePayment(t, db, ctx, "P-1", 100, -9500, "MORT")
	seedMortgagePayment(t, db, ctx, "P-2", 200, -44000, "MORT")

	nodes := planHashJoins(t, db, ctx, "cashflow_txn_nodes(0, 100000000000)")
	base := planHashJoins(t, db, ctx, "cashflow_lines_base(0, 100000000000)")
	if nodes == 0 {
		t.Fatal("the resolution plans no hash join at all; this guard reads nothing")
	}
	// Twice the resolution is the failure, so the bar is just under it.
	// That leaves room for the base's own joins — the pool, and the
	// allocation's small tables — and none for a second copy. The
	// version this guards against planted four and came in at nine
	// times the resolution's count.
	if limit := 2*nodes - 1; base > limit {
		t.Errorf("cashflow_lines_base plans %d hash joins against the resolution's %d "+
			"(limit %d): the resolution looks planted more than once",
			base, nodes, limit)
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
// question is whether the SOURCE went on observing anything after this
// mortgage stopped — not whether a newer MORTGAGE snapshot exists,
// which is the same date as the mortgage's own last observation
// wherever a source tracks a single one, leaving the arm unable to
// fire and the final repayment drawn as interest.
func TestAClosedMortgageRetiresWhatWasLeft(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	// The only mortgage, last seen at day 200 owing 40,000...
	seedMortgageBalances(t, db, ctx, map[int]float64{100: -50000, 200: -40000})
	// ...while the source goes on observing something else.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                               position_key, asset_class, currency, market_value)
             VALUES ('cf', ?, 'BROK', 'BROK', 'equity', 'USD', 1000)`,
		int64(300)*mortDay); err != nil {
		t.Fatalf("seed a later observation: %v", err)
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
