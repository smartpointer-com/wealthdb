package ubs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Realized lots (docs/DESIGN.md §7.4): the sales UBS states together
// with the cost they drew on. Two web surfaces state them, one row per
// sale, never per lot, since UBS keeps a holding at its average cost.
//
//   - The transaction list a statement of assets prints
//     (`statement_trades`, collector migration 0014): every booking in
//     the statement's period. For a sale it prints the proceeds and
//     the cost value in the statement's reporting currency, and the
//     realized P/L only as a percentage. Document kind `statement`.
//   - The portfolio transaction list (`portfolio_transactions`,
//     collector migration 0012): the export of a managed portfolio's
//     movements. For a sale it states the value and the realized P/L,
//     both in the currency the export is valued in. Document kind
//     `trade`.
//
// Which rows count each sale once (is_primary):
//
//   - A booking recurs in every statement whose period covers it. The
//     portfolio and the settlement number name it across statements; a
//     booking printed without a number is named by its figures and its
//     booking text. The first statement that prints it keeps it, and
//     later copies are not primary. A row without a reporting currency
//     states no figure, so it is dropped before the first copy is
//     chosen.
//   - A sale the bank reversed is not a sale. The bank prints the
//     reversal as a booking of its own: its booking text names it a
//     reversal, and it carries the sale's figures with the units coming
//     back. It cancels one sale with those figures, which is then not
//     primary. A reversed purchase disposes of nothing and is no sale.
//     The export's reversals cancel its own sales the same way.
//   - The export restates sales the statements print, and reaches
//     beyond the last of them. An export sale is primary only on a day
//     no statement's list covers for its account, so on one account and
//     day only one kind can count. A sale both kinds state therefore
//     counts once wherever both book it on the same account. A sale on
//     a covered day that no list prints counts nowhere. Both kinds rank
//     alike: a tax year a statement reaches only in part still counts
//     the sales after its end.

// RealizedLots implements silver.RealizedLotReader.
func (c *Connection) RealizedLots(ctx context.Context) ([]canonical.RealizedLotChange, error) {
	if c.web == nil {
		return nil, nil
	}
	accounts, err := newRealizedAccounts(ctx, c.psn)
	if err != nil {
		return nil, err
	}
	valors, err := buildValorIndex(ctx, c.psn, c.web)
	if err != nil {
		return nil, err
	}
	lots, statementCounts, covered, err := c.web.statementSales(ctx, accounts, valors)
	if err != nil {
		return nil, err
	}
	trades, tradeCounts, err := c.web.exportSales(ctx, accounts, valors, covered)
	if err != nil {
		return nil, err
	}
	lots = append(lots, trades...)
	counts := map[string]bool{}
	maps.Copy(counts, statementCounts)
	maps.Copy(counts, tradeCounts)
	silver.MarkPrimary(lots, realizedRank, func(r *canonical.RealizedLotChange) bool {
		return counts[r.RealizedLotExternalID]
	})
	return lots, nil
}

var _ silver.RealizedLotReader = (*Connection)(nil)

// realizedRank ranks the two kinds alike: they are not two documents
// of one sale but two reaches in time, and eligibility already keeps
// an export sale from counting on a day a statement's list covers (see
// the file comment).
func realizedRank(k canonical.RealizedDocKind) int {
	switch k {
	case canonical.RealizedStatement, canonical.RealizedTrade:
		return 0
	}
	return -1
}

// realizedAccounts names the account a sale is booked on in gold: the
// safekeeping account the positions use.
type realizedAccounts struct {
	// known holds the safekeeping accounts PSN reports, in its form.
	known map[string]bool
	// byPortfolio is the statement era's portfolio mapping
	// (psnReader.safekeepingByPortfolio).
	byPortfolio map[string]string
}

