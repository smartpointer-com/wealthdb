package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"testing"
)

// The four reports, and the two identities the design says are
// structural: the summary's four sections sum to net_cash_flow, and a
// bucket's flows rows — the cash row included — sum to zero.
//
// Both are guards against arithmetic rather than against a wrong
// population, which is what the reconciliation memo is for. They are
// worth pinning all the same: they are what makes the diagram balance
// by construction, and an aggregate that broke one would draw a Sankey
// whose two sides did not meet.

// seedReportFixture lays a window of lines over the resolution fixture:
// two months, every section represented, and an FX rate so a value
// column is not universally NULL. Every figure is invented.
func seedReportFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
             VALUES ('cf', 0, 'USD', 'USD', 1.0);`); err != nil {
		t.Fatalf("seed fx: %v", err)
	}
	seedLines(t, db, ctx, []line{
		// Operating in, three classes.
		{id: "R-WAGE", account: "CASH", kind: "deposit", amount: 6000, income: "INCOME_WAGES"},
		{id: "R-DIV", account: "BROK", kind: "dividend", amount: 900, instrument: "EQ", income: "INCOME_DIVIDENDS"},
		{id: "R-REFUND-IN", account: "CASH", kind: "deposit", amount: 50, income: "INCOME_TAX_REFUND"},
		// Operating out, the three lifted classes and consumption.
		{id: "R-SHOP", account: "CARD", kind: "purchase", amount: -400, spend: "FOOD_AND_DRINK_GROCERIES"},
		{id: "R-SHOP-BACK", account: "CARD", kind: "refund", amount: 40, spend: "FOOD_AND_DRINK_GROCERIES"},
		{id: "R-FEE", account: "BROK", kind: "fee", amount: -60, spend: "BANK_FEES_INVESTMENT_FEES"},
		{id: "R-TAX", account: "CASH", kind: "tax", amount: -1200, spend: "GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT"},
		{id: "R-GIVE", account: "CASH", kind: "withdrawal", amount: -300, spend: "GOVERNMENT_AND_NON_PROFIT_DONATIONS"},
		// Investing, two asset classes moving opposite ways.
		{id: "R-BUY", account: "BROK", kind: "buy", amount: -2000, instrument: "EQ"},
		{id: "R-DIST", account: "BROK", kind: "distribution", amount: 700, instrument: "PE"},
		// Financing and a vehicle crossing.
		{id: "R-MORT", account: "CASH", kind: "withdrawal", amount: -1800,
			spend: "internal_transfer", farClass: "mortgage"},
		{id: "R-RET", account: "CASH", kind: "withdrawal", amount: -500, spend: "retirement_transfer"},
		// Two rows the statement never draws.
		{id: "R-INTERNAL", account: "CASH", kind: "withdrawal", amount: -100,
			spend: "internal_transfer", farAccount: "SAVE"},
		{id: "R-FX", account: "CASH", kind: "fx", amount: -25},
	})
}

func mustFloat(t *testing.T, s *string, what string) float64 {
	t.Helper()
	if s == nil {
		t.Fatalf("%s is nil", what)
	}
	v, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		t.Fatalf("%s = %q: %v", what, *s, err)
	}
	return v
}

// TestSummaryClosesOnNetCashFlow pins the statement's own arithmetic:
// operating is the signed difference of the two magnitudes, and the
// four sections sum to net_cash_flow.
func TestSummaryClosesOnNetCashFlow(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)

	rows, err := CashflowSummary(ctx, db, 0, 3500000, "USD", "total")
	if err != nil {
		t.Fatalf("CashflowSummary: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d buckets, want 1", len(rows))
	}
	r := rows[0]
	income := mustFloat(t, r.OperatingIn, "operating_in")
	spending := mustFloat(t, r.OperatingOut, "operating_out")
	operating := mustFloat(t, r.Operating, "operating")
	investing := mustFloat(t, r.Investing, "investing")
	financing := mustFloat(t, r.Financing, "financing")
	vehicles := mustFloat(t, r.Vehicles, "vehicles")
	net := mustFloat(t, r.NetCashFlow, "net_cash_flow")

	// The two operating magnitudes are positive, as the families'
	// spend and refund columns are.
	if income <= 0 || spending <= 0 {
		t.Errorf("income %.2f / spending %.2f: both are magnitudes", income, spending)
	}
	if math.Abs(operating-(income-spending)) > 0.005 {
		t.Errorf("operating %.2f, want income - spending = %.2f", operating, income-spending)
	}
	if want := operating + investing + financing + vehicles; math.Abs(net-want) > 0.005 {
		t.Errorf("net_cash_flow %.2f, want the four sections summed = %.2f", net, want)
	}
	// Every section on the right side of zero for this window.
	if investing >= 0 {
		t.Errorf("investing %.2f: the window bought more than it sold", investing)
	}
	if financing >= 0 || vehicles >= 0 {
		t.Errorf("financing %.2f / vehicles %.2f: both were serviced, not drawn", financing, vehicles)
	}
	// The memo columns: yield is the class alone, and the three lifted
	// outflow classes are magnitudes.
	if y := mustFloat(t, r.Yield, "yield"); math.Abs(y-900) > 0.005 {
		t.Errorf("yield %.2f, want the dividend alone", y)
	}
	for _, tc := range []struct {
		name string
		got  *string
	}{{"taxes", r.Taxes}, {"fees", r.Fees}, {"giving", r.Giving}} {
		if v := mustFloat(t, tc.got, tc.name); v <= 0 {
			t.Errorf("%s %.2f: the lifted classes are magnitudes", tc.name, v)
		}
	}
	if r.SavingsRate == nil {
		t.Error("savings_rate is nil on a window with income")
	}
	// The rows the statement never draws are in no figure at all.
	if r.TxnCount != 12 {
		t.Errorf("txn_count = %d, want 12 — a pool-internal wire and an unsigned kind are not lines", r.TxnCount)
	}
}

// TestFlowsSumToZero pins the identity that makes the diagram balance:
// a bucket's rows, the Cash row included, sum to zero. The Cash row is
// the residual seen from the pool's side, so cash the household kept is
// cash the pool absorbed, which is a use.
func TestFlowsSumToZero(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)

	for _, level := range []string{"section", "class", "group"} {
		for _, investing := range []string{"whole", "class"} {
			rows, err := CashflowFlows(ctx, db, 0, 3500000, "USD", "total", level, investing)
			if err != nil {
				t.Fatalf("CashflowFlows(%s, %s): %v", level, investing, err)
			}
			var total, shares float64
			var sawCash bool
			for _, r := range rows {
				total += mustFloat(t, r.Net, "net")
				if r.Share != nil {
					shares += *r.Share
				}
				if r.Section == "cash" {
					sawCash = true
					if r.Inflow != nil || r.Outflow != nil {
						t.Errorf("%s/%s: the cash row reports a gross, which it has none of", level, investing)
					}
				}
				if level == "section" && r.Class != nil {
					t.Errorf("%s: a section-level row names class %q", level, *r.Class)
				}
				if level != "group" && r.Group != nil {
					t.Errorf("%s: a %s-level row names group %q", level, level, *r.Group)
				}
			}
			if !sawCash {
				t.Errorf("%s/%s: no cash row, so the bucket cannot close", level, investing)
			}
			if math.Abs(total) > 0.005 {
				t.Errorf("%s/%s: the bucket's rows sum to %.4f, want 0", level, investing, total)
			}
			// Every node's share is against the same hub, and the two
			// sides each sum to it, so the shares sum to two.
			if math.Abs(shares-2) > 0.001 {
				t.Errorf("%s/%s: shares sum to %.4f, want 2 — one hub on each side",
					level, investing, shares)
			}
		}
	}
}

// TestFlowsNetAtTheLevelAsked pins what netting means: the same window
// read at two levels gives the same total and a different number of
// rows, and the investing toggle changes the investing rows alone.
func TestFlowsNetAtTheLevelAsked(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)

	whole, err := CashflowFlows(ctx, db, 0, 3500000, "USD", "total", "class", "whole")
	if err != nil {
		t.Fatalf("flows whole: %v", err)
	}
	byClass, err := CashflowFlows(ctx, db, 0, 3500000, "USD", "total", "class", "class")
	if err != nil {
		t.Fatalf("flows by class: %v", err)
	}
	countInvesting := func(rows []CashflowFlowRow) (n int, sum float64) {
		for _, r := range rows {
			if r.Section == "investing" {
				n++
				sum += mustFloat(t, r.Net, "net")
			}
		}
		return n, sum
	}
	nWhole, sumWhole := countInvesting(whole)
	nClass, sumClass := countInvesting(byClass)
	if nWhole != 1 {
		t.Errorf("investing as a whole draws %d nodes, want 1", nWhole)
	}
	if nClass != 2 {
		t.Errorf("investing by class draws %d nodes, want 2", nClass)
	}
	if math.Abs(sumWhole-sumClass) > 0.005 {
		t.Errorf("the toggle moved the section's total: %.2f vs %.2f", sumWhole, sumClass)
	}
	// The gross beside the net is what shows the churn a net hides.
	for _, r := range whole {
		if r.Section != "investing" {
			continue
		}
		in, out := mustFloat(t, r.Inflow, "inflow"), mustFloat(t, r.Outflow, "outflow")
		if in <= 0 || out <= 0 {
			t.Errorf("investing gross = (%.2f, %.2f): the window both bought and received", in, out)
		}
	}
}

// TestSankeyConservesFlow pins the diagram: every stage conserves flow,
// each side sums to the hub, and no node appears on both sides.
func TestSankeyConservesFlow(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)

	for _, level := range []string{"group", "class"} {
		rows, err := CashflowSankey(ctx, db, 0, 3500000, "USD", level, "whole")
		if err != nil {
			t.Fatalf("CashflowSankey(%s): %v", level, err)
		}
		if len(rows) == 0 {
			t.Fatalf("%s: the diagram is empty", level)
		}
		var intoHub, outOfHub float64
		side := map[string]int{}
		stages := map[int64]bool{}
		for _, r := range rows {
			v := mustFloat(t, r.Value, "value")
			if v <= 0 {
				t.Errorf("%s: an edge carries %.2f; a zero or negative edge is not drawn", level, v)
			}
			stages[r.Stage] = true
			switch {
			case r.Target == "Household":
				intoHub += v
				side[r.Source] |= 1
			case r.Source == "Household":
				outOfHub += v
				side[r.Target] |= 2
			default:
				// A leaf stage: the class end is on the same side as
				// the leaf, which is what `stays` means.
				side[r.Source] |= 4
				side[r.Target] |= 4
			}
		}
		if math.Abs(intoHub-outOfHub) > 0.005 {
			t.Errorf("%s: %.2f reaches the hub and %.2f leaves it", level, intoHub, outOfHub)
		}
		for node, seen := range side {
			if seen&1 != 0 && seen&2 != 0 {
				t.Errorf("%s: %q is drawn on both sides", level, node)
			}
		}
		if level == "class" {
			if stages[1] || stages[4] {
				t.Errorf("%s emits a leaf stage; --level class is the two inner ones", level)
			}
		} else if !stages[1] && !stages[4] {
			t.Errorf("%s emits no leaf stage at all", level)
		}
	}
}

// TestSankeyStagesConserveThroughTheClass pins the harder half of the
// diagram: an operating class's edge into the hub is the sum of the
// leaves drawn through it, so a leaf detached for running the other way
// is neither double-counted nor lost.
func TestSankeyStagesConserveThroughTheClass(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
             VALUES ('cf', 0, 'USD', 'USD', 1.0);`); err != nil {
		t.Fatalf("seed fx: %v", err)
	}
	// A consumption class whose net is an outflow, holding one leaf
	// whose refunds exceeded its purchases.
	seedLines(t, db, ctx, []line{
		{id: "S-WAGE", account: "CASH", kind: "deposit", amount: 5000, income: "INCOME_WAGES"},
		{id: "S-RENT", account: "CASH", kind: "purchase", amount: -2000, spend: "RENT_AND_UTILITIES_RENT"},
		{id: "S-SWING", account: "CARD", kind: "refund", amount: 300, spend: "TRAVEL_FLIGHTS"},
	})

	rows, err := CashflowSankey(ctx, db, 0, 3500000, "USD", "group", "whole")
	if err != nil {
		t.Fatalf("CashflowSankey: %v", err)
	}
	byEdge := map[string]float64{}
	for _, r := range rows {
		byEdge[r.Source+" -> "+r.Target] = mustFloat(t, r.Value, "value")
	}
	// The swing leaf supplied cash, so it attaches to the hub directly
	// rather than hanging off a class on the other side.
	if v, ok := byEdge["Travel -> Household"]; !ok || math.Abs(v-300) > 0.005 {
		t.Errorf("the swing leaf is not attached to the hub: %v", byEdge)
	}
	// Its class carries only the leaf that stayed with it.
	if v, ok := byEdge["Household -> Consumption"]; !ok || math.Abs(v-2000) > 0.005 {
		t.Errorf("the class edge is %v, want the 2000 that stayed with it", byEdge)
	}
	var in, out float64
	for _, r := range rows {
		v := mustFloat(t, r.Value, "value")
		if r.Target == "Household" {
			in += v
		} else if r.Source == "Household" {
			out += v
		}
	}
	if math.Abs(in-out) > 0.005 {
		t.Errorf("the hub is %.2f on one side and %.2f on the other", in, out)
	}
}

