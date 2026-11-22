package angellist

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// carryPositions loads seedSQL and returns the positions by snapshot time
// and position key, with the snapshot times in order.
//
// In seedSQL, Dn is UTC midnight of day n and Hn an hour later on that day:
// events sit at midnight, ledger rows at a time of day.
func carryPositions(t *testing.T, seedSQL string) (map[int64]map[string]canonical.PositionChange, []int64) {
	t.Helper()
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(days(seedSQL)); err != nil {
		t.Fatal(err)
	}
	byT := map[int64]map[string]canonical.PositionChange{}
	var times []int64
	for _, b := range collectBatches(t, openAdapter(t, path)) {
		for _, p := range b.Positions {
			if byT[p.SnapshotAt] == nil {
				byT[p.SnapshotAt] = map[string]canonical.PositionChange{}
				times = append(times, p.SnapshotAt)
			}
			byT[p.SnapshotAt][p.PositionKey] = p
		}
	}
	return byT, times
}

// wantCarry checks one position's value, valued-at day and payload.
func wantCarry(t *testing.T, byT map[int64]map[string]canonical.PositionChange, at int64, key, value string, valuedAt int64, payload string) {
	t.Helper()
	p, ok := byT[at][key]
	if !ok {
		t.Errorf("t=%d %s: not held", at, key)
		return
	}
	if p.MarketValue == nil || p.MarketValue.StringFixed(2) != value {
		t.Errorf("t=%d %s market_value = %v, want %s", at, key, p.MarketValue, value)
	}
	if p.ValuedAt != valuedAt {
		t.Errorf("t=%d %s valued at %d, want %d", at, key, p.ValuedAt, valuedAt)
	}
	if got := string(p.Payload); got != payload {
		t.Errorf("t=%d %s payload = %s, want %s", at, key, got, payload)
	}
}

// days replaces each Dn in s with day n's midnight and each Hn with an
// hour after it, longest first so D10 is not read as D1.
func days(s string) string {
	for n := 40; n >= 0; n-- {
		s = strings.ReplaceAll(s, fmt.Sprintf("D%d", n), fmt.Sprint(n*day))
		s = strings.ReplaceAll(s, fmt.Sprintf("H%d", n), fmt.Sprint(n*day+3600))
	}
	return s
}

func d(n int64) int64 { return n * day }
func h(n int64) int64 { return n*day + 3600 }

const cost = `{"valuation_basis":"cost"}`

// A position with no mark yet is at the capital the ledger paid in by
// then, the investment's own event stating none. Each payment is a
// snapshot time at the end of its ledger day, where the value moves. The
// line is valued at its first payment: the cost is no fresh mark. Its
// book value is the ledger's capital paid in less refunds. The first
// statement then replaces the cost with its mark.
func TestAPositionWithNoMarkIsAtTheCapitalPaidIn(t *testing.T) {
	byT, times := carryPositions(t, `
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (D30, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date) VALUES
            ('f1', 'fund', 'Example Fund', D1);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('f1', D1, 'investment', NULL, 1, 'USD', NULL,  'cost',      NULL,  D30),
            ('f1', D5, 'statement',  NULL, 1, 'USD', 70000, 'tax_basis', 60000, D30);
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type,
            amount_minor, currency, position_external_id) VALUES
            ('t0', H0, 'deposit',    100000, 'USD', NULL),
            ('t1', H1, 'investment', -30000, 'USD', 'f1'),
            ('t2', H2, 'investment', -40000, 'USD', 'f1'),
            ('t3', H3, 'refund',      10000, 'USD', 'f1');
    `)
	if want := []int64{d(1), h(1), h(2), h(3), d(5)}; !slices.Equal(times, want) {
		t.Fatalf("snapshot times = %v, want %v: the investment, each payment, the mark", times, want)
	}
	wantCarry(t, byT, d(1), "f1", "0.00", d(1), cost)
	wantCarry(t, byT, h(1), "f1", "300.00", h(1), cost)
	wantCarry(t, byT, h(2), "f1", "700.00", h(1), cost)
	wantCarry(t, byT, h(3), "f1", "600.00", h(1), cost)
	wantCarry(t, byT, d(5), "f1", "700.00", d(5), `{"tax_basis_contributed":"600"}`)
	if p := byT[h(3)]["f1"]; p.BookValue == nil || p.BookValue.StringFixed(2) != "600.00" || p.Basis != ledgerPaidInBasis {
		t.Errorf("h3 f1 book value = %v %+v, want the ledger's 600.00 paid in, derived", p.BookValue, p.Basis)
	}
}

