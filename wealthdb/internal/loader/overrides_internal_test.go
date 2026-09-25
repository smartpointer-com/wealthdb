package loader

// Internal tests for the config-override patchers: they are unexported,
// and what they must NOT touch is as load-bearing as what they must.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// TestTransactionInstrumentsLinksOnlyWhatTheAdapterCouldNot pins the
// config link that closes the tail: it keys on the token the adapter
// looked up and failed on, never second-guesses a row that resolved,
// and no-ops silently on a token nothing states any more — which is
// what an adapter learning to resolve it looks like.
//
// And pins what the link takes AWAY. A trade's own taxonomy pair is
// the adapter's answer for a row nothing could name; once config names
// the instrument, that answer is the worse of the two and would mask
// the dimension's, which the cash flow statement reads in preference.
func TestTransactionInstrumentsLinksOnlyWhatTheAdapterCouldNot(t *testing.T) {
	already := "EXAMPLE0001"
	txns := []canonical.TransactionChange{
		{TransactionExternalID: "t1", InstrumentHint: "1234567"},
		{TransactionExternalID: "t2", InstrumentHint: "Example Fund, Renamed"},
		// Already resolved: config closes the tail, it does not
		// overrule the adapter.
		{TransactionExternalID: "t3", InstrumentExternalID: &already, InstrumentHint: "1234567"},
		// States no token at all.
		{TransactionExternalID: "t4"},
		// A token the list does not carry: keeps the coarse pair its
		// feed could state, having nothing better to fall back on.
		{TransactionExternalID: "t5", InstrumentHint: "9999999",
			AssetClass: canonical.AssetClassMetal, Vehicle: canonical.VehiclePhysical},
		// A linked row hands the question back to the instrument.
		{TransactionExternalID: "t6", InstrumentHint: "7654321",
			AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleFund},
	}
	applyTransactionInstruments(txns, map[string]string{
		"1234567":               "EXAMPLE0009",
		"Example Fund, Renamed": "EXAMPLE0010",
		"7654321":               "EXAMPLE0012",
		// A token nothing states any more: a no-op, not an error.
		"0000001": "EXAMPLE0011",
	})
	for _, tc := range []struct {
		id, instrument string
		class          canonical.AssetClass
		vehicle        canonical.Vehicle
	}{
		{"t1", "EXAMPLE0009", "", ""},
		{"t2", "EXAMPLE0010", "", ""},
		{"t3", "EXAMPLE0001", "", ""},
		{"t4", "", "", ""},
		{"t5", "", canonical.AssetClassMetal, canonical.VehiclePhysical},
		{"t6", "EXAMPLE0012", "", ""},
	} {
		var got canonical.TransactionChange
		for _, x := range txns {
			if x.TransactionExternalID == tc.id {
				got = x
			}
		}
		id := ""
		if got.InstrumentExternalID != nil {
			id = *got.InstrumentExternalID
		}
		if id != tc.instrument {
			t.Errorf("%s linked to %q, want %q", tc.id, id, tc.instrument)
		}
		if got.AssetClass != tc.class || got.Vehicle != tc.vehicle {
			t.Errorf("%s kept (%q, %q), want (%q, %q)",
				tc.id, got.AssetClass, got.Vehicle, tc.class, tc.vehicle)
		}
	}
}