// TestSharedValuesAreTwoNodesInTheDiagram pins the disambiguation a
// Sankey needs: a gift given and a gift received are two nodes, and a
// visualisation keys a node by its NAME, so the two names differ.
func TestSharedValuesAreTwoNodesInTheDiagram(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
             VALUES ('cf', 0, 'USD', 'USD', 1.0);`); err != nil {
		t.Fatalf("seed fx: %v", err)
	}
	seedLines(t, db, ctx, []line{
		{id: "G-IN", account: "CASH", kind: "deposit", amount: 1000, income: "gift"},
		{id: "G-OUT", account: "CASH", kind: "withdrawal", amount: -400, spend: "gift"},
		{id: "U-IN", account: "CASH", kind: "deposit", amount: 70},
		{id: "U-OUT", account: "CASH", kind: "withdrawal", amount: -70},
	})
	rows, err := CashflowSankey(ctx, db, 0, 3500000, "USD", "group", "whole")
	if err != nil {
		t.Fatalf("CashflowSankey: %v", err)
	}
	names := map[string]bool{}
	for _, r := range rows {
		names[r.Source] = true
		names[r.Target] = true
	}
	for _, want := range []string{"Gift in", "Gift out", "Uncategorised in", "Uncategorised out"} {
		if !names[want] {
			t.Errorf("the diagram has no node named %q: %v", want, keysOf(names))
		}
	}
	if names["Gift"] {
		t.Error("a bare \"Gift\" node would merge a gift given with a gift received")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCashflowTransactionsNameTheRightThing pins the transactions
// view's one judgement: the instrument on an investing line, the payer
// or merchant on an operating one, and nothing at all on a line whose
// counterparty is an account.
func TestCashflowTransactionsNameTheRightThing(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        UPDATE spend_txn_enrichment SET merchant_signature = 'CORNER MARKET'
         WHERE transaction_external_id = 'R-SHOP';
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name)
             VALUES ('CORNER MARKET', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES', 1, 100, 'test');`); err != nil {
		t.Fatalf("seed a merchant: %v", err)
	}

	rows, err := CashflowTransactions(ctx, db, 0, 3500000, "USD")
	if err != nil {
		t.Fatalf("CashflowTransactions: %v", err)
	}
	got := map[string]CashflowTransactionRow{}
	for _, r := range rows {
		got[r.TransactionExternalID] = r
	}
	if n := got["R-BUY"].Name; n == nil || *n != "Example Equity Fund" {
		t.Errorf("an investing line names %v, want the instrument", n)
	}
	if n := got["R-SHOP"].Name; n == nil || *n != "Corner Market" {
		t.Errorf("an outflow line names %v, want the merchant", n)
	}
	for _, id := range []string{"R-MORT", "R-RET"} {
		if n := got[id].Name; n != nil {
			t.Errorf("%s names %q; a financing or vehicle line's counterparty is an account", id, *n)
		}
	}
	if got["R-INTERNAL"].TransactionExternalID != "" {
		t.Error("a pool-internal wire is a transactions row")
	}
	// The node and the family verdict behind it, side by side.
	if r := got["R-WAGE"]; r.Section != "operating_in" || r.Class != "earnings" ||
		r.ClassLabel != "Earnings" || r.GroupLabel != "Wages" {
		t.Errorf("R-WAGE resolved to %+v", r)
	}
	if v := got["R-WAGE"].Verdict; v == nil || *v != "INCOME_WAGES" {
		t.Errorf("R-WAGE verdict = %v, want the family value behind the node", v)
	}
}

