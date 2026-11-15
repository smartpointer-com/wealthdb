package silver

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The reference tables: the silver shape of the refdata collector
// (collectors/refdata/migrations), which the synthetic kind repeats so a
// generator can write the same records. ReferenceSchema.Stream reads them
// into canonical reference records (canonical/refdata.go): compositions
// and lines into records of their own, every other table by its entry of
// canonical.RefTables.
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
//	fund_returns     one row per fund, day and month
//
// Every row carries as_of, the UTC midnight of the day it describes (for
// a period fact, its period's last day; for a distribution, the day its
// source stated it); all but lines carry run_at, the run that wrote it.
// fund_risk, characteristics, period_facts and the lines columns from
// coupon_kind on come with the refdata silver's schema v2
// (0002_yield_facts.sql), dividend_events with v3
// (0003_dividend_calendar.sql), fund_returns with v4. A file at an older
// version holds none of them, and its records read with those fields
// absent (ReferenceSchema).

// ReferenceTables are the tables every reference silver holds: the first
// six of the list above.
var ReferenceTables = []string{"compositions", "lines", "attributes", "counterparties", "securities", "security_facts"}

// ReferenceYieldTables are the tables schema v2 adds.
var ReferenceYieldTables = []string{"fund_risk", "characteristics", "period_facts"}

// ReferenceCalendarTable is the table schema v3 adds: the distributions.
const ReferenceCalendarTable = "dividend_events"

// ReferenceReturnsTable is the table schema v4 adds: a fund's monthly
// returns.
const ReferenceReturnsTable = "fund_returns"

// ReferenceTableKinds names the kind of record each reference table's
// rows are (canonical.Window.Spans). A fund's tables, lines among them,
// are its fund-days; each security table is a kind of its own.
var ReferenceTableKinds = referenceTableKinds()

func referenceTableKinds() map[string]string {
	kinds := map[string]string{"compositions": canonical.RefFundDays, "lines": canonical.RefFundDays}
	for _, t := range canonical.RefTables {
		kinds[t.Silver] = t.Kind
	}
	return kinds
}

// ReferenceSchema is which parts of the later schema versions a
// reference silver holds: each table of canonical.RefTables past v1, and
// the lines columns of v2 (ReferenceLineYieldColumns). A file last
// migrated before a version lacks its parts and reads as stating none of
// their records. ProbeReferenceSchema reads it.
type ReferenceSchema struct {
	// tables are the later versions' tables, by silver name, each with
	// whether the file holds it.
	tables    map[string]bool
	lineYield bool
}

// ProbeReferenceSchema reads which parts of the later versions db holds,
// and notes the parts a file older than them lacks (ProbeOptional), in
// the order of their versions. silver is the file's base name, for the
// note.
func ProbeReferenceSchema(ctx context.Context, db *sql.DB, silver string) (ReferenceSchema, SchemaNote, error) {
	s := ReferenceSchema{tables: map[string]bool{}}
	parts := []Optional{{Table: "lines", Columns: ReferenceLineYieldColumns, Version: 2, Has: &s.lineYield}}
	held := map[string]*bool{}
	for _, t := range canonical.RefTables {
		if t.Version > 1 {
			held[t.Silver] = new(bool)
			parts = append(parts, Optional{Table: t.Silver, Version: t.Version, Has: held[t.Silver]})
		}
	}
	slices.SortStableFunc(parts, func(a, b Optional) int { return a.Version - b.Version })
	note, err := ProbeOptional(ctx, db, silver, parts...)
	for name, h := range held {
		s.tables[name] = *h
	}
	return s, note, err
}

// holds says the file holds t: a table of v1, or a later one it holds.
func (schema ReferenceSchema) holds(t *canonical.RefTable) bool {
	return t.Version <= 1 || schema.tables[t.Silver]
}

// ReferenceRunTables are the reference tables whose rows carry run_at:
// every one but lines, whose rows belong to their composition's fund-day
// and run, and only the later versions' tables the file holds (s). The
// first six of the list above come first, then canonical.RefTables'
// later tables in its order.
func ReferenceRunTables(s ReferenceSchema) []string {
	tables := slices.Clone(ReferenceTables)
	for _, t := range canonical.RefTables {
		if t.Version > 1 && s.holds(t) {
			tables = append(tables, t.Silver)
		}
	}
	return slices.DeleteFunc(tables, func(t string) bool { return t == "lines" })
}