// TestTaxableWrapperMovesOnlyTheGenericTaxableAnswer pins the blanket
// rule and, more importantly, what it must not touch. An adapter says
// `taxable_personal` because a bank feed states what a product is and
// never who holds it; every other wrapper is something it had positive
// evidence for, and a source-wide statement about joint ownership has
// no business overruling that.
func TestTaxableWrapperMovesOnlyTheGenericTaxableAnswer(t *testing.T) {
	w := func(s canonical.TaxWrapper) *canonical.TaxWrapper { return &s }
	accounts := []canonical.AccountChange{
		{AccountExternalID: "a1", TaxWrapper: w(canonical.TaxWrapperTaxablePersonal)},
		{AccountExternalID: "a2", TaxWrapper: w(canonical.TaxWrapperRothIRA)},
		{AccountExternalID: "a3", TaxWrapper: w(canonical.TaxWrapperTrustNonGrantor)},
		{AccountExternalID: "a4", TaxWrapper: w(canonical.TaxWrapperCustodialUTMA)},
		// No wrapper at all: absent is not the same as taxable, and
		// guessing here is the error the coverage canary reports.
		{AccountExternalID: "a5"},
	}
	applyTaxableWrapper(accounts, string(canonical.TaxWrapperTaxableJoint))
	want := map[string]canonical.TaxWrapper{
		"a1": canonical.TaxWrapperTaxableJoint,
		"a2": canonical.TaxWrapperRothIRA,
		"a3": canonical.TaxWrapperTrustNonGrantor,
		"a4": canonical.TaxWrapperCustodialUTMA,
		"a5": "",
	}
	for _, a := range accounts {
		got := canonical.TaxWrapper("")
		if a.TaxWrapper != nil {
			got = *a.TaxWrapper
		}
		if got != want[a.AccountExternalID] {
			t.Errorf("%s = %q, want %q", a.AccountExternalID, got, want[a.AccountExternalID])
		}
	}
	// Unset is a no-op, not a wipe.
	before := *accounts[0].TaxWrapper
	applyTaxableWrapper(accounts, "")
	if *accounts[0].TaxWrapper != before {
		t.Errorf("an empty rule changed %q", *accounts[0].TaxWrapper)
	}
}

// TestDropSupersededCutsByDateNotByAccount pins the cut an account
// outliving its source needs: everything from the handover on belongs
// to whatever carries the account now, and everything before it is
// still this source's to contribute. Dropping the account outright
// would throw away the earlier half of whatever document straddles the
// date — which for a statement archive is the part nothing else has.
func TestDropSupersededCutsByDateNotByAccount(t *testing.T) {
	const handover = int64(2000)
	rows := []canonical.TransactionChange{
		{TransactionExternalID: "before", AccountExternalID: "A", OccurredAt: 1999},
		{TransactionExternalID: "on", AccountExternalID: "A", OccurredAt: handover},
		{TransactionExternalID: "after", AccountExternalID: "A", OccurredAt: 2001},
		{TransactionExternalID: "other-account", AccountExternalID: "B", OccurredAt: 5000},
	}
	got := dropSuperseded(rows, map[string]int64{"A": handover},
		func(x canonical.TransactionChange) (string, int64) {
			return x.AccountExternalID, x.OccurredAt
		})

	var kept []string
	for _, r := range got {
		kept = append(kept, r.TransactionExternalID)
	}
	// The handover date itself belongs to the NEW source: the two must
	// tile, and an inclusive cut on both sides would double that day.
	want := []string{"before", "other-account"}
	if len(kept) != len(want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Errorf("kept[%d] = %q, want %q", i, kept[i], want[i])
		}
	}
}

// TestDropSupersededIsANoOpWhenNothingMatches: an entry naming an
// account no row carries is a success, not an error — it is what a
// source that stopped emitting the account on its own looks like — and
// an unconfigured source must be untouched.
func TestDropSupersededIsANoOpWhenNothingMatches(t *testing.T) {
	rows := []canonical.PositionChange{
		{AccountExternalID: "A", SnapshotAt: 9999},
	}
	at := func(p canonical.PositionChange) (string, int64) {
		return p.AccountExternalID, p.SnapshotAt
	}
	for name, ends := range map[string]map[string]int64{
		"nothing configured":    nil,
		"names another account": {"Z": 1},
	} {
		if got := dropSuperseded(rows, ends, at); len(got) != 1 {
			t.Errorf("%s: kept %d row(s), want 1", name, len(got))
		}
	}
}

