package ubs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// newRatesFixture builds a ubs-web silver holding the rates table
// (collector migration 0016). Every value is invented.
func newRatesFixture(t *testing.T, inserts string) *webReader {
	t.Helper()
	const schema = `
CREATE TABLE account_rates (
    snapshot_at INTEGER NOT NULL, account_external_id TEXT NOT NULL,
    side TEXT NOT NULL, tier INTEGER NOT NULL, rate_pct REAL NOT NULL,
    upper_limit REAL, valid_from TEXT, surface TEXT NOT NULL,
    payload TEXT NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, side, tier));`
	_, db := silvertest.NewDB(t, schema)
	if inserts != "" {
		if _, err := db.Exec(inserts); err != nil {
			t.Fatal(err)
		}
	}
	return &webReader{db: db}
}

func balance(at int64, account string, kind canonical.BalanceKind, payload string) canonical.CashBalanceChange {
	return canonical.CashBalanceChange{SnapshotAt: at, AccountExternalID: account, Currency: "CHF",
		BalanceKind: kind, Amount: canonical.NewDecimalFromInt(1000), Payload: json.RawMessage(payload)}
}

// stamped runs the batches through the stream the adapter wraps its
// snapshots in and returns each balance's payload keys by (instant,
// account, kind).
func stamped(t *testing.T, r *webReader, batches []canonical.SnapshotBatch) map[string]map[string]any {
	t.Helper()
	rates, err := r.creditRates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := &rateStampStream{inner: silver.NewSnapshotStream(batches), rates: rates}
	out := map[string]map[string]any{}
	for {
		b, more, err := s.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, cb := range b.CashBalances {
			var m map[string]any
			if err := json.Unmarshal(cb.Payload, &m); err != nil {
				t.Fatal(err)
			}
			out[jsonKey(cb.SnapshotAt, cb.AccountExternalID, string(cb.BalanceKind))] = m
		}
		if !more {
			return out
		}
	}
}

func jsonKey(at int64, account, kind string) string {
	b, _ := json.Marshal([]any{at, account, kind})
	return string(b)
}

func hasRateKeys(m map[string]any) bool {
	for _, k := range []string{canonical.PayloadInterestRate, canonical.PayloadInterestTiers,
		canonical.PayloadInterestValidFrom, canonical.PayloadInterestSide} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

func TestATieredCreditRateLandsOnTheNewestBalanceBeforeTheCapture(t *testing.T) {
	r := newRatesFixture(t, `
INSERT INTO account_rates VALUES
 (250, 'ACCT-T', 'credit', 1, 0.5, 25000, '2026-02-01', 'api', '{}'),
 (250, 'ACCT-T', 'credit', 2, 0.25, 100000, '2026-02-01', 'api', '{}'),
 (250, 'ACCT-T', 'credit', 3, 0, NULL, '2026-02-01', 'api', '{}'),
 (250, 'ACCT-T', 'debit', 1, 9.75, NULL, '2025-07-01', 'api', '{}')`)
	got := stamped(t, r, []canonical.SnapshotBatch{
		{CashBalances: []canonical.CashBalanceChange{balance(100, "ACCT-T", canonical.BalanceKindClosing, `{"note":"a"}`)}},
		{CashBalances: []canonical.CashBalanceChange{
			balance(200, "ACCT-T", canonical.BalanceKindClosing, `{"note":"b"}`),
			balance(200, "ACCT-T", canonical.BalanceKindAvailable, `{}`),
		}},
		{CashBalances: []canonical.CashBalanceChange{balance(300, "ACCT-T", canonical.BalanceKindClosing, `{}`)}},
	})
	for _, k := range []string{jsonKey(100, "ACCT-T", "closing"), jsonKey(300, "ACCT-T", "closing")} {
		if hasRateKeys(got[k]) {
			t.Errorf("%s carries rate keys: %v", k, got[k])
		}
	}
	for _, kind := range []string{"closing", "available"} {
		m := got[jsonKey(200, "ACCT-T", kind)]
		tiers, _ := json.Marshal(m[canonical.PayloadInterestTiers])
		if string(tiers) != `[{"rate":0.5,"up_to":25000},{"rate":0.25,"up_to":100000},{"rate":0,"up_to":null}]` {
			t.Errorf("%s tiers %s", kind, tiers)
		}
		if _, ok := m[canonical.PayloadInterestRate]; ok {
			t.Errorf("%s states a flat rate beside its tiers", kind)
		}
		if _, ok := m[canonical.PayloadInterestConditions]; ok {
			t.Errorf("%s states one day's tiers as several conditions", kind)
		}
		if m[canonical.PayloadInterestValidFrom] != "2026-02-01" || m[canonical.PayloadInterestSide] != "credit" {
			t.Errorf("%s keys %v", kind, m)
		}
	}
	if got[jsonKey(200, "ACCT-T", "closing")]["note"] != "b" {
		t.Errorf("the balance's own payload was not kept: %v", got[jsonKey(200, "ACCT-T", "closing")])
	}
}

// stampedPayload stamps one balance at the instant 300 with the fixture's
// rates and returns its raw payload.
func stampedPayload(t *testing.T, r *webReader, account string) []byte {
	t.Helper()
	rates, err := r.creditRates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	batches := []canonical.SnapshotBatch{{CashBalances: []canonical.CashBalanceChange{
		balance(300, account, canonical.BalanceKindClosing, `{}`)}}}
	if err := stampRates(batches, rates); err != nil {
		t.Fatal(err)
	}
	return batches[0].CashBalances[0].Payload
}

func TestFlatRowsOfTwoDaysAreTwoConditions(t *testing.T) {
	r := newRatesFixture(t, `
INSERT INTO account_rates VALUES
 (300, 'ACCT-2', 'credit', 1, 0.5, NULL, '2026-01-01', 'api', '{}'),
 (300, 'ACCT-2', 'credit', 2, 0.25, NULL, '2026-04-01', 'api', '{}')`)
	payload := stampedPayload(t, r, "ACCT-2")
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatal(err)
	}
	if hasRateKeys(m) {
		t.Errorf("top-level keys beside the conditions: %s", payload)
	}
	conds, _ := json.Marshal(m[canonical.PayloadInterestConditions])
	if string(conds) != `[{"interest_rate":0.5,"interest_side":"credit","interest_valid_from":"2026-01-01"},`+
		`{"interest_rate":0.25,"interest_side":"credit","interest_valid_from":"2026-04-01"}]` {
		t.Errorf("conditions %s", conds)
	}
	got := canonical.ReadStatedRates(payload)
	if len(got) != 2 || got[0].Tiers[0].Pct != 0.5 || got[1].Tiers[0].Pct != 0.25 {
		t.Errorf("read %+v", got)
	}
}