// TestTransactionsCarriesTheCashflowTrio pins the one surface that
// shows a row from every side: the three cashflow columns beside the
// spending and income ones, read from the RESOLUTION so a row this
// feature declines still says what it was resolved as.
func TestTransactionsCarriesTheCashflowTrio(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)

	rows, err := TransactionsBetween(ctx, db, 0, 3500000, "USD", SortAscending)
	if err != nil {
		t.Fatalf("TransactionsBetween: %v", err)
	}
	got := map[string]TransactionRow{}
	for _, r := range rows {
		got[r.TransactionExternalID] = r
	}
	for id, want := range map[string]string{
		"R-WAGE":     "operating_in.earnings.INCOME_WAGES",
		"R-BUY":      "investing.public_equity.trades",
		"R-MORT":     "financing.mortgage.mortgage",
		"R-INTERNAL": "",
	} {
		r := got[id]
		node := ""
		if r.CashflowSection != nil {
			node = *r.CashflowSection + "." + *r.CashflowClass + "." + *r.CashflowGroup
		}
		if want == "" {
			// A pool-internal move has no node, and the columns say so
			// rather than naming one.
			if r.CashflowSection != nil {
				t.Errorf("%s carries node %q; a pool-internal wire reaches none", id, node)
			}
			continue
		}
		if node != want {
			t.Errorf("%s node = %q, want %q", id, node, want)
		}
	}
	// A row that is still on `wealthdb transactions` from both families'
	// sides keeps both trios.
	if r := got["R-SHOP"]; r.SpendDetailed == nil || *r.SpendDetailed != "FOOD_AND_DRINK_GROCERIES" {
		t.Errorf("the spending trio moved: %+v", r.SpendDetailed)
	}
}

