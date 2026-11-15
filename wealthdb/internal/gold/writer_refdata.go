package gold

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The reference records' inserts (canonical/refdata.go). Each table is
// windowed by as_of, which the load's windowed delete has cleared first,
// so every one is a plain insert but the dividend calendar's: a later
// statement of a distribution moves its as_of past the window of the
// load that wrote the earlier one, and replaces that row.

// InsertReference writes every reference record of a batch.
func (w *Writer) InsertReference(ctx context.Context, b *canonical.SnapshotBatch) error {
	inserts := []func() error{
		func() error { return w.insertFundCompositions(ctx, b.FundCompositions) },
		func() error { return w.insertFundLines(ctx, b.FundLines) },
	}
	for _, t := range canonical.RefTables {
		for _, r := range b.Reference {
			if r.Table == t {
				inserts = append(inserts, func() error { return w.insertRefRows(ctx, r) })
			}
		}
	}
	inserts = append(inserts,
		func() error { return w.insertRateObservations(ctx, b.RateObservations) },
		func() error { return w.insertStatedPeriods(ctx, b.StatedPeriods) },
		func() error { return w.insertExposureReports(ctx, b.ExposureReports) })
	for _, insert := range inserts {
		if err := insert(); err != nil {
			return err
		}
	}
	return nil
}

// insertRefRows writes one table's rows of a batch (canonical.RefTables),
// each a value per column after the source's id. A value outside its
// column's vocabulary (RefTable.Enums) is refused. A table read whole by
// a group (RefTable.WholeBy) replaces each group's rows: the first batch
// of a writer to name a group clears its rows first, so gold drops what
// silver no longer holds, such as a distribution whose days the source
// corrected, and each row replaces one of the same key an earlier
// statement wrote.
func (w *Writer) insertRefRows(ctx context.Context, r canonical.RefRows) error {
	t := r.Table
	if len(r.Rows) == 0 {
		return nil
	}
	values := func(row []any, cols []string) []any {
		out := make([]any, len(cols))
		for i, c := range cols {
			out[i] = row[t.Col(c)]
		}
		return out
	}
	keyText := func(row []any) string {
		parts := make([]string, len(t.Key))
		for i, v := range values(row, t.Key) {
			parts[i] = fmt.Sprint(v)
		}
		return strings.Join(parts, ", ")
	}
	for i, row := range r.Rows {
		for _, c := range t.Cols {
			allowed, ok := t.Enums[c.Gold]
			if v := row[t.Col(c.Gold)]; ok && v != nil && !slices.Contains(allowed, v.(string)) {
				return fmt.Errorf("%s row %d (%s): invalid %s %q", t.Gold, i, keyText(row), c.Gold, v)
			}
		}
	}
	verb := "INSERT"
	if len(t.WholeBy) > 0 {
		verb = "INSERT OR REPLACE"
		if w.replaced == nil {
			w.replaced = map[[3]string]bool{}
		}
		where := make([]string, len(t.WholeBy))
		for i, c := range t.WholeBy {
			where[i] = c + " = ?"
		}
		clear := "DELETE FROM " + t.Gold + "\n WHERE silver_source_id = ? AND " + strings.Join(where, " AND ")
		for _, row := range r.Rows {
			by := values(row, t.WholeBy)
			parts := make([]string, len(by))
			for i, v := range by {
				parts[i] = fmt.Sprint(v)
			}
			group := [3]string{t.Gold, r.SilverSourceID, strings.Join(parts, "\x00")}
			if w.replaced[group] {
				continue
			}
			if _, err := w.tx.ExecContext(ctx, clear, append([]any{r.SilverSourceID}, by...)...); err != nil {
				return fmt.Errorf("%s: clear (%s): %w", t.Gold, strings.Join(parts, ", "), err)
			}
			w.replaced[group] = true
		}
	}
	cols := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		cols[i] = c.Gold
	}
	head := "\n" + verb + " INTO " + t.Gold + " (\n    silver_source_id, " + strings.Join(cols, ", ") + "\n) VALUES "
	return InsertChunked(ctx, w.tx, t.Gold, head, placeholders("silver_source_id, "+strings.Join(cols, ", ")), len(r.Rows),
		func(i int, args []any) []any {
			return append(append(args, r.SilverSourceID), r.Rows[i]...)
		})
}

func (w *Writer) insertFundCompositions(ctx context.Context, batch []canonical.FundCompositionChange) error {
	if len(batch) == 0 {
		return nil
	}
	for i := range batch {
		if !batch[i].Basis.Valid() {
			return fmt.Errorf("fund_compositions row %d: invalid basis %q", i, batch[i].Basis)
		}
		switch batch[i].Origin {
		case "", canonical.CompositionProvider, canonical.CompositionParsed:
		default:
			return fmt.Errorf("fund_compositions row %d: invalid origin %q", i, batch[i].Origin)
		}
	}
	const head = `
INSERT INTO fund_compositions (
    silver_source_id, fund_ref, as_of, basis, provider, source_kind, fund_name,
    currency, net_assets, line_count, weight_sum, units_outstanding, fetched_at, payload,
    origin, valid_until
) VALUES `
	return InsertChunked(ctx, w.tx, "fund_compositions", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			origin := r.Origin
			if origin == "" {
				origin = canonical.CompositionProvider
			}
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf, string(r.Basis), r.Provider,
				nullableEnumValue(r.SourceKind), nullable(r.FundName),
				nullable(r.Currency), nullable(r.NetAssets), r.LineCount,
				nullable(r.WeightSum), nullable(r.UnitsOutstanding),
				nullableInstant(r.FetchedAt), nullableJSON(r.Payload),
				origin, nullable(r.ValidUntil))
		})
}

