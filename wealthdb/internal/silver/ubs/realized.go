package ubs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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
//   - A booking recurs in every statement whose period covers it. Its
//     settlement number names it across statements, and the first
//     statement that prints it keeps it; later copies are not primary.
//   - A sale the bank reversed is not a sale. The list prints the
//     reversal as a booking of its own, with the sale's figures and the
//     units coming back, so the sale it cancels is found by those
//     figures and is not primary.
//   - The export restates sales the statements print, and reaches
//     beyond the last of them. An export sale is primary only on a day
//     no statement's list covers for its account. So the two kinds
//     never state the same sale as primary, and both rank alike: a tax
//     year a statement reaches only in part still counts the sales
//     after its end.

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
	lots, notPrimary, covered, err := c.web.statementSales(ctx, accounts, valors)
	if err != nil {
		return nil, err
	}
	trades, uncovered, err := c.web.exportSales(ctx, accounts, valors, covered)
	if err != nil {
		return nil, err
	}
	lots = append(lots, trades...)
	silver.MarkPrimary(lots, realizedRank, func(r *canonical.RealizedLotChange) bool {
		if r.DocumentKind == canonical.RealizedTrade {
			return uncovered[r.RealizedLotExternalID]
		}
		return !notPrimary[r.RealizedLotExternalID]
	})
	return lots, nil
}

var _ silver.RealizedLotReader = (*Connection)(nil)

// realizedRank ranks the two kinds alike: they are not two documents
// of one sale but two reaches in time, and eligibility already keeps
// an export sale a statement prints from counting (see the file
// comment).
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
}

// bookingKey names the booking a row prints, across statements.
func (t statementTrade) bookingKey() string {
	if t.settlementNo == "" {
		return "row|" + t.token + "|" + strconv.FormatInt(t.seq, 10)
	}
	return t.account + "|" + t.settlementNo
}

// figuresKey is what a reversal shares with the sale it cancels: the
// account, the security, the trade day, and the quantity and value
// with their signs dropped.
func (t statementTrade) figuresKey() string {
	return strings.Join([]string{
		t.account, t.isin, t.valor,
		strconv.FormatInt(t.tradeDate.Int64, 10),
		strconv.FormatFloat(math.Abs(t.quantity.Float64), 'f', 6, 64),
		strconv.FormatFloat(math.Abs(t.value.Float64), 'f', 2, 64),
	}, "|")
}