// TestMigration0082DDLIsRerunnable holds the reports to the replay bar.
func TestMigration0082DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)
	rerunMigrationDDL(t, db, ctx, "0082_report_cashflow.sql")
	// Replaying a SUPERSEDED migration is a downgrade, not a no-op:
	// 0082 puts report_cashflow_summary back at its own 14-column shape,
	// and CashflowSummary below scans the CURRENT one positionally.
	// Replay forward, exactly as Migrate would — and extend this list
	// when another migration re-issues the macro.
	for _, later := range []string{
		"0083_cashflow_reconciliation.sql",    // the memo
		"0086_cashflow_operating_columns.sql", // the operating halves, the memo's flow term
	} {
		rerunMigrationDDL(t, db, ctx, later)
	}
	rows, err := CashflowSummary(ctx, db, 0, 3500000, "USD", "total")
	if err != nil || len(rows) != 1 {
		t.Fatalf("the replayed summary returned %d rows: %v", len(rows), err)
	}
}

// TestCashflowReportsTolerateAnEmptyWindow pins the degenerate case
// every report has to survive: a window with no lines returns nothing
// rather than a row of zeroes or a division by zero.
func TestCashflowReportsTolerateAnEmptyWindow(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)

	summary, err := CashflowSummary(ctx, db, 0, 3500000, "USD", "total")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	flows, err := CashflowFlows(ctx, db, 0, 3500000, "USD", "total", "group", "whole")
	if err != nil {
		t.Fatalf("flows: %v", err)
	}
	sankey, err := CashflowSankey(ctx, db, 0, 3500000, "USD", "group", "whole")
	if err != nil {
		t.Fatalf("sankey: %v", err)
	}
	txns, err := CashflowTransactions(ctx, db, 0, 3500000, "USD")
	if err != nil {
		t.Fatalf("transactions: %v", err)
	}
	if n := len(summary) + len(flows) + len(sankey) + len(txns); n != 0 {
		t.Errorf("an empty window produced %d rows: %s", n,
			fmt.Sprint(len(summary), len(flows), len(sankey), len(txns)))
	}
}