func newRealizedAccounts(ctx context.Context, psn *psnReader) (realizedAccounts, error) {
	out := realizedAccounts{known: map[string]bool{}}
	if psn == nil || psn.db == nil {
		return out, nil
	}
	rows, err := psn.db.QueryContext(ctx, `SELECT DISTINCT account_external_id FROM safekeeping_accounts`)
	if err != nil {
		return out, fmt.Errorf("ubs psn safekeeping accounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			return out, fmt.Errorf("ubs psn safekeeping accounts scan: %w", err)
		}
		out.known[account] = true
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.byPortfolio, err = psn.safekeepingByPortfolio(ctx)
	return out, err
}

// resolve returns the account for a custody account as a list states
// it, in either form, within the given portfolio. A custody account PSN
// does not report falls back to where the statement era puts the
// portfolio's securities, so a sale sits beside the positions it came
// from.
func (a realizedAccounts) resolve(custody, portfolio string) string {
	custody = strings.TrimSpace(custody)
	if a.known[custody] {
		return custody
	}
	if id := custodyAccountCanonical(custody); a.known[id] {
		return id
	}
	account, _ := statementSecuritiesAccount(a.byPortfolio, portfolio)
	return account
}

// custodyAccountCanonical converts a custody account as a statement
// prints it ('BBB-AAAAAA.S1') into the form PSN keys safekeeping
// accounts by: the branch zero-padded to four digits, the account body
// to ten, then the suffix. The same rule as the collector's
// portfolio_account_canonical, which reads the export's spaced form.
// Any other shape returns "" rather than a near-miss id.
func custodyAccountCanonical(printed string) string {
	m := custodyAccountRe.FindStringSubmatch(strings.TrimSpace(printed))
	if m == nil {
		return ""
	}
	return zeroPad(m[1], 4) + zeroPad(m[2], 10) + m[3]
}

var custodyAccountRe = regexp.MustCompile(`^(\d{3,4})[- ]+(\d{1,10})\.([A-Z0-9]{1,6})$`)

func zeroPad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return strings.Repeat("0", n-len(s)) + s
}

// listPeriod is the span one statement's transaction list covers for
// one account.
type listPeriod struct{ start, end int64 }

// statementTrade is one row of a statement's transaction list that
// states a sale's figures: a sale, or the reversal of one.
type statementTrade struct {
	token                                       string
	seq                                         int64
	asOf                                        int64
	portfolio, custody, account                 string
	reportingCcy, bookingText, name, valor      string
	isin, tradeCcy, settlementNo, orderNo       string
	settlementCcy, document                     string
	tradeDate, valueDate                        sql.NullInt64
	quantity, costPrice, acquisitionFxRate      sql.NullFloat64
	costBasis, price, priceFxRate               sql.NullFloat64
	priceGainPct, fxGainPct, realizedPct, value sql.NullFloat64
	settlementAmount                            sql.NullFloat64
	// booking names the booking the row prints, the same in every
	// statement that prints it (see statementSales).
	booking booking
}

// booking names one booking two ways: key is the same in every copy
// that prints it, and figures is what a reversal shares with it.
type booking struct{ key, figures string }

// saleFigures is what a reversal shares with the sale it cancels: where
// it was booked, the security, the trade day, and the quantity and value
// with their signs dropped.
func saleFigures(where, isin, valor string, day int64, quantity, value sql.NullFloat64) string {
	return strings.Join([]string{
		where, isin, normalizeValor(valor),
		strconv.FormatInt(day, 10),
		strconv.FormatFloat(math.Abs(quantity.Float64), 'f', 6, 64),
		strconv.FormatFloat(math.Abs(value.Float64), 'f', 2, 64),
	}, "|")
}

// isReversal reports whether a booking text names a reversal. The bank
// prints one as the text of the booking it reverses with the word
// "Reversal" in front, or behind a separator after it, in any case.
func isReversal(text string) bool {
	for _, w := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) }) {
		if strings.EqualFold(w, "reversal") {
			return true
		}
	}
	return false
}