// statementSales reads the sales the statements' transaction lists
// print. It returns one realized lot per printed sale, the ids that
// are not primary (a later copy of a booking, a reversed sale) and the
// periods the lists cover, per account.
//
// A sale is a booking with a negative quantity that prints a sale's
// price and settles cash: that leaves out the corporate actions and
// write-offs, which print neither, and a delivery free of payment,
// which settles nothing.
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
		if t.quantity.Float64 < 0 {
			sales = append(sales, t)
		} else {
			reversals = append(reversals, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	// The first copy of each booking is the one that can count.
	kept := map[string]bool{}
	notPrimary := map[string]bool{}
	ids := make([]string, len(sales))
	for i, t := range sales {
		ids[i] = realizedID("statement", t.token, strconv.FormatInt(t.seq, 10))
		if k := t.bookingKey(); kept[k] {
			notPrimary[ids[i]] = true
		} else {
			kept[k] = true
		}
	}

	// Each reversal, once however many statements print it, cancels
	// one sale booking with the same figures.
	reversedBy := map[string]statementTrade{} // sale booking key → its reversal
	open := map[string][]string{}             // figures → sale booking keys not yet cancelled
	for _, t := range sales {
		k := t.bookingKey()
		if fk := t.figuresKey(); !slices.Contains(open[fk], k) {
			open[fk] = append(open[fk], k)
		}
	}
	seenReversal := map[string]bool{}
	for _, t := range reversals {
		k := t.bookingKey()
		if seenReversal[k] {
			continue
		}
		seenReversal[k] = true
		fk := t.figuresKey()
		if len(open[fk]) == 0 {
			continue
		}
		reversedBy[open[fk][0]] = t
		open[fk] = open[fk][1:]
	}

	lots := make([]canonical.RealizedLotChange, 0, len(sales))
	skipped := 0
	for i, t := range sales {
		currency := strings.ToUpper(strings.TrimSpace(t.reportingCcy))
		if currency == "" {
			// Proceeds and cost are in the reporting currency, and a
			// figure without its currency is no figure.
			skipped++
			continue
		}
		var reversal *statementTrade
		if r, ok := reversedBy[t.bookingKey()]; ok {
			reversal = &r
			notPrimary[ids[i]] = true
		}
		lot := canonical.RealizedLotChange{
			RealizedLotExternalID: ids[i],
			AccountExternalID:     t.account,
			Description:           silver.StrPtrIfNonEmpty(t.name),
			DocumentKind:          canonical.RealizedStatement,
			TaxYear:               taxYear(t.tradeDate, t.valueDate, t.asOf),
			DisposalDate:          silver.DatePtrFromNullUnix(t.tradeDate),
			SettlementDate:        silver.DatePtrFromNullUnix(t.valueDate),
			Currency:              currency,
			Quantity:              absDecimal(t.quantity),
			Proceeds:              absDecimal(t.value),
			SourceDocument:        silver.StrPtrIfNonEmpty(t.document),
			Payload:               t.payload(reversal),
		}
		lot.InstrumentExternalID, lot.InstrumentHint = realizedInstrument(valors, t.isin, t.valor)
		if book := absDecimal(t.costBasis); book != nil {
			lot.BookValue, lot.Basis = book, statedAverageBasis
		}
		lots = append(lots, lot)
	}
	if skipped > 0 {
		log.Printf("ubs adapter: skipped %d statement sale(s) printed without a reporting currency", skipped)
	}
	return lots, notPrimary, covered, nil
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

// exportSales reads the sales the portfolio export states a realized
// P/L for, and returns them with the ids of those no statement's list
// covers (the ones that can count).
//
// The export states the value and the realized P/L in its valuation
// currency, whatever currency the trade and its cash moved in. The
// cost is the value less the P/L.
func (r *webReader) exportSales(ctx context.Context, accounts realizedAccounts, valors map[string]string, covered map[string][]listPeriod) ([]canonical.RealizedLotChange, map[string]bool, error) {
	ok, err := r.hasTable(ctx, "portfolio_transactions")
	if err != nil || !ok {
		return nil, nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT transaction_external_id, safekeeping_account_external_id, portfolio_external_id,
       trade_date, value_date, COALESCE(security_name, ''), COALESCE(valor, ''),
       COALESCE(isin, ''), quantity, COALESCE(valuation_currency_iso, ''),
       trans_value, realized_pl, payload
  FROM portfolio_transactions
 WHERE quantity < 0 AND realized_pl IS NOT NULL
 ORDER BY value_date, transaction_external_id, safekeeping_account_external_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("ubs-web export sales: %w", err)
	}
	defer rows.Close()
	var lots []canonical.RealizedLotChange
	uncovered := map[string]bool{}
	skipped := 0
	for rows.Next() {
		var (
			txID, custody, portfolio, name, valor, isin, currency, payload string
			tradeDate                                                      sql.NullInt64
			valueDate                                                      int64
			quantity, value, pl                                            sql.NullFloat64
		)
		if err := rows.Scan(&txID, &custody, &portfolio, &tradeDate, &valueDate,
			&name, &valor, &isin, &quantity, &currency, &value, &pl, &payload); err != nil {
			return nil, nil, fmt.Errorf("ubs-web export sales scan: %w", err)
		}
		currency = strings.ToUpper(strings.TrimSpace(currency))
		if currency == "" {
			skipped++
			continue
		}
		account := accounts.resolve(custody, portfolio)
		valueAt := sql.NullInt64{Int64: valueDate, Valid: true}
		lot := canonical.RealizedLotChange{
			RealizedLotExternalID: realizedID("trade", txID, custody),
			AccountExternalID:     account,
			Description:           silver.StrPtrIfNonEmpty(name),
			DocumentKind:          canonical.RealizedTrade,
			TaxYear:               taxYear(tradeDate, valueAt, valueDate),
			DisposalDate:          silver.DatePtrFromNullUnix(tradeDate),
			SettlementDate:        silver.DatePtrFromNullUnix(valueAt),
			Currency:              currency,
			Quantity:              absDecimal(quantity),
			Proceeds:              absDecimal(value),
			RealizedGainLoss:      silver.DecimalPtrFromNullFloat(pl),
			Payload:               json.RawMessage(payload),
		}
		lot.InstrumentExternalID, lot.InstrumentHint = realizedInstrument(valors, strings.TrimSpace(isin), valor)
		if lot.Proceeds != nil {
			if book := lot.Proceeds.Sub(*lot.RealizedGainLoss); !book.IsNegative() {
				lot.BookValue, lot.Basis = &book, derivedAverageBasis
			}
		}
		day := valueDate
		if tradeDate.Valid {
			day = tradeDate.Int64
		}
		if !coveredOn(covered[account], day) {
			uncovered[lot.RealizedLotExternalID] = true
		}
		lots = append(lots, lot)
	}
	if skipped > 0 {
		log.Printf("ubs adapter: skipped %d exported sale(s) stating no valuation currency", skipped)
	}
	return lots, uncovered, rows.Err()
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

// taxYear is the year of the first stated date: the trade, the value
// date, the document.
func taxYear(trade, value sql.NullInt64, document int64) int {
	at := document
	switch {
	case trade.Valid:
		at = trade.Int64
	case value.Valid:
		at = value.Int64
	}
	return time.Unix(at, 0).UTC().Year()
}

// absDecimal is a stated figure's magnitude, or nil when none is
// stated.
func absDecimal(f sql.NullFloat64) *canonical.Decimal {
	if !f.Valid {
		return nil
	}
	d := canonical.NewDecimalFromFloat(math.Abs(f.Float64))
	return &d
}
