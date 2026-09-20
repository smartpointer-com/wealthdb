package ubs

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// The portfolio transaction list: the third rail a managed portfolio's
// trades reach gold on. These tests pin what it books, where it books
// it, and what it declines to book because another rail already did.
//
// Every id, account, portfolio, ISIN, valor, figure and date below is
// invented: IBAN-spec placeholder letters, ISINs in the XX
// reserved-looking range, and days in a decade the source cannot have
// booked in.

const (
	ptxSafe      = "00000000000000S9"
	ptxPortfolio = "0000xxxxxxxx0001"
	ptxCashUSD   = "CH00PTXUSD0000000001"
	ptxCashHKD   = "CH00PTXHKD0000000001"
	ptxISIN      = "XX0000000001"
	// 2099-01-01 and 2099-01-03: the list dates a row by the day the
	// cash settles, which is the day the other rails book it on too.
	ptxTradeDay  int64 = 4070908800
	ptxSettleDay int64 = 4071081600
)

// newPortfolioTxFixture builds a ubs-web silver carrying the cash
// tables the transaction stream reads plus the portfolio transaction
// list (collector migration 0012).
func newPortfolioTxFixture(t *testing.T) *webReader {
	t.Helper()
	r := newWebTxFixture(t)
	if _, err := r.db.Exec(`
CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY);
CREATE TABLE portfolio_transactions (
    transaction_external_id TEXT NOT NULL,
    safekeeping_account_external_id TEXT NOT NULL,
    portfolio_external_id TEXT NOT NULL,
    snapshot_at INTEGER NOT NULL, trade_date INTEGER, booking_date INTEGER,
    value_date INTEGER NOT NULL, booking_type TEXT NOT NULL,
    security_name TEXT, valor TEXT, isin TEXT, quantity REAL,
    settlement_currency_iso TEXT, trans_price REAL, exchange_rate REAL,
    valuation_currency_iso TEXT, trans_value REAL,
    accrued_interest REAL, realized_pl REAL, order_no TEXT,
    external_reference TEXT, asset_class TEXT, sub_asset_class TEXT,
    instrument_category TEXT, payload TEXT NOT NULL,
    PRIMARY KEY (transaction_external_id, safekeeping_account_external_id));`,
	); err != nil {
		t.Fatalf("portfolio_transactions schema: %v", err)
	}
	return r
}

// ptxRow is one row of the list, in the terms the export states it:
// signed the export's way (positive leaves the portfolio), valued in
// the portfolio's reporting currency, settling in another.
type ptxRow struct {
	id            string
	bookingType   string
	valueDate     int64
	quantity      any
	settlementCcy any
	price         any
	rate          any
	valuationCcy  any
	value         any
	isin          any
	valor         any
}

func seedPortfolioTx(t *testing.T, r *webReader, row ptxRow) {
	t.Helper()
	if row.valueDate == 0 {
		row.valueDate = ptxSettleDay
	}
	if _, err := r.db.Exec(`
        INSERT INTO portfolio_transactions (
            transaction_external_id, safekeeping_account_external_id,
            portfolio_external_id, snapshot_at, trade_date, value_date,
            booking_type, security_name, valor, isin, quantity,
            settlement_currency_iso, trans_price, exchange_rate,
            valuation_currency_iso, trans_value, payload)
        VALUES (?, ?, ?, 1000, ?, ?, ?, 'Fake Equity No1', ?, ?, ?, ?, ?, ?, ?, ?, '{}')`,
		row.id, ptxSafe, ptxPortfolio, ptxTradeDay, row.valueDate,
		row.bookingType, row.valor, row.isin, row.quantity,
		row.settlementCcy, row.price, row.rate,
		row.valuationCcy, row.value); err != nil {
		t.Fatalf("seed portfolio tx %s: %v", row.id, err)
	}
}

// ptxTrade is the ordinary case: a USD purchase inside a USD-reporting
// mandate, 100 units at 25, nothing else to resolve.
func ptxTrade(id string) ptxRow {
	return ptxRow{
		id: id, bookingType: "Stock Market Spot Purchase",
		quantity: 100.0, settlementCcy: "USD", price: 25.0,
		valuationCcy: "USD", value: 2500.0, isin: ptxISIN,
	}
}