// cancelReversed pairs each reversal with the sale it cancels: the
// first sale, in the order given, with the same figures that no
// reversal has cancelled yet. A key given twice is one booking, so a
// sale or a reversal two statements print counts once. It returns the
// index of the cancelling reversal per cancelled sale's key.
func cancelReversed(sales, reversals []booking) map[string]int {
	open := map[string][]string{} // figures → sale keys not yet cancelled
	seen := map[string]bool{}
	for _, s := range sales {
		if !seen[s.key] {
			seen[s.key] = true
			open[s.figures] = append(open[s.figures], s.key)
		}
	}
	out := map[string]int{}
	used := map[string]bool{}
	for i, r := range reversals {
		if used[r.key] || len(open[r.figures]) == 0 {
			continue
		}
		used[r.key] = true
		out[open[r.figures][0]] = i
		open[r.figures] = open[r.figures][1:]
	}
	return out
}

// statementSales reads the sales the statements' transaction lists
// print. It returns one realized lot per printed sale, the ids that can
// count (the first copy of a booking no reversal cancels) and the
// periods the lists cover, per account.
//
// A sale is a booking with a negative quantity that prints a sale's
// price and settles cash, and whose booking text names no reversal:
// that leaves out the corporate actions and write-offs, which print
// neither, a delivery free of payment, which settles nothing, and a
// reversed purchase.
//
// The portfolio and the settlement number name a booking across the
// statements that print it. A row without a number falls back to its
// figures and booking text, with its rank among the statement's rows of
// the same figures and text, so two like bookings in one statement stay
// two.
func (r *webReader) statementSales(ctx context.Context, accounts realizedAccounts, valors map[string]string) ([]canonical.RealizedLotChange, map[string]bool, map[string][]listPeriod, error) {
	ok, err := r.hasTable(ctx, "statement_trades")
	if err != nil || !ok {
		return nil, nil, nil, err
	}
	covered, err := r.listPeriods(ctx, accounts)
	if err != nil {
		return nil, nil, nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT s.source_doc_token, s.seq, s.as_of_date, s.portfolio_external_id,
       COALESCE(s.custody_account, ''), COALESCE(s.reporting_currency_iso, ''),
       s.trade_date, s.value_date, s.booking_text, s.quantity,
       COALESCE(s.security_name, ''), COALESCE(s.valor, ''), COALESCE(s.isin, ''),
       COALESCE(s.currency_iso, ''), s.cost_price, s.acquisition_fx_rate, s.cost_basis,
       s.transaction_price, s.transaction_fx_rate, s.transaction_gain_pct,
       s.exchange_gain_pct, s.realized_pl_pct, s.transaction_value,
       s.settlement_amount, COALESCE(s.settlement_currency_iso, ''),
       COALESCE(s.settlement_no, ''), COALESCE(s.order_no, ''),
       COALESCE(d.content_sha256, '')
  FROM statement_trades s
  LEFT JOIN documents d ON d.doc_token = s.source_doc_token
 WHERE s.quantity <> 0
   AND s.transaction_price IS NOT NULL
   AND s.settlement_amount IS NOT NULL
 ORDER BY s.as_of_date, s.source_doc_token, s.seq`)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ubs-web statement sales: %w", err)
	}
	defer rows.Close()
	var sales, reversals []statementTrade
	nth := map[string]int{} // statement, figures and text → rows of them so far
	skipped := 0
	for rows.Next() {
		var t statementTrade
		if err := rows.Scan(&t.token, &t.seq, &t.asOf, &t.portfolio,
			&t.custody, &t.reportingCcy,
			&t.tradeDate, &t.valueDate, &t.bookingText, &t.quantity,
			&t.name, &t.valor, &t.isin,
			&t.tradeCcy, &t.costPrice, &t.acquisitionFxRate, &t.costBasis,
			&t.price, &t.priceFxRate, &t.priceGainPct,
			&t.fxGainPct, &t.realizedPct, &t.value,
			&t.settlementAmount, &t.settlementCcy,
			&t.settlementNo, &t.orderNo, &t.document); err != nil {
			return nil, nil, nil, fmt.Errorf("ubs-web statement sales scan: %w", err)
		}
		t.account = accounts.resolve(t.custody, t.portfolio)
		t.isin = strings.TrimSpace(t.isin)
		t.reportingCcy = strings.ToUpper(strings.TrimSpace(t.reportingCcy))
		// The portfolio, not the resolved account: two copies of a
		// booking must agree however each resolves.
		t.booking.figures = saleFigures(t.portfolio, t.isin, t.valor, t.tradeDate.Int64, t.quantity, t.value)
		if t.settlementNo != "" {
			t.booking.key = "no|" + t.portfolio + "|" + t.settlementNo
		} else {
			like := t.booking.figures + "|" + t.bookingText
			t.booking.key = "figures|" + like + "|" + strconv.Itoa(nth[t.token+"|"+like])
			nth[t.token+"|"+like]++
		}
		reversal := isReversal(t.bookingText)
		switch {
		case reversal && t.quantity.Float64 > 0:
			reversals = append(reversals, t)
		case reversal || t.quantity.Float64 > 0:
			// A purchase, or the reversal of one, disposes of nothing.
		case t.reportingCcy == "":
			// Proceeds and cost are in the reporting currency, and a
			// figure without its currency is no figure.
			skipped++
		default:
			sales = append(sales, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	if skipped > 0 {
		log.Printf("ubs adapter: skipped %d statement sale(s) printed without a reporting currency", skipped)
	}

	reversedBy := cancelReversed(statementBookings(sales), statementBookings(reversals))
	lots := make([]canonical.RealizedLotChange, 0, len(sales))
	counts := map[string]bool{}
	kept := map[string]bool{}
	for _, t := range sales {
		id := realizedID("statement", t.token, strconv.FormatInt(t.seq, 10))
		var reversal *statementTrade
		if i, ok := reversedBy[t.booking.key]; ok {
			reversal = &reversals[i]
		}
		// The first copy of a booking is the one that can count.
		if !kept[t.booking.key] && reversal == nil {
			counts[id] = true
		}
		kept[t.booking.key] = true
		lot := canonical.RealizedLotChange{
			RealizedLotExternalID: id,
			AccountExternalID:     t.account,
			Description:           silver.StrPtrIfNonEmpty(t.name),
			DocumentKind:          canonical.RealizedStatement,
			DisposalDate:          silver.DatePtrFromNullUnix(t.tradeDate),
			SettlementDate:        silver.DatePtrFromNullUnix(t.valueDate),
			Currency:              t.reportingCcy,
			Quantity:              silver.AbsPtr(silver.DecimalPtrFromNullFloat(t.quantity)),
			Proceeds:              silver.AbsPtr(silver.DecimalPtrFromNullFloat(t.value)),
			SourceDocument:        silver.StrPtrIfNonEmpty(t.document),
			Payload:               t.payload(reversal),
		}
		statementDate := time.Unix(t.asOf, 0).UTC()
		lot.TaxYear = silver.TaxYearOf(lot.DisposalDate, lot.SettlementDate, &statementDate)
		lot.InstrumentExternalID, lot.InstrumentHint = realizedInstrument(valors, t.isin, t.valor)
		lot.SetBookValue(silver.AbsPtr(silver.DecimalPtrFromNullFloat(t.costBasis)), statedAverageBasis)
		lots = append(lots, lot)
	}
	return lots, counts, covered, nil
}

func statementBookings(rows []statementTrade) []booking {
	out := make([]booking, len(rows))
	for i, t := range rows {
		out[i] = t.booking
	}
	return out
}

// listPeriods returns, per account, the periods the statements'
// transaction lists cover. A list that prints no period of its own
// covers its first booking to the statement date.
func (r *webReader) listPeriods(ctx context.Context, accounts realizedAccounts) (map[string][]listPeriod, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT COALESCE(custody_account, ''), portfolio_external_id,
       COALESCE(MIN(period_start), MIN(trade_date), MIN(value_date)),
       COALESCE(MAX(period_end), MAX(as_of_date))
  FROM statement_trades
 GROUP BY source_doc_token, custody_account, portfolio_external_id`)
	if err != nil {
		return nil, fmt.Errorf("ubs-web statement list periods: %w", err)
	}
	defer rows.Close()
	out := map[string][]listPeriod{}
	for rows.Next() {
		var (
			custody, portfolio string
			start, end         sql.NullInt64
		)
		if err := rows.Scan(&custody, &portfolio, &start, &end); err != nil {
			return nil, fmt.Errorf("ubs-web statement list periods scan: %w", err)
		}
		if !start.Valid || !end.Valid {
			continue
		}
		account := accounts.resolve(custody, portfolio)
		out[account] = append(out[account], listPeriod{start.Int64, end.Int64})
	}
	return out, rows.Err()
}