// ---- the reconciliation memo ---------------------------------------------

// seedBalanceSpine gives the report fixture's pooled accounts a balance
// history consistent with the lines seeded over them, so the memo has
// something to measure. Both boundaries are observed, and each closing
// balance is the opening one plus the bucket's own lines, which is what
// the arithmetic says it should be.
func seedBalanceSpine(t *testing.T, db *sql.DB, ctx context.Context, opening, closing float64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                   currency, balance_kind, amount) VALUES
            ('cf', 86400,   'CASH', 'USD', 'current', CAST(? AS DECIMAL(28,4))),
            ('cf', 3456000, 'CASH', 'USD', 'current', CAST(? AS DECIMAL(28,4)));`,
		opening, closing); err != nil {
		t.Fatalf("seed the balance spine: %v", err)
	}
}

// TestTheMemoClosesWhenTheBalancesAgree pins the memo's arithmetic: on a
// pool whose observed balance change is exactly the lines that moved it,
// `unexplained` is zero.
func TestTheMemoClosesWhenTheBalancesAgree(t *testing.T) {
	db, ctx := openMigrated(t)
	// The pooled lines on CASH, plus the wire to a pooled account whose
	// balances are not collected: that leaves the MEASURED spine even
	// though it never leaves the household, so it moves this balance.
	seedBalanceSpine(t, db, ctx, 10000, 10000+6000+50-1200-300-1800-500-100)
	seedReportFixture(t, db, ctx)

	rows, err := CashflowSummary(ctx, db, 172800, 3500000, "USD", "total")
	if err != nil {
		t.Fatalf("CashflowSummary: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d buckets, want 1", len(rows))
	}
	r := rows[0]
	if r.CashMeasured == nil || r.FXEffect == nil || r.Unexplained == nil {
		t.Fatalf("the memo is blank on an observed window: %+v", r)
	}
	// One currency, one rate: the revaluation term is zero and the
	// measured change is the pooled account's own lines.
	if fx := mustFloat(t, r.FXEffect, "fx_effect"); math.Abs(fx) > 0.005 {
		t.Errorf("fx_effect %.2f in a single-currency window", fx)
	}
	if u := mustFloat(t, r.Unexplained, "unexplained"); math.Abs(u) > 0.005 {
		t.Errorf("unexplained %.2f, want 0 — the balances and the lines agree", u)
	}
}

// TestTheMemoSeesAHoleInThePopulation is what the memo is FOR. A row the
// resolution declines still moved the money, so a balance change it
// cannot explain is exactly the signal the two families' honesty
// surfaces cannot give: theirs is structural, and their canary counts
// only rows nothing placed.
func TestTheMemoSeesAHoleInThePopulation(t *testing.T) {
	db, ctx := openMigrated(t)
	// The closing balance is 25 lower than anything accounts for: the
	// `fx` row on the pooled cash account moved money and the
	// resolution declines it, so it reaches no section and no memo
	// term.
	seedBalanceSpine(t, db, ctx, 10000, 10000+6000+50-1200-300-1800-500-100-25)
	seedReportFixture(t, db, ctx)

	rows, err := CashflowSummary(ctx, db, 172800, 3500000, "USD", "total")
	if err != nil {
		t.Fatalf("CashflowSummary: %v", err)
	}
	if u := mustFloat(t, rows[0].Unexplained, "unexplained"); math.Abs(u+25) > 0.005 {
		t.Errorf("unexplained %.2f, want -25 — the declined row is the hole", u)
	}
}

// TestAnAccountIsMeasuredFromItsFirstObservation pins the rule that
// makes the memo readable on a real deployment. A collector begins,
// back-loads years of transaction history, and has balances only from
// the day it first ran; an account joins the memo at its first snapshot
// and the lines it contributed before that are outside the comparison.
//
// Blanking the bucket instead — the first reading of the design's rule —
// blanks every year before the last collector was added, which is every
// year a household would want to read.
func TestAnAccountIsMeasuredFromItsFirstObservation(t *testing.T) {
	db, ctx := openMigrated(t)
	// The balance history begins on day 30, inside the window, and the
	// fixture's lines are all on day 20 — before the account is
	// measurable at all.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                   currency, balance_kind, amount) VALUES
            ('cf', 2592000, 'CASH', 'USD', 'current', CAST(10000 AS DECIMAL(28,4))),
            ('cf', 3456000, 'CASH', 'USD', 'current', CAST(10000 AS DECIMAL(28,4)));`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedReportFixture(t, db, ctx)

	rows, err := CashflowSummary(ctx, db, 172800, 3500000, "USD", "total")
	if err != nil {
		t.Fatalf("CashflowSummary: %v", err)
	}
	r := rows[0]
	if r.CashMeasured == nil {
		t.Fatal("the memo blanked a window it can measure part of")
	}
	// Nothing moved after the account joined, and no line is compared
	// against a balance it predates.
	if m := mustFloat(t, r.CashMeasured, "cash_measured"); math.Abs(m) > 0.005 {
		t.Errorf("cash_measured %.2f, want 0", m)
	}
	if u := mustFloat(t, r.Unexplained, "unexplained"); math.Abs(u) > 0.005 {
		t.Errorf("unexplained %.2f, want 0 — the earlier lines are outside the slice", u)
	}
	// The statement itself is unaffected: the memo is a diagnostic,
	// never an input, and it covers less than the statement by design.
	if r.NetCashFlow == nil {
		t.Error("the memo's restriction reached net_cash_flow")
	}
}