func TestAFutureDatedRowStaysBesideTodaysRate(t *testing.T) {
	// A capture that states today's tiers and an announced flat rate from
	// a later day states both; each holds from its own day, so today's
	// stays in force until the later one starts.
	r := newRatesFixture(t, `
INSERT INTO account_rates VALUES
 (300, 'ACCT-N', 'credit', 1, 0.5, 25000, '2026-01-01', 'api', '{}'),
 (300, 'ACCT-N', 'credit', 2, 0.1, NULL, '2026-01-01', 'api', '{}'),
 (300, 'ACCT-N', 'credit', 3, 0.2, NULL, '2026-12-01', 'api', '{}')`)
	got := canonical.ReadStatedRates(stampedPayload(t, r, "ACCT-N"))
	if len(got) != 2 {
		t.Fatalf("read %d conditions: %+v", len(got), got)
	}
	today, later := got[0], got[1]
	if today.ValidFrom == nil || today.ValidFrom.Format("2006-01-02") != "2026-01-01" ||
		today.Side != canonical.RateSideCredit || len(today.Tiers) != 2 ||
		today.Tiers[0].Pct != 0.5 || today.Tiers[0].UpTo == nil || *today.Tiers[0].UpTo != 25000 ||
		today.Tiers[1].Pct != 0.1 || today.Tiers[1].UpTo != nil {
		t.Errorf("today's condition %+v", today)
	}
	if later.ValidFrom == nil || later.ValidFrom.Format("2006-01-02") != "2026-12-01" ||
		len(later.Tiers) != 1 || later.Tiers[0].Pct != 0.2 {
		t.Errorf("the later condition %+v", later)
	}
}

func TestAFlatCreditRateLandsOnTheBalanceAtTheCapture(t *testing.T) {
	r := newRatesFixture(t, `
INSERT INTO account_rates VALUES
 (300, 'ACCT-F', 'credit', 1, 0.75, NULL, '2026-03-01', 'legacy', '{}')`)
	got := stamped(t, r, []canonical.SnapshotBatch{
		{CashBalances: []canonical.CashBalanceChange{balance(200, "ACCT-F", canonical.BalanceKindClosing, `{}`)}},
		{CashBalances: []canonical.CashBalanceChange{balance(300, "ACCT-F", canonical.BalanceKindClosing, `{}`)}},
	})
	if hasRateKeys(got[jsonKey(200, "ACCT-F", "closing")]) {
		t.Errorf("the earlier balance carries rate keys")
	}
	m := got[jsonKey(300, "ACCT-F", "closing")]
	if m[canonical.PayloadInterestRate] != 0.75 || m[canonical.PayloadInterestValidFrom] != "2026-03-01" ||
		m[canonical.PayloadInterestSide] != "credit" {
		t.Errorf("keys %v", m)
	}
	if _, ok := m[canonical.PayloadInterestTiers]; ok {
		t.Errorf("a flat rate states tiers: %v", m)
	}
}

func TestAnAccountWithNoCreditRowGetsNoKeys(t *testing.T) {
	// A debit condition alone is not emitted, and an account with no row
	// at all reads as no rate, never as zero.
	r := newRatesFixture(t, `
INSERT INTO account_rates VALUES
 (300, 'ACCT-D', 'debit', 1, 9.75, NULL, '2025-07-01', 'api', '{}')`)
	got := stamped(t, r, []canonical.SnapshotBatch{
		{CashBalances: []canonical.CashBalanceChange{
			balance(300, "ACCT-D", canonical.BalanceKindClosing, `{}`),
			balance(300, "ACCT-NONE", canonical.BalanceKindClosing, `{}`),
		}},
	})
	for k, m := range got {
		if hasRateKeys(m) {
			t.Errorf("%s carries rate keys: %v", k, m)
		}
	}
}

