package silver

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The reference tables: the silver shape of the refdata collector
// (collectors/refdata/migrations), which the synthetic kind repeats so a
// generator can write the same records. NewReferenceStream reads them
// into canonical reference records (canonical/refdata.go).
//
//	compositions    one row per fund, day and basis
//	lines           one row per composition line
//	attributes      one row per fund and day
//	counterparties  one row per fund, day, role and name
//	securities      one row per security, lookup day and provider
//	security_facts  one row per security, field, lookup day and provider
//
// Every row carries as_of, the UTC midnight of the day it describes; all
// but lines carry run_at, the run that wrote it.

// ReferenceTables are the reference tables, as the list above names them.
var ReferenceTables = []string{"compositions", "lines", "attributes", "counterparties", "securities", "security_facts"}

// ReferenceLineBudget bounds the lines one batch carries. A batch holds
// whole fund-days, so a fund with more lines than this rides alone. It
// is a variable so a test can lower it.
var ReferenceLineBudget = 20000

// ReferenceTailBudget bounds the security records of one batch: a page
// of identities or of facts. It is a variable so a test can lower it.
var ReferenceTailBudget = 5000

// fundDay is one fund on one day: the unit a batch holds whole.
type fundDay struct {
	ref  string
	asOf int64
}

// referenceStream emits the reference rows of a window: the fund-days in
// order, each with its compositions, lines, attributes and
// counterparties, then the security identities and the security facts,
// a page at a time. It reads as it goes, so a window of years holds one
// batch in memory at a time.
type referenceStream struct {
	db   *sql.DB
	w    canonical.Window
	days []fundDay
	next int
	// The keyset cursors of the two security tables: the last
	// (security_key, as_of, provider[, field]) a page ended on.
	secAfter, factAfter []any
	secsDone, factsDone bool
}

// NewReferenceStream streams the reference rows whose as_of lies in w.
// Nothing is read for a window without changes.
func NewReferenceStream(ctx context.Context, db *sql.DB, w canonical.Window) (SnapshotStream, error) {
	if !w.HasChanges {
		return NewSnapshotStream(nil), nil
	}
	rows, err := db.QueryContext(ctx, `
SELECT fund_ref, as_of FROM compositions   WHERE as_of BETWEEN ?1 AND ?2
UNION SELECT fund_ref, as_of FROM attributes     WHERE as_of BETWEEN ?1 AND ?2
UNION SELECT fund_ref, as_of FROM counterparties WHERE as_of BETWEEN ?1 AND ?2
ORDER BY 2, 1`, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("reference fund-days: %w", err)
	}
	defer rows.Close()
	s := &referenceStream{db: db, w: w,
		secAfter: []any{"", int64(-1), ""}, factAfter: []any{"", "", int64(-1), ""}}
	for rows.Next() {
		var d fundDay
		if err := rows.Scan(&d.ref, &d.asOf); err != nil {
			return nil, fmt.Errorf("reference fund-days scan: %w", err)
		}
		s.days = append(s.days, d)
	}
	return s, rows.Err()
}

func (s *referenceStream) Close() error { return nil }

func (s *referenceStream) Next(ctx context.Context) (canonical.SnapshotBatch, bool, error) {
	var b canonical.SnapshotBatch
	if s.next < len(s.days) {
		for s.next < len(s.days) && len(b.FundLines) < ReferenceLineBudget {
			if err := s.readFundDay(ctx, s.days[s.next], &b); err != nil {
				return b, false, err
			}
			s.next++
		}
		return b, true, nil
	}
	var err error
	switch {
	case !s.secsDone:
		b.Securities, err = s.readSecurities(ctx)
	case !s.factsDone:
		b.SecurityFacts, err = s.readFacts(ctx)
	}
	return b, !s.factsDone, err
}

func (s *referenceStream) readFundDay(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	if err := s.readCompositions(ctx, d, b); err != nil {
		return err
	}
	if err := s.readLines(ctx, d, b); err != nil {
		return err
	}
	if err := s.readAttributes(ctx, d, b); err != nil {
		return err
	}
	return s.readCounterparties(ctx, d, b)
}