// TestAMoveOutOfTheMeasuredSpineIsAFlowOfIt pins the rule a first
// reading gets wrong. The spine is narrower than the household: a wire
// to a pooled account whose balances are not collected leaves the
// MEASURED pool while staying inside the household's, and the statement
// rightly draws nothing. Left out of the memo's flow side it would read
// as a permanent hole, and on a real deployment it is the largest term
// there is.
func TestAMoveOutOfTheMeasuredSpineIsAFlowOfIt(t *testing.T) {
	db, ctx := openMigrated(t)
	// Only CASH is measurable; SAVE has no balance history.
	seedBalanceSpine(t, db, ctx, 10000, 9000)
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
             VALUES ('cf', 0, 'USD', 'USD', 1.0);`); err != nil {
		t.Fatalf("seed fx: %v", err)
	}
	seedLines(t, db, ctx, []line{
		{id: "M-OUT", account: "CASH", kind: "withdrawal", amount: -1000,
			spend: "internal_transfer", farAccount: "SAVE"},
	})

	rows, err := CashflowSummary(ctx, db, 172800, 3500000, "USD", "total")
	if err != nil {
		t.Fatalf("CashflowSummary: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("the statement drew %d buckets for a window of one invisible wire", len(rows))
	}
	// Read the memo on its own: the statement has no bucket, because a
	// pool-internal move is not a line — and that is the point.
	memo, err := db.QueryContext(ctx,
		`SELECT COALESCE(unexplained, '(blank)') FROM report_cashflow_reconciliation(?, ?, 'USD', 'total')`,
		172800, 3500000)
	if err != nil {
		t.Fatalf("read the memo: %v", err)
	}
	defer memo.Close()
	for memo.Next() {
		var u string
		if err := memo.Scan(&u); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("the memo reported %q for a bucket the statement does not have", u)
	}
}

// TestTheMemoIsBlankWithNoBalancesAtAll pins the degenerate case: a
// deployment whose pool has no balance history reports no memo rather
// than a figure made of zeroes.
func TestTheMemoIsBlankWithNoBalancesAtAll(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)

	rows, err := CashflowSummary(ctx, db, 172800, 3500000, "USD", "total")
	if err != nil {
		t.Fatalf("CashflowSummary: %v", err)
	}
	if r := rows[0]; r.CashMeasured != nil || r.Unexplained != nil {
		t.Errorf("a pool with no balances reported a memo: %v / %v", r.CashMeasured, r.Unexplained)
	}
}

// TestTheMemoSeparatesRevaluationFromError pins the second FX term, the
// one a first draft would leave out: a line received mid-window and held
// to its end is counted at its own day's rate in the flow and at the
// window's end in the balance, and that difference is revaluation rather
// than a hole.
func TestTheMemoSeparatesRevaluationFromError(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        -- USD is the output currency and never moves; the account holds
        -- EUR, whose rate doubles across the window.
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('cf', 86400,   'USD', 'EUR', CAST(1.0 AS DECIMAL(20,10))),
            ('cf', 3456000, 'USD', 'EUR', CAST(2.0 AS DECIMAL(20,10)));
        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                   currency, balance_kind, amount) VALUES
            ('cf', 86400,   'CASH', 'EUR', 'current', CAST(100 AS DECIMAL(28,4))),
            ('cf', 3456000, 'CASH', 'EUR', 'current', CAST(150 AS DECIMAL(28,4)));
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount)
             VALUES ('cf', 'F-WAGE', 172800, 'CASH', 'deposit', 'EUR', CAST(50 AS DECIMAL(28,4)));
        INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
                payer_signature, signature_version, income_detailed, provenance, assigned_at)
             VALUES ('cf', 'F-WAGE', 'sig', 1, 'INCOME_WAGES', 'rule', 100);`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rows, err := CashflowSummary(ctx, db, 90000, 3500000, "USD", "total")
	if err != nil {
		t.Fatalf("CashflowSummary: %v", err)
	}
	r := rows[0]
	// Measured: 150 EUR at 2.0 minus 100 EUR at 1.0 = 200. The flow is
	// 50 EUR at its own day's rate of 1.0 = 50. The rest is
	// revaluation: 100 opening × (2.0 − 1.0) + 50 × (2.0 − 1.0) = 150.
	if m := mustFloat(t, r.CashMeasured, "cash_measured"); math.Abs(m-200) > 0.005 {
		t.Errorf("cash_measured %.2f, want 200", m)
	}
	if n := mustFloat(t, r.NetCashFlow, "net_cash_flow"); math.Abs(n-50) > 0.005 {
		t.Errorf("net_cash_flow %.2f, want 50 — the flow is valued at its own day", n)
	}
	if fx := mustFloat(t, r.FXEffect, "fx_effect"); math.Abs(fx-150) > 0.005 {
		t.Errorf("fx_effect %.2f, want 150 — both terms, not just the opening balance", fx)
	}
	if u := mustFloat(t, r.Unexplained, "unexplained"); math.Abs(u) > 0.005 {
		t.Errorf("unexplained %.2f, want 0 — a held balance revaluing is not an error", u)
	}
}