// payload carries what the list prints beside the realized lot's own
// columns: the booking, the prices and rates in the trade's currency,
// the gains as the percentages it prints them, and the settlement. A
// reversed sale says so, and names the reversal's settlement number.
func (t statementTrade) payload(reversal *statementTrade) json.RawMessage {
	m := map[string]any{
		"booking_text":     t.bookingText,
		"statement_date":   time.Unix(t.asOf, 0).UTC().Format(time.DateOnly),
		"source_doc_token": t.token,
		"seq":              t.seq,
	}
	if reversal != nil {
		m["reversed"] = true
		if reversal.settlementNo != "" {
			m["reversed_by"] = reversal.settlementNo
		}
	}
	for k, v := range map[string]string{
		"settlement_no":           t.settlementNo,
		"order_no":                t.orderNo,
		"trade_currency":          t.tradeCcy,
		"settlement_currency_iso": t.settlementCcy,
	} {
		if v != "" {
			m[k] = v
		}
	}
	for k, v := range map[string]sql.NullFloat64{
		"cost_price":           t.costPrice,
		"acquisition_fx_rate":  t.acquisitionFxRate,
		"transaction_price":    t.price,
		"transaction_fx_rate":  t.priceFxRate,
		"transaction_gain_pct": t.priceGainPct,
		"exchange_gain_pct":    t.fxGainPct,
		"realized_pl_pct":      t.realizedPct,
		"settlement_amount":    t.settlementAmount,
	} {
		if v.Valid {
			m[k] = v.Float64
		}
	}
	blob, _ := json.Marshal(m)
	return blob
}