// TestSupersessionClosingZeroesWhatWasStillHeld: dropping an account's
// rows from the handover on is only half the cut. Gold carries a key
// forward until something supersedes it, and a source that just stops
// reporting supersedes nothing — so without an explicit zero the last
// pre-handover mark lingers and double-counts against whatever carries
// the account now.
//
// What gets zeroed is what the account held at its LAST snapshot before
// the handover. A key already gone by then has already ended; zeroing it
// too would resurrect it for a day.
func TestSupersessionClosingZeroesWhatWasStillHeld(t *testing.T) {
	const handover = int64(3000)
	ends := map[string]int64{"A": handover}
	closing := newSupersessionClosing(ends)

	// An earlier snapshot holds two keys; the later one holds only the
	// second, so GONE has already left the series.
	closing.observe([]canonical.PositionChange{
		{AccountExternalID: "A", SnapshotAt: 1000, PositionKey: "GONE"},
		{AccountExternalID: "A", SnapshotAt: 1000, PositionKey: "HELD"},
	}, nil)
	closing.observe([]canonical.PositionChange{
		{AccountExternalID: "A", SnapshotAt: 2000, PositionKey: "HELD",
			Payload: json.RawMessage(`{"quantity": "100"}`)},
	}, nil)
	// Past the handover, and another account entirely: neither is ours.
	closing.observe([]canonical.PositionChange{
		{AccountExternalID: "A", SnapshotAt: 4000, PositionKey: "LATER"},
		{AccountExternalID: "B", SnapshotAt: 2000, PositionKey: "OTHER"},
	}, nil)

	positions, _ := closing.rows("src")
	if len(positions) != 1 {
		t.Fatalf("closed %d position(s), want 1", len(positions))
	}
	got := positions[0]
	if got.PositionKey != "HELD" {
		t.Errorf("closed %q, want the key still held at the last pre-handover snapshot", got.PositionKey)
	}
	if got.SnapshotAt != handover {
		t.Errorf("closed at %d, want the handover %d", got.SnapshotAt, handover)
	}
	if got.SilverSourceID != "src" {
		t.Errorf("silver_source_id = %q, want the source being loaded", got.SilverSourceID)
	}
	if got.MarketValue == nil || !got.MarketValue.IsZero() {
		t.Errorf("market_value = %v, want zero — that is what ends the key", got.MarketValue)
	}
	if got.Quantity == nil || !got.Quantity.IsZero() {
		t.Errorf("quantity = %v, want zero", got.Quantity)
	}
	// The payload of the row this was built from would be the one place
	// the zero still stated the holding it ends.
	if strings.Contains(string(got.Payload), "quantity") {
		t.Errorf("payload = %s, want the marker, not the holding it ends", got.Payload)
	}
	if !strings.Contains(string(got.Payload), "supersession_marker") {
		t.Errorf("payload = %s, want the row to say it was synthesised", got.Payload)
	}
}

// TestSupersessionClosingHasNothingToEndWhenTheSourceNeverCarriedIt:
// an entry naming an account no snapshot carries closes nothing. There
// is no series to end, and a zero row would invent the account.
func TestSupersessionClosingHasNothingToEndWhenTheSourceNeverCarriedIt(t *testing.T) {
	closing := newSupersessionClosing(map[string]int64{"ABSENT": 3000})
	positions, balances := closing.rows("src")
	if len(positions) != 0 || len(balances) != 0 {
		t.Errorf("closed %d position(s) and %d balance(s), want none",
			len(positions), len(balances))
	}
}

// TestSupersessionClosingEndsEveryCurrencyItHeld: gold keys a cash
// series on (source, account, currency) and carries each one forward on
// its own, so an account holding two currencies needs two zeros.
// Collecting one row per balance_kind would end whichever currency the
// batch happened to carry last and leave the rest lingering past the
// handover — the double-count the closing write exists to prevent.
func TestSupersessionClosingEndsEveryCurrencyItHeld(t *testing.T) {
	const handover = int64(3000)
	closing := newSupersessionClosing(map[string]int64{"A": handover})

	closing.observe(nil, []canonical.CashBalanceChange{
		{AccountExternalID: "A", SnapshotAt: 2000, Currency: "USD",
			BalanceKind: canonical.BalanceKindCurrent, Amount: canonical.NewDecimalFromInt(1000)},
		{AccountExternalID: "A", SnapshotAt: 2000, Currency: "EUR",
			BalanceKind: canonical.BalanceKindCurrent, Amount: canonical.NewDecimalFromInt(2000)},
		// Same currency, other kind: gold keys on the pair, so this is a
		// third series and not a repeat of the first.
		{AccountExternalID: "A", SnapshotAt: 2000, Currency: "USD",
			BalanceKind: canonical.BalanceKindAvailable, Amount: canonical.NewDecimalFromInt(500)},
	})
	// Past the handover, and another account entirely: neither is ours.
	closing.observe(nil, []canonical.CashBalanceChange{
		{AccountExternalID: "A", SnapshotAt: 4000, Currency: "CHF", BalanceKind: canonical.BalanceKindCurrent},
		{AccountExternalID: "B", SnapshotAt: 2000, Currency: "USD", BalanceKind: canonical.BalanceKindCurrent},
	})

	_, balances := closing.rows("src")
	got := map[balanceKey]canonical.CashBalanceChange{}
	for _, b := range balances {
		got[balanceKey{b.Currency, b.BalanceKind}] = b
	}
	if len(balances) != 3 || len(got) != 3 {
		t.Fatalf("closed %d balance(s) over %d series, want 3 of each — one per (currency, kind) held", len(balances), len(got))
	}
	for key, b := range got {
		if !b.Amount.IsZero() {
			t.Errorf("%v: amount = %v, want zero — that is what ends the series", key, b.Amount)
		}
		if b.SnapshotAt != handover {
			t.Errorf("%v: closed at %d, want the handover %d", key, b.SnapshotAt, handover)
		}
		if b.SilverSourceID != "src" {
			t.Errorf("%v: silver_source_id = %q, want the source being loaded", key, b.SilverSourceID)
		}
		if !strings.Contains(string(b.Payload), "supersession_marker") {
			t.Errorf("%v: payload = %s, want the row to say it was synthesised", key, b.Payload)
		}
	}
}

