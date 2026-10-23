package silver

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The reference tables: the silver shape of the refdata collector
// (collectors/refdata/migrations), which the synthetic kind repeats so a
// generator can write the same records. NewReferenceStream reads them
// into canonical reference records (canonical/refdata.go).
//
//	compositions     one row per fund, day and basis
//	lines            one row per composition line
//	attributes       one row per fund and day
//	counterparties   one row per fund, day, role and name
//	securities       one row per security, lookup day and provider
//	security_facts   one row per security, field, lookup day and provider
//	fund_risk        one row per fund, day, currency and tenor
//	characteristics  one row per fund, day, provider, field and label
//	period_facts     one row per security, field, period and provider
//	dividend_events  one row per fund or security, provider and distribution
//
// Every row carries as_of, the UTC midnight of the day it describes (for
// a period fact, its period's last day; for a distribution, the day its
// source stated it); all but lines carry run_at, the run that wrote it.
// fund_risk, characteristics, period_facts and the lines columns from
// coupon_kind on come with the refdata silver's schema v2
// (0002_yield_facts.sql), dividend_events with v3
// (0003_dividend_calendar.sql). A file at an older version holds none of
// them, and its records read with those fields absent.

// ReferenceTables are the tables every reference silver holds: the first
// six of the list above.
var ReferenceTables = []string{"compositions", "lines", "attributes", "counterparties", "securities", "security_facts"}

// ReferenceYieldTables are the tables schema v2 adds.
var ReferenceYieldTables = []string{"fund_risk", "characteristics", "period_facts"}

// ReferenceCalendarTable is the table schema v3 adds: the distributions.
const ReferenceCalendarTable = "dividend_events"

// ReferenceTableKinds names the kind of record each reference table's
// rows are (canonical.Window.Spans). A fund's tables, lines among them,
// are its fund-days; each security table is a kind of its own.
var ReferenceTableKinds = map[string]string{
	"compositions": canonical.RefFundDays, "lines": canonical.RefFundDays,
	"attributes": canonical.RefFundDays, "counterparties": canonical.RefFundDays,
	"fund_risk": canonical.RefFundDays, "characteristics": canonical.RefFundDays,
	"securities": canonical.RefSecurities, "security_facts": canonical.RefSecurityFacts,
	"period_facts": canonical.RefPeriodFacts, ReferenceCalendarTable: canonical.RefDividendEvents,
}

// ReferenceSchema says which of the later schema versions' tables a
// reference silver holds: v2's (ReferenceYieldTables) and v3's
// (ReferenceCalendarTable). A file last migrated before a version lacks
// its tables and reads as stating none of their records.
type ReferenceSchema struct{ Yield, Calendar bool }

// ProbeReferenceSchema reads which of the later versions' tables db
// holds.
func ProbeReferenceSchema(ctx context.Context, db *sql.DB) (ReferenceSchema, error) {
	var s ReferenceSchema
	var err error
	if s.Yield, err = HasTables(ctx, db, ReferenceYieldTables...); err != nil {
		return s, err
	}
	s.Calendar, err = HasTables(ctx, db, ReferenceCalendarTable)
	return s, err
}

// ReferenceRunTables are the reference tables whose rows carry run_at,
// in the order of the list above: every one but lines, whose rows belong
// to their composition's fund-day and run, and only the later versions'
// tables the file holds (s).
func ReferenceRunTables(s ReferenceSchema) []string {
	tables := slices.Clone(ReferenceTables)
	if s.Yield {
		tables = append(tables, ReferenceYieldTables...)
	}
	if s.Calendar {
		tables = append(tables, ReferenceCalendarTable)
	}
	return slices.DeleteFunc(tables, func(t string) bool { return t == "lines" })
}

// ReferenceLineYieldColumns are the lines columns schema v2 adds, in the
// order readLines scans them.
var ReferenceLineYieldColumns = []string{"coupon_kind", "is_default", "in_arrears", "paid_in_kind",
	"exchange_rate", "duration", "modified_duration", "yield_to_maturity", "yield_to_worst", "yield_to_call",
	"real_duration", "real_yield", "accrual_date"}

// ReferenceRecordBudget bounds the records of one batch of fund-days:
// its compositions, lines, attributes, counterparties, risk rows and
// characteristics. A batch holds whole fund-days, so a fund-day with
// more records than this rides alone. It is a variable so a test can
// lower it.
var ReferenceRecordBudget = 20000