func (s *referenceStream) readCompositions(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT basis, provider, source_kind, fund_name, currency, net_assets, line_count,
       weight_sum, units_outstanding, run_at, payload
  FROM compositions WHERE fund_ref = ? AND as_of = ? ORDER BY basis`, d.ref, d.asOf)
	if err != nil {
		return fmt.Errorf("reference compositions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			basis, provider, kind               string
			name, ccy, net, sum, units, payload sql.NullString
			lines, runAt                        int64
		)
		if err := rows.Scan(&basis, &provider, &kind, &name, &ccy, &net, &lines, &sum, &units, &runAt, &payload); err != nil {
			return fmt.Errorf("reference compositions scan: %w", err)
		}
		b.FundCompositions = append(b.FundCompositions, canonical.FundCompositionChange{
			FundRef: d.ref, AsOf: d.asOf, Basis: canonical.CompositionBasis(basis),
			Provider: provider, SourceKind: kind,
			FundName: NullStringPtr(name), Currency: NullStringPtr(ccy),
			NetAssets: DecimalPtrOrNil(net), LineCount: lines,
			WeightSum: DecimalPtrOrNil(sum), UnitsOutstanding: DecimalPtrOrNil(units),
			FetchedAt: runAt, Payload: JSONOrNil(payload),
		})
	}
	return rows.Err()
}

func (s *referenceStream) readLines(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT basis, line_no, isin, cusip, figi, ticker, exchange, name, asset_type,
       weight, quantity, market_value, currency, country, risk_country, sector, industry,
       maturity, coupon, long_currency, long_amount, short_currency, short_amount, payload
  FROM lines WHERE fund_ref = ? AND as_of = ? ORDER BY basis, line_no`, d.ref, d.asOf)
	if err != nil {
		return fmt.Errorf("reference lines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			basis                                            string
			lineNo                                           int64
			isin, cusip, figi, ticker, exch, name, assetType sql.NullString
			payload                                          sql.NullString
			weight, qty, mv, ccy, country, risk              sql.NullString
			sector, industry, maturity, coupon               sql.NullString
			longCcy, longAmt, shortCcy, shortAmt             sql.NullString
		)
		if err := rows.Scan(&basis, &lineNo, &isin, &cusip, &figi, &ticker, &exch, &name, &assetType,
			&weight, &qty, &mv, &ccy, &country, &risk, &sector, &industry,
			&maturity, &coupon, &longCcy, &longAmt, &shortCcy, &shortAmt, &payload); err != nil {
			return fmt.Errorf("reference lines scan: %w", err)
		}
		b.FundLines = append(b.FundLines, canonical.FundLineChange{
			FundRef: d.ref, AsOf: d.asOf, Basis: canonical.CompositionBasis(basis), LineNo: lineNo,
			SecurityKey: canonical.SecurityKey(isin.String, cusip.String, figi.String, ticker.String, name.String),
			ISIN:        NullStringPtr(isin), CUSIP: NullStringPtr(cusip), FIGI: NullStringPtr(figi),
			Ticker: NullStringPtr(ticker), Exchange: NullStringPtr(exch), Name: NullStringPtr(name),
			AssetType: NullStringPtr(assetType),
			Weight:    DecimalPtrOrNil(weight), Quantity: DecimalPtrOrNil(qty), MarketValue: DecimalPtrOrNil(mv),
			Currency: NullStringPtr(ccy), Country: NullStringPtr(country), RiskCountry: NullStringPtr(risk),
			Sector: NullStringPtr(sector), Industry: NullStringPtr(industry),
			Maturity: ISODate(maturity.String), Coupon: DecimalPtrOrNil(coupon),
			LongCurrency: NullStringPtr(longCcy), LongAmount: DecimalPtrOrNil(longAmt),
			ShortCurrency: NullStringPtr(shortCcy), ShortAmount: DecimalPtrOrNil(shortAmt),
			Payload: JSONOrNil(payload),
		})
	}
	return rows.Err()
}

func (s *referenceStream) readAttributes(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT domicile, legal_form, ucits, replication, distribution, ter, hedged_currency,
       lending, index_name, depositary, payload
  FROM attributes WHERE fund_ref = ? AND as_of = ?`, d.ref, d.asOf)
	if err != nil {
		return fmt.Errorf("reference attributes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			dom, form, repl, dist, ter, hedged, idx, depo sql.NullString
			payload                                       sql.NullString
			ucits, lending                                sql.NullBool
		)
		if err := rows.Scan(&dom, &form, &ucits, &repl, &dist, &ter, &hedged, &lending, &idx, &depo, &payload); err != nil {
			return fmt.Errorf("reference attributes scan: %w", err)
		}
		b.FundAttributes = append(b.FundAttributes, canonical.FundAttributesChange{
			FundRef: d.ref, AsOf: d.asOf,
			Domicile: NullStringPtr(dom), LegalForm: NullStringPtr(form), UCITS: boolPtr(ucits),
			Replication: NullStringPtr(repl), Distribution: NullStringPtr(dist), TER: DecimalPtrOrNil(ter),
			HedgedCurrency: NullStringPtr(hedged), Lending: boolPtr(lending),
			IndexName: NullStringPtr(idx), Depositary: NullStringPtr(depo),
			Payload: JSONOrNil(payload),
		})
	}
	return rows.Err()
}

func (s *referenceStream) readCounterparties(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT role, name, lei, share, payload
  FROM counterparties WHERE fund_ref = ? AND as_of = ? ORDER BY role, name`, d.ref, d.asOf)
	if err != nil {
		return fmt.Errorf("reference counterparties: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			role, name          string
			lei, share, payload sql.NullString
		)
		if err := rows.Scan(&role, &name, &lei, &share, &payload); err != nil {
			return fmt.Errorf("reference counterparties scan: %w", err)
		}
		b.FundCounterparties = append(b.FundCounterparties, canonical.FundCounterpartyChange{
			FundRef: d.ref, AsOf: d.asOf, Role: role, Name: name,
			LEI: NullStringPtr(lei), Share: DecimalPtrOrNil(share),
			Payload: JSONOrNil(payload),
		})
	}
	return rows.Err()
}