// TestSupersessionClosingRunsPositionsAndCashOnSeparateClocks: gold
// resolves a positions series and a cash series on their own tables, so
// a source's two series can stop at different snapshots. Reading both
// off one high-water mark would let a cash row dated after the last
// positions snapshot discard every position collected for the account —
// and those positions would then be ended nowhere.
func TestSupersessionClosingRunsPositionsAndCashOnSeparateClocks(t *testing.T) {
	const handover = int64(3000)
	closing := newSupersessionClosing(map[string]int64{"A": handover, "B": handover})

	// A's positions stop at 1000 while its cash runs on to 2000.
	closing.observe(
		[]canonical.PositionChange{{AccountExternalID: "A", SnapshotAt: 1000, PositionKey: "AAAA"}},
		[]canonical.CashBalanceChange{{AccountExternalID: "A", SnapshotAt: 1000,
			Currency: "USD", BalanceKind: canonical.BalanceKindCurrent}})
	closing.observe(nil,
		[]canonical.CashBalanceChange{{AccountExternalID: "A", SnapshotAt: 2000,
			Currency: "USD", BalanceKind: canonical.BalanceKindCurrent}})
	// B is the mirror: its cash stops at 1000 while its positions run on.
	closing.observe(
		[]canonical.PositionChange{{AccountExternalID: "B", SnapshotAt: 1000, PositionKey: "BBBB"}},
		[]canonical.CashBalanceChange{{AccountExternalID: "B", SnapshotAt: 1000,
			Currency: "USD", BalanceKind: canonical.BalanceKindCurrent}})
	closing.observe(
		[]canonical.PositionChange{{AccountExternalID: "B", SnapshotAt: 2000, PositionKey: "BBBB"}}, nil)

	positions, balances := closing.rows("src")
	if len(positions) != 2 {
		t.Errorf("closed %d position(s), want 2 — a later cash row ends no position", len(positions))
	}
	if len(balances) != 2 {
		t.Errorf("closed %d balance(s), want 2 — a later position row ends no balance", len(balances))
	}
}