func TestACaptureBeforeEveryBalanceLandsNowhere(t *testing.T) {
	r := newRatesFixture(t, `
INSERT INTO account_rates VALUES
 (50, 'ACCT-E', 'credit', 1, 0.75, NULL, '2026-03-01', 'api', '{}')`)
	got := stamped(t, r, []canonical.SnapshotBatch{
		{CashBalances: []canonical.CashBalanceChange{balance(100, "ACCT-E", canonical.BalanceKindClosing, `{}`)}},
	})
	if hasRateKeys(got[jsonKey(100, "ACCT-E", "closing")]) {
		t.Errorf("a later balance carries an earlier capture's keys")
	}
}

func TestALaterCaptureOnTheSameBalanceWins(t *testing.T) {
	r := newRatesFixture(t, `
INSERT INTO account_rates VALUES
 (210, 'ACCT-L', 'credit', 1, 0.5, NULL, '2026-01-01', 'api', '{}'),
 (220, 'ACCT-L', 'credit', 1, 0.4, NULL, '2026-04-01', 'api', '{}')`)
	got := stamped(t, r, []canonical.SnapshotBatch{
		{CashBalances: []canonical.CashBalanceChange{balance(200, "ACCT-L", canonical.BalanceKindClosing, `{}`)}},
	})
	if m := got[jsonKey(200, "ACCT-L", "closing")]; m[canonical.PayloadInterestRate] != 0.4 {
		t.Errorf("keys %v", m)
	}
}

func TestASilverWithoutTheRatesTableReadsNone(t *testing.T) {
	_, db := silvertest.NewDB(t, `CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY, run_dir TEXT);`)
	rates, err := (&webReader{db: db}).creditRates(context.Background())
	if err != nil || len(rates) != 0 {
		t.Fatalf("rates %v, err %v", rates, err)
	}
}

// TestTheMergedStreamCarriesTheRate drives Connection.Snapshots on a
// web-only silver: the statement's closing balance before the capture is
// the newest the account has, and it carries the capture's rate.
func TestTheMergedStreamCarriesTheRate(t *testing.T) {
	ctx := context.Background()
	const day = int64(86400)
	web := newWebFixture(t)
	if _, err := web.db.Exec(`
CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY, run_dir TEXT);
CREATE TABLE portfolios (
    snapshot_at INTEGER, portfolio_external_id TEXT, banking_relationship_id TEXT,
    description TEXT, payload TEXT);
CREATE TABLE accounts (
    snapshot_at INTEGER, account_external_id TEXT, kind TEXT, currency_iso TEXT,
    banking_relationship_id TEXT, portfolio_external_id TEXT, description TEXT, payload TEXT);
CREATE TABLE positions (
    snapshot_at INTEGER, instrument_isin TEXT, currency_iso TEXT, description TEXT);
CREATE TABLE account_rates (
    snapshot_at INTEGER, account_external_id TEXT, side TEXT, tier INTEGER,
    rate_pct REAL, upper_limit REAL, valid_from TEXT, surface TEXT, payload TEXT);
INSERT INTO historical_cash_balances
    (period_end, account_external_id, currency_iso, period_start, opening_balance,
     closing_balance, source_doc_token, payload)
VALUES (30 * 86400, 'ACCT-M', 'CHF', 1 * 86400, 900, 1000, 'tok', '{"page":1}');
INSERT INTO account_rates VALUES
 (40 * 86400, 'ACCT-M', 'credit', 1, 0.75, NULL, '2026-01-01', 'api', '{}')`); err != nil {
		t.Fatal(err)
	}
	stream, err := (&Connection{web: web}).Snapshots(ctx, canonical.Window{Start: 0, End: 50 * day, HasChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := map[canonical.BalanceKind]map[string]any{}
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, cb := range batch.CashBalances {
			var m map[string]any
			if err := json.Unmarshal(cb.Payload, &m); err != nil {
				t.Fatal(err)
			}
			got[cb.BalanceKind] = m
		}
		if !more {
			break
		}
	}
	if m := got[canonical.BalanceKindClosing]; m[canonical.PayloadInterestRate] != 0.75 || m["page"] != 1.0 {
		t.Errorf("closing payload %v", m)
	}
	if hasRateKeys(got[canonical.BalanceKindOpening]) {
		t.Errorf("the older opening balance carries rate keys: %v", got[canonical.BalanceKindOpening])
	}
}

func TestARateDayThatIsNoDateFailsTheRead(t *testing.T) {
	r := newRatesFixture(t, `
INSERT INTO account_rates VALUES
 (300, 'ACCT-D', 'credit', 1, 0.75, NULL, '01.03.2026', 'legacy', '{}')`)
	if _, err := r.creditRates(context.Background()); err == nil {
		t.Error("a valid_from that is no date: want an error")
	}
}