// ReferenceLineYieldColumns are the lines columns schema v2 adds, in the
// order readLines scans them.
var ReferenceLineYieldColumns = []string{"coupon_kind", "is_default", "in_arrears", "paid_in_kind",
	"exchange_rate", "duration", "modified_duration", "yield_to_maturity", "yield_to_worst", "yield_to_call",
	"real_duration", "real_yield", "accrual_date"}

// ReferenceRecordBudget bounds the records of one batch of fund-days:
// its compositions, lines and the rows of every fund table. A batch
// holds whole fund-days, so a fund-day with more records than this rides
// alone. It is a variable so a test can lower it.
var ReferenceRecordBudget = 20000

// ReferenceTailBudget bounds the security records of one batch: a page
// of one security table. It is a variable so a test can lower it.
var ReferenceTailBudget = 5000

// fundDay is one fund on one day: the unit a batch holds whole.
type fundDay struct {
	ref  string
	asOf int64
}

// referenceStream emits the reference rows of a window: the fund-days in
// order, each with its compositions, lines and the rows of every fund
// table, then each security table (canonical.RefTables) a page at a
// time. It reads as it goes, so a window of years holds one batch in
// memory at a time.
type referenceStream struct {
	db *sql.DB
	w  canonical.Window
	// lineYieldSelect is the select list of the v2 lines columns, a NULL
	// for each where the file has none.
	lineYieldSelect string
	// fundTables are the fund tables the file holds, each with the query
	// of one fund-day's rows.
	fundTables []refQuery
	days       []fundDay
	next       int
	// pages are the security tables, each with its query and cursor, in
	// the order the stream reads them.
	pages []refPage
}

// refQuery is a reference table and the query that reads its rows.
type refQuery struct {
	t *canonical.RefTable
	q string
}

// refPage is a security table read a page at a time.
type refPage struct {
	refQuery
	k keyset
}