// Money moved after a mark adds to it at cost until the next mark,
// whatever the latest event is, and the line stays valued at the mark's
// day. A mark covers its whole UTC day: a payment later that day is in it.
func TestMoneyMovedAfterAMarkAddsToItAtCost(t *testing.T) {
	byT, times := carryPositions(t, `
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (D30, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date) VALUES
            ('f1', 'fund', 'Example Fund', D3),
            ('m1', 'spv',  'Example Co',   D1);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('f1', D2,  'statement',  NULL,   1, 'USD', 50000,  'tax_basis', 40000, D30),
            ('f1', D3,  'investment', NULL,   1, 'USD', NULL,   'cost',      NULL,  D30),
            ('f1', D10, 'valuation',  'live', 1, 'USD', 95000,  'fmv',       65000, D30),
            ('m1', D1,  'valuation',  'live', 1, 'USD', 80000,  'fmv',       50000, D30),
            ('m1', D9,  'valuation',  'live', 1, 'USD', 110000, 'fmv',       70000, D30);
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type,
            amount_minor, currency, position_external_id) VALUES
            ('t1', H2, 'investment', -40000, 'USD', 'f1'),
            ('t2', H3, 'investment', -15000, 'USD', 'f1'),
            ('t3', H4, 'investment', -10000, 'USD', 'f1'),
            ('t4', H1, 'investment', -50000, 'USD', 'm1'),
            ('t5', H6, 'investment', -20000, 'USD', 'm1'),
            ('t6', H9, 'refund',       5000, 'USD', 'm1');
    `)
	if want := []int64{d(1), h(1), d(2), h(2), d(3), h(3), h(4), h(6), d(9), h(9), d(10)}; !slices.Equal(times, want) {
		t.Fatalf("snapshot times = %v, want %v", times, want)
	}
	tax := `"tax_basis_contributed":"400"`
	wantCarry(t, byT, h(2), "f1", "500.00", d(2), `{`+tax+`}`)
	wantCarry(t, byT, h(3), "f1", "650.00", d(2), `{"net_paid_in_since_mark":"150",`+tax+`,"valuation_basis":"mark_plus_cost"}`)
	wantCarry(t, byT, h(4), "f1", "750.00", d(2), `{"net_paid_in_since_mark":"250",`+tax+`,"valuation_basis":"mark_plus_cost"}`)
	wantCarry(t, byT, d(10), "f1", "950.00", d(10), `{`+tax+`}`)
	wantCarry(t, byT, h(1), "m1", "800.00", d(1), "")
	wantCarry(t, byT, h(6), "m1", "1000.00", d(1), `{"net_paid_in_since_mark":"200","valuation_basis":"mark_plus_cost"}`)
	wantCarry(t, byT, h(9), "m1", "1100.00", d(9), "")
}

// A value the collector set at cost is no mark: with ledger payments the
// position is at what they paid in by then, not at the cost the event
// states, and its stated book value stands.
func TestAStatedCostYieldsToTheLedger(t *testing.T) {
	byT, _ := carryPositions(t, `
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (D30, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date) VALUES
            ('c1', 'fund', 'Example Fund', D1);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('c1', D1, 'investment', NULL, 1, 'USD', 90000, 'cost', 90000, D30);
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type,
            amount_minor, currency, position_external_id) VALUES
            ('t1', H1, 'investment', -30000, 'USD', 'c1'),
            ('t2', H7, 'investment', -60000, 'USD', 'c1');
    `)
	wantCarry(t, byT, h(1), "c1", "300.00", h(1), cost)
	wantCarry(t, byT, h(7), "c1", "900.00", h(1), cost)
	if p := byT[h(1)]["c1"]; p.BookValue == nil || p.BookValue.StringFixed(2) != "900.00" || p.Basis != paidInBasis {
		t.Errorf("c1 book value = %v %+v, want the portal's stated 900.00", p.BookValue, p.Basis)
	}
}

