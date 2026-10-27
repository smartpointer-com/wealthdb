package gold

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The reference records' inserts (canonical/refdata.go). Each table is
// windowed by as_of, which the load's windowed delete has cleared first,
// so every one is a plain insert but the dividend calendar's: a later
// statement of a distribution moves its as_of past the window of the
// load that wrote the earlier one, and replaces that row.

// ReferenceTables are the tables the reference records load into, every
// one keyed by silver_source_id and windowed by as_of: a load's windowed
// delete and a reset reach them by those two columns.
var ReferenceTables = []string{
	"fund_compositions", "fund_lines", "fund_attributes",
	"fund_counterparties", "security_identities", "security_facts",
	"fund_risk", "fund_characteristics", "security_period_facts",
	"rate_observations", "dividend_calendar", "stated_periods",
	"exposure_reports",
}

// ReferenceTableKinds are the kind of record each reference table holds
// (canonical.Window.Spans), so a load's windowed delete clears a table
// over its kind's span. rate_observations, stated_periods and
// exposure_reports, which no kind names, take the window whole.
var ReferenceTableKinds = map[string]string{
	"fund_compositions": canonical.RefFundDays, "fund_lines": canonical.RefFundDays,
	"fund_attributes": canonical.RefFundDays, "fund_counterparties": canonical.RefFundDays,
	"fund_risk": canonical.RefFundDays, "fund_characteristics": canonical.RefFundDays,
	"security_identities": canonical.RefSecurities, "security_facts": canonical.RefSecurityFacts,
	"security_period_facts": canonical.RefPeriodFacts, "dividend_calendar": canonical.RefDividendEvents,
}

// InsertReference writes every reference record of a batch.
func (w *Writer) InsertReference(ctx context.Context, b *canonical.SnapshotBatch) error {
	for _, insert := range []func() error{
		func() error { return w.insertFundCompositions(ctx, b.FundCompositions) },
		func() error { return w.insertFundLines(ctx, b.FundLines) },
		func() error { return w.insertFundAttributes(ctx, b.FundAttributes) },
		func() error { return w.insertFundCounterparties(ctx, b.FundCounterparties) },
		func() error { return w.insertSecurityIdentities(ctx, b.Securities) },
		func() error { return w.insertSecurityFacts(ctx, b.SecurityFacts) },
		func() error { return w.insertFundRisk(ctx, b.FundRisk) },
		func() error { return w.insertFundCharacteristics(ctx, b.FundCharacteristics) },
		func() error { return w.insertSecurityPeriodFacts(ctx, b.SecurityPeriodFacts) },
		func() error { return w.insertRateObservations(ctx, b.RateObservations) },
		func() error { return w.insertDividendEvents(ctx, b.DividendEvents) },
		func() error { return w.insertStatedPeriods(ctx, b.StatedPeriods) },
		func() error { return w.insertExposureReports(ctx, b.ExposureReports) },
	} {
		if err := insert(); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) insertFundCompositions(ctx context.Context, batch []canonical.FundCompositionChange) error {
	if len(batch) == 0 {
		return nil
	}
	for i := range batch {
		if !batch[i].Basis.Valid() {
			return fmt.Errorf("fund_compositions row %d: invalid basis %q", i, batch[i].Basis)
		}
	}
	const head = `
INSERT INTO fund_compositions (
    silver_source_id, fund_ref, as_of, basis, provider, source_kind, fund_name,
    currency, net_assets, line_count, weight_sum, units_outstanding, fetched_at, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "fund_compositions", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf, string(r.Basis), r.Provider,
				nullableEnumValue(r.SourceKind), nullable(r.FundName),
				nullable(r.Currency), nullable(r.NetAssets), r.LineCount,
				nullable(r.WeightSum), nullable(r.UnitsOutstanding),
				nullableInstant(r.FetchedAt), nullableJSON(r.Payload))
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
    real_duration, real_yield, accrual_date, lei, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "fund_lines", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+
			`?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
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
				nullable(payloadLEI(r.Payload)), nullableJSON(r.Payload))
		})
}

func (w *Writer) insertFundAttributes(ctx context.Context, batch []canonical.FundAttributesChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO fund_attributes (
    silver_source_id, fund_ref, as_of, domicile, legal_form, ucits, replication,
    distribution, ter, hedged_currency, lending, index_name, depositary, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "fund_attributes", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf,
				nullable(r.Domicile), nullable(r.LegalForm), nullable(r.UCITS),
				nullable(r.Replication), nullable(r.Distribution), nullable(r.TER),
				nullable(r.HedgedCurrency), nullable(r.Lending),
				nullable(r.IndexName), nullable(r.Depositary), nullableJSON(r.Payload))
		})
}

func (w *Writer) insertFundCounterparties(ctx context.Context, batch []canonical.FundCounterpartyChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO fund_counterparties (
    silver_source_id, fund_ref, as_of, role, name, lei, share, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "fund_counterparties", head,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf, r.Role, r.Name,
				nullable(r.LEI), nullable(r.Share), nullableJSON(r.Payload))
		})
}