// newPortfolioPSN builds a ubs-psn silver holding the account master
// data that names the cash account a trade settles on.
func newPortfolioPSN(t *testing.T) *sql.DB {
	t.Helper()
	_, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO safekeeping_accounts (snapshot_at, relationship_id,
            account_external_id, portfolio_external_id, payload)
        VALUES (1, 'R1', ?, ?, '{}')`, ptxSafe, ptxPortfolio); err != nil {
		t.Fatalf("seed safekeeping account: %v", err)
	}
	for _, a := range []struct{ account, currency string }{
		{ptxCashUSD, "USD"}, {ptxCashHKD, "HKD"},
	} {
		if _, err := db.Exec(`
            INSERT INTO cash_accounts (snapshot_at, relationship_id,
                account_external_id, portfolio_external_id, payload)
            VALUES (1, 'R1', ?, ?, json_object('AcctCcyIsoCd', ?))`,
			a.account, ptxPortfolio, a.currency); err != nil {
			t.Fatalf("seed cash account %s: %v", a.currency, err)
		}
	}
	return db
}

// emitPortfolio runs the pass over the full window and keys the result
// by transaction id.
func emitPortfolio(t *testing.T, r *webReader, psnDB *sql.DB, settled map[settledDayKey]int) map[string]canonical.TransactionChange {
	t.Helper()
	var psn *psnReader
	if psnDB != nil {
		psn = &psnReader{db: psnDB}
	}
	batch, err := r.portfolioTransactions(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, psn, settled)
	if err != nil {
		t.Fatalf("portfolioTransactions: %v", err)
	}
	out := map[string]canonical.TransactionChange{}
	for _, tx := range batch.Transactions {
		out[tx.TransactionExternalID] = tx
	}
	return out
}

// ----------------------------------------------------------------
// Where a trade books, and for how much
// ----------------------------------------------------------------

func TestPortfolioTradeBooksOnTheSettlingCashAccount(t *testing.T) {
	r := newPortfolioTxFixture(t)
	seedPortfolioTx(t, r, ptxTrade("ptx:REF1"))
	rows := emitPortfolio(t, r, newPortfolioPSN(t), nil)

	tx, ok := rows["ptx:REF1@"+ptxSafe]
	if !ok {
		t.Fatalf("no row emitted, got %v", rows)
	}
	// The custody account the securities moved in is not an account any
	// cash reconciliation knows; the portfolio's cash account is.
	if tx.AccountExternalID != ptxCashUSD {
		t.Errorf("account = %q, want the portfolio's USD cash account", tx.AccountExternalID)
	}
	if tx.Kind != canonical.TxKindBuy {
		t.Errorf("kind = %q, want buy", tx.Kind)
	}
	if tx.Currency != "USD" {
		t.Errorf("currency = %q, want USD", tx.Currency)
	}
	if tx.NetAmount == nil || tx.NetAmount.String() != "-2500" {
		t.Errorf("net = %v, want -2500 (cash leaving)", tx.NetAmount)
	}
	if tx.OccurredAt != ptxSettleDay {
		t.Errorf("occurred_at = %d, want the settlement day %d", tx.OccurredAt, ptxSettleDay)
	}
	if tx.InstrumentExternalID == nil || *tx.InstrumentExternalID != ptxISIN {
		t.Errorf("instrument = %v, want %s", tx.InstrumentExternalID, ptxISIN)
	}
	if tx.ProviderCategory == nil || *tx.ProviderCategory != "Stock Market Spot Purchase" {
		t.Errorf("provider category = %v, want the bank's booking type", tx.ProviderCategory)
	}
	// The sibling feed states a quantity unsigned and lets the side say
	// which way it went; two rails disagreeing on that sign would read
	// as two different trades.
	if tx.Quantity == nil || tx.Quantity.String() != "100" {
		t.Errorf("quantity = %v, want 100 unsigned", tx.Quantity)
	}
}

func TestPortfolioDisposalBooksCashArriving(t *testing.T) {
	r := newPortfolioTxFixture(t)
	row := ptxTrade("ptx:REF1")
	row.bookingType = "Stock Market Spot Sale"
	row.quantity, row.value = -100.0, -2500.0
	seedPortfolioTx(t, r, row)

	tx := emitPortfolio(t, r, newPortfolioPSN(t), nil)["ptx:REF1@"+ptxSafe]
	if tx.Kind != canonical.TxKindSell {
		t.Errorf("kind = %q, want sell", tx.Kind)
	}
	if tx.NetAmount == nil || tx.NetAmount.String() != "2500" {
		t.Errorf("net = %v, want 2500 (cash arriving)", tx.NetAmount)
	}
}

// A booking type the vocabulary has never seen still books, on the side
// its own figure states. Losing a cash movement because the bank coined
// a word is the failure this rail exists to fix.
func TestPortfolioUnknownBookingTypeStillSettles(t *testing.T) {
	r := newPortfolioTxFixture(t)
	row := ptxTrade("ptx:REF1")
	row.bookingType = "Some Booking Type Invented Next Year"
	seedPortfolioTx(t, r, row)

	tx := emitPortfolio(t, r, newPortfolioPSN(t), nil)["ptx:REF1@"+ptxSafe]
	if tx.Kind != canonical.TxKindBuy {
		t.Errorf("kind = %q, want buy from the figure's sign", tx.Kind)
	}
}

func TestPortfolioForeignSettlementConvertsToTheCashCurrency(t *testing.T) {
	r := newPortfolioTxFixture(t)
	// 4'000 units at 10 HKD is 40'000 HKD; the mandate reports in USD
	// at 0.125, so the list states 5'000. The cash left the HKD
	// account, in HKD.
	seedPortfolioTx(t, r, ptxRow{
		id: "ptx:REF1", bookingType: "Stock Market Spot Purchase",
		quantity: 4000.0, settlementCcy: "HKD", price: 10.0, rate: 0.125,
		valuationCcy: "USD", value: 5000.0, isin: ptxISIN,
	})

	tx := emitPortfolio(t, r, newPortfolioPSN(t), nil)["ptx:REF1@"+ptxSafe]
	if tx.AccountExternalID != ptxCashHKD {
		t.Errorf("account = %q, want the HKD cash account", tx.AccountExternalID)
	}
	if tx.Currency != "HKD" {
		t.Errorf("currency = %q, want HKD", tx.Currency)
	}
	if tx.NetAmount == nil || tx.NetAmount.String() != "-40000" {
		t.Errorf("net = %v, want -40000 HKD", tx.NetAmount)
	}
}

func TestPortfolioForeignSettlementWithoutARateIsRefused(t *testing.T) {
	r := newPortfolioTxFixture(t)
	row := ptxRow{
		id: "ptx:REF1", bookingType: "Stock Market Spot Purchase",
		quantity: 4000.0, settlementCcy: "HKD", price: 10.0,
		valuationCcy: "USD", value: 5000.0, isin: ptxISIN,
	}
	seedPortfolioTx(t, r, row)
	// Booking 5'000 as HKD would be a plausible number in the wrong
	// money, which reads as a closed gap and is not one.
	if rows := emitPortfolio(t, r, newPortfolioPSN(t), nil); len(rows) != 0 {
		t.Errorf("emitted %d row(s), want none", len(rows))
	}
}

// A row stating no settlement currency settled in the one it was valued
// in — the portfolio's own, and the only other currency it names.
func TestPortfolioRowWithoutASettlementCurrencyUsesTheValuationOne(t *testing.T) {
	r := newPortfolioTxFixture(t)
	seedPortfolioTx(t, r, ptxRow{
		id: "ptx:REF1", bookingType: "Reduction",
		quantity: -250.0, price: 100.0, valuationCcy: "USD",
		value: -250.0, valor: "10000001",
	})

	tx := emitPortfolio(t, r, newPortfolioPSN(t), nil)["ptx:REF1@"+ptxSafe]
	if tx.AccountExternalID != ptxCashUSD || tx.Currency != "USD" {
		t.Errorf("booked on %q/%q, want the USD cash account", tx.AccountExternalID, tx.Currency)
	}
}

// ----------------------------------------------------------------
// What states no cash leg
// ----------------------------------------------------------------

func TestPortfolioRowsWithoutAValueSettleNothing(t *testing.T) {
	r := newPortfolioTxFixture(t)
	// A corporate action: securities moved, no money.
	seedPortfolioTx(t, r, ptxRow{
		id: "ptx:CA", bookingType: "Incoming Rights",
		quantity: 25.0, valuationCcy: "USD", isin: ptxISIN,
	})
	// A currency conversion: the list states the PAIR in one cell, so
	// the parser leaves the figure empty rather than book one leg's
	// amount under the other leg's currency.
	seedPortfolioTx(t, r, ptxRow{
		id: "ptx:FX", bookingType: "Purchase FX Spot", price: 0.5,
	})

	if rows := emitPortfolio(t, r, newPortfolioPSN(t), nil); len(rows) != 0 {
		t.Errorf("emitted %d row(s), want none", len(rows))
	}
}

func TestPortfolioFreeOfPaymentTransferSettlesNothing(t *testing.T) {
	r := newPortfolioTxFixture(t)
	// Valued, because the securities are worth something — and paid
	// for by nobody. Booking the value as cash would invent a payment
	// on the delivering and the receiving account at once.
	for _, bt := range []string{
		"Custody account transfer delivery without payment",
		"Custody account transfer receipt without payment",
	} {
		row := ptxTrade("ptx:" + bt)
		row.bookingType = bt
		seedPortfolioTx(t, r, row)
	}

	if rows := emitPortfolio(t, r, newPortfolioPSN(t), nil); len(rows) != 0 {
		t.Errorf("emitted %d row(s), want none", len(rows))
	}
}

// ----------------------------------------------------------------
// The fold against the rails that already carry the booking
// ----------------------------------------------------------------

func TestPortfolioTradeTheConfirmationSettlesIsFolded(t *testing.T) {
	r := newPortfolioTxFixture(t)
	seedPortfolioTx(t, r, ptxTrade("ptx:REF1"))
	psnDB := newPortfolioPSN(t)
	// The sibling feed's confirmation of the same trade: it dates the
	// trade to its execution and the cash to the settlement day, which
	// is the day the list states.
	if _, err := psnDB.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES ('mt515:1', ?, 'R1', ?, 'trade_confirmation', 'USD',
                json_object('side', 'BUY', 'isin', ?, 'net_amount', '2503.75',
                            'net_currency', 'USD', 'quantity', 100,
                            'cash_account_external_id', ?,
                            'settlement_date_unix', ?))`,
		ptxTradeDay, ptxSafe, ptxISIN, ptxCashUSD, ptxSettleDay); err != nil {
		t.Fatal(err)
	}

	if rows := emitPortfolio(t, r, psnDB, nil); len(rows) != 0 {
		t.Errorf("emitted %d row(s), want none — the confirmation carries it", len(rows))
	}
}

