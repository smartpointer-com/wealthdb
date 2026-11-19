package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The portfolio transaction list (ubs-web collector migration 0012):
// what a MANAGED portfolio did, on the third of the three rails a UBS
// securities trade reaches gold on.
//
// The other two leave a hole between them. An annual Account Statement
// reprints a calendar year's trades the following January, so the
// current year stays unreadable for as long as twelve months; the
// sibling feed's MT515 confirmations begin only where that feed was
// first ingested, and nothing earlier is recoverable from it. Between
// the last statement and the first confirmation a discretionary
// mandate's cash account shows money arriving and leaving with nothing
// to explain it — which a cash flow statement can only read as saving.
// The hole is not only that seam: a year whose statement was never
// published for an account is the same hole, and this rail fills that
// too.
//
// WHAT IT EMITS. One ledger row per securities settlement, booked on
// the CASH ACCOUNT that paid for it — the account whose balance the
// trade moved, and the one the coverage report reconciles. Rows that
// move securities without moving money are not ledger rows here: they
// state no cash, the positions snapshots already carry their effect,
// and the sibling feed publishes corporate-action events of its own
// that nothing could pair them against.
//
// WHAT IT LEAVES ALONE. The currency conversions the list also carries
// state no settlement amount: a conversion is a PAIR of figures in one
// cell, which is no single amount, and the rule below excludes it for
// that reason rather than by naming it.
//
// Nothing is lost by that. Where the MT940 feed covers both cash
// accounts a conversion moves, both legs are already in the ledger as
// ordinary entries. Where it covers one — an account the bank sends
// no MT940 for — the covered side's statement line still states the
// other leg, and that is where it is booked from (conversionMirrors);
// the list does not carry that account's conversions either, so there
// is nothing here to emit for it.
//
// GROSS, NOT NET. The list states a trade's VALUE; the commission is
// not in it. Measured against the confirmations that carry both, the
// difference is a few tenths of a percent. So a coverage gap this rail
// closes closes to within the fees rather than exactly, and a row it
// emits is a faithful statement of the trade rather than of the bank's
// final debit.

// portfolioCashKey is what the export says about where a trade
// settled: the portfolio that traded, and the currency the cash moved
// in.
type portfolioCashKey struct {
	portfolio string
	currency  string
}

// settledDayKey identifies a securities settlement as the reconcile
// table's list rule keys it (ruleSettledOverList): one cash account, one
// currency, one settlement day.
//
// It carries no amount, deliberately. The three rails state different
// amounts for the same trade — this one the gross value, the statement
// and the confirmation the net debit — so an amount would pair
// nothing. What is left still separates a day the other rails cover
// from one they do not, which is the question being asked.
type settledDayKey struct {
	account  string
	currency string
	day      int64
}

// newSettledDayKey is the one place the key is composed, so every rail
// keys by it the same way.
func newSettledDayKey(account, currency string, at int64) settledDayKey {
	return settledDayKey{
		account:  account,
		currency: bookingCurrency(currency),
		day:      utcDay(at),
	}
}

// portfolioCashKind reports the ledger kind for one row of the
// portfolio transaction list, and whether the row settles against cash
// at all.
//
// The export signs its own figures — positive is leaving the
// portfolio, which on the cash leg means money going out — so the
// DIRECTION is read off the value rather than off the booking type.
// That is what keeps this small against a vocabulary of several dozen
// types that grows without notice: a new spelling of "bought
// something" needs no case of its own, and a type this adapter has
// never seen still books on the side its own figure states. The types
// are still visible where they belong, as the row's provider category.
//
// A row with no value stated settles nothing. That covers the
// corporate actions, the in-kind issues, and the currency conversions
// — the last because a conversion states a PAIR of figures in the one
// cell, which is no single settlement amount and is not read as one.
func portfolioCashKind(bookingType string, value sql.NullFloat64) (canonical.TxKind, bool) {
	if !value.Valid || value.Float64 == 0 {
		return "", false
	}
	if bookingOf(bookingType).settles == settlesNothing {
		return "", false
	}
	if value.Float64 > 0 {
		return canonical.TxKindBuy, true
	}
	return canonical.TxKindSell, true
}

// portfolioSettlementAmount converts the value the export states into
// the currency the cash actually moved in.
//
// The value is stated in the PORTFOLIO's reporting currency, which is
// not the settlement currency whenever a mandate buys abroad: a Hong
// Kong trade inside a USD-reporting mandate names HKD as the
// settlement currency, USD as the valuation currency, and the rate
// between them. The cash leaves the portfolio's HKD account, in HKD.
//
// ok is false where the two currencies differ and no rate is stated.
// Booking the valuation figure under the settlement currency would
// state a plausible number in the wrong money, which is worse than the
// gap it would appear to close.
func portfolioSettlementAmount(value float64, settlement, valuation string, rate sql.NullFloat64) (float64, bool) {
	if valuation == "" || settlement == valuation {
		return value, true
	}
	if !rate.Valid || rate.Float64 == 0 {
		return 0, false
	}
	return value / rate.Float64, true
}

