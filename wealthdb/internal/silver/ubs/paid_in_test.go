package ubs

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The paid-in basis of a private-markets fund's units, from the capital
// calls ubs-web reads. Every fund, account, amount and day below is
// invented.

const (
	pmFund      = "XX00000PM001" // called, held on one account
	pmShared    = "XX00000PM002" // called, held on two accounts
	pmMixed     = "XX00000PM003" // calls in two currencies
	pmUndated   = "XX00000PM004" // one call states no date
	pmSafe      = "00000000000000S1"
	pmOther     = "00000000000000S2"
	pmPortfolio = "0000000000000001"
	pmDay       = int64(86400)
)

// addAdvices adds collector migration 0013's advices table to a web
// silver and seeds the calls.
func addAdvices(t *testing.T, r *webReader, seed string) {
	t.Helper()
	if _, err := r.db.Exec(`
CREATE TABLE advices (
    source_doc_token TEXT NOT NULL PRIMARY KEY, kind TEXT NOT NULL, title TEXT,
    doc_date INTEGER, trade_date INTEGER, value_date INTEGER,
    instrument_isin TEXT, valor TEXT, security_name TEXT, currency_iso TEXT,
    quantity REAL, price REAL, amount REAL, prepayment REAL,
    placement_fee REAL, stamp_duty REAL, settlement_amount REAL,
    settlement_currency_iso TEXT, fx_rate REAL, fx_rate_pair TEXT,
    payload TEXT NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(seed); err != nil {
		t.Fatal(err)
	}
}

// pmCalls: the fund is called twice, on day 10 and day 20; a contract
// note on the same fund is not a call and adds nothing.
const pmCalls = `
INSERT INTO advices (source_doc_token, kind, value_date, instrument_isin, currency_iso, amount, settlement_amount, payload) VALUES
    ('c1', 'capital_call', 10 * 86400, '` + pmFund + `', 'USD', 100, 101, '{}'),
    ('c2', 'capital_call', 20 * 86400, '` + pmFund + `', 'USD', 50, 50, '{}'),
    ('n1', 'contract_note', 12 * 86400, '` + pmFund + `', 'USD', 999, 999, '{}'),
    ('c3', 'capital_call', 10 * 86400, '` + pmShared + `', 'USD', 100, 100, '{}'),
    ('c4', 'capital_call', 10 * 86400, '` + pmMixed + `', 'USD', 100, 100, '{}'),
    ('c5', 'capital_call', 20 * 86400, '` + pmMixed + `', 'EUR', 100, 100, '{}'),
    ('c6', 'capital_call', 10 * 86400, '` + pmUndated + `', 'USD', 100, 100, '{}'),
    ('c7', 'capital_call', NULL, '` + pmUndated + `', 'USD', 100, 100, '{}');`

// newPaidInPSN is a PSN silver whose roster puts one safekeeping
// account in the portfolio, holding every fund, and a second account
// elsewhere that also holds the shared one.
func newPaidInPSN(t *testing.T) *psnReader {
	t.Helper()
	_, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO safekeeping_accounts (snapshot_at, relationship_id, account_external_id, portfolio_external_id, payload) VALUES
            (1, 'R1', '` + pmSafe + `', '` + pmPortfolio + `', '{"PrtflId":"` + pmPortfolio + `"}');
        INSERT INTO holdings (snapshot_at, relationship_id, safekeeping_external_id, isin, payload) VALUES
            (15 * 86400, 'R1', '` + pmSafe + `', '` + pmFund + `', '{}'),
            (15 * 86400, 'R1', '` + pmSafe + `', '` + pmShared + `', '{}'),
            (15 * 86400, 'R1', '` + pmOther + `', '` + pmShared + `', '{}'),
            (15 * 86400, 'R1', '` + pmSafe + `', '` + pmMixed + `', '{}'),
            (15 * 86400, 'R1', '` + pmSafe + `', '` + pmUndated + `', '{}');`); err != nil {
		t.Fatal(err)
	}
	return &psnReader{db: db}
}

// TestCapitalCallsJoinOnlyAnUnambiguousFund: a fund's calls become a
// series only when every call is dated and in one currency and the
// fund is held on one account across both feeds. The statement era's
// portfolio maps onto the same safekeeping account, so it is no second
// holder.
func TestCapitalCallsJoinOnlyAnUnambiguousFund(t *testing.T) {
	ctx := context.Background()
	web := newWebFixture(t)
	addAdvices(t, web, pmCalls)
	if _, err := web.db.Exec(`
        INSERT INTO historical_position_snapshots
            (as_of_date, portfolio_external_id, account_external_id, instrument_isin,
             currency_iso, units, market_value, market_value_currency, source_doc_token, payload)
        VALUES (12 * 86400, '` + pmPortfolio + `', '', '` + pmFund + `', 'USD', 1, 120, 'USD', 'tok', '{}')`); err != nil {
		t.Fatal(err)
	}
	psn := newPaidInPSN(t)
	byPortfolio, err := psn.safekeepingByPortfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	series, err := web.paidInByISIN(ctx, psn, byPortfolio)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 {
		t.Fatalf("series = %v, want only %s", series, pmFund)
	}
	s, ok := series[pmFund]
	if !ok || s.account != pmSafe || s.currency != "USD" {
		t.Fatalf("series[%s] = %+v, want USD on %s", pmFund, s, pmSafe)
	}
	for at, want := range map[int64]string{5 * pmDay: "", 10 * pmDay: "100", 15 * pmDay: "100", 25 * pmDay: "150"} {
		got := s.at(at)
		switch {
		case want == "" && got != nil:
			t.Errorf("paid in at day %d = %v, want NULL before the first call", at/pmDay, got)
		case want != "" && (got == nil || got.String() != want):
			t.Errorf("paid in at day %d = %v, want %s", at/pmDay, got, want)
		}
	}
}