// TestMigration0083DDLIsRerunnable holds the memo to the replay bar.
func TestMigration0083DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedBalanceSpine(t, db, ctx, 10000, 12150)
	seedReportFixture(t, db, ctx)
	rerunMigrationDDL(t, db, ctx, "0083_cashflow_reconciliation.sql")
	// A downgrade, not a no-op: 0083 puts both macros back at their
	// pre-0086 shape and CashflowSummary scans the CURRENT one
	// positionally. Replay forward, exactly as Migrate would.
	rerunMigrationDDL(t, db, ctx, "0086_cashflow_operating_columns.sql")
	if rows, err := CashflowSummary(ctx, db, 172800, 3500000, "USD", "total"); err != nil || len(rows) != 1 {
		t.Fatalf("the replayed summary returned %d rows: %v", len(rows), err)
	}
}

// TestMigration0086DDLIsRerunnable holds the re-issued summary and memo
// to the replay bar: two OR REPLACE macros and nothing else.
func TestMigration0086DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedBalanceSpine(t, db, ctx, 10000, 12150)
	seedReportFixture(t, db, ctx)
	rerunMigrationDDL(t, db, ctx, "0086_cashflow_operating_columns.sql")
	rows, err := CashflowSummary(ctx, db, 172800, 3500000, "USD", "total")
	if err != nil || len(rows) != 1 {
		t.Fatalf("the replayed summary returned %d rows: %v", len(rows), err)
	}
	if rows[0].CashFlowMeasured == nil {
		t.Error("the replayed summary lost the memo's flow term")
	}
}

// ---- the serving view ----------------------------------------------------

// TestWebCashflowIsLineGrainAndReadyToDraw pins what the serving view
// adds over the report macro it wraps: a row per (line, reporting
// currency), and node NAMES a Sankey can key on. A visualisation keys a
// node by the string it is called, so a gift given and a gift received
// sharing a name would merge into one node with a self-edge.
func TestWebCashflowIsLineGrainAndReadyToDraw(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)
	seedLines(t, db, ctx, []line{
		{id: "W-GIFT-IN", account: "CASH", kind: "deposit", amount: 500, income: "gift"},
		{id: "W-GIFT-OUT", account: "CASH", kind: "withdrawal", amount: -200, spend: "gift"},
		{id: "W-UNPLACED-IN", account: "CASH", kind: "deposit", amount: 30},
		{id: "W-UNPLACED-OUT", account: "CASH", kind: "withdrawal", amount: -30},
	})

	// The node names, read straight off the view.
	named, err := db.QueryContext(ctx, `
        SELECT DISTINCT section, class_node, group_node FROM web_cashflow
         WHERE grp IN ('gift', '(uncategorized)') ORDER BY 1, 3`)
	if err != nil {
		t.Fatalf("read web_cashflow: %v", err)
	}
	defer named.Close()
	got := map[string]bool{}
	for named.Next() {
		var section, classNode, groupNode string
		if err := named.Scan(&section, &classNode, &groupNode); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[groupNode] = true
		if section == "operating_in" && classNode == "Uncategorised" {
			t.Error("the uncategorised CLASS node is not disambiguated by side")
		}
	}
	for _, want := range []string{"Gift in", "Gift out", "Uncategorised in", "Uncategorised out"} {
		if !got[want] {
			t.Errorf("web_cashflow has no node named %q: %v", want, got)
		}
	}

	// One row per line, valued in all three reporting currencies.
	var n, lines int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_cashflow`).Scan(&n); err != nil {
		t.Fatalf("count web_cashflow: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cashflow_lines_base(0, 9223372036854775807)`).Scan(&lines); err != nil {
		t.Fatalf("count the base: %v", err)
	}
	if n != lines {
		t.Errorf("web_cashflow holds %d rows over a base of %d", n, lines)
	}
	// Nothing an account is identified by beyond the two the dashboard
	// groups on, and never a raw taxonomy value where a label belongs.
	for _, col := range []string{"class_node", "group_node", "account_label", "display_name", "name"} {
		var present int
		if err := db.QueryRowContext(ctx, `
            SELECT COUNT(*) FROM duckdb_columns()
             WHERE table_name = 'web_cashflow' AND column_name = ?`, col).Scan(&present); err != nil {
			t.Fatalf("inspect web_cashflow: %v", err)
		}
		if present != 1 {
			t.Errorf("web_cashflow has no %s column", col)
		}
	}
}