// TestSupersessionClosingRewritesTheRowsTheLastLoadWrote: the zeros are
// stamped at the handover, a date that comes out of the config and not
// out of silver. A source whose data stops before it reports a change
// window that never covers them, and the delete that opens a load
// clears only the window — so a second load of such a source would
// insert the zeros on top of themselves and abort on the primary key,
// leaving the archive unloadable short of a reset.
func TestSupersessionClosingRewritesTheRowsTheLastLoadWrote(t *testing.T) {
	ctx := context.Background()
	g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatalf("gold.OpenFresh: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	const (
		sourceID   = "stub-source"
		account    = "ACCT0001"
		snapshotAt = int64(1600000000)
		handover   = int64(1700000000)
	)
	spec := SourceSpec{ID: sourceID, Supersession: map[string]int64{account: handover}}
	// The window a source that stopped before the handover reports: it
	// spans that source's own last data and nothing later.
	window := canonical.Window{Start: snapshotAt, End: snapshotAt, HasChanges: true, NewChangeNumber: 1}

	// Twice, in the order a load runs them.
	for i := 1; i <= 2; i++ {
		tx, err := g.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("load %d: begin: %v", i, err)
		}
		if err := deleteWindow(ctx, tx, spec.ID, window); err != nil {
			t.Fatalf("load %d: delete window: %v", i, err)
		}
		if _, err := applySnapshots(ctx, tx, stubSilverAt(account, snapshotAt), window, spec); err != nil {
			_ = tx.Rollback()
			t.Fatalf("load %d: apply snapshots: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("load %d: commit: %v", i, err)
		}
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := g.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	// One zero per series at the handover: the second load replaced what
	// the first wrote rather than adding to it.
	if n := count(`SELECT count(*) FROM positions WHERE silver_source_id = ? AND snapshot_at = ?`,
		sourceID, handover); n != 1 {
		t.Errorf("%d closing position(s) at the handover, want 1", n)
	}
	if n := count(`SELECT count(*) FROM cash_balances WHERE silver_source_id = ? AND snapshot_at = ?`,
		sourceID, handover); n != 1 {
		t.Errorf("%d closing balance(s) at the handover, want 1", n)
	}
	if n := count(`SELECT count(*) FROM positions
	                WHERE silver_source_id = ? AND snapshot_at = ? AND market_value = 0`,
		sourceID, handover); n != 1 {
		t.Errorf("the closing position is not zeroed (%d zero row(s))", n)
	}
	// And the delete reaches only the handover: the real rows stand.
	if n := count(`SELECT count(*) FROM positions WHERE silver_source_id = ? AND snapshot_at = ?`,
		sourceID, snapshotAt); n != 1 {
		t.Errorf("%d position(s) at the last real snapshot, want 1", n)
	}
}

// An incremental window that opens AFTER the handover recomputes no
// closing rows, because it cannot see the pre-handover history they are
// a function of. It must therefore leave the standing zeros alone: a
// delete scoped to the configured accounts rather than to the rows this
// load actually produced would wipe them, and the last pre-handover
// mark would start carrying forward again — the double-count the
// feature exists to prevent, arrived at silently.
func TestSupersessionClosingSurvivesAWindowPastTheHandover(t *testing.T) {
	ctx := context.Background()
	g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatalf("gold.OpenFresh: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	const (
		sourceID = "stub-source"
		account  = "ACCT0001"
		before   = int64(1600000000) // the source's own last data
		handover = int64(1700000000)
		after    = int64(1800000000) // a later dump, past the handover
	)
	spec := SourceSpec{ID: sourceID, Supersession: map[string]int64{account: handover}}

	load := func(at int64, w canonical.Window) {
		t.Helper()
		tx, err := g.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := deleteWindow(ctx, tx, spec.ID, w); err != nil {
			t.Fatalf("delete window: %v", err)
		}
		if _, err := applySnapshots(ctx, tx, stubSilverAt(account, at), w, spec); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply snapshots: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// Load 1 sees the pre-handover history and writes the zeros.
	load(before, canonical.Window{Start: before, End: before, HasChanges: true, NewChangeNumber: 1})
	// Load 2 is a true incremental window, starting strictly after the
	// watermark — so it never carries a row dropSuperseded would keep.
	load(after, canonical.Window{Start: handover + 1, End: after, HasChanges: true, NewChangeNumber: 2})

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := g.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM positions WHERE silver_source_id = ? AND snapshot_at = ?`,
		sourceID, handover); n != 1 {
		t.Errorf("%d closing position(s) at the handover after the later load, want 1", n)
	}
	if n := count(`SELECT count(*) FROM cash_balances WHERE silver_source_id = ? AND snapshot_at = ?`,
		sourceID, handover); n != 1 {
		t.Errorf("%d closing balance(s) at the handover after the later load, want 1", n)
	}
	// And the later dump itself contributed nothing: it is all past the
	// handover, so dropSuperseded discarded it.
	if n := count(`SELECT count(*) FROM positions WHERE silver_source_id = ? AND snapshot_at = ?`,
		sourceID, after); n != 0 {
		t.Errorf("%d position(s) survived past the handover, want 0", n)
	}
}

// stubSilver is a silver connection carrying one snapshot of one
// account — enough for applySnapshots, which is the only caller that
// pulls from it. Status and ChangeWindow belong to the Connection
// contract and are never reached here.
type stubSilver struct{ batch canonical.SnapshotBatch }

// stubSilverAt builds the silver of a source whose data all PRECEDES
// the handover another source takes its account over at: one position
// and one cash balance, both dated before it.
func stubSilverAt(account string, snapshotAt int64) *stubSilver {
	value := canonical.NewDecimalFromInt(5000)
	return &stubSilver{batch: canonical.SnapshotBatch{
		Positions: []canonical.PositionChange{{
			SnapshotAt: snapshotAt, AccountExternalID: account, PositionKey: "AAAA",
			AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock,
			Currency: "USD", Quantity: &value, MarketValue: &value,
		}},
		CashBalances: []canonical.CashBalanceChange{{
			SnapshotAt: snapshotAt, AccountExternalID: account,
			Currency: "USD", BalanceKind: canonical.BalanceKindCurrent,
			Amount: canonical.NewDecimalFromInt(1000),
		}},
	}}
}

func (s *stubSilver) Close() error { return nil }

func (s *stubSilver) Status(context.Context) (canonical.Status, error) {
	return canonical.Status{}, nil
}

func (s *stubSilver) ChangeWindow(context.Context, int64) (canonical.Window, error) {
	return canonical.Window{}, nil
}

func (s *stubSilver) Snapshots(context.Context, canonical.Window) (silver.SnapshotStream, error) {
	return &stubSnapshotStream{batch: s.batch}, nil
}

func (s *stubSilver) Transactions(context.Context, canonical.Window) (silver.TransactionStream, error) {
	return stubTransactionStream{}, nil
}

type stubSnapshotStream struct{ batch canonical.SnapshotBatch }

func (s *stubSnapshotStream) Next(context.Context) (canonical.SnapshotBatch, bool, error) {
	return s.batch, false, nil
}

func (s *stubSnapshotStream) Close() error { return nil }

type stubTransactionStream struct{}

func (stubTransactionStream) Next(context.Context) (canonical.TransactionBatch, bool, error) {
	return canonical.TransactionBatch{}, false, nil
}

func (stubTransactionStream) Close() error { return nil }

// TestSupersededTransfersAreRefusedRatherThanDropped: the adapter's rows
// are cut at the handover because a statement straddling it is expected
// and its later half is the successor's to state. An equity-transfer
// ledger row is the opposite — written by hand, one row per real
// capital flow, and nothing else carries it. Dropping one silently
// would delete money the returns engine would then read as performance
// inside the account, so the load names it instead.
func TestSupersededTransfersAreRefusedRatherThanDropped(t *testing.T) {
	const handover = int64(2000)
	ends := map[string]int64{"ACCT0001": handover}
	// Stands in for gold's resolver: the ledger names an account by id or
	// by nickname, and supersession is keyed on the id either way.
	resolve := func(ref string) (string, error) {
		if ref == "the nickname" {
			return "ACCT0001", nil
		}
		return ref, nil
	}

	for name, e := range map[string]TransferEntry{
		// The handover date itself belongs to the successor, as it does
		// for every other row kind.
		"on the handover":   {Account: "ACCT0001", OccurredAt: handover},
		"past the handover": {Account: "ACCT0001", OccurredAt: handover + 86400},
		"named by nickname": {Account: "the nickname", OccurredAt: handover + 86400},
	} {
		err := checkSupersededTransfers("src", []TransferEntry{e}, ends, resolve)
		if err == nil {
			t.Errorf("%s: loaded, want a refusal naming the row", name)
			continue
		}
		if !strings.Contains(err.Error(), "ACCT0001") {
			t.Errorf("%s: error %q does not name the account", name, err)
		}
	}

	keep := []TransferEntry{
		{Account: "ACCT0001", OccurredAt: handover - 86400}, // this source's half
		{Account: "ACCT0002", OccurredAt: handover + 86400}, // an account nothing supersedes
	}
	if err := checkSupersededTransfers("src", keep, ends, resolve); err != nil {
		t.Errorf("a row before the handover, and one for an unlisted account, must load: %v", err)
	}
}
