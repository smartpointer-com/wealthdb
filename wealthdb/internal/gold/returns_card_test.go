package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// seedCashOnlyAcct upserts an account whose whole value is carried as cash (no
// positions) plus its transactions — the shape of a deposit account and, with a
// negative balance, of a credit card. All values synthetic.
func seedCashOnlyAcct(t *testing.T, db *sql.DB, ctx context.Context, src, acct string,
	kind canonical.AccountKind, snaps []snap, txns []txn) {
	t.Helper()
	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: src, AccountExternalID: acct, AccountKind: kind,
			BaseCurrency: &usd,
			FirstSeenAt:  snaps[0].at, LastSeenAt: snaps[len(snaps)-1].at,
		}})
	})
	inTx(t, db, ctx, func(w *Writer) error {
		var cash []canonical.CashBalanceChange
		for _, s := range snaps {
			cash = append(cash, canonical.CashBalanceChange{
				SilverSourceID: src, SnapshotAt: s.at, AccountExternalID: acct,
				Currency: "USD", BalanceKind: canonical.BalanceKindCurrent,
				Amount: *decp(s.val),
			})
		}
		if err := w.InsertCashBalances(ctx, cash); err != nil {
			return err
		}
		if len(txns) == 0 {
			return nil
		}
		var tx []canonical.TransactionChange
		for i, x := range txns {
			tx = append(tx, canonical.TransactionChange{
				SilverSourceID: src, TransactionExternalID: fmt.Sprintf("%s-tx%d", acct, i),
				OccurredAt: x.at, AccountExternalID: acct, Kind: x.kind,
				Currency: "USD", NetAmount: decp(x.amt),
			})
		}
		return w.InsertTransactions(ctx, tx)
	})
}

// seedCardFixture builds the investment fixture the card tests compare against:
// a brokerage in a portfolio ("brok") and a checking account ("bank") that pays
// a card down with a transfer_out. With withCard it adds a third source holding
// a credit card that carries BOTH value (a negative cash balance) and flows —
// its own purchase / interest / fee / reward, plus the transfer_in counter-leg
// of the checking payment, equal and same-day, which the cross-source matcher
// would happily pair with the checking leg if the card were ever loaded.
//
// The card source is registered under an ordinary brokerage adapter kind on
// purpose: its ReturnsPolicy has no card handling whatsoever, so nothing but
// the engine's own kind-keyed seam can keep the card out. Without it the card
// would emit rows of its own at the accounts and sources grains and move every
// aggregate. Returns the window end. All values synthetic.
func seedCardFixture(t *testing.T, db *sql.DB, ctx context.Context, withCard bool) int64 {
	t.Helper()
	seedReturnsSource(t, db, ctx, "brok", "schwab")
	seedReturnsSource(t, db, ctx, "bank", "chase")

	t0, tMid, t1 := dy(2024, time.January, 2), dy(2024, time.April, 1), dy(2024, time.July, 2)
	pf := "PF1"
	seedAcct(t, db, ctx, "brok", "BROK1", canonical.AccountKindBrokerage, &pf,
		[]snap{{t0, 1000}, {t1, 1200}}, []txn{{tMid, canonical.TxKindDeposit, 100}})
	seedCashOnlyAcct(t, db, ctx, "bank", "CHK1", canonical.AccountKindCash,
		[]snap{{t0, 500}, {t1, 300}}, []txn{{tMid, canonical.TxKindTransferOut, -200}})

	if withCard {
		seedReturnsSource(t, db, ctx, "cardco", "schwab")
		seedCashOnlyAcct(t, db, ctx, "cardco", "CARD1", canonical.AccountKindCard,
			[]snap{{t0, -800}, {t1, -600}}, []txn{
				{tMid, canonical.TxKindPurchase, -150},
				{tMid, canonical.TxKindInterest, -12},
				{tMid, canonical.TxKindFee, -95},
				{tMid, canonical.TxKindReward, 10},
				// The counter-leg of CHK1's -200 payment: same day, same
				// native amount, a different source ⇒ a cross-source match
				// candidate but for the card seam.
				{tMid, canonical.TxKindTransferIn, 200},
			})
	}

	seedFX(t, db, dy(2024, time.January, 1), "USD", "CHF", "1.10")
	seedFX(t, db, dy(2024, time.January, 1), "USD", "EUR", "1.05")
	return time.Date(2024, time.July, 2, 23, 59, 59, 0, time.UTC).Unix()
}

// cardTestMatching is deliberately ON in every card test: cross-source matching
// is the path a card leg could do the most damage on (netting away a real
// checking withdrawal), so the comparisons run with it enabled.
var cardTestMatching = &TransferMatching{WindowDays: 3, TolerancePct: 1}

func fmtFloatPtr(v *float64) string {
	if v == nil {
		return "-"
	}
	// Full precision: "identical" here is a bit-level claim about the
	// aggregates, not a rounded-display one.
	return strconv.FormatFloat(*v, 'g', 17, 64)
}