// TestMigration0084DDLIsRerunnable holds the serving view to the replay
// bar: one OR REPLACE VIEW.
func TestMigration0084DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)
	rerunMigrationDDL(t, db, ctx, "0084_web_cashflow.sql")
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_cashflow`).Scan(&n); err != nil {
		t.Fatalf("web_cashflow after replay: %v", err)
	}
	if n == 0 {
		t.Error("the replayed view answers nothing")
	}
}

// TestNoNodeIsDrawnIntoItself pins the one shape a Sankey cannot render:
// an edge whose two ends are the same node. The backlog class IS its own
// leaf — nothing placed those rows, so there is nothing finer to say
// about them — so it attaches to the hub directly rather than through a
// leaf stage, the way cash and the vehicles do.
func TestNoNodeIsDrawnIntoItself(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReportFixture(t, db, ctx)
	seedLines(t, db, ctx, []line{
		{id: "L-UNPLACED-IN", account: "CASH", kind: "deposit", amount: 700},
		{id: "L-UNPLACED-OUT", account: "CASH", kind: "withdrawal", amount: -300},
	})

	for _, level := range []string{"group", "class"} {
		rows, err := CashflowSankey(ctx, db, 0, 3500000, "USD", level, "whole")
		if err != nil {
			t.Fatalf("CashflowSankey(%s): %v", level, err)
		}
		var sawBacklog bool
		for _, r := range rows {
			if r.Source == r.Target {
				t.Errorf("%s: %q is drawn into itself", level, r.Source)
			}
			if r.Source == "Uncategorised in" || r.Target == "Uncategorised out" {
				sawBacklog = true
			}
		}
		if !sawBacklog {
			t.Errorf("%s: the backlog vanished from the diagram rather than attaching to the hub", level)
		}
	}
	// It still conserves: the backlog enters the hub at its own net,
	// not at the sum of leaves it does not have.
	rows, err := CashflowSankey(ctx, db, 0, 3500000, "USD", "group", "whole")
	if err != nil {
		t.Fatalf("CashflowSankey: %v", err)
	}
	var in, out float64
	for _, r := range rows {
		v := mustFloat(t, r.Value, "value")
		if r.Target == "Household" {
			in += v
		} else if r.Source == "Household" {
			out += v
		}
	}
	if math.Abs(in-out) > 0.005 {
		t.Errorf("the hub is %.2f on one side and %.2f on the other", in, out)
	}
}

// ---- the coverage report -------------------------------------------------

// TestCoverageNamesTheAccountAndItsBlindSpot pins the three things the
// report exists to do that a naive per-account query does not: it reports
// a real disagreement, it declines to call one where the account's
// unsigned volume could explain it, and it says so out loud where the
// balances cannot answer at all.
func TestCoverageNamesTheAccountAndItsBlindSpot(t *testing.T) {
	db, ctx := openMigrated(t)
	seedBalanceSpine(t, db, ctx, 10000, 10000)
	seedReportFixture(t, db, ctx)

	rows, err := CashflowCoverage(ctx, db, 172800, 3500000, "total")
	if err != nil {
		t.Fatalf("CashflowCoverage: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the coverage report is empty on a fixture with balances and lines")
	}

	byAccount := map[string]CashflowCoverageRow{}
	for _, r := range rows {
		byAccount[r.Account+"/"+r.Currency] = r
		if r.SourceID == "" || r.Currency == "" || r.Status == "" {
			t.Errorf("a coverage row is missing its identity: %+v", r)
		}
	}

	// The one account with both boundaries observed — the report names
	// it by its nickname, as every account-facing view does. Its
	// balances did not move while its lines did, so the report must
	// name a gap rather than stay silent.
	cash, ok := byAccount["Everyday/USD"]
	if !ok {
		t.Fatalf("the measured account is absent from the report: %v", byAccount)
	}
	if cash.Status != "measured" && cash.Status != "obscured" {
		t.Errorf("the measured account's status = %q, want a measured verdict", cash.Status)
	}
	if cash.Measured == nil || cash.Gap == nil {
		t.Errorf("both boundaries are observed but the row reports no delta: %+v", cash)
	}

	// Every pooled account with no balance history at all is REPORTED,
	// not dropped — being absent from a diagnostic reads as fine, and
	// the whole point is that it is unknown.
	var unmeasurable int
	for _, r := range rows {
		if r.Status == "unmeasurable" {
			unmeasurable++
			if r.Measured != nil || r.Gap != nil {
				t.Errorf("an unmeasurable row carries a delta: %+v", r)
			}
		}
	}
	if unmeasurable == 0 {
		t.Error("no account reported unmeasurable; the fixture has pooled accounts with no balances")
	}
}

// TestCoverageIsInTheAccountsOwnCurrency pins the decision not to
// convert: a gap carried to an output currency would answer a question
// nobody asked and would carry a rate error into the one number the
// report exists to make trustworthy.
func TestCoverageIsInTheAccountsOwnCurrency(t *testing.T) {
	db, ctx := openMigrated(t)
	seedBalanceSpine(t, db, ctx, 10000, 10000)
	seedReportFixture(t, db, ctx)

	rows, err := CashflowCoverage(ctx, db, 172800, 3500000, "total")
	if err != nil {
		t.Fatalf("CashflowCoverage: %v", err)
	}
	for _, r := range rows {
		if r.Currency == "" {
			t.Errorf("a row carries no currency, so its amounts mean nothing: %+v", r)
		}
	}
}