// A refund and a payout leave the position at cost: the value falls by
// what came back, never below zero, until the next mark. A payout leaves
// the paid-in book value alone.
func TestAPayoutComesOffTheCarriedValue(t *testing.T) {
	byT, _ := carryPositions(t, `
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (D30, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date) VALUES
            ('f1', 'fund', 'Example Fund', D1),
            ('s1', 'spv',  'Example Co',   D1);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('f1', D2, 'statement',  NULL, 1, 'USD', 50000, 'tax_basis', 40000, D30),
            ('s1', D1, 'investment', NULL, 1, 'USD', NULL,  'cost',      NULL,  D30);
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type,
            amount_minor, currency, position_external_id) VALUES
            ('t1', H1, 'investment',   -40000, 'USD', 'f1'),
            ('t2', H3, 'disbursement',  15000, 'USD', 'f1'),
            ('t3', H1, 'investment',   -20000, 'USD', 's1'),
            ('t4', H4, 'disbursement',  90000, 'USD', 's1');
    `)
	wantCarry(t, byT, h(3), "f1", "350.00", d(2), `{"net_paid_in_since_mark":"-150","tax_basis_contributed":"400","valuation_basis":"mark_plus_cost"}`)
	wantCarry(t, byT, h(3), "s1", "200.00", h(1), cost)
	wantCarry(t, byT, h(4), "s1", "0.00", h(1), cost)
	if p := byT[h(4)]["s1"]; p.BookValue == nil || p.BookValue.StringFixed(2) != "200.00" {
		t.Errorf("s1 book value after the payout = %v, want the 200.00 paid in", p.BookValue)
	}
}

// A position whose first payment comes before its investment event is
// held at cost from that payment. A deal the ledger alone names is held at
// cost while what it moved in is above zero: a commitment refunded in full
// leaves nothing behind, and one never refunded stays held.
func TestTheLedgerOpensAPositionAtItsFirstPayment(t *testing.T) {
	byT, times := carryPositions(t, `
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (D30, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date) VALUES
            ('s1', 'spv', 'Example Co', D3),
            ('funding:example-deal', 'spv', 'Example Deal', NULL),
            ('funding:kept-deal',    'spv', 'Kept Deal',    NULL);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('s1', D3, 'investment', NULL,   1, 'USD', NULL,  'cost', NULL, D30),
            ('s1', D8, 'valuation',  'live', 1, 'USD', 45000, 'fmv',  NULL, D30);
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type,
            amount_minor, currency, position_external_id) VALUES
            ('t1', H1, 'investment', -30000, 'USD', 's1'),
            ('t2', H2, 'investment', -10000, 'USD', 'funding:example-deal'),
            ('t3', H5, 'refund',      10000, 'USD', 'funding:example-deal'),
            ('t4', H6, 'investment', -25000, 'USD', 'funding:kept-deal');
    `)
	if want := []int64{h(1), h(2), d(3), h(5), h(6), d(8)}; !slices.Equal(times, want) {
		t.Fatalf("snapshot times = %v, want %v", times, want)
	}
	wantCarry(t, byT, h(1), "s1", "300.00", h(1), cost)
	wantCarry(t, byT, d(3), "s1", "300.00", h(1), cost)
	wantCarry(t, byT, h(2), "funding:example-deal", "100.00", h(2), cost)
	wantCarry(t, byT, d(3), "funding:example-deal", "100.00", h(2), cost)
	for _, at := range []int64{h(5), h(6), d(8)} {
		if _, ok := byT[at]["funding:example-deal"]; ok {
			t.Errorf("t=%d still holds the refunded deal", at)
		}
	}
	wantCarry(t, byT, d(8), "funding:kept-deal", "250.00", h(6), cost)
	if p := byT[h(1)]["s1"]; p.AcquisitionDate == nil || p.BookValue == nil || p.BookValue.StringFixed(2) != "300.00" {
		t.Errorf("h1 s1 = %+v, want the offering's acquisition date and the 300.00 paid in as book value", p)
	}
}

// With no ledger payments a position is valued by its events: a later
// event with no value keeps the last mark, plus the change in the capital
// the two events state contributed, valued at the mark's day.
func TestAValuelessEventKeepsTheMarkWithoutALedger(t *testing.T) {
	byT, _ := carryPositions(t, `
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (D30, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date) VALUES
            ('p1', 'spv', 'Example Co', D1),
            ('p2', 'spv', 'Example Two', D1);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('p1', D1, 'investment', NULL,   1, 'USD', 40000, 'cost', 40000, D30),
            ('p1', D2, 'valuation',  'live', 1, 'USD', 90000, 'fmv',  40000, D30),
            ('p1', D3, 'investment', NULL,   1, 'USD', 50000, 'cost', 50000, D30),
            ('p2', D1, 'valuation',  'live', 1, 'USD', 70000, 'fmv',  NULL,  D30),
            ('p2', D3, 'investment', NULL,   1, 'USD', NULL,  'cost', NULL,  D30);
    `)
	wantCarry(t, byT, d(1), "p1", "400.00", d(1), cost)
	wantCarry(t, byT, d(2), "p1", "900.00", d(2), "")
	wantCarry(t, byT, d(3), "p1", "1000.00", d(2), `{"net_paid_in_since_mark":"100","valuation_basis":"mark_plus_cost"}`)
	wantCarry(t, byT, d(3), "p2", "700.00", d(1), "")
}