// readSecurities reads the next page of the window's security
// identities. The keys are gold's own, so they pass through as written.
func (s *referenceStream) readSecurities(ctx context.Context) ([]canonical.SecurityChange, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT security_key, as_of, provider, isin, cusip, figi, lei, ticker, exchange, name,
       security_type, listing_currency, payload
  FROM securities
 WHERE as_of BETWEEN ? AND ? AND (security_key, as_of, provider) > (?, ?, ?)
 ORDER BY security_key, as_of, provider LIMIT ?`,
		append(append([]any{s.w.Start, s.w.End}, s.secAfter...), ReferenceTailBudget)...)
	if err != nil {
		return nil, fmt.Errorf("reference securities: %w", err)
	}
	defer rows.Close()
	var out []canonical.SecurityChange
	for rows.Next() {
		var (
			key, provider                                       string
			asOf                                                int64
			isin, cusip, figi, lei, ticker, exch, name, typ, lc sql.NullString
			payload                                             sql.NullString
		)
		if err := rows.Scan(&key, &asOf, &provider, &isin, &cusip, &figi, &lei, &ticker, &exch, &name,
			&typ, &lc, &payload); err != nil {
			return nil, fmt.Errorf("reference securities scan: %w", err)
		}
		out = append(out, canonical.SecurityChange{
			SecurityKey: key, AsOf: asOf, Provider: provider,
			ISIN: NullStringPtr(isin), CUSIP: NullStringPtr(cusip), FIGI: NullStringPtr(figi),
			LEI: NullStringPtr(lei), Ticker: NullStringPtr(ticker), Exchange: NullStringPtr(exch),
			Name: NullStringPtr(name), SecurityType: NullStringPtr(typ), ListingCurrency: NullStringPtr(lc),
			Payload: JSONOrNil(payload),
		})
		s.secAfter = []any{key, asOf, provider}
	}
	s.secsDone = len(out) < ReferenceTailBudget
	return out, rows.Err()
}

// readFacts reads the next page of the window's security facts.
func (s *referenceStream) readFacts(ctx context.Context) ([]canonical.SecurityFactChange, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT security_key, field, as_of, provider, value, scheme, payload
  FROM security_facts
 WHERE as_of BETWEEN ? AND ? AND (security_key, field, as_of, provider) > (?, ?, ?, ?)
 ORDER BY security_key, field, as_of, provider LIMIT ?`,
		append(append([]any{s.w.Start, s.w.End}, s.factAfter...), ReferenceTailBudget)...)
	if err != nil {
		return nil, fmt.Errorf("reference security facts: %w", err)
	}
	defer rows.Close()
	var out []canonical.SecurityFactChange
	for rows.Next() {
		var (
			key, field, provider, value string
			asOf                        int64
			scheme, payload             sql.NullString
		)
		if err := rows.Scan(&key, &field, &asOf, &provider, &value, &scheme, &payload); err != nil {
			return nil, fmt.Errorf("reference security facts scan: %w", err)
		}
		out = append(out, canonical.SecurityFactChange{
			SecurityKey: key, Field: field, AsOf: asOf, Provider: provider, Value: value,
			Scheme: NullStringPtr(scheme), Payload: JSONOrNil(payload),
		})
		s.factAfter = []any{key, field, asOf, provider}
	}
	s.factsDone = len(out) < ReferenceTailBudget
	return out, rows.Err()
}

func boolPtr(n sql.NullBool) *bool {
	if !n.Valid {
		return nil
	}
	v := n.Bool
	return &v
}