// TestACallNoticeReadTwiceCountsOnce: silver keys an advice by its
// document, so a notice that arrives twice is two rows, and the call
// still adds once. A call is dated by its value date, whatever trade
// date it prints.
func TestACallNoticeReadTwiceCountsOnce(t *testing.T) {
	ctx := context.Background()
	web := newWebFixture(t)
	addAdvices(t, web, `
INSERT INTO advices (source_doc_token, kind, trade_date, value_date, instrument_isin, currency_iso, amount, payload) VALUES
    ('c1', 'capital_call', NULL, 10 * 86400, '`+pmFund+`', 'USD', 100, '{}'),
    ('c1-again', 'capital_call', NULL, 10 * 86400, '`+pmFund+`', 'USD', 100, '{}'),
    ('c2', 'capital_call', 15 * 86400, 20 * 86400, '`+pmFund+`', 'USD', 50, '{}');`)
	psn := newPaidInPSN(t)
	byPortfolio, err := psn.safekeepingByPortfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	series, err := web.paidInByISIN(ctx, psn, byPortfolio)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := series[pmFund]
	if !ok {
		t.Fatalf("series = %v, want %s", series, pmFund)
	}
	for day, want := range map[int64]string{10: "100", 17: "100", 20: "150"} {
		if got := s.at(day * pmDay); got == nil || got.String() != want {
			t.Errorf("paid in at day %d = %v, want %s", day, got, want)
		}
	}
}

// TestThePaidInBasisReachesOnlyTheJoinedPositions: the stream sets the
// basis on the fund's positions on its account and in its currency, and
// leaves alone a position elsewhere, one in another currency, and one
// whose holding states its own book value.
func TestThePaidInBasisReachesOnlyTheJoinedPositions(t *testing.T) {
	series := map[string]paidInSeries{pmFund: {
		account: pmSafe, currency: "USD",
		dates:  []int64{10 * pmDay, 20 * pmDay},
		totals: []canonical.Decimal{canonical.NewDecimalFromInt(100), canonical.NewDecimalFromInt(150)},
	}}
	stated := canonical.NewDecimalFromInt(7)
	pos := func(at int64, account, currency string) canonical.PositionChange {
		return canonical.PositionChange{SnapshotAt: at, AccountExternalID: account, PositionKey: pmFund, Currency: currency}
	}
	withBook := pos(25*pmDay, pmSafe, "USD")
	withBook.SetBookValue(&stated, statedAverageBasis)
	inner := silver.NewSnapshotStream([]canonical.SnapshotBatch{{Positions: []canonical.PositionChange{
		pos(5*pmDay, pmSafe, "USD"),
		pos(15*pmDay, pmSafe, "USD"),
		pos(25*pmDay, pmSafe, "USD"),
		pos(25*pmDay, pmOther, "USD"),
		pos(25*pmDay, pmSafe, "EUR"),
		withBook,
	}}})
	s := &paidInStream{inner: inner, series: series}
	defer s.Close()
	batch, _, err := s.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		book  string
		basis canonical.Basis
	}{
		{"", canonical.Basis{}},
		{"100", paidInBasis},
		{"150", paidInBasis},
		{"", canonical.Basis{}},
		{"", canonical.Basis{}},
		{"7", statedAverageBasis},
	}
	for i, w := range want {
		p := batch.Positions[i]
		got := ""
		if p.BookValue != nil {
			got = p.BookValue.String()
		}
		if got != w.book || p.Basis != w.basis {
			t.Errorf("position %d: book %q basis %+v, want %q %+v", i, got, p.Basis, w.book, w.basis)
		}
	}
}

// TestAWebOnlySilverStampsTheStatementEra drives the merged stream: with
// no PSN feed the statement's fund rows sit on the portfolio overlay,
// which is then the one holder, and Connection.Snapshots gives them the
// paid-in basis.
func TestAWebOnlySilverStampsTheStatementEra(t *testing.T) {
	ctx := context.Background()
	web := newWebFixture(t)
	addAdvices(t, web, pmCalls)
	// The live roster's tables, empty: the merged stream reads them.
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
INSERT INTO historical_position_snapshots
    (as_of_date, portfolio_external_id, account_external_id, instrument_isin,
     currency_iso, units, market_value, market_value_currency, cost_price, source_doc_token, payload)
VALUES (12 * 86400, '` + pmPortfolio + `', '', '` + pmFund + `', 'USD', 1, 120, 'USD', NULL, 'tok', '{}')`); err != nil {
		t.Fatal(err)
	}
	stream, err := (&Connection{web: web}).Snapshots(ctx, canonical.Window{Start: 0, End: 30 * pmDay, HasChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var positions []canonical.PositionChange
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		positions = append(positions, batch.Positions...)
		if !more {
			break
		}
	}
	if len(positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(positions))
	}
	p := positions[0]
	if p.AccountExternalID != overlayAccountID(pmPortfolio) {
		t.Errorf("account = %q, want the portfolio overlay", p.AccountExternalID)
	}
	if p.BookValue == nil || p.BookValue.String() != "100" || p.Basis != paidInBasis {
		t.Errorf("book %v basis %+v, want 100 paid in", p.BookValue, p.Basis)
	}
}

// TestASilverWithoutAdvicesHasNoPaidInBasis: an older web silver has no
// advices table and so no series.
func TestASilverWithoutAdvicesHasNoPaidInBasis(t *testing.T) {
	series, err := newWebFixture(t).paidInByISIN(context.Background(), nil, nil)
	if err != nil || series != nil {
		t.Errorf("series = %v, err = %v; want none", series, err)
	}
}