func fmtStrPtr(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

// renderReturnRows serializes a returns result to a comparable string, every
// field included, in the engine's own row order.
func renderReturnRows(rows []ReturnRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%t|%d|%d|%s|%s|%s|%s|%s|%s|%s|%s\n",
			r.SilverSourceID, r.EntityID, r.EntityLabel, r.Period, r.IsSummary,
			r.StartDay, r.EndDay,
			fmtStrPtr(r.StartValue), fmtStrPtr(r.EndValue), fmtStrPtr(r.NetFlow),
			fmtFloatPtr(r.TWR), fmtFloatPtr(r.TWRAnnualized),
			fmtFloatPtr(r.MWR), fmtFloatPtr(r.MWRAnnualized),
			strings.Join(r.Quality, ","))
	}
	return b.String()
}

// dumpTable renders a whole table in a deterministic order, for byte-level
// comparison of two runs.
func dumpTable(t *testing.T, db *sql.DB, ctx context.Context, query string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("dump %q: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("dump columns: %v", err)
	}
	var b strings.Builder
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("dump scan: %v", err)
		}
		for i, c := range cells {
			if i > 0 {
				b.WriteByte('|')
			}
			fmt.Fprintf(&b, "%v", c)
		}
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump rows: %v", err)
	}
	return b.String()
}

// TestReturnsCardEmitsNoRowsAndLeavesAggregatesIdentical is the C4 guard: a
// credit card carrying both value and flows produces ZERO rows at every grain,
// and every aggregate is byte-identical to a fixture with no card at all.
// Identical output is the strong half — it proves the card contributes nothing
// to the value spine, nothing to the flow series, and nothing to the
// cross-source matcher (a leaked leg would net CHK1's -200 away and move the
// bank/global numbers).
func TestReturnsCardEmitsNoRowsAndLeavesAggregatesIdentical(t *testing.T) {
	base, bctx := openMigrated(t)
	end := seedCardFixture(t, base, bctx, false)
	withCard, cctx := openMigrated(t)
	if got := seedCardFixture(t, withCard, cctx, true); got != end {
		t.Fatalf("fixture windows differ: %d vs %d", got, end)
	}

	for _, level := range []string{"accounts", "portfolios", "sources", "global"} {
		for _, period := range []string{"total", "quarterly", "monthly"} {
			p := params(level, 0, end)
			p.Period = period
			p.TransferMatching = cardTestMatching

			want, err := RunReturns(bctx, base, p)
			if err != nil {
				t.Fatalf("%s/%s baseline: %v", level, period, err)
			}
			got, err := RunReturns(cctx, withCard, p)
			if err != nil {
				t.Fatalf("%s/%s with card: %v", level, period, err)
			}
			for _, r := range got {
				if r.SilverSourceID == "cardco" || r.EntityID == "CARD1" {
					t.Errorf("%s/%s emitted a card row: %+v", level, period, r)
				}
			}
			if g, w := renderReturnRows(got), renderReturnRows(want); g != w {
				t.Errorf("%s/%s not identical with a card present:\n got:\n%s\nwant:\n%s",
					level, period, g, w)
			}
		}
	}
}

// TestMaterializeReturnsCardInvisible re-runs the same proof through the
// multi-currency loader: MaterializeReturns shares appendSeries / attachOneFlow
// with RunReturns, and its whole report_returns table (48 partitions plus the
// windowed summaries, in USD/CHF/EUR) must be identical with and without a card.
func TestMaterializeReturnsCardInvisible(t *testing.T) {
	const dump = `SELECT * FROM report_returns ORDER BY ALL`

	base, bctx := openMigrated(t)
	end := seedCardFixture(t, base, bctx, false)
	withCard, cctx := openMigrated(t)
	seedCardFixture(t, withCard, cctx, true)

	mp := MaterializeParams{ToEpoch: end, ComputedAt: 1000, TransferMatching: cardTestMatching}
	nBase, err := MaterializeReturns(bctx, base, mp)
	if err != nil {
		t.Fatalf("materialize baseline: %v", err)
	}
	nCard, err := MaterializeReturns(cctx, withCard, mp)
	if err != nil {
		t.Fatalf("materialize with card: %v", err)
	}
	if nBase != nCard {
		t.Errorf("materialized row count = %d with a card, %d without", nCard, nBase)
	}
	if g, w := dumpTable(t, withCard, cctx, dump), dumpTable(t, base, bctx, dump); g != w {
		t.Errorf("report_returns not identical with a card present:\n got:\n%s\nwant:\n%s", g, w)
	}
}