func TestPortfolioTradeTheCashLedgerAlreadyHoldsIsFolded(t *testing.T) {
	r := newPortfolioTxFixture(t)
	seedPortfolioTx(t, r, ptxTrade("ptx:REF1"))
	settled := map[settledDayKey]int{
		newSettledDayKey(ptxCashUSD, "USD", ptxSettleDay): 1,
	}
	if rows := emitPortfolio(t, r, newPortfolioPSN(t), settled); len(rows) != 0 {
		t.Errorf("emitted %d row(s), want none — the cash pass carries it", len(rows))
	}
}

// The fold COUNTS. A day the other rails cover partly is the common
// case where a statement was published for some of a year's trades and
// not others; a set-valued fold would drop the surplus with them.
func TestPortfolioFoldKeepsTheTradesTheOtherRailDidNotCarry(t *testing.T) {
	r := newPortfolioTxFixture(t)
	for _, id := range []string{"ptx:REF1", "ptx:REF2", "ptx:REF3"} {
		seedPortfolioTx(t, r, ptxTrade(id))
	}
	settled := map[settledDayKey]int{
		newSettledDayKey(ptxCashUSD, "USD", ptxSettleDay): 1,
	}
	if rows := emitPortfolio(t, r, newPortfolioPSN(t), settled); len(rows) != 2 {
		t.Errorf("emitted %d row(s), want 2 — one of three was already held", len(rows))
	}
}