// ReferenceTailBudget bounds the security records of one batch: a page
// of identities, of facts, of period facts or of dividend events. It is a
// variable so a test can lower it.
var ReferenceTailBudget = 5000

// fundDay is one fund on one day: the unit a batch holds whole.
type fundDay struct {
	ref  string
	asOf int64
}

// referenceStream emits the reference rows of a window: the fund-days in
// order, each with its compositions, lines, attributes, counterparties,
// risk rows and characteristics, then the security identities, the
// security facts, the period facts and the dividend events, a page at a
// time. It reads as it goes, so a window of years holds one batch in
// memory at a time.
type referenceStream struct {
	db *sql.DB
	w  canonical.Window
	// yield says the file holds the v2 tables. lineYieldSelect is the
	// select list of the v2 lines columns, a NULL for each where the
	// file has none.
	yield           bool
	lineYieldSelect string
	days            []fundDay
	next            int
	// The cursors of the three security tables and the distributions.
	secs, facts, periods, events keyset
}

// NewReferenceStream streams the reference rows whose as_of lies in w,
// each kind of record over its own span of w (canonical.Window.SpanOf),
// and the distributions of each ref and provider whole (readEvents).
// Nothing is read for a window without changes, nor for a kind without.
func NewReferenceStream(ctx context.Context, db *sql.DB, w canonical.Window) (SnapshotStream, error) {
	if !w.HasChanges {
		return NewSnapshotStream(nil), nil
	}
	schema, err := ProbeReferenceSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	hasLineYield, err := HasColumns(ctx, db, "lines", ReferenceLineYieldColumns...)
	if err != nil {
		return nil, err
	}
	s := &referenceStream{db: db, w: w, yield: schema.Yield,
		lineYieldSelect: strings.Repeat(", NULL", len(ReferenceLineYieldColumns)),
		secs:            keyset{after: []any{"", int64(-1), ""}, done: !w.Changes(canonical.RefSecurities)},
		facts:           keyset{after: []any{"", "", int64(-1), ""}, done: !w.Changes(canonical.RefSecurityFacts)},
		periods: keyset{after: []any{"", "", int64(-1), "", ""},
			done: !schema.Yield || !w.Changes(canonical.RefPeriodFacts)},
		events: keyset{after: []any{"", "", ""}, done: !schema.Calendar || !w.Changes(canonical.RefDividendEvents)}}
	if hasLineYield {
		s.lineYieldSelect = ", " + strings.Join(ReferenceLineYieldColumns, ", ")
	}
	days, ok := w.SpanOf(canonical.RefFundDays)
	if !ok {
		return s, nil
	}
	// The fund-days are those a fund-day table's rows name; a line's is
	// its composition's.
	var parts []string
	for _, t := range ReferenceRunTables(schema) {
		if ReferenceTableKinds[t] == canonical.RefFundDays {
			parts = append(parts, "SELECT fund_ref, as_of FROM "+t+" WHERE as_of BETWEEN ?1 AND ?2")
		}
	}
	rows, err := db.QueryContext(ctx, strings.Join(parts, "\nUNION ")+`
ORDER BY 2, 1`, days.Start, days.End)
	if err != nil {
		return nil, fmt.Errorf("reference fund-days: %w", err)
	}
	defer rows.Close()
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

// span is the span the window's records of kind change over; a kind
// without changes has its keyset done before any read.
func (s *referenceStream) span(kind string) canonical.Span {
	span, _ := s.w.SpanOf(kind)
	return span
}

func (s *referenceStream) Next(ctx context.Context) (canonical.SnapshotBatch, bool, error) {
	var b canonical.SnapshotBatch
	if s.next < len(s.days) {
		for s.next < len(s.days) && b.ReferenceRows() < ReferenceRecordBudget {
			if err := s.readFundDay(ctx, s.days[s.next], &b); err != nil {
				return b, false, err
			}
			s.next++
		}
		return b, true, nil
	}
	var err error
	switch {
	case !s.secs.done:
		b.Securities, err = s.readSecurities(ctx)
	case !s.facts.done:
		b.SecurityFacts, err = s.readFacts(ctx)
	case !s.periods.done:
		b.SecurityPeriodFacts, err = s.readPeriodFacts(ctx)
	case !s.events.done:
		b.DividendEvents, err = s.readEvents(ctx)
	}
	return b, !s.secs.done || !s.facts.done || !s.periods.done || !s.events.done, err
}

func (s *referenceStream) readFundDay(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	readers := []func(context.Context, fundDay, *canonical.SnapshotBatch) error{
		s.readCompositions, s.readLines, s.readAttributes, s.readCounterparties,
	}
	if s.yield {
		readers = append(readers, s.readFundRisk, s.readCharacteristics)
	}
	for _, read := range readers {
		if err := read(ctx, d, b); err != nil {
			return err
		}
	}
	return nil
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
       maturity, coupon, long_currency, long_amount, short_currency, short_amount, payload`+s.lineYieldSelect+`
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
			couponKind, exRate, accrual                      sql.NullString
			dur, modDur, ytm, ytw, ytc, realDur, realYield   sql.NullString
			isDefault, inArrears, paidInKind                 sql.NullBool
		)
		if err := rows.Scan(&basis, &lineNo, &isin, &cusip, &figi, &ticker, &exch, &name, &assetType,
			&weight, &qty, &mv, &ccy, &country, &risk, &sector, &industry,
			&maturity, &coupon, &longCcy, &longAmt, &shortCcy, &shortAmt, &payload,
			&couponKind, &isDefault, &inArrears, &paidInKind, &exRate,
			&dur, &modDur, &ytm, &ytw, &ytc, &realDur, &realYield, &accrual); err != nil {
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
			CouponKind: NullStringPtr(couponKind), IsDefault: boolPtr(isDefault), InArrears: boolPtr(inArrears),
			PaidInKind: boolPtr(paidInKind), ExchangeRate: DecimalPtrOrNil(exRate),
			Duration: DecimalPtrOrNil(dur), ModifiedDuration: DecimalPtrOrNil(modDur),
			YieldToMaturity: DecimalPtrOrNil(ytm), YieldToWorst: DecimalPtrOrNil(ytw), YieldToCall: DecimalPtrOrNil(ytc),
			RealDuration: DecimalPtrOrNil(realDur), RealYield: DecimalPtrOrNil(realYield),
			AccrualDate: ISODate(accrual.String),
			Payload:     JSONOrNil(payload),
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

func (s *referenceStream) readFundRisk(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT currency, tenor, dv01, dv100, credit_ig_dv01, credit_hy_dv01, payload
  FROM fund_risk WHERE fund_ref = ? AND as_of = ? ORDER BY currency, tenor`, d.ref, d.asOf)
	if err != nil {
		return fmt.Errorf("reference fund risk: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ccy, tenor                   string
			dv01, dv100, ig, hy, payload sql.NullString
		)
		if err := rows.Scan(&ccy, &tenor, &dv01, &dv100, &ig, &hy, &payload); err != nil {
			return fmt.Errorf("reference fund risk scan: %w", err)
		}
		b.FundRisk = append(b.FundRisk, canonical.FundRiskChange{
			FundRef: d.ref, AsOf: d.asOf, Currency: ccy, Tenor: tenor,
			DV01: DecimalPtrOrNil(dv01), DV100: DecimalPtrOrNil(dv100),
			CreditIGDV01: DecimalPtrOrNil(ig), CreditHYDV01: DecimalPtrOrNil(hy),
			Payload: JSONOrNil(payload),
		})
	}
	return rows.Err()
}

// readCharacteristics reads a fund-day's characteristics. A value that is
// no decimal fails the load: the figure is the row's whole point.
func (s *referenceStream) readCharacteristics(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT provider, field, label, value, payload
  FROM characteristics WHERE fund_ref = ? AND as_of = ? ORDER BY provider, field, label`, d.ref, d.asOf)
	if err != nil {
		return fmt.Errorf("reference characteristics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			provider, field, label, value string
			payload                       sql.NullString
		)
		if err := rows.Scan(&provider, &field, &label, &value, &payload); err != nil {
			return fmt.Errorf("reference characteristics scan: %w", err)
		}
		v, err := canonical.NewDecimalFromString(value)
		if err != nil {
			return fmt.Errorf("reference characteristic (%s, %d, %s, %s, %q): value %q: %w",
				d.ref, d.asOf, provider, field, label, value, err)
		}
		b.FundCharacteristics = append(b.FundCharacteristics, canonical.FundCharacteristicChange{
			FundRef: d.ref, AsOf: d.asOf, Provider: provider, Field: field, Label: label,
			Value: v, Payload: JSONOrNil(payload),
		})
	}
	return rows.Err()
}

// readSecurities reads the next page of the window's security
// identities. The keys are gold's own, so they pass through as written.
func (s *referenceStream) readSecurities(ctx context.Context) ([]canonical.SecurityChange, error) {
	return readPage(ctx, s.db, s.span(canonical.RefSecurities), ReferenceTailBudget, "reference securities", `
SELECT security_key, as_of, provider, isin, cusip, figi, lei, ticker, exchange, name,
       security_type, listing_currency, payload
  FROM securities
 WHERE as_of BETWEEN ? AND ? AND (security_key, as_of, provider) > (?, ?, ?)
 ORDER BY security_key, as_of, provider LIMIT ?`, &s.secs,
		func(rows *sql.Rows) (canonical.SecurityChange, []any, error) {
			var (
				key, provider                                       string
				asOf                                                int64
				isin, cusip, figi, lei, ticker, exch, name, typ, lc sql.NullString
				payload                                             sql.NullString
			)
			if err := rows.Scan(&key, &asOf, &provider, &isin, &cusip, &figi, &lei, &ticker, &exch, &name,
				&typ, &lc, &payload); err != nil {
				return canonical.SecurityChange{}, nil, fmt.Errorf("reference securities scan: %w", err)
			}
			return canonical.SecurityChange{
				SecurityKey: key, AsOf: asOf, Provider: provider,
				ISIN: NullStringPtr(isin), CUSIP: NullStringPtr(cusip), FIGI: NullStringPtr(figi),
				LEI: NullStringPtr(lei), Ticker: NullStringPtr(ticker), Exchange: NullStringPtr(exch),
				Name: NullStringPtr(name), SecurityType: NullStringPtr(typ), ListingCurrency: NullStringPtr(lc),
				Payload: JSONOrNil(payload),
			}, []any{key, asOf, provider}, nil
		})
}

// readFacts reads the next page of the window's security facts.
func (s *referenceStream) readFacts(ctx context.Context) ([]canonical.SecurityFactChange, error) {
	return readPage(ctx, s.db, s.span(canonical.RefSecurityFacts), ReferenceTailBudget, "reference security facts", `
SELECT security_key, field, as_of, provider, value, scheme, payload
  FROM security_facts
 WHERE as_of BETWEEN ? AND ? AND (security_key, field, as_of, provider) > (?, ?, ?, ?)
 ORDER BY security_key, field, as_of, provider LIMIT ?`, &s.facts,
		func(rows *sql.Rows) (canonical.SecurityFactChange, []any, error) {
			var (
				key, field, provider, value string
				asOf                        int64
				scheme, payload             sql.NullString
			)
			if err := rows.Scan(&key, &field, &asOf, &provider, &value, &scheme, &payload); err != nil {
				return canonical.SecurityFactChange{}, nil, fmt.Errorf("reference security facts scan: %w", err)
			}
			return canonical.SecurityFactChange{
				SecurityKey: key, Field: field, AsOf: asOf, Provider: provider, Value: value,
				Scheme: NullStringPtr(scheme), Payload: JSONOrNil(payload),
			}, []any{key, field, asOf, provider}, nil
		})
}

// readPeriodFacts reads the next page of the window's period facts. A
// value that is no decimal, or a period start that is no date, fails the
// load: gold holds neither without one.
func (s *referenceStream) readPeriodFacts(ctx context.Context) ([]canonical.SecurityPeriodFactChange, error) {
	return readPage(ctx, s.db, s.span(canonical.RefPeriodFacts), ReferenceTailBudget, "reference period facts", `
SELECT security_key, field, as_of, period_start, provider, value, scheme, unit,
       fiscal_year, fiscal_period, form, filed, payload
  FROM period_facts
 WHERE as_of BETWEEN ? AND ? AND (security_key, field, as_of, period_start, provider) > (?, ?, ?, ?, ?)
 ORDER BY security_key, field, as_of, period_start, provider LIMIT ?`, &s.periods,
		func(rows *sql.Rows) (canonical.SecurityPeriodFactChange, []any, error) {
			var (
				key, field, start, provider, value         string
				asOf                                       int64
				scheme, unit, period, form, filed, payload sql.NullString
				year                                       sql.NullInt64
			)
			if err := rows.Scan(&key, &field, &asOf, &start, &provider, &value, &scheme, &unit,
				&year, &period, &form, &filed, &payload); err != nil {
				return canonical.SecurityPeriodFactChange{}, nil, fmt.Errorf("reference period facts scan: %w", err)
			}
			v, err := canonical.NewDecimalFromString(value)
			if err != nil {
				return canonical.SecurityPeriodFactChange{}, nil, fmt.Errorf(
					"reference period fact (%s, %s, %d, %s, %s): value %q: %w",
					key, field, asOf, start, provider, value, err)
			}
			first := ISODate(start)
			if first == nil {
				return canonical.SecurityPeriodFactChange{}, nil, fmt.Errorf(
					"reference period fact (%s, %s, %d, %s, %s): period_start %q is no date",
					key, field, asOf, start, provider, start)
			}
			return canonical.SecurityPeriodFactChange{
				SecurityKey: key, Field: field, AsOf: asOf, PeriodStart: *first, Provider: provider,
				Value: v, Scheme: NullStringPtr(scheme), Unit: NullStringPtr(unit),
				FiscalYear: int64Ptr(year), FiscalPeriod: NullStringPtr(period), Form: NullStringPtr(form),
				Filed: ISODate(filed.String), Payload: JSONOrNil(payload),
			}, []any{key, field, asOf, start, provider}, nil
		})
}

// readEvents reads the next page of the window's distributions: every
// row of each ref and provider that a row of the span names, whatever
// its own as_of. A provider that states a ref's whole history drops from
// silver the rows it no longer states, which leaves nothing in the span
// to say so; gold replaces each ref's and provider's rows with the ones
// read here (gold.Writer). A day or an amount that is empty reads as
// none. One that is no date or no decimal fails the load: gold would
// hold the distribution without it.
func (s *referenceStream) readEvents(ctx context.Context) ([]canonical.DividendEventChange, error) {
	return readPage(ctx, s.db, s.span(canonical.RefDividendEvents), ReferenceTailBudget, "reference dividend events", `
SELECT ref, provider, event_id, as_of, kind, ex_date, record_date, pay_date, declared_date, amount, currency, status
  FROM dividend_events d
 WHERE EXISTS (SELECT 1 FROM dividend_events c
                WHERE c.ref = d.ref AND c.provider = d.provider AND c.as_of BETWEEN ? AND ?)
   AND (ref, provider, event_id) > (?, ?, ?)
 ORDER BY ref, provider, event_id LIMIT ?`, &s.events,
		func(rows *sql.Rows) (canonical.DividendEventChange, []any, error) {
			var (
				ref, provider, id, kind               string
				asOf                                  int64
				ex, record, pay, declared, amount, cc sql.NullString
				status                                sql.NullString
			)
			if err := rows.Scan(&ref, &provider, &id, &asOf, &kind, &ex, &record, &pay, &declared, &amount, &cc,
				&status); err != nil {
				return canonical.DividendEventChange{}, nil, fmt.Errorf("reference dividend events scan: %w", err)
			}
			e := canonical.DividendEventChange{Ref: ref, Provider: provider, EventID: id, AsOf: asOf,
				Kind: canonical.DividendEventKind(kind), Currency: NullStringPtr(cc)}
			if status.Valid {
				st := canonical.DividendEventStatus(status.String)
				e.Status = &st
			}
			for _, d := range []struct {
				column string
				value  sql.NullString
				into   **time.Time
			}{{"ex_date", ex, &e.ExDay}, {"record_date", record, &e.RecordDay}, {"pay_date", pay, &e.PayDay},
				{"declared_date", declared, &e.DeclaredDay}} {
				if strings.TrimSpace(d.value.String) == "" {
					continue
				}
				if *d.into = ISODate(d.value.String); *d.into == nil {
					return canonical.DividendEventChange{}, nil, fmt.Errorf(
						"reference dividend event (%s, %s, %s): %s %q is no date", ref, provider, id, d.column, d.value.String)
				}
			}
			if strings.TrimSpace(amount.String) != "" {
				v, err := canonical.NewDecimalFromString(amount.String)
				if err != nil {
					return canonical.DividendEventChange{}, nil, fmt.Errorf(
						"reference dividend event (%s, %s, %s): amount %q: %w", ref, provider, id, amount.String, err)
				}
				e.Amount = &v
			}
			return e, []any{ref, provider, id}, nil
		})
}

func boolPtr(n sql.NullBool) *bool {
	if !n.Valid {
		return nil
	}
	v := n.Bool
	return &v
}

func int64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