// portfolioTradePrice is the per-unit price, carried only where it
// reconciles with the value the same row states.
//
// The export qualifies some prices with the unit they are quoted in —
// a call deposit at "100%" is the plainest — and a unit does not
// survive into a numeric column. So an unqualified figure can mean
// something other than money per unit, and multiplying it out against
// the row's own value is what tells the two apart. A price that
// disagrees with the value beside it is not carried at all: gold would
// hold two figures that contradict each other, and neither would say
// which to believe.
func portfolioTradePrice(quantity, price sql.NullFloat64, amount float64) *canonical.Decimal {
	if !quantity.Valid || !price.Valid || quantity.Float64 == 0 {
		return nil
	}
	implied := math.Abs(quantity.Float64 * price.Float64)
	// The value is published rounded to the whole currency unit, so a
	// small trade is legitimately off by up to a unit.
	if math.Abs(implied-math.Abs(amount)) > math.Max(1, math.Abs(amount)*0.01) {
		return nil
	}
	d := canonical.NewDecimalFromFloat(price.Float64)
	return &d
}

// listRow is one row of the portfolio transaction list that settles
// cash (portfolioCashKind), resolved to the cash booking it makes: the
// portfolio's cash account of the settlement currency, and the amount in
// that currency. cashAccount is "" where no one account answers, and
// priced is false where the value cannot be read in the settlement
// currency; neither books.
type listRow struct {
	txID, account, bookingType, payload string
	valueDate                           int64
	securityName, valor, isin           sql.NullString
	quantity, transPrice                sql.NullFloat64

	kind        canonical.TxKind
	currency    string
	cashAccount string
	amount      float64
	priced      bool
}

// books reports whether the row books: its cash account is named and
// its amount read.
func (l *listRow) books() bool { return l.cashAccount != "" && l.priced }

// emittedID is the id the row reaches gold under. Silver keys these by
// (id, custody account) so that a booking reported against two accounts
// — a transfer between them — stays two rows. Gold's key is the id
// alone, so the pair is folded into one here.
func (l *listRow) emittedID() string { return l.txID + "@" + l.account }

// portfolioListRows reads every row of the list that settles cash, in
// the list's order (value date, then id), each resolved against the
// account master data (listRow). None without the list or without a
// portfolio cash account in the roster.
func (r *webReader) portfolioListRows(ctx context.Context, roster *psnRoster) ([]*listRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	ok, err := r.hasTable(ctx, "portfolio_transactions")
	if err != nil || !ok {
		return nil, err
	}
	cashAccounts := roster.portfolioCashAccounts()
	if len(cashAccounts) == 0 {
		return nil, nil
	}
	portfolioOf := roster.portfolioOfSafekeeping()
	const q = `
SELECT transaction_external_id, safekeeping_account_external_id, value_date,
       booking_type, security_name, valor, isin, quantity,
       settlement_currency_iso, valuation_currency_iso,
       trans_price, exchange_rate, trans_value, payload
  FROM portfolio_transactions
 ORDER BY value_date, transaction_external_id`
	var out []*listRow
	err = silver.ScanRows(ctx, r.db, "ubs-web portfolio transactions", q, func(rows *sql.Rows) error {
		var (
			l                           listRow
			settlementCcy, valuationCcy sql.NullString
			exchangeRate, txValue       sql.NullFloat64
		)
		if err := rows.Scan(&l.txID, &l.account, &l.valueDate, &l.bookingType,
			&l.securityName, &l.valor, &l.isin, &l.quantity,
			&settlementCcy, &valuationCcy,
			&l.transPrice, &exchangeRate, &txValue, &l.payload); err != nil {
			return fmt.Errorf("ubs-web portfolio transactions scan: %w", err)
		}
		kind, isCash := portfolioCashKind(l.bookingType, txValue)
		if !isCash {
			return nil
		}
		l.kind = kind
		// Which currency the cash moved in. A row that states none
		// settled in the currency it was valued in — the portfolio's
		// own, which is the only other currency it names.
		l.currency = bookingCurrency(settlementCcy.String)
		if !settlementCcy.Valid || settlementCcy.String == "" {
			l.currency = bookingCurrency(valuationCcy.String)
		}
		l.cashAccount = cashAccounts[portfolioCashKey{
			portfolio: portfolioOf[l.account],
			currency:  l.currency,
		}]
		if l.cashAccount != "" {
			l.amount, l.priced = portfolioSettlementAmount(
				txValue.Float64, l.currency, bookingCurrency(valuationCcy.String), exchangeRate)
		}
		out = append(out, &l)
		return nil
	})
	return out, err
}

