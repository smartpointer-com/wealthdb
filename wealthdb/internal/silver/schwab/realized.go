package schwab

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Realized lots: the lots the year-end tax documents print
// (schwab-web `closed_lots`, silver migration 0007). Three documents
// state them, and the same sale appears in more than one: a 1099-B lot
// is also in that year's Year-End Summary, a corrected 1099 repeats
// the original, and a Year-End Summary can arrive twice. Gold keeps
// every copy and marks the set that counts each sale once
// (docs/DESIGN.md §7.4):
//
//   - the best document kind present per account and tax year: the
//     1099-B, else the Year-End Summary, else the Gain/Loss Report;
//   - within it, the latest document only (latestDocuments).
//
// The lots are not cut at the api's coverage start the way the web
// transactions are: the api states no lots, so every tax year comes
// from here.

// realizedRank orders the document kinds for silver.MarkPrimary. The
// 1099-B is what Schwab reports to the IRS; the Year-End Summary
// restates it with the gain printed; the Gain/Loss Report also covers
// accounts that get no 1099-B.
func realizedRank(k canonical.RealizedDocKind) int {
	switch k {
	case canonical.RealizedForm1099B:
		return 0
	case canonical.RealizedYearEndSummary:
		return 1
	case canonical.RealizedGainLossReport:
		return 2
	}
	return -1
}