// A payment into a position the portal stopped listing is no snapshot
// time: the position is gone by then.
func TestAPaymentAfterARemovalIsNoSnapshotTime(t *testing.T) {
	_, times := carryPositions(t, `
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (D10, 'acct'), (D20, 'acct');
        INSERT INTO portfolio_summary(snapshot_at, invest_account_slug, total_investments_count) VALUES
            (D10, 'acct', 2), (D20, 'acct', 1);
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date, first_seen_at, last_seen_at) VALUES
            ('p1', 'spv',  'Example Co',   D1, D10, D20),
            ('f1', 'fund', 'Example Fund', D1, D10, D10);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('p1', D1, 'valuation',  'live', 1, 'USD', 40000, 'fmv',  40000, D20),
            ('f1', D1, 'investment', NULL,   1, 'USD', NULL,  'cost', NULL,  D10);
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type,
            amount_minor, currency, position_external_id) VALUES
            ('t1', H1,  'investment', -30000, 'USD', 'f1'),
            ('t2', H15, 'investment', -30000, 'USD', 'f1'),
            ('t3', H25, 'investment', -30000, 'USD', 'f1');
    `)
	if want := []int64{d(1), h(1), h(15), d(20)}; !slices.Equal(times, want) {
		t.Errorf("snapshot times = %v, want %v: the payment after the removal is none", times, want)
	}
}

// The portal may carry a deal at cost without stating a basis: a mark
// worth exactly its nonzero book value states cost. The book value may be
// the one the ledger derives (l1), and a K-1's tax basis rides beside the
// stamp (k1). A mark worth more, one with no book value and one worth 0
// at a book of 0 state no basis. s1's mark states cost, and the mark plus
// cost after it stays mark_plus_cost, though it is worth its book value.
func TestAMarkAtItsBookValueStatesCost(t *testing.T) {
	byT, _ := carryPositions(t, `
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (D30, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date) VALUES
            ('a1', 'spv', 'Example A', D1), ('u1', 'spv', 'Example U', D1), ('n1', 'spv', 'Example N', D1),
            ('z1', 'spv', 'Example Z', D1), ('s1', 'spv', 'Example S', D1), ('l1', 'spv', 'Example L', D1),
            ('k1', 'spv', 'Example K', D1);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('a1', D2, 'valuation',  'live', 1, 'USD', 50000, 'fmv',       50000, D30),
            ('u1', D2, 'valuation',  'live', 1, 'USD', 90000, 'fmv',       50000, D30),
            ('n1', D2, 'valuation',  'live', 1, 'USD', 40000, 'fmv',       NULL,  D30),
            ('z1', D2, 'valuation',  'live', 1, 'USD', 0,     'fmv',       0,     D30),
            ('s1', D2, 'valuation',  'live', 1, 'USD', 20000, 'fmv',       20000, D30),
            ('s1', D4, 'investment', NULL,   1, 'USD', NULL,  'cost',      30000, D30),
            ('l1', D2, 'valuation',  'live', 1, 'USD', 40000, 'fmv',       NULL,  D30),
            ('k1', D1, 'statement',  NULL,   1, 'USD', 30000, 'tax_basis', 45000, D30),
            ('k1', D2, 'valuation',  'live', 1, 'USD', 50000, 'fmv',       50000, D30);
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type,
            amount_minor, currency, position_external_id) VALUES
            ('t1', H1, 'investment', -40000, 'USD', 'l1');
    `)
	wantCarry(t, byT, d(2), "a1", "500.00", d(2), cost)
	wantCarry(t, byT, d(2), "u1", "900.00", d(2), "")
	wantCarry(t, byT, d(2), "n1", "400.00", d(2), "")
	wantCarry(t, byT, d(2), "z1", "0.00", d(2), "")
	wantCarry(t, byT, d(2), "s1", "200.00", d(2), cost)
	wantCarry(t, byT, d(4), "s1", "300.00", d(2), `{"net_paid_in_since_mark":"100","valuation_basis":"mark_plus_cost"}`)
	wantCarry(t, byT, d(2), "l1", "400.00", d(2), cost)
	if p := byT[d(2)]["l1"]; p.BookValue == nil || p.BookValue.StringFixed(2) != "400.00" || p.Basis != ledgerPaidInBasis {
		t.Errorf("d2 l1 book value = %v %+v, want the ledger's 400.00 paid in, derived", p.BookValue, p.Basis)
	}
	wantCarry(t, byT, d(2), "k1", "500.00", d(2), `{"tax_basis_contributed":"450","valuation_basis":"cost"}`)
}