func (w *Writer) insertFundLines(ctx context.Context, batch []canonical.FundLineChange) error {
	if len(batch) == 0 {
		return nil
	}
	for i := range batch {
		if !batch[i].Basis.Valid() {
			return fmt.Errorf("fund_lines row %d: invalid basis %q", i, batch[i].Basis)
		}
		if r := batch[i].Region; r != nil && (*r == canonical.Unknown ||
			!canonical.DeclaredValueValid(canonical.DimensionRegion, *r)) {
			return fmt.Errorf("fund_lines row %d: invalid region %q", i, *r)
		}
	}
	const head = `
INSERT INTO fund_lines (
    silver_source_id, fund_ref, as_of, basis, line_no, security_key,
    isin, cusip, figi, ticker, exchange, name, asset_type,
    weight, quantity, market_value, currency, country, risk_country,
    sector, industry, maturity, coupon,
    long_currency, long_amount, short_currency, short_amount,
    coupon_kind, is_default, in_arrears, paid_in_kind, exchange_rate,
    duration, modified_duration, yield_to_maturity, yield_to_worst, yield_to_call,
    real_duration, real_yield, accrual_date, lei, payload, region
) VALUES `
	return InsertChunked(ctx, w.tx, "fund_lines", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+
			`?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf, string(r.Basis), r.LineNo,
				nullableEnumValue(r.SecurityKey),
				nullable(r.ISIN), nullable(r.CUSIP), nullable(r.FIGI),
				nullable(r.Ticker), nullable(r.Exchange), nullable(r.Name),
				nullable(r.AssetType),
				nullable(r.Weight), nullable(r.Quantity), nullable(r.MarketValue),
				nullable(r.Currency), nullable(r.Country), nullable(r.RiskCountry),
				nullable(r.Sector), nullable(r.Industry),
				nullable(r.Maturity), nullable(r.Coupon),
				nullable(r.LongCurrency), nullable(r.LongAmount),
				nullable(r.ShortCurrency), nullable(r.ShortAmount),
				nullable(r.CouponKind), nullable(r.IsDefault), nullable(r.InArrears),
				nullable(r.PaidInKind), nullable(r.ExchangeRate),
				nullable(r.Duration), nullable(r.ModifiedDuration),
				nullable(r.YieldToMaturity), nullable(r.YieldToWorst), nullable(r.YieldToCall),
				nullable(r.RealDuration), nullable(r.RealYield), nullable(r.AccrualDate),
				nullable(payloadLEI(r.Payload)), nullableJSON(r.Payload), nullable(r.Region))
		})
}

func (w *Writer) insertRateObservations(ctx context.Context, batch []canonical.RateObservationChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO rate_observations (
    silver_source_id, series_id, as_of, value, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "rate_observations", head,
		`(?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.SeriesID, r.AsOf, r.Value, nullableJSON(r.Payload))
		})
}

func (w *Writer) insertStatedPeriods(ctx context.Context, batch []canonical.StatedPeriodChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO stated_periods (
    silver_source_id, account_external_id, as_of, period_start, currency,
    beginning_value, ending_value, accrual, net_additions, net_gain, income, fees,
    source_sha256, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "stated_periods", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.AccountExternalID, r.AsOf, r.PeriodStart, r.Currency,
				nullable(r.BeginningValue), nullable(r.EndingValue), nullable(r.Accrual),
				nullable(r.NetAdditions), nullable(r.NetGain), nullable(r.Income), nullable(r.Fees),
				r.SourceSHA256, nullableJSON(r.Payload))
		})
}

// insertExposureReports writes a batch's parsed splits, each
// dimension's as JSON. A dimension outside the declarations', or a value
// outside its dimension's vocabulary (canonical.DeclaredValueValid), is
// refused.
func (w *Writer) insertExposureReports(ctx context.Context, batch []canonical.ExposureReportChange) error {
	if len(batch) == 0 {
		return nil
	}
	dims := make([]string, len(batch))
	for i, r := range batch {
		for dim := range r.Dimensions {
			if !slices.Contains(canonical.DeclarationDimensions, dim) {
				return fmt.Errorf("exposure_reports row %d (%s, %d): invalid dimension %q", i,
					r.InstrumentExternalID, r.AsOf, dim)
			}
			for v := range r.Dimensions[dim] {
				if !canonical.DeclaredValueValid(dim, v) {
					return fmt.Errorf("exposure_reports row %d (%s %s, %d): invalid %s %q", i,
						r.SilverSourceID, r.InstrumentExternalID, r.AsOf, dim, v)
				}
			}
		}
		b, err := json.Marshal(r.Dimensions)
		if err != nil {
			return fmt.Errorf("exposure_reports row %d: %w", i, err)
		}
		dims[i] = string(b)
	}
	const head = `
INSERT INTO exposure_reports (
    silver_source_id, instrument_external_id, as_of, dimensions, source, note
) VALUES `
	return InsertChunked(ctx, w.tx, "exposure_reports", head, `(?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args, r.SilverSourceID, r.InstrumentExternalID, r.AsOf, dims[i],
				nullableJSON(r.Source), nullableEnumValue(r.Note))
		})
}

// payloadLEI is the LEI a fund line's payload states under its `lei`
// key (N-PORT's issuer LEI), nil where it states no text there; the
// look-through reads it as a column, as migration 0137's backfill does.
func payloadLEI(p json.RawMessage) *string {
	var m map[string]json.RawMessage
	var lei string
	if len(p) == 0 || json.Unmarshal(p, &m) != nil || json.Unmarshal(m["lei"], &lei) != nil || lei == "" {
		return nil
	}
	return &lei
}
