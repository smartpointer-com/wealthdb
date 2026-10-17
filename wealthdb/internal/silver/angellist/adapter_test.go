package angellist

import (
	"context"
	"database/sql"
	_ "embed"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return silvertest.NewDB(t, silverSchemaSQL)
}

func openAdapter(t *testing.T, path string) silver.Connection {
	t.Helper()
	return silvertest.Open(t, &Adapter{}, path)
}

// seed builds an event-sourced book exercising every forward-fill path: p1
// has an investment (cost), an annual K-1 statement (tax basis, whose
// cumulative contributions differ from the portal's), then the current
// valuation (FMV); p2 first appears at its investment; p3 is exited at its
// final statement (is_open=0).
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (1000, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date) VALUES
            ('p1', 'spv',  'SPV Alpha', 100),
            ('p2', 'fund', 'Fund One',  200),
            ('p3', 'spv',  'SPV Gamma', 100);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            -- p1: investment (cost) -> K-1 statement (tax basis) -> current FMV
            ('p1', 100,  'investment', NULL,     1, 'USD', 40000, 'cost',      40000, 1000),
            ('p1', 200,  'statement',  NULL,     1, 'USD', 60000, 'tax_basis', 38800, 1000),
            ('p1', 1000, 'valuation',  'live',   1, 'USD', 90000, 'fmv',       40000, 1000),
            -- p2: first appears at its investment, then current FMV
            ('p2', 200,  'investment', NULL,     1, 'USD', 20000, 'cost',      20000, 1000),
            ('p2', 1000, 'valuation',  'live',   1, 'USD', 35000, 'fmv',       20000, 1000),
            -- p3: investment, then a final (exited) statement
            ('p3', 100,  'investment', NULL,     1, 'USD', 15000, 'cost',      15000, 1000),
            ('p3', 300,  'statement',  'exited', 0, 'USD', 0,     'tax_basis', 15000, 1000);
    `); err != nil {
		t.Fatal(err)
	}
}

func TestKindIsAngellist(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "angellist" {
		t.Errorf("Kind() = %q, want angellist", got)
	}
}

func TestStatusEmpty(t *testing.T) {
	silvertest.AssertEmptyStatus(t, &Adapter{}, silverSchemaSQL)
}

func TestStatusAndChangeWindow(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Oldest spans the earliest position event (100); LatestChangeNumber
	// pins to the dump_run (1000).
	if s.OldestSnapshotAt != 100 || s.LatestSnapshotAt != 1000 || s.LatestChangeNumber != 1000 {
		t.Errorf("Status = %+v, want Oldest=100 Latest=1000 ChangeNumber=1000", s)
	}
	if s.OldestTransactionAt != -1 || s.LatestTransactionAt != -1 {
		t.Errorf("tx extrema = %d/%d, want -1/-1", s.OldestTransactionAt, s.LatestTransactionAt)
	}

	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start != 100 || w.End != 1000 || w.NewChangeNumber != 1000 {
		t.Errorf("ChangeWindow(-1) = %+v, want HasChanges Start=100 End=1000 NewCN=1000", w)
	}
	w2, err := conn.ChangeWindow(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if w2.HasChanges {
		t.Errorf("ChangeWindow(1000).HasChanges = true, want false (no new download)")
	}
}

func TestSnapshotsForwardFillPerEventDate(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var batches []canonical.SnapshotBatch
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		batches = append(batches, b)
		if !more {
			break
		}
	}
	// One batch per distinct event date: 100, 200, 300, 1000.
	if len(batches) != 4 {
		t.Fatalf("batches = %d, want 4 (event dates 100/200/300/1000)", len(batches))
	}

	posByT := map[int64]map[string]canonical.PositionChange{}
	instByT := map[int64]int{}
	instByID := map[string]canonical.InstrumentChange{}
	var acct *canonical.AccountChange
	for i := range batches {
		for _, p := range batches[i].Positions {
			if posByT[p.SnapshotAt] == nil {
				posByT[p.SnapshotAt] = map[string]canonical.PositionChange{}
			}
			posByT[p.SnapshotAt][p.PositionKey] = p
		}
		for _, in := range batches[i].Instruments {
			instByID[in.InstrumentExternalID] = in
		}
		for j := range batches[i].Accounts {
			acct = &batches[i].Accounts[j]
		}
		instByT[batchDate(batches[i])] = len(batches[i].Instruments)
	}

	// t=100: p1 (cost) + p3 (cost) live; p2 not invested yet.
	if got := len(posByT[100]); got != 2 {
		t.Errorf("t=100 positions = %d, want 2 (p1, p3)", got)
	}
	if mv := posByT[100]["p1"].MarketValue; mv == nil || mv.StringFixed(2) != "400.00" {
		t.Errorf("t=100 p1 market_value = %v, want 400.00 (cost)", mv)
	}
	// Taxonomy: a single-company SPV is unlisted-company ownership
	// held via a single-deal vehicle -> (private_equity, spv). The
	// position and its instrument must carry the same admitted pair.
	if ac, v := posByT[100]["p1"].AssetClass, posByT[100]["p1"].Vehicle; ac != canonical.AssetClassPrivateEquity || v != canonical.VehicleSPV {
		t.Errorf("p1 (asset_class, vehicle) = (%q, %q), want (private_equity, spv)", ac, v)
	}
	if !canonical.ValidTaxonomyPair(posByT[100]["p1"].AssetClass, posByT[100]["p1"].Vehicle) {
		t.Errorf("p1 pair (%q, %q) not admitted by ValidTaxonomyPair", posByT[100]["p1"].AssetClass, posByT[100]["p1"].Vehicle)
	}
	if in := instByID["p1"]; in.AssetClass != posByT[100]["p1"].AssetClass || in.Vehicle != posByT[100]["p1"].Vehicle {
		t.Errorf("p1 instrument pair = (%q, %q), want it to match the position (private_equity, spv)", in.AssetClass, in.Vehicle)
	}

	// t=200: p1 forward-fills to its K-1 statement (tax basis); p2 appears.
	if got := len(posByT[200]); got != 3 {
		t.Errorf("t=200 positions = %d, want 3 (p1, p2, p3)", got)
	}
	if mv := posByT[200]["p1"].MarketValue; mv == nil || mv.StringFixed(2) != "600.00" {
		t.Errorf("t=200 p1 market_value = %v, want 600.00 (tax basis)", mv)
	}
	// The book value stays the portal's contributed capital; the K-1's
	// cumulative contributions ride in the payload as tax-basis capital.
	for _, at := range []int64{200, 1000} {
		p := posByT[at]["p1"]
		if bv := p.BookValue; bv == nil || bv.StringFixed(2) != "400.00" {
			t.Errorf("t=%d p1 book_value = %v, want 400.00 (portal contributed)", at, bv)
		}
		if got := string(p.Payload); got != `{"tax_basis_contributed":"388"}` {
			t.Errorf("t=%d p1 payload = %s, want the K-1 contributions as tax_basis_contributed", at, got)
		}
	}
	if p := posByT[100]["p1"]; p.Payload != nil {
		t.Errorf("t=100 p1 payload = %s, want none before the first K-1", p.Payload)
	}
	// Taxonomy: a multi-company private fund keeps the private_equity
	// exposure but rides the pooled-fund vehicle -> (private_equity, fund).
	if ac, v := posByT[200]["p2"].AssetClass, posByT[200]["p2"].Vehicle; ac != canonical.AssetClassPrivateEquity || v != canonical.VehicleFund {
		t.Errorf("p2 (asset_class, vehicle) = (%q, %q), want (private_equity, fund)", ac, v)
	}
	if !canonical.ValidTaxonomyPair(posByT[200]["p2"].AssetClass, posByT[200]["p2"].Vehicle) {
		t.Errorf("p2 pair (%q, %q) not admitted by ValidTaxonomyPair", posByT[200]["p2"].AssetClass, posByT[200]["p2"].Vehicle)
	}
	if in := instByID["p2"]; in.AssetClass != posByT[200]["p2"].AssetClass || in.Vehicle != posByT[200]["p2"].Vehicle {
		t.Errorf("p2 instrument pair = (%q, %q), want it to match the position (private_equity, fund)", in.AssetClass, in.Vehicle)
	}

	// t=300: p3's latest event is its exit (is_open=0) → dropped.
	if got := len(posByT[300]); got != 2 {
		t.Errorf("t=300 positions = %d, want 2 (p1, p2; p3 exited)", got)
	}
	if _, ok := posByT[300]["p3"]; ok {
		t.Errorf("t=300 still contains exited p3")
	}

	// t=1000: current FMV; p3 still excluded.
	if got := len(posByT[1000]); got != 2 {
		t.Errorf("t=1000 positions = %d, want 2 (p1, p2)", got)
	}
	if mv := posByT[1000]["p1"].MarketValue; mv == nil || mv.StringFixed(2) != "900.00" {
		t.Errorf("t=1000 p1 market_value = %v, want 900.00 (current FMV)", mv)
	}
	if bv := posByT[1000]["p1"].BookValue; bv == nil || bv.StringFixed(2) != "400.00" {
		t.Errorf("t=1000 p1 book_value = %v, want 400.00 (contributed)", bv)
	}

	// One instrument per held position in each batch (FK satisfied).
	if instByT[100] != 2 || instByT[200] != 3 || instByT[1000] != 2 {
		t.Errorf("instruments per batch = %v, want 100→2 200→3 1000→2", instByT)
	}
	if acct == nil || acct.AccountExternalID != "acct" {
		t.Fatalf("account = %v, want external id 'acct'", acct)
	}
	if acct.AccountKind != canonical.AccountKindCustody {
		t.Errorf("account_kind = %q, want custody (LP interests, not a brokerage)", acct.AccountKind)
	}
	if acct.ManagementStyle == nil || *acct.ManagementStyle != canonical.ManagementStyleSelfDirected {
		t.Errorf("management_style = %v, want self_directed", acct.ManagementStyle)
	}
}

// batchDate returns the snapshot date a batch is for (all its positions share
// one SnapshotAt; empty batches return -1).
func batchDate(b canonical.SnapshotBatch) int64 {
	if len(b.Positions) > 0 {
		return b.Positions[0].SnapshotAt
	}
	return -1
}

// TestTransactionsFunding verifies the funding-ledger → canonical mapping:
// each AngelList type maps to the right TxKind, the source sign is preserved
// (a refund is a POSITIVE contribution reversal), and Status reports the tx
// extrema from the ledger.
func TestTransactionsFunding(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (1000, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name) VALUES
            ('pos-x', 'spv', 'X'), ('pos-f', 'fund', 'F');
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type,
            amount_minor, currency, description, syndicate_name, position_external_id) VALUES
            ('t1', 100, 'deposit',       500000, 'USD', 'Deposit from bank',        NULL,    NULL),
            ('t2', 200, 'investment',   -300000, 'USD', 'Investment in X',          'X SPV', 'pos-x'),
            ('t3', 300, 'disbursement',  120000, 'USD', 'Disbursement - X',         NULL,    'pos-x'),
            ('t4', 400, 'refund',         50000, 'USD', 'Refund (oversubscribed)',  'Y SPV', NULL),
            ('t5', 500, 'withdrawal',    -80000, 'USD', 'Withdrawal to bank',       NULL,    NULL),
            ('t6', 600, 'transfer',      -20000, 'USD', 'Transfer to bank',         NULL,    NULL),
            ('t7', 600, 'disbursement',   30000, 'USD', 'Distribution - F',         NULL,    'pos-f');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)

	w, _ := conn.ChangeWindow(context.Background(), -1)
	if !w.HasChanges || w.Start != 100 || w.End != 600 {
		t.Fatalf("ChangeWindow = %+v, want HasChanges Start=100 End=600", w)
	}
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	by := map[string]canonical.TransactionChange{}
	for _, tx := range batch.Transactions {
		by[tx.TransactionExternalID] = tx
	}
	if len(by) != 7 {
		t.Fatalf("transactions = %d, want 7", len(batch.Transactions))
	}
	// (id, want kind, want signed net)
	cases := []struct {
		id, kind, net string
	}{
		{"funding:t1", string(canonical.TxKindDeposit), "5000.00"},
		{"funding:t2", string(canonical.TxKindContribution), "-3000.00"},
		{"funding:t3", string(canonical.TxKindSell), "1200.00"}, // an SPV pays out: its stake is sold

		{"funding:t4", string(canonical.TxKindContribution), "500.00"}, // refund: + reversal
		{"funding:t5", string(canonical.TxKindWithdrawal), "-800.00"},
		{"funding:t6", string(canonical.TxKindWithdrawal), "-200.00"},  // transfer → withdrawal
		{"funding:t7", string(canonical.TxKindDistribution), "300.00"}, // a fund pays out and goes on
	}
	for _, c := range cases {
		tx, ok := by[c.id]
		if !ok {
			t.Errorf("%s missing", c.id)
			continue
		}
		if string(tx.Kind) != c.kind {
			t.Errorf("%s kind = %q, want %q", c.id, tx.Kind, c.kind)
		}
		if tx.NetAmount == nil || tx.NetAmount.StringFixed(2) != c.net {
			t.Errorf("%s net = %v, want %s", c.id, tx.NetAmount, c.net)
		}
		if tx.AccountExternalID != "acct" {
			t.Errorf("%s account = %q, want acct", c.id, tx.AccountExternalID)
		}
	}

	// the resolved SPV instrument rides on contribution/sell/distribution;
	// bank flows (deposit/withdrawal) carry none.
	if iid := by["funding:t3"].InstrumentExternalID; iid == nil || *iid != "pos-x" {
		t.Errorf("t3 (disbursement) instrument = %v, want pos-x", iid)
	}
	if iid := by["funding:t7"].InstrumentExternalID; iid == nil || *iid != "pos-f" {
		t.Errorf("t7 (disbursement) instrument = %v, want pos-f", iid)
	}
	if iid := by["funding:t2"].InstrumentExternalID; iid == nil || *iid != "pos-x" {
		t.Errorf("t2 (investment) instrument = %v, want pos-x", iid)
	}
	if by["funding:t1"].InstrumentExternalID != nil {
		t.Errorf("t1 (deposit) instrument = %v, want nil (bank flow)", by["funding:t1"].InstrumentExternalID)
	}

	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.OldestTransactionAt != 100 || s.LatestTransactionAt != 600 {
		t.Errorf("tx extrema = [%d,%d], want [100,600]", s.OldestTransactionAt, s.LatestTransactionAt)
	}
}

// TestSnapshotsFundingCashBalance verifies the funding account's current
// uninvested cash is emitted as a CashBalanceChange dated at the latest
// funding movement.
func TestSnapshotsFundingCashBalance(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (1000, 'acct');
        INSERT INTO funding_accounts(funding_account_external_id, currency, balance_minor)
            VALUES ('fa1', 'USD', 250000);
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type, amount_minor, currency) VALUES
            ('t1', 100, 'deposit',     500000, 'USD'),
            ('t2', 700, 'withdrawal', -250000, 'USD');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var cbs []canonical.CashBalanceChange
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		cbs = append(cbs, b.CashBalances...)
		if !more {
			break
		}
	}
	if len(cbs) != 1 {
		t.Fatalf("cash balances = %d, want 1", len(cbs))
	}
	cb := cbs[0]
	if cb.BalanceKind != canonical.BalanceKindCurrent {
		t.Errorf("balance_kind = %q, want current", cb.BalanceKind)
	}
	if cb.Amount.StringFixed(2) != "2500.00" { // 250000 minor; == deposit − withdrawal
		t.Errorf("amount = %v, want 2500.00", cb.Amount)
	}
	if cb.SnapshotAt != 700 {
		t.Errorf("snapshot_at = %d, want 700 (last funding movement)", cb.SnapshotAt)
	}
	if cb.AccountExternalID != "acct" {
		t.Errorf("account = %q, want acct", cb.AccountExternalID)
	}
}

// TestExitedInstruments verifies that an offering with no position_snapshots
// (an exited investment derived from the funding ledger) is still emitted as
// an instrument, so its contributions/distributions have something to link to.
func TestExitedInstruments(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (1000, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name)
            VALUES ('funding:foo', 'spv', 'Foo Co');
        INSERT INTO funding_transactions(transaction_external_id, occurred_at, type,
            amount_minor, currency, position_external_id)
            VALUES ('t1', 500, 'investment', -1000, 'USD', 'funding:foo');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var insts []canonical.InstrumentChange
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		insts = append(insts, b.Instruments...)
		if !more {
			break
		}
	}
	var foo *canonical.InstrumentChange
	for i := range insts {
		if insts[i].InstrumentExternalID == "funding:foo" {
			foo = &insts[i]
		}
	}
	if foo == nil {
		t.Fatalf("exited instrument funding:foo not emitted (got %d instruments)", len(insts))
	}
	if foo.Name == nil || *foo.Name != "Foo Co" {
		t.Errorf("name = %v, want Foo Co", foo.Name)
	}
	// The taxonomy pair rides the exited-instrument path too: (private_equity, spv).
	if foo.AssetClass != canonical.AssetClassPrivateEquity || foo.Vehicle != canonical.VehicleSPV {
		t.Errorf("(asset_class, vehicle) = (%q, %q), want (private_equity, spv)", foo.AssetClass, foo.Vehicle)
	}
	if !canonical.ValidTaxonomyPair(foo.AssetClass, foo.Vehicle) {
		t.Errorf("pair (%q, %q) not admitted by ValidTaxonomyPair", foo.AssetClass, foo.Vehicle)
	}
	if foo.FirstSeenAt != 500 || foo.LastSeenAt != 500 {
		t.Errorf("seen-range = [%d,%d], want [500,500]", foo.FirstSeenAt, foo.LastSeenAt)
	}
}

// collectBatches drains a snapshot stream.
func collectBatches(t *testing.T, conn silver.Connection) []canonical.SnapshotBatch {
	t.Helper()
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	return silvertest.Snapshots(t, conn, w)
}

// TestAPositionADumpStopsListingLeavesAtThatDump pins the roster step: a
// position a complete dump no longer lists leaves the stream at that dump's
// time, which is a snapshot time where every other position keeps its own
// mark date; the earlier snapshots still carry it. The dump time can lie
// after every mark, so the status and the window reach it. A dump is
// complete when the positions listed around it number what its summary
// declares.
func TestAPositionADumpStopsListingLeavesAtThatDump(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (1000, 'acct'), (2000, 'acct');
        INSERT INTO portfolio_summary(snapshot_at, invest_account_slug, total_investments_count) VALUES
            (1000, 'acct', 2), (2000, 'acct', 1);
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date, first_seen_at, last_seen_at) VALUES
            ('p1', 'spv', 'SPV Alpha', 100, 1000, 2000),
            ('p2', 'spv', 'SPV Beta',  100, 1000, 1000);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            -- p1: listed by both dumps
            ('p1', 100, 'investment', NULL,   1, 'USD', 40000, 'cost', 40000, 2000),
            ('p1', 500, 'valuation',  'live', 1, 'USD', 90000, 'fmv',  40000, 2000),
            -- p2: open on its latest row, last listed by the first dump
            ('p2', 100, 'investment', NULL,   1, 'USD', 15000, 'cost', 15000, 1000),
            ('p2', 300, 'valuation',  'live', 1, 'USD', 20000, 'fmv',  15000, 1000);
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	s, err := conn.Status(context.Background())
	if err != nil || s.LatestSnapshotAt != 2000 {
		t.Errorf("Status latest snapshot = %d %v, want the dump that dropped p2", s.LatestSnapshotAt, err)
	}
	w, _ := conn.ChangeWindow(context.Background(), -1)
	if w.End != 2000 {
		t.Errorf("window end = %d, want the dump that dropped p2", w.End)
	}
	posByT := map[int64]map[string]canonical.PositionChange{}
	for _, b := range collectBatches(t, conn) {
		for _, p := range b.Positions {
			if posByT[p.SnapshotAt] == nil {
				posByT[p.SnapshotAt] = map[string]canonical.PositionChange{}
			}
			posByT[p.SnapshotAt][p.PositionKey] = p
		}
	}
	if got := len(posByT); got != 4 {
		t.Fatalf("snapshot times = %d, want the marks 100/300/500 and the dump 2000: %v", got, posByT)
	}
	if _, ok := posByT[500]["p2"]; !ok || len(posByT[500]) != 2 {
		t.Errorf("t=500 still lists p2: %v", posByT[500])
	}
	if p := posByT[500]["p2"]; p.ValuedAt != 300 {
		t.Errorf("t=500 p2 valued at %d, want its mark date 300", p.ValuedAt)
	}
	at := posByT[2000]
	if len(at) != 1 {
		t.Fatalf("t=2000 positions = %v, want p1 alone", at)
	}
	if p := at["p1"]; p.ValuedAt != 500 || p.MarketValue == nil || p.MarketValue.StringFixed(2) != "900.00" {
		t.Errorf("t=2000 p1 = %+v, want its mark of 500 unchanged", p)
	}
}

// TestAnIncompleteDumpRemovesNothing pins the gate on the roster step: a
// dump without a portfolio summary, and a dump whose listing fell short of
// the count its summary declares, say nothing about the roster, so every
// position stays and the stream ends at the marks.
func TestAnIncompleteDumpRemovesNothing(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (1000, 'acct'), (2000, 'acct'), (3000, 'acct');
        -- 2000 captured no summary; 3000 declares two investments and listed one.
        INSERT INTO portfolio_summary(snapshot_at, invest_account_slug, total_investments_count) VALUES
            (1000, 'acct', 2), (3000, 'acct', 2);
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date, first_seen_at, last_seen_at) VALUES
            ('p1', 'spv', 'SPV Alpha', 100, 1000, 3000),
            ('p2', 'spv', 'SPV Beta',  100, 1000, 1000);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('p1', 100, 'investment', NULL, 1, 'USD', 40000, 'cost', 40000, 3000),
            ('p2', 100, 'investment', NULL, 1, 'USD', 15000, 'cost', 15000, 1000);
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	if s, err := conn.Status(context.Background()); err != nil || s.LatestSnapshotAt != 100 {
		t.Errorf("Status latest snapshot = %d %v, want the mark alone", s.LatestSnapshotAt, err)
	}
	batches := collectBatches(t, conn)
	if len(batches) != 1 || len(batches[0].Positions) != 2 {
		t.Fatalf("batches = %+v, want the one mark-dated snapshot with both positions", batches)
	}
}

// TestTheLastPositionADumpStopsListingClosesTheBook pins the closure marker
// on a roster removal: when a complete dump drops the only open position,
// that dump's snapshot replays it at zero value instead of being empty.
func TestTheLastPositionADumpStopsListingClosesTheBook(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (1000, 'acct'), (2000, 'acct');
        INSERT INTO portfolio_summary(snapshot_at, invest_account_slug, total_investments_count) VALUES
            (1000, 'acct', 1), (2000, 'acct', 0);
        INSERT INTO offerings(position_external_id, kind, company_name, investment_date, first_seen_at, last_seen_at) VALUES
            ('p9', 'spv', 'Solo SPV', 100, 1000, 1000);
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            status, is_open, currency, market_value_minor, valuation_basis,
            contributed_minor, snapshot_at) VALUES
            ('p9', 100, 'investment', NULL, 1, 'USD', 15000, 'cost', 15000, 1000);
    `); err != nil {
		t.Fatal(err)
	}
	batches := collectBatches(t, openAdapter(t, path))
	if len(batches) != 2 {
		t.Fatalf("batches = %d, want the held snapshot and the closure marker", len(batches))
	}
	marker := batches[1]
	if len(marker.Positions) != 1 || len(marker.Accounts) != 1 || len(marker.Instruments) != 1 {
		t.Fatalf("marker shape = %d pos / %d accts / %d insts, want 1/1/1", len(marker.Positions), len(marker.Accounts), len(marker.Instruments))
	}
	p := marker.Positions[0]
	if p.PositionKey != "p9" || p.SnapshotAt != 2000 || p.MarketValue == nil || !p.MarketValue.IsZero() {
		t.Errorf("marker position = %+v, want p9 at zero value at the dump time", p)
	}
	if marker.Accounts[0].AccountExternalID != "acct" {
		t.Errorf("marker account = %q, want the invest account", marker.Accounts[0].AccountExternalID)
	}
}