// RealizedLots implements silver.RealizedLotReader. Every lot of a
// bridged account is returned, whatever its tax year. A lot of an
// account the api roster does not hold is dropped, like every other
// web row of it, and counted in the load log.
func (c *Connection) RealizedLots(ctx context.Context) ([]canonical.RealizedLotChange, error) {
	if c.web == nil {
		return nil, nil
	}
	ok, err := silver.HasTables(ctx, c.web.db, "closed_lots")
	if err != nil || !ok {
		return nil, err
	}
	bridge, err := c.ensureBridge(ctx)
	if err != nil {
		return nil, err
	}
	symbolBridge, err := c.ensureSymbolBridge(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.web.db.QueryContext(ctx, `
SELECT logical_doc_key, document_kind, lot_index, account_external_id,
       tax_year, security_name, cusip, instrument_key, quantity,
       acquired_date, disposed_date, proceeds, cost_basis,
       wash_sale_disallowed, accrued_market_discount, realized_gain_loss,
       term, covered, form_8949_box, source_sha256, payload
  FROM closed_lots
 ORDER BY logical_doc_key, document_kind, lot_index`)
	if err != nil {
		return nil, fmt.Errorf("schwab-web closed lots: %w", err)
	}
	defer rows.Close()

	var (
		out                              []canonical.RealizedLotChange
		docOf                            = map[string]string{} // lot id → logical_doc_key
		unbridged, unplaced, unknownKind int
	)
	for rows.Next() {
		var (
			docKey, kind, suffix  string
			lotIndex              int64
			taxYear, covered      sql.NullInt64
			name, cusip, instrKey sql.NullString
			acquired, disposed    sql.NullString
			term, box             sql.NullString
			quantity, proceeds    sql.NullFloat64
			costBasis, washSale   sql.NullFloat64
			marketDiscount, gain  sql.NullFloat64
			sha, payload          string
		)
		if err := rows.Scan(&docKey, &kind, &lotIndex, &suffix,
			&taxYear, &name, &cusip, &instrKey, &quantity,
			&acquired, &disposed, &proceeds, &costBasis,
			&washSale, &marketDiscount, &gain,
			&term, &covered, &box, &sha, &payload); err != nil {
			return nil, fmt.Errorf("schwab-web closed lots scan: %w", err)
		}
		docKind := canonical.RealizedDocKind(kind)
		if !docKind.Valid() {
			unknownKind++
			continue
		}
		hash, ok := bridge[suffix]
		if !ok {
			unbridged++
			continue
		}
		r := canonical.RealizedLotChange{
			RealizedLotExternalID: realizedLotID(docKey, kind, lotIndex),
			AccountExternalID:     hash,
			Description:           silver.StrPtrIfNonEmpty(strings.TrimSpace(name.String)),
			DocumentKind:          docKind,
			AcquisitionDate:       isoDate(acquired.String),
			AcquiredVarious:       strings.EqualFold(strings.TrimSpace(acquired.String), "Various"),
			DisposalDate:          isoDate(disposed.String),
			Currency:              "USD",
			Quantity:              magnitude(quantity),
			Proceeds:              magnitude(proceeds),
			BookValue:             magnitude(costBasis),
			RealizedGainLoss:      silver.DecimalPtrFromNullFloat(gain),
			WashSaleDisallowed:    silver.DecimalPtrFromNullFloat(washSale),
			AccruedMarketDiscount: silver.DecimalPtrFromNullFloat(marketDiscount),
			Term:                  lotTerm(term.String),
			Form8949Box:           silver.StrPtrIfNonEmpty(strings.TrimSpace(box.String)),
			SourceDocument:        silver.StrPtrIfNonEmpty(sha),
			Payload:               json.RawMessage(payload),
		}
		switch {
		case taxYear.Valid:
			r.TaxYear = int(taxYear.Int64)
		case r.DisposalDate != nil:
			r.TaxYear = r.DisposalDate.Year()
		default:
			unplaced++
			continue
		}
		if r.BookValue != nil {
			r.Basis = statementBasis
		}
		if covered.Valid {
			v := covered.Int64 == 1
			r.Covered = &v
		}
		r.InstrumentExternalID, r.InstrumentHint = realizedInstrument(instrKey.String, cusip.String, name.String, symbolBridge)
		docOf[r.RealizedLotExternalID] = docKey
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	latest := latestDocuments(out, docOf)
	silver.MarkPrimary(out, realizedRank, func(r *canonical.RealizedLotChange) bool {
		return docOf[r.RealizedLotExternalID] == latest[docSlot{r.AccountExternalID, r.TaxYear, r.DocumentKind}]
	})

	if unbridged > 0 {
		log.Printf("schwab adapter: dropped %d realized lot(s) of accounts the api roster does not hold", unbridged)
	}
	if unplaced > 0 {
		log.Printf("schwab adapter: dropped %d realized lot(s) that state neither a tax year nor a disposal date", unplaced)
	}
	if unknownKind > 0 {
		log.Printf("schwab adapter: dropped %d realized lot(s) of a document kind gold does not know", unknownKind)
	}
	return out, nil
}

// realizedLotID hashes the silver row's own key, so a lot keeps its id
// across loads and its copies in other documents get theirs.
func realizedLotID(docKey, kind string, lotIndex int64) string {
	sum := sha256.Sum256([]byte(docKey + "\x00" + kind + "\x00" + strconv.FormatInt(lotIndex, 10)))
	return "schwab-rl:" + hex.EncodeToString(sum[:16])
}

// realizedInstrument resolves the security a lot names the way the web
// transactions resolve theirs (webInstrument). The Year-End Summary
// keys a lot by CUSIP, the api's own key; the Gain/Loss Report by
// ticker; both by contract for an option. A 1099-B lot states its
// security by name alone (securityNameKey). A name of neither shape
// resolves to nothing and becomes the hint a config link can close.
func realizedInstrument(instrumentKey, cusip, securityName string, symbolToCUSIP map[string]string) (*string, string) {
	key := strings.TrimSpace(instrumentKey)
	if key == "" {
		key = strings.TrimSpace(cusip)
	}
	if key == "" {
		key, _ = securityNameKey(securityName)
	}
	if key == "" {
		return nil, strings.TrimSpace(securityName)
	}
	id := webInstrument(key, symbolToCUSIP)
	return &id, ""
}

// magnitude is a printed figure as gold's realized lots carry it,
// unsigned.
func magnitude(n sql.NullFloat64) *canonical.Decimal {
	d := silver.DecimalPtrFromNullFloat(n)
	if d == nil {
		return nil
	}
	a := d.Abs()
	return &a
}

// docSlot is where one document kind states an account's tax year.
type docSlot struct {
	account string
	year    int
	kind    canonical.RealizedDocKind
}

// latestDocuments picks, per account, tax year and document kind, the
// one document whose lots may count. A corrected 1099 repeats every lot
// of the original, and a Year-End Summary can arrive twice, as a PDF
// of its own and as the second half of the 1099 Composite: each later
// copy restates the year rather than adding to it. The latest document
// date wins (logical_doc_key is account|doc_date|filename); a tie, two
// copies dated alike, goes to the greater key so the pick is stable.
func latestDocuments(lots []canonical.RealizedLotChange, docOf map[string]string) map[docSlot]string {
	latest := map[docSlot]string{}
	for i := range lots {
		r := &lots[i]
		slot := docSlot{r.AccountExternalID, r.TaxYear, r.DocumentKind}
		doc := docOf[r.RealizedLotExternalID]
		if cur, ok := latest[slot]; !ok || laterDocument(doc, cur) {
			latest[slot] = doc
		}
	}
	return latest
}

// laterDocument reports whether document a is dated after b, or dated
// alike with the greater key.
func laterDocument(a, b string) bool {
	da, db := docDate(a), docDate(b)
	if da != db {
		return da > db
	}
	return a > b
}

// docDate reads the document date out of a logical_doc_key, as the
// collector writes it (Unix seconds); 0 when it does not parse.
func docDate(key string) int64 {
	parts := strings.SplitN(key, "|", 3)
	if len(parts) < 2 {
		return 0
	}
	d, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return 0
	}
	return d
}