// Stream streams the reference rows of a file of schema s whose as_of
// lies in w, each kind of record over its own span of w
// (canonical.Window.SpanOf), and the rows of a table read whole by a group
// (canonical.RefTable's WholeBy) for each group a row of the span names.
// Nothing is read for a window without changes, nor for a kind without.
func (schema ReferenceSchema) Stream(ctx context.Context, db *sql.DB, w canonical.Window) (SnapshotStream, error) {
	if !w.HasChanges {
		return NewSnapshotStream(nil), nil
	}
	s := &referenceStream{db: db, w: w, lineYieldSelect: ", " + ColumnOr(schema.lineYield, ReferenceLineYieldColumns...)}
	for _, t := range canonical.RefTables {
		if !schema.holds(t) {
			continue
		}
		if t.Kind == canonical.RefFundDays {
			s.fundTables = append(s.fundTables, refQuery{t, fundDayQuery(t)})
			continue
		}
		s.pages = append(s.pages, refPage{refQuery{t, pageQuery(t)}, keyset{after: firstKey(t), done: !w.Changes(t.Kind)}})
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
	var err error
	s.days, err = Query(ctx, db, "reference fund-days", strings.Join(parts, "\nUNION ")+`
ORDER BY 2, 1`, func(rows *sql.Rows) (fundDay, error) {
		var d fundDay
		if err := rows.Scan(&d.ref, &d.asOf); err != nil {
			return d, fmt.Errorf("reference fund-days scan: %w", err)
		}
		return d, nil
	}, days.Start, days.End)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// columnList is t's silver columns, comma-separated.
func columnList(t *canonical.RefTable) string {
	names := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// fundDayQuery reads one fund-day's rows of a fund table, ordered by its
// key.
func fundDayQuery(t *canonical.RefTable) string {
	q := "SELECT " + columnList(t) + "\n  FROM " + t.Silver + " WHERE fund_ref = ? AND as_of = ?"
	if len(t.Key) > 0 {
		q += " ORDER BY " + strings.Join(t.Key, ", ")
	}
	return q
}

// pageQuery reads a page of a security table past its cursor (readPage).
// A table read whole by a group takes every row of each group a row of
// the span names.
func pageQuery(t *canonical.RefTable) string {
	key := strings.Join(t.Key, ", ")
	where := "as_of BETWEEN ? AND ?"
	from := t.Silver
	if len(t.WholeBy) > 0 {
		from += " d"
		same := make([]string, len(t.WholeBy))
		for i, c := range t.WholeBy {
			same[i] = "c." + c + " = d." + c
		}
		where = "EXISTS (SELECT 1 FROM " + t.Silver + " c\n                WHERE " + strings.Join(same, " AND ") +
			" AND c.as_of BETWEEN ? AND ?)"
	}
	return "SELECT " + columnList(t) + "\n  FROM " + from + "\n WHERE " + where +
		" AND (" + key + ") > (" + strings.TrimSuffix(strings.Repeat("?, ", len(t.Key)), ", ") + ")" +
		"\n ORDER BY " + key + " LIMIT ?"
}

// firstKey is the cursor before a security table's first row: -1 for an
// integer key column, "" for a text one.
func firstKey(t *canonical.RefTable) []any {
	after := make([]any, len(t.Key))
	for i, k := range t.Key {
		after[i] = ""
		if t.Cols[t.Col(k)].Kind == canonical.RefIntReq {
			after[i] = int64(-1)
		}
	}
	return after
}

func (s *referenceStream) Close() error { return nil }

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
	// One page a batch, of the first table not yet read through.
	read, more := false, false
	for i := range s.pages {
		p := &s.pages[i]
		if p.k.done {
			continue
		}
		if !read {
			read = true
			span, _ := s.w.SpanOf(p.t.Kind)
			rows, err := readPage(ctx, s.db, span, ReferenceTailBudget, "reference "+p.t.What, p.q, &p.k,
				func(rows *sql.Rows) ([]any, []any, error) { return scanRef(rows, p.t, true) })
			if err != nil {
				return b, false, err
			}
			for _, r := range rows {
				b.AppendRef(p.t, r)
			}
		}
		more = more || !p.k.done
	}
	return b, more, nil
}

func (s *referenceStream) readFundDay(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	if err := s.readCompositions(ctx, d, b); err != nil {
		return err
	}
	if err := s.readLines(ctx, d, b); err != nil {
		return err
	}
	for _, f := range s.fundTables {
		if err := s.readFundRows(ctx, f, d, b); err != nil {
			return err
		}
	}
	return nil
}

func (s *referenceStream) readCompositions(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	return ScanRows(ctx, s.db, "reference compositions", `
SELECT basis, provider, source_kind, fund_name, currency, net_assets, line_count,
       weight_sum, units_outstanding, run_at, payload
  FROM compositions WHERE fund_ref = ? AND as_of = ? ORDER BY basis`, func(rows *sql.Rows) error {
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
		return nil
	}, d.ref, d.asOf)
}

func (s *referenceStream) readLines(ctx context.Context, d fundDay, b *canonical.SnapshotBatch) error {
	return ScanRows(ctx, s.db, "reference lines", `
SELECT basis, line_no, isin, cusip, figi, ticker, exchange, name, asset_type,
       weight, quantity, market_value, currency, country, risk_country, sector, industry,
       maturity, coupon, long_currency, long_amount, short_currency, short_amount, payload`+s.lineYieldSelect+`
  FROM lines WHERE fund_ref = ? AND as_of = ? ORDER BY basis, line_no`, func(rows *sql.Rows) error {
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
		return nil
	}, d.ref, d.asOf)
}

// readFundRows reads a fund-day's rows of one fund table.
func (s *referenceStream) readFundRows(ctx context.Context, f refQuery, d fundDay, b *canonical.SnapshotBatch) error {
	return ScanRows(ctx, s.db, "reference "+f.t.What, f.q, func(rows *sql.Rows) error {
		row, _, err := scanRef(rows, f.t, false)
		if err != nil {
			return err
		}
		b.AppendRef(f.t, row)
		return nil
	}, d.ref, d.asOf)
}

// scanRef reads one row of t into a value per column (canonical.RefRows),
// and, where key is set, its cursor: the key columns as silver holds
// them. A value that fails to read, as its column's kind says, fails the
// row with an error that names it (canonical.RefTable's Record).
func scanRef(rows *sql.Rows, t *canonical.RefTable, key bool) ([]any, []any, error) {
	dest := make([]any, len(t.Cols))
	for i, c := range t.Cols {
		switch c.Kind {
		case canonical.RefTextReq, canonical.RefDecimalReq, canonical.RefDateReq:
			dest[i] = new(string)
		case canonical.RefIntReq:
			dest[i] = new(int64)
		case canonical.RefInt:
			dest[i] = new(sql.NullInt64)
		case canonical.RefBool:
			dest[i] = new(sql.NullBool)
		default:
			dest[i] = new(sql.NullString)
		}
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, nil, fmt.Errorf("reference %s scan: %w", t.What, err)
	}
	// raw is a column's value as silver holds it.
	raw := func(name string) any {
		switch v := dest[t.Col(name)].(type) {
		case *string:
			return *v
		case *int64:
			return *v
		}
		return nil
	}
	record := func() string {
		args := make([]any, len(t.RecordBy))
		for i, c := range t.RecordBy {
			args[i] = raw(c)
		}
		return fmt.Sprintf(t.Record, args...)
	}
	row := make([]any, len(t.Cols))
	for i, c := range t.Cols {
		v, err := refValue(c, dest[i])
		if err != nil {
			return nil, nil, fmt.Errorf("reference %s: %w", record(), err)
		}
		row[i] = v
	}
	var cursor []any
	if key {
		cursor = make([]any, len(t.Key))
		for i, k := range t.Key {
			cursor[i] = raw(k)
		}
	}
	return row, cursor, nil
}

// errNoDate is a column's text that is no date.
type errNoDate struct{ column, text string }

func (e errNoDate) Error() string { return fmt.Sprintf("%s %q is no date", e.column, e.text) }

// refValue reads a scanned column into its value, as its kind says: nil
// for NULL, or for what the kind reads as none. The error of a value that
// fails to read names the column and its text.
func refValue(c canonical.RefColumn, scanned any) (any, error) {
	switch v := scanned.(type) {
	case *string:
		switch c.Kind {
		case canonical.RefDecimalReq:
			d, err := canonical.NewDecimalFromString(*v)
			if err != nil {
				return nil, decimalErr(c, *v, err)
			}
			return d, nil
		case canonical.RefDateReq:
			d := ISODate(*v)
			if d == nil {
				return nil, errNoDate{c.Name, *v}
			}
			return *d, nil
		}
		return *v, nil
	case *int64:
		return *v, nil
	case *sql.NullInt64:
		if !v.Valid {
			return nil, nil
		}
		return v.Int64, nil
	case *sql.NullBool:
		if !v.Valid {
			return nil, nil
		}
		return v.Bool, nil
	}
	ns := *scanned.(*sql.NullString)
	switch c.Kind {
	case canonical.RefDecimal:
		if d := DecimalPtrOrNil(ns); d != nil {
			return *d, nil
		}
	case canonical.RefDecimalStrict:
		if strings.TrimSpace(ns.String) == "" {
			return nil, nil
		}
		d, err := canonical.NewDecimalFromString(ns.String)
		if err != nil {
			return nil, decimalErr(c, ns.String, err)
		}
		return d, nil
	case canonical.RefDate:
		if d := ISODate(ns.String); d != nil {
			return *d, nil
		}
	case canonical.RefDateStrict:
		if strings.TrimSpace(ns.String) == "" {
			return nil, nil
		}
		d := ISODate(ns.String)
		if d == nil {
			return nil, errNoDate{c.Name, ns.String}
		}
		return *d, nil
	case canonical.RefJSON:
		if p := JSONOrNil(ns); p != nil {
			return []byte(p), nil
		}
	default:
		if ns.Valid {
			return ns.String, nil
		}
	}
	return nil, nil
}

// decimalErr is the error of a column's text that is no decimal: the
// parse's own where the column is Bare, else naming the column and text.
func decimalErr(c canonical.RefColumn, text string, err error) error {
	if c.Bare {
		return err
	}
	return fmt.Errorf("%s %q: %w", c.Name, text, err)
}

func boolPtr(n sql.NullBool) *bool {
	if !n.Valid {
		return nil
	}
	v := n.Bool
	return &v
}