// TestReturnsCardExcludedFromValueSpine is the reconciliation variant: with a
// card loaded, the returns global deliberately NO LONGER equals report_global.
// The gap is exactly the card balance — the same precedent as mortgages and
// other liabilities, which the rollups already drop.
func TestReturnsCardExcludedFromValueSpine(t *testing.T) {
	db, ctx := openMigrated(t)
	end := seedCardFixture(t, db, ctx, true)

	p := params("global", 0, end)
	p.TransferMatching = cardTestMatching
	g, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("global: %v", err)
	}
	if len(g) != 1 {
		t.Fatalf("global rows = %d, want 1", len(g))
	}
	ga, err := GlobalAsOf(ctx, db, end, "USD")
	if err != nil {
		t.Fatalf("GlobalAsOf: %v", err)
	}

	got, gok := parseFloatPtr(g[0].EndValue)
	total, tok := parseFloatPtr(ga.TotalValueOutCcy)
	if !gok || !tok {
		t.Fatalf("unparseable values: returns=%v report_global=%v", g[0].EndValue, ga.TotalValueOutCcy)
	}
	// The card's terminal balance (negative cash) is in report_global and
	// must NOT be in the returns spine.
	const cardBalance = -600.0
	if math.Abs(got-(total-cardBalance)) > 1e-6 {
		t.Errorf("returns global end = %v, want report_global (%v) minus the card (%v)",
			got, total, cardBalance)
	}
	if math.Abs(got-total) < 1e-6 {
		t.Errorf("returns global == report_global (%v): the card leaked into the spine", got)
	}

	// The checking-side leg of the card payment stays a real external
	// withdrawal: money left the returns-visible system. Global net flow is
	// the brokerage deposit (+100) plus that withdrawal (-200); had the card
	// leg been loaded, the cross-source matcher would have paired the two
	// -200/+200 legs and netted the withdrawal away to leave +100.
	flow, fok := parseFloatPtr(g[0].NetFlow)
	if !fok || math.Abs(flow-(-100)) > 1e-6 {
		t.Errorf("global net flow = %v, want -100 (the card leg must not net the payment away)",
			g[0].NetFlow)
	}
}

// TestWebTransactionsAccountKindFence pins the shape the web's income and fee
// charts rely on: web_transactions exposes account_kind, and the fence they
// apply keeps every non-card row INCLUDING one whose account is unknown (the
// macro's LEFT JOIN leaves account_kind NULL, which a bare `<> 'card'` would
// silently drop).
func TestWebTransactionsAccountKindFence(t *testing.T) {
	db, ctx := openMigrated(t)
	end := seedCardFixture(t, db, ctx, true)

	// A transaction on an account that never made it into `accounts`.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
			account_external_id, kind, currency, net_amount)
		VALUES ('brok', 'ORPHAN1', ?, 'NOSUCH', 'fee', 'USD', -5)`, end-86400); err != nil {
		t.Fatalf("seed orphan transaction: %v", err)
	}

	got := dumpTable(t, db, ctx, `
		SELECT kind, account_kind FROM web_transactions
		 WHERE account_kind IS NULL OR account_kind NOT IN ('card')
		 ORDER BY ALL`)
	if !strings.Contains(got, "fee|<nil>") {
		t.Errorf("the unknown-account row must survive the fence; got:\n%s", got)
	}
	if strings.Contains(got, "|card") {
		t.Errorf("a card row survived the fence; got:\n%s", got)
	}
	// The card's own interest / fee charges are exactly what the fence exists
	// to keep out of the investment charts.
	all := dumpTable(t, db, ctx,
		`SELECT kind, account_kind FROM web_transactions WHERE account_kind = 'card' ORDER BY ALL`)
	for _, want := range []string{"fee|card", "interest|card"} {
		if !strings.Contains(all, want) {
			t.Errorf("fixture should book %q on the card; got:\n%s", want, all)
		}
	}
}

// TestTransactionsBetweenCarriesAccountKind pins the C5 lockstep: the
// report_transactions macro gained account_kind (migration 0039) and
// TransactionsBetween scans it positionally, so a drifted projection would
// surface here rather than at runtime.
func TestTransactionsBetweenCarriesAccountKind(t *testing.T) {
	db, ctx := openMigrated(t)
	end := seedCardFixture(t, db, ctx, true)

	rows, err := TransactionsBetween(ctx, db, 0, end, "USD", SortAscending)
	if err != nil {
		t.Fatalf("TransactionsBetween: %v", err)
	}
	seen := map[string]string{}
	for _, r := range rows {
		if r.AccountKind == nil {
			t.Errorf("%s/%s has no account_kind", r.SilverSourceID, r.TransactionExternalID)
			continue
		}
		seen[r.AccountExternalID] = *r.AccountKind
		// The scan is positional: a shifted column would land an account
		// id or a kind in the wrong field, so check a neighbour too.
		if r.Currency != "USD" {
			t.Errorf("%s currency = %q, want USD (scan alignment)", r.TransactionExternalID, r.Currency)
		}
	}
	for acct, want := range map[string]string{
		"BROK1": string(canonical.AccountKindBrokerage),
		"CHK1":  string(canonical.AccountKindCash),
		"CARD1": string(canonical.AccountKindCard),
	} {
		if got := seen[acct]; got != want {
			t.Errorf("%s account_kind = %q, want %q", acct, got, want)
		}
	}
}