// exportTrade is one row of the portfolio export that states a sale's
// figures: a sale, or the reversal of one.
type exportTrade struct {
	txID, custody, portfolio, bookingType string
	name, valor, isin, currency, payload  string
	tradeDate                             sql.NullInt64
	valueDate                             int64
	quantity, value, pl                   sql.NullFloat64
	booking                               booking
}

// exportSales reads the sales the portfolio export states a realized
// P/L for, and returns them with the ids that can count: those no
// reversal cancels and no statement's list covers.
//
// The export states the value and the realized P/L in its valuation
// currency, whatever currency the trade and its cash moved in. The
// cost is the value less the P/L. A sale and its reversal are told
// apart the way the statements' lists tell them (statementSales); each
// row is one booking, keyed by the export's own key.
func (r *webReader) exportSales(ctx context.Context, accounts realizedAccounts, valors map[string]string, covered map[string][]listPeriod) ([]canonical.RealizedLotChange, map[string]bool, error) {
	ok, err := r.hasTable(ctx, "portfolio_transactions")
	if err != nil || !ok {
		return nil, nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT transaction_external_id, safekeeping_account_external_id, portfolio_external_id,
       booking_type, trade_date, value_date, COALESCE(security_name, ''), COALESCE(valor, ''),
       COALESCE(isin, ''), quantity, COALESCE(valuation_currency_iso, ''),
       trans_value, realized_pl, payload
  FROM portfolio_transactions
 WHERE quantity <> 0
 ORDER BY value_date, transaction_external_id, safekeeping_account_external_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("ubs-web export sales: %w", err)
	}
	defer rows.Close()
	var sales, reversals []exportTrade
	skipped := 0
	for rows.Next() {
		var t exportTrade
		if err := rows.Scan(&t.txID, &t.custody, &t.portfolio, &t.bookingType,
			&t.tradeDate, &t.valueDate, &t.name, &t.valor, &t.isin, &t.quantity,
			&t.currency, &t.value, &t.pl, &t.payload); err != nil {
			return nil, nil, fmt.Errorf("ubs-web export sales scan: %w", err)
		}
		t.isin = strings.TrimSpace(t.isin)
		t.currency = strings.ToUpper(strings.TrimSpace(t.currency))
		t.booking = booking{
			key:     t.txID + "|" + t.custody,
			figures: saleFigures(t.custody, t.isin, t.valor, t.day(), t.quantity, t.value),
		}
		reversal := isReversal(t.bookingType)
		switch {
		case reversal && t.quantity.Float64 > 0:
			reversals = append(reversals, t)
		case reversal || t.quantity.Float64 > 0 || !t.pl.Valid:
			// A purchase or its reversal disposes of nothing, and a
			// disposal without a realized P/L states no cost.
		case t.currency == "":
			skipped++
		default:
			sales = append(sales, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if skipped > 0 {
		log.Printf("ubs adapter: skipped %d exported sale(s) stating no valuation currency", skipped)
	}

	reversedBy := cancelReversed(exportBookings(sales), exportBookings(reversals))
	lots := make([]canonical.RealizedLotChange, 0, len(sales))
	counts := map[string]bool{}
	for _, t := range sales {
		account := accounts.resolve(t.custody, t.portfolio)
		payload := json.RawMessage(t.payload)
		i, reversed := reversedBy[t.booking.key]
		if reversed {
			payload = silver.PayloadWith(t.payload, map[string]any{
				"reversed": true, "reversed_by": reversals[i].txID,
			})
		}
		lot := canonical.RealizedLotChange{
			RealizedLotExternalID: realizedID("trade", t.txID, t.custody),
			AccountExternalID:     account,
			Description:           silver.StrPtrIfNonEmpty(t.name),
			DocumentKind:          canonical.RealizedTrade,
			DisposalDate:          silver.DatePtrFromNullUnix(t.tradeDate),
			SettlementDate:        silver.DatePtrFromNullUnix(sql.NullInt64{Int64: t.valueDate, Valid: true}),
			Currency:              t.currency,
			Quantity:              silver.AbsPtr(silver.DecimalPtrFromNullFloat(t.quantity)),
			Proceeds:              silver.AbsPtr(silver.DecimalPtrFromNullFloat(t.value)),
			RealizedGainLoss:      silver.DecimalPtrFromNullFloat(t.pl),
			Payload:               payload,
		}
		lot.TaxYear = silver.TaxYearOf(lot.DisposalDate, lot.SettlementDate)
		lot.InstrumentExternalID, lot.InstrumentHint = realizedInstrument(valors, t.isin, t.valor)
		if lot.Proceeds != nil {
			if book := lot.Proceeds.Sub(*lot.RealizedGainLoss); !book.IsNegative() {
				lot.SetBookValue(&book, derivedAverageBasis)
			}
		}
		if !reversed && !coveredOn(covered[account], t.day()) {
			counts[lot.RealizedLotExternalID] = true
		}
		lots = append(lots, lot)
	}
	return lots, counts, nil
}

// day is the trade day, else the value day.
func (t exportTrade) day() int64 {
	if t.tradeDate.Valid {
		return t.tradeDate.Int64
	}
	return t.valueDate
}

func exportBookings(rows []exportTrade) []booking {
	out := make([]booking, len(rows))
	for i, t := range rows {
		out[i] = t.booking
	}
	return out
}

func coveredOn(periods []listPeriod, day int64) bool {
	for _, p := range periods {
		if p.start <= day && day <= p.end {
			return true
		}
	}
	return false
}

// realizedInstrument resolves a sale's security the way the trade rows
// do: the ISIN where the list states one, else the valor through the
// valor index, which leaves the valor as the hint a config link closes.
func realizedInstrument(valors map[string]string, isin, valor string) (*string, string) {
	if isin != "" {
		return &isin, ""
	}
	return resolveValor(valors, valor)
}

// realizedID is a realized lot's id: the document kind and a hash of
// the silver row's own key, stable across loads.
func realizedID(kind string, key ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(key, "\x1f")))
	return kind + ":" + hex.EncodeToString(sum[:12])
}
