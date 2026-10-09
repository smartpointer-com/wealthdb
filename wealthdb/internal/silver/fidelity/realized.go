package fidelity

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

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
//     its settlement date and no acquired or sold date. Its tax year
//     is the settlement year; the transaction cost and the specific-
//     share mark stay in the payload.
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
			AcquisitionDate:       isoDate(acquired),
			AcquiredVarious:       strings.EqualFold(acquired, "Various"),
			DisposalDate:          isoDate(disposed),
			SettlementDate:        isoDate(settled),
			Currency:              currency,
			Quantity:              magnitude(qty),
			Proceeds:              magnitude(proceeds),
			BookValue:             magnitude(cost),
			RealizedGainLoss:      silver.DecimalPtrOrNil(gain),
			WashSaleDisallowed:    silver.DecimalPtrOrNil(wash),
			AccruedMarketDiscount: silver.DecimalPtrOrNil(discount),
			Term:                  lotTerm(term),
			Covered:               boolPtr(covered),
			Form8949Box:           silver.NullStringPtr(box),
			SourceDocument:        silver.StrPtrIfNonEmpty(sha),
		}
		switch {
		case taxYear.Valid:
			r.TaxYear = int(taxYear.Int64)
		case r.DisposalDate != nil:
			r.TaxYear = r.DisposalDate.Year()
		case r.SettlementDate != nil:
			r.TaxYear = r.SettlementDate.Year()
		default:
			undated++
			continue
		}
		if r.BookValue != nil {
			r.Basis = lotBasis
		}
		if instr, ok := ids.resolve(key); ok {
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
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if undated > 0 {
		log.Printf("fidelity adapter: skipped %d realized lot(s) stating no tax year, sale or settlement date", undated)
	}
	silver.MarkPrimary(out, realizedRank, nil)
	return out, nil
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

// magnitude parses a quantity or an amount gold holds as a magnitude.
func magnitude(s sql.NullString) *canonical.Decimal {
	d := silver.DecimalPtrOrNil(s)
	if d != nil {
		*d = d.Abs()
	}
	return d
}

// boolPtr reads a 0/1 silver flag, nil where NULL.
func boolPtr(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	b := n.Int64 != 0
	return &b
}
