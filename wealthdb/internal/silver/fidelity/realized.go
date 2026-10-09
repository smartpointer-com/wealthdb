package fidelity

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Realized lots (fidelity-web migrations 0010 and 0012). Silver's
// `closed_lots` holds one row per realized lot as a document states it,
// tagged by `document_kind`:
//
//   - form_1099b: the Form 1099-B lots of a Consolidated 1099. Silver
//     keeps one form per account and tax year, the one prepared
//     latest, so a corrected form has already replaced the original.
//   - closed_positions: the positions page's closed lots for the tax
//     year the collector queried.
//   - statement: a supplied statement's sales, one row per sale with
//     its settlement date and no acquired or sold date. Neither the row
//     nor its payload states a trade date or the statement's period, so
//     its tax year is the settlement year, except at a year end
//     (redateYearEndSales). The transaction cost and the specific-share
//     mark stay in the payload.
//
// The same sale in two documents is two rows, as in silver. Per
// account and tax year the best-ranked kind present is primary
// (realizedRank): the 1099-B is what was reported, the closed-positions
// page covers the years no 1099-B has reached yet, and the statements
// cover the rest.

var _ silver.RealizedLotReader = (*Connection)(nil)

// realizedRank orders the document kinds for silver.MarkPrimary.
func realizedRank(k canonical.RealizedDocKind) int {
	switch k {
	case canonical.RealizedForm1099B:
		return 0
	case canonical.RealizedClosedPositions:
		return 1
	case canonical.RealizedStatement:
		return 2
	}
	return -1
}