// portfolioTransactions emits the managed portfolios' securities
// settlements, on the cash accounts that paid for them.
//
// No PSN cutoff applies and none could: the cut arbitrates between two
// records of one cash booking, and the question here is the opposite
// one — whether any rail recorded this booking at all. That is what
// the reconcile table's last rule answers, row by row (rc).
//
// Nothing is emitted without the account master data. A trade booked
// against a custody account rather than the cash account that paid
// would sit outside every cash reconciliation gold has, and a rail
// added to close a reconciliation gap must not open one somewhere
// else.
func (r *webReader) portfolioTransactions(ctx context.Context, w canonical.Window, psn *psnReader, rc *reconciliation) (canonical.TransactionBatch, error) {
	var out canonical.TransactionBatch
	if !w.HasChanges {
		return out, nil
	}
	ok, err := r.hasTable(ctx, "portfolio_transactions")
	if err != nil || !ok {
		return out, err
	}
	roster, err := psn.roster(ctx)
	if err != nil {
		return out, err
	}
	if len(roster.portfolioCashAccounts()) == 0 {
		var n int
		if err := r.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM portfolio_transactions`).Scan(&n); err != nil {
			return out, fmt.Errorf("ubs-web portfolio transactions count: %w", err)
		}
		if n > 0 {
			log.Printf("ubs adapter: %d portfolio trade(s) left unread — the feed carrying the account master data that names a settling cash account is not configured", n)
		}
		return out, nil
	}
	valorToISIN, err := r.valorIndex(ctx, psn)
	if err != nil {
		return out, err
	}

	folded, unplaced, unpriced := 0, 0, 0
	for _, l := range rc.list {
		if l.valueDate < w.Start || l.valueDate > w.End {
			continue
		}
		switch {
		case l.cashAccount == "":
			unplaced++
			continue
		case !l.priced:
			unpriced++
			continue
		case rc.lostTo(recList, l.emittedID()) != "":
			folded++
			continue
		}

		// The list states cash out as a positive value, and the kind is
		// read off that sign (portfolioCashKind): the cash the account
		// gained is its negation.
		net := canonical.NewDecimalFromFloat(-l.amount)
		tx := canonical.TransactionChange{
			TransactionExternalID: l.emittedID(),
			// The settlement day: when the cash moved, which is what
			// the balance it has to reconcile against moved on.
			OccurredAt:        l.valueDate,
			AccountExternalID: l.cashAccount,
			Kind:              l.kind,
			Currency:          l.currency,
			// One figure, not two. The list states a gross value and
			// no commission, so carrying it as both the gross and the
			// net would assert a commission of zero.
			NetAmount: &net,
			Price:     portfolioTradePrice(l.quantity, l.transPrice, l.amount),
			// The security as the bank labels it. A name is a hint,
			// never an identity — the ISIN beside it is the identity.
			Description: silver.StrPtrIfNonEmpty(l.securityName.String),
			// The bank's own booking type, verbatim, as the cash rail
			// carries its own: the closest thing a bank has to a
			// provider category, and the vocabulary a reader needs to
			// see a type this adapter classified by direction alone.
			ProviderCategory: silver.StrPtrIfNonEmpty(l.bookingType),
			Payload:          json.RawMessage(l.payload),
		}
		if l.quantity.Valid {
			// Unsigned, as the sibling feed's confirmations state it:
			// the kind carries the direction, and two rails on one
			// account disagreeing about the sign of a quantity would
			// be read as two different trades.
			q := canonical.NewDecimalFromFloat(math.Abs(l.quantity.Float64))
			tx.Quantity = &q
		}
		switch {
		case l.isin.Valid && l.isin.String != "":
			id := l.isin.String
			tx.InstrumentExternalID = &id
		default:
			// The valor identifies the security where the export
			// states no ISIN; a stated ISIN outranks it.
			tx.InstrumentExternalID, tx.InstrumentHint = resolveValor(valorToISIN, l.valor.String)
		}
		out.Transactions = append(out.Transactions, tx)
	}
	if folded > 0 {
		log.Printf("ubs adapter: folded %d portfolio trade(s) another rail already settles — one booking, one row", folded)
	}
	if unplaced > 0 {
		log.Printf("ubs adapter: skipped %d portfolio trade(s) whose settling cash account could not be named", unplaced)
	}
	if unpriced > 0 {
		log.Printf("ubs adapter: skipped %d portfolio trade(s) stating no rate between the currency they settled in and the one they are valued in", unpriced)
	}
	return out, nil
}

// portfolioTxnRange is the span of value dates the portfolio pass
// emits at.
//
// It widens the reader's ChangeWindow, for the reason cardRange does:
// gold deletes the window before re-inserting it, so a row emitted
// outside [Start, End] is inserted again without its predecessor being
// removed, and duplicates on every load. This rail reaches years back
// past the oldest live dump, so nothing else in the window computation
// reaches it.
//
// Returns (-1, -1) when there is nothing to include.
func (r *webReader) portfolioTxnRange(ctx context.Context) (int64, int64, error) {
	ok, err := r.hasTable(ctx, "portfolio_transactions")
	if err != nil || !ok {
		return -1, -1, err
	}
	return silver.Extrema(ctx, r.db, "portfolioTxnRange", []string{
		`SELECT MIN(value_date), MAX(value_date) FROM portfolio_transactions`,
	})
}