// A settlement on another day is not this day's booking, however alike.
func TestPortfolioFoldDoesNotReachAcrossDays(t *testing.T) {
	r := newPortfolioTxFixture(t)
	seedPortfolioTx(t, r, ptxTrade("ptx:REF1"))
	settled := map[settledDayKey]int{
		newSettledDayKey(ptxCashUSD, "USD", ptxSettleDay-86400): 1,
	}
	if rows := emitPortfolio(t, r, newPortfolioPSN(t), settled); len(rows) != 1 {
		t.Errorf("emitted %d row(s), want 1", len(rows))
	}
}

// The MT940 line for a trade's cash leg is folded away by the feed's
// own settlement fold in favour of the confirmation. Counting both
// would fold two of this list's rows for one of the bank's bookings.
func TestPortfolioFoldCountsOneSettlementPerConfirmedTrade(t *testing.T) {
	r := newPortfolioTxFixture(t)
	seedPortfolioTx(t, r, ptxTrade("ptx:REF1"))
	seedPortfolioTx(t, r, ptxTrade("ptx:REF2"))
	psnDB := newPortfolioPSN(t)
	if _, err := psnDB.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES ('mt515:1', ?, 'R1', ?, 'trade_confirmation', 'USD',
                json_object('side', 'BUY', 'isin', ?, 'net_amount', '2500',
                            'net_currency', 'USD', 'quantity', 100,
                            'cash_account_external_id', ?,
                            'settlement_date_unix', ?)),
               ('mt940:1', ?, 'R1', ?, 'cash_movement', 'USD',
                json_object('amount', '2500', 'credit_debit', 'D',
                            'narrative', 'B00?', 'account', ?,
                            'funds', 'USD', 'txn_type', 'NSEC'))`,
		ptxTradeDay, ptxSafe, ptxISIN, ptxCashUSD, ptxSettleDay,
		ptxSettleDay, ptxCashUSD, ptxCashUSD); err != nil {
		t.Fatal(err)
	}

	if rows := emitPortfolio(t, r, psnDB, nil); len(rows) != 1 {
		t.Errorf("emitted %d row(s), want 1 — the two feed rows are one booking", len(rows))
	}
}

// ----------------------------------------------------------------
// What the pass declines to guess
// ----------------------------------------------------------------

func TestPortfolioAmbiguousCashAccountEmitsNothing(t *testing.T) {
	r := newPortfolioTxFixture(t)
	seedPortfolioTx(t, r, ptxTrade("ptx:REF1"))
	psnDB := newPortfolioPSN(t)
	// A dormant USD account beside the live one. Settling against the
	// wrong one would move a balance that never moved.
	if _, err := psnDB.Exec(`
        INSERT INTO cash_accounts (snapshot_at, relationship_id,
            account_external_id, portfolio_external_id, payload)
        VALUES (1, 'R1', 'CH00PTXUSD0000000002', ?,
                json_object('AcctCcyIsoCd', 'USD'))`, ptxPortfolio); err != nil {
		t.Fatal(err)
	}

	if rows := emitPortfolio(t, r, psnDB, nil); len(rows) != 0 {
		t.Errorf("emitted %d row(s), want none", len(rows))
	}
}

func TestPortfolioWithoutTheFeedsMasterDataEmitsNothing(t *testing.T) {
	r := newPortfolioTxFixture(t)
	seedPortfolioTx(t, r, ptxTrade("ptx:REF1"))
	if rows := emitPortfolio(t, r, nil, nil); len(rows) != 0 {
		t.Errorf("emitted %d row(s), want none", len(rows))
	}
}

func TestPortfolioSilverWithoutTheListLoadsUnchanged(t *testing.T) {
	r := newWebTxFixture(t)
	if rows := emitPortfolio(t, r, newPortfolioPSN(t), nil); len(rows) != 0 {
		t.Errorf("emitted %d row(s), want none", len(rows))
	}
}

// ----------------------------------------------------------------
// The instrument, and the price
// ----------------------------------------------------------------

func TestPortfolioValorResolvesTheInstrumentWhereNoISINIsStated(t *testing.T) {
	r := newPortfolioTxFixture(t)
	row := ptxTrade("ptx:REF1")
	row.isin, row.valor = nil, "10000001"
	seedPortfolioTx(t, r, row)
	psnDB := newPortfolioPSN(t)
	if _, err := psnDB.Exec(`
        INSERT INTO instruments (snapshot_at, relationship_id, isin, payload)
        VALUES (1, 'R1', ?, json_object('InstrIdtfr', json_object('Valor', '10000001')))`,
		ptxISIN); err != nil {
		t.Fatal(err)
	}

	tx := emitPortfolio(t, r, psnDB, nil)["ptx:REF1@"+ptxSafe]
	if tx.InstrumentExternalID == nil || *tx.InstrumentExternalID != ptxISIN {
		t.Errorf("instrument = %v, want %s resolved from the valor", tx.InstrumentExternalID, ptxISIN)
	}
}

func TestPortfolioUnresolvedValorBecomesTheHint(t *testing.T) {
	r := newPortfolioTxFixture(t)
	row := ptxTrade("ptx:REF1")
	row.isin, row.valor = nil, "10000001"
	seedPortfolioTx(t, r, row)

	tx := emitPortfolio(t, r, newPortfolioPSN(t), nil)["ptx:REF1@"+ptxSafe]
	if tx.InstrumentExternalID != nil {
		t.Errorf("instrument = %v, want none", tx.InstrumentExternalID)
	}
	if tx.InstrumentHint != "10000001" {
		t.Errorf("hint = %q, want the valor a config link would close", tx.InstrumentHint)
	}
}

// A price the export qualified with the unit it is quoted in loses that
// unit in a numeric column. Multiplied out against the row's own value
// it disagrees, and two contradicting figures in gold are worse than
// one missing one.
func TestPortfolioPriceIsDroppedWhenItContradictsTheValue(t *testing.T) {
	r := newPortfolioTxFixture(t)
	// A call deposit quoted at "100%": nominal 250, value 250.
	seedPortfolioTx(t, r, ptxRow{
		id: "ptx:REF1", bookingType: "Reduction",
		quantity: -250.0, settlementCcy: "USD", price: 100.0,
		valuationCcy: "USD", value: -250.0, valor: "10000001",
	})

	tx := emitPortfolio(t, r, newPortfolioPSN(t), nil)["ptx:REF1@"+ptxSafe]
	if tx.Price != nil {
		t.Errorf("price = %v, want none", tx.Price)
	}
	if tx.NetAmount == nil || tx.NetAmount.String() != "250" {
		t.Errorf("net = %v, want 250 — the value is still good", tx.NetAmount)
	}
}

func TestPortfolioPriceSurvivesTheValuesOwnRounding(t *testing.T) {
	r := newPortfolioTxFixture(t)
	// The list publishes a value rounded to the whole currency unit, so
	// a small trade is legitimately off by a fraction of one.
	seedPortfolioTx(t, r, ptxRow{
		id: "ptx:REF1", bookingType: "Stock Market Spot Sale",
		quantity: -0.023, settlementCcy: "USD", price: 202.173913,
		valuationCcy: "USD", value: -5.0, isin: ptxISIN,
	})

	tx := emitPortfolio(t, r, newPortfolioPSN(t), nil)["ptx:REF1@"+ptxSafe]
	if tx.Price == nil {
		t.Error("price dropped, want it kept within the value's own rounding")
	}
}

// ----------------------------------------------------------------
// The window
// ----------------------------------------------------------------

// Gold deletes the window before re-inserting it, so a row emitted
// outside it is inserted again without its predecessor being removed —
// and duplicates on every load. This rail reaches years back past the
// oldest live dump, so nothing else in the window computation reaches
// it.
func TestPortfolioRangeWidensTheChangeWindow(t *testing.T) {
	r := newPortfolioTxFixture(t)
	if _, err := r.db.Exec(
		`INSERT INTO dump_runs (snapshot_at) VALUES (?)`, ptxSettleDay+1); err != nil {
		t.Fatal(err)
	}
	seedPortfolioTx(t, r, ptxTrade("ptx:OLD"))

	w, err := r.ChangeWindow(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges {
		t.Fatal("no changes reported")
	}
	if w.Start > ptxSettleDay {
		t.Errorf("window starts at %d, after the trade at %d", w.Start, ptxSettleDay)
	}
	if w.End < ptxSettleDay {
		t.Errorf("window ends at %d, before the trade at %d", w.End, ptxSettleDay)
	}
}