// RealizedLots returns every realized lot silver states, primaries
// marked. A silver from before migration 0012 names its columns
// differently and yields none until its next load migrates it; the svb
// silvers state none.
func (c *Connection) RealizedLots(ctx context.Context) ([]canonical.RealizedLotChange, error) {
	ok, err := silver.HasColumn(ctx, c.db, "closed_lots", "security_name")
	if err != nil || !ok {
		return nil, err
	}
	ids, err := c.realizedInstruments(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT lot_id, document_kind, account_external_id, tax_year, form_prepared,
       security_name, COALESCE(instrument_key, ''), cusip, action,
       CAST(quantity                AS VARCHAR),
       COALESCE(acquired_date, ''), COALESCE(disposed_date, ''),
       COALESCE(settlement_date, ''),
       CAST(proceeds                AS VARCHAR),
       CAST(cost_basis              AS VARCHAR),
       CAST(accrued_market_discount AS VARCHAR),
       CAST(wash_sale_disallowed    AS VARCHAR),
       CAST(realized_gain_loss      AS VARCHAR),
       CAST(fees                    AS VARCHAR),
       CAST(federal_tax_withheld    AS VARCHAR),
       COALESCE(term, ''), covered, form_8949_box, specific_share_id,
       corrected, currency, source_sha256, payload
  FROM closed_lots
 ORDER BY lot_id`)
	if err != nil {
		return nil, fmt.Errorf("fidelity RealizedLots: %w", err)
	}
	defer rows.Close()
	var (
		out     []canonical.RealizedLotChange
		idents  [][]string
		undated int
	)
	for rows.Next() {
		var (
			id, kind, acct, key, acquired, disposed, settled string
			term, currency, sha, payload                     string
			taxYear, covered, specificShare, corrected       sql.NullInt64
			prepared, name, cusip, action, box               sql.NullString
			qty, proceeds, cost, discount, wash, gain, fees  sql.NullString
			withheld                                         sql.NullString
		)
		if err := rows.Scan(&id, &kind, &acct, &taxYear, &prepared, &name, &key,
			&cusip, &action, &qty, &acquired, &disposed, &settled, &proceeds, &cost,
			&discount, &wash, &gain, &fees, &withheld, &term, &covered, &box,
			&specificShare, &corrected, &currency, &sha, &payload); err != nil {
			return nil, fmt.Errorf("fidelity RealizedLots scan: %w", err)
		}
		r := canonical.RealizedLotChange{
			RealizedLotExternalID: id,
			AccountExternalID:     acct,
			Description:           silver.NullStringPtr(name),
			DocumentKind:          canonical.RealizedDocKind(kind),
			AcquisitionDate:       silver.ISODate(acquired),
			AcquiredVarious:       strings.EqualFold(acquired, "Various"),
			DisposalDate:          silver.ISODate(disposed),
			SettlementDate:        silver.ISODate(settled),
			Currency:              currency,
			Quantity:              silver.AbsPtr(silver.DecimalPtrOrNil(qty)),
			Proceeds:              silver.AbsPtr(silver.DecimalPtrOrNil(proceeds)),
			RealizedGainLoss:      silver.DecimalPtrOrNil(gain),
			WashSaleDisallowed:    silver.DecimalPtrOrNil(wash),
			AccruedMarketDiscount: silver.DecimalPtrOrNil(discount),
			Term:                  canonical.ParseLotTerm(term),
			Covered:               boolPtr(covered),
			Form8949Box:           silver.NullStringPtr(box),
			SourceDocument:        silver.StrPtrIfNonEmpty(sha),
		}
		if taxYear.Valid {
			r.TaxYear = int(taxYear.Int64)
		} else if r.TaxYear = silver.TaxYearOf(r.DisposalDate, r.SettlementDate); r.TaxYear == 0 {
			undated++
			continue
		}
		r.SetBookValue(silver.AbsPtr(silver.DecimalPtrOrNil(cost)), lotBasis)
		instr, ok := ids.resolve(key)
		if ok {
			r.InstrumentExternalID = &instr
		} else {
			r.InstrumentHint = key
		}
		extra := map[string]any{}
		putText(extra, "action", action)
		putText(extra, "cusip", cusip)
		putText(extra, "form_prepared", prepared)
		putNumber(extra, "fees", fees)
		putNumber(extra, "federal_tax_withheld", withheld)
		if b := boolPtr(specificShare); b != nil {
			extra["specific_share_id"] = *b
		}
		if b := boolPtr(corrected); b != nil {
			extra["corrected"] = *b
		}
		if r.AcquisitionDate == nil && acquired != "" {
			extra["acquired_date"] = acquired
		}
		r.Payload = silver.PayloadWith(payload, extra)
		out = append(out, r)
		idents = append(idents, distinctNonEmpty(instr, key, cusip.String))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if undated > 0 {
		log.Printf("fidelity adapter: skipped %d realized lot(s) stating no tax year, sale or settlement date", undated)
	}
	redateYearEndSales(out, idents)
	silver.MarkPrimary(out, realizedRank, nil)
	return out, nil
}

// settleDays bounds how many days after its trade a sale settles:
// three business days at the longest, across a weekend and the New
// Year holiday.
const settleDays = 7

// tradeDated are the kinds that date a sale by its trade. Both rank
// above the statement.
var tradeDated = []canonical.RealizedDocKind{
	canonical.RealizedForm1099B, canonical.RealizedClosedPositions,
}

// tradeKey names the rows one document kind states as sold in one
// account, of one instrument, on one day: the trade date of a
// trade-dated kind, the settlement date of a statement.
type tradeKey struct {
	account, instrument string
	kind                canonical.RealizedDocKind
	day                 string // YYYY-MM-DD
}

// tradeSum is the quantity those rows sum to.
type tradeSum struct {
	qty  canonical.Decimal
	rows int
}

func (s *tradeSum) add(q canonical.Decimal) { s.qty, s.rows = s.qty.Add(q), s.rows+1 }

// matches reports whether two sums agree within the thousandth each
// printed figure is rounded to.
func (s *tradeSum) matches(o *tradeSum) bool {
	tolerance := canonical.NewDecimalFromFloat(0.001).Mul(canonical.NewDecimalFromInt(int64(s.rows + o.rows)))
	return !s.qty.Sub(o.qty).Abs().GreaterThan(tolerance)
}

// redateYearEndSales moves a statement sale settled in a year's first
// days to the year before when it traded then.
//
// A statement dates a sale by its settlement, the 1099-B and the
// closed-positions page by its trade. A sale traded in late December
// and settled in January therefore falls in two tax years. Where an
// account's primary kind is the same in both years, that is harmless.
// Where it differs, the settlement year counts the sale twice (the
// form's primaries in December's year, the statements' in January's)
// or not at all (the other way round).
//
// Such a sale is looked up among the trade-dated rows of its account
// and instrument sold within settleDays up to its settlement. The
// latest day on which one kind's lots sum to the sale's quantity, or
// to the quantity of all the statement's sales of the instrument that
// settle with it, is its trade date and gives its year. Without such a
// day, a sale that could have traded in December moves there when a
// trade-dated kind covers its settlement year: that kind lists every
// sale of its year, and it does not list this one. Both steps rest on
// that kind being complete for its year and naming the instrument as
// the statement does, or by a CUSIP that joins the two.
func redateYearEndSales(lots []canonical.RealizedLotChange, idents [][]string) {
	type yearKey struct {
		account string
		year    int
	}
	statement := realizedRank(canonical.RealizedStatement)
	best := map[yearKey]int{}
	sums := map[tradeKey]*tradeSum{}
	for i := range lots {
		r := &lots[i]
		rank := realizedRank(r.DocumentKind)
		if rank < 0 {
			continue
		}
		k := yearKey{r.AccountExternalID, r.TaxYear}
		if b, seen := best[k]; !seen || rank < b {
			best[k] = rank
		}
		day := r.DisposalDate
		if r.DocumentKind == canonical.RealizedStatement {
			day = r.SettlementDate
		}
		if day == nil || r.Quantity == nil {
			continue
		}
		for _, id := range idents[i] {
			tk := tradeKey{r.AccountExternalID, id, r.DocumentKind, day.Format(time.DateOnly)}
			if sums[tk] == nil {
				sums[tk] = &tradeSum{}
			}
			sums[tk].add(*r.Quantity)
		}
	}
	// tradeYear is the year of the sale's trade date as a trade-dated
	// row states it, 0 when none does.
	tradeYear := func(i int) int {
		r := &lots[i]
		settled := *r.SettlementDate
		own := &tradeSum{qty: *r.Quantity, rows: 1}
		for d := settled; !d.Before(settled.AddDate(0, 0, -settleDays)); d = d.AddDate(0, 0, -1) {
			for _, id := range idents[i] {
				together := sums[tradeKey{r.AccountExternalID, id, canonical.RealizedStatement, settled.Format(time.DateOnly)}]
				for _, kind := range tradeDated {
					s := sums[tradeKey{r.AccountExternalID, id, kind, d.Format(time.DateOnly)}]
					if s != nil && (s.matches(own) || s.matches(together)) {
						return d.Year()
					}
				}
			}
		}
		return 0
	}
	for i := range lots {
		r := &lots[i]
		if r.DocumentKind != canonical.RealizedStatement || r.SettlementDate == nil ||
			r.DisposalDate != nil || r.TaxYear != r.SettlementDate.Year() ||
			r.SettlementDate.AddDate(0, 0, -settleDays).Year() == r.TaxYear {
			continue
		}
		if r.Quantity != nil {
			if y := tradeYear(i); y != 0 {
				r.TaxYear = y
				continue
			}
		}
		if b, seen := best[yearKey{r.AccountExternalID, r.TaxYear}]; seen && b < statement {
			r.TaxYear--
		}
	}
}

// distinctNonEmpty lists the non-empty strings among ss, once each.
func distinctNonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// realizedIDs resolves a realized lot's instrument_key, a symbol or a
// CUSIP as the document prints it, to the instrument id positions and
// transactions use.
type realizedIDs struct {
	// symbolFor maps a CUSIP to the one symbol silver states beside
	// it, "" where it states several.
	symbolFor map[string]string
	// known holds the keys positions and transactions use as is.
	known map[string]struct{}
}

// resolve maps a CUSIP to its symbol where one is stated, else takes a
// key positions or transactions use as is. Anything else, such as a
// CUSIP no page or form pairs with a symbol, does not resolve.
func (r realizedIDs) resolve(key string) (string, bool) {
	if sym := r.symbolFor[key]; sym != "" {
		return sym, true
	}
	if _, ok := r.known[key]; ok && key != "" {
		return key, true
	}
	return "", false
}

// realizedInstruments reads the identity realizedIDs needs from the
// whole silver. The positions page states a holding's CUSIP beside its
// symbol, open or closed, in the identity space positions use; a
// 1099-B states it beside the symbol it prints. The page wins where
// both pair a CUSIP, so a symbol the form prints differently does not
// make it ambiguous.
func (c *Connection) realizedInstruments(ctx context.Context) (realizedIDs, error) {
	ids := realizedIDs{symbolFor: map[string]string{}, known: map[string]struct{}{}}
	hasOpen, err := silver.HasTables(ctx, c.db, "open_lots")
	if err != nil {
		return ids, err
	}
	const pair = ` cusip IS NOT NULL AND cusip <> '' AND instrument_key IS NOT NULL AND cusip <> instrument_key`
	q := `
SELECT DISTINCT CASE document_kind WHEN 'form_1099b' THEN 1 ELSE 0 END, cusip, instrument_key
  FROM closed_lots
 WHERE document_kind IN ('closed_positions', 'form_1099b') AND` + pair
	if hasOpen {
		q += `
UNION
SELECT DISTINCT 0, cusip, instrument_key FROM open_lots WHERE` + pair
	}
	q += `
ORDER BY 1`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return ids, fmt.Errorf("realizedInstruments: %w", err)
	}
	defer rows.Close()
	tierOf := map[string]int{}
	for rows.Next() {
		var (
			tier          int
			cusip, symbol string
		)
		if err := rows.Scan(&tier, &cusip, &symbol); err != nil {
			return ids, err
		}
		prev, seen := ids.symbolFor[cusip]
		switch {
		case !seen:
			ids.symbolFor[cusip], tierOf[cusip] = symbol, tier
		case tierOf[cusip] == tier && prev != symbol:
			ids.symbolFor[cusip] = ""
		}
	}
	if err := rows.Err(); err != nil {
		return ids, err
	}

	hasHist, err := c.hasHistoricalTable(ctx)
	if err != nil {
		return ids, err
	}
	q = `
SELECT instrument_key FROM positions
UNION SELECT instrument_key FROM transactions WHERE instrument_key IS NOT NULL`
	if hasHist {
		q += `
UNION SELECT instrument_key FROM historical_position_snapshots WHERE instrument_key IS NOT NULL`
	}
	keys, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return ids, fmt.Errorf("realizedInstruments: %w", err)
	}
	defer keys.Close()
	for keys.Next() {
		var k string
		if err := keys.Scan(&k); err != nil {
			return ids, err
		}
		ids.known[k] = struct{}{}
	}
	return ids, keys.Err()
}

// boolPtr reads a 0/1 silver flag, nil where NULL.
func boolPtr(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	b := n.Int64 != 0
	return &b
}