func (w *Writer) insertSecurityIdentities(ctx context.Context, batch []canonical.SecurityChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO security_identities (
    silver_source_id, security_key, as_of, provider, isin, cusip, figi, lei,
    ticker, exchange, name, security_type, listing_currency, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "security_identities", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.SecurityKey, r.AsOf, r.Provider,
				nullable(r.ISIN), nullable(r.CUSIP), nullable(r.FIGI),
				nullable(r.LEI), nullable(r.Ticker), nullable(r.Exchange),
				nullable(r.Name), nullable(r.SecurityType),
				nullable(r.ListingCurrency), nullableJSON(r.Payload))
		})
}

func (w *Writer) insertSecurityFacts(ctx context.Context, batch []canonical.SecurityFactChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO security_facts (
    silver_source_id, security_key, field, as_of, provider, value, scheme, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "security_facts", head,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.SecurityKey, r.Field, r.AsOf, r.Provider,
				r.Value, nullable(r.Scheme), nullableJSON(r.Payload))
		})
}

func (w *Writer) insertFundRisk(ctx context.Context, batch []canonical.FundRiskChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO fund_risk (
    silver_source_id, fund_ref, as_of, currency, tenor,
    dv01, dv100, credit_ig_dv01, credit_hy_dv01, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "fund_risk", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf, r.Currency, r.Tenor,
				nullable(r.DV01), nullable(r.DV100),
				nullable(r.CreditIGDV01), nullable(r.CreditHYDV01), nullableJSON(r.Payload))
		})
}

func (w *Writer) insertFundCharacteristics(ctx context.Context, batch []canonical.FundCharacteristicChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO fund_characteristics (
    silver_source_id, fund_ref, as_of, provider, field, label, value, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "fund_characteristics", head,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf, r.Provider, r.Field, r.Label,
				r.Value, nullableJSON(r.Payload))
		})
}

func (w *Writer) insertSecurityPeriodFacts(ctx context.Context, batch []canonical.SecurityPeriodFactChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO security_period_facts (
    silver_source_id, security_key, field, as_of, period_start, provider, value,
    scheme, unit, fiscal_year, fiscal_period, form, filed, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "security_period_facts", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.SecurityKey, r.Field, r.AsOf, r.PeriodStart, r.Provider, r.Value,
				nullable(r.Scheme), nullable(r.Unit), nullable(r.FiscalYear),
				nullable(r.FiscalPeriod), nullable(r.Form), nullable(r.Filed), nullableJSON(r.Payload))
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

// insertDividendEvents writes a batch's distributions, each replacing the
// row of the same distribution an earlier statement wrote. The first
// batch to name a source's ref and provider clears that pair's rows
// first: the reference stream reads each pair whole
// (silver.NewReferenceStream), so gold drops what silver no longer
// holds, such as a distribution whose days the source corrected. A kind
// or a status outside the vocabulary is refused.
func (w *Writer) insertDividendEvents(ctx context.Context, batch []canonical.DividendEventChange) error {
	if len(batch) == 0 {
		return nil
	}
	for i, r := range batch {
		if !r.Kind.Valid() {
			return fmt.Errorf("dividend_calendar row %d (%s, %s, %s): invalid kind %q", i, r.Ref, r.Provider, r.EventID,
				r.Kind)
		}
		if r.Status != nil && !r.Status.Valid() {
			return fmt.Errorf("dividend_calendar row %d (%s, %s, %s): invalid status %q", i, r.Ref, r.Provider,
				r.EventID, *r.Status)
		}
	}
	if w.replaced == nil {
		w.replaced = map[[3]string]bool{}
	}
	for _, r := range batch {
		pair := [3]string{r.SilverSourceID, r.Ref, r.Provider}
		if w.replaced[pair] {
			continue
		}
		if _, err := w.tx.ExecContext(ctx, `DELETE FROM dividend_calendar
 WHERE silver_source_id = ? AND ref = ? AND provider = ?`, pair[0], pair[1], pair[2]); err != nil {
			return fmt.Errorf("dividend_calendar: clear (%s, %s): %w", r.Ref, r.Provider, err)
		}
		w.replaced[pair] = true
	}
	const head = `
INSERT OR REPLACE INTO dividend_calendar (
    silver_source_id, ref, provider, event_id, as_of, kind, ex_day, record_day, pay_day, declared_day,
    amount, currency, status
) VALUES `
	return InsertChunked(ctx, w.tx, "dividend_calendar", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.Ref, r.Provider, r.EventID, r.AsOf, string(r.Kind),
				nullable(r.ExDay), nullable(r.RecordDay), nullable(r.PayDay), nullable(r.DeclaredDay),
				nullable(r.Amount), nullable(r.Currency), nullableEnumString(r.Status))
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
