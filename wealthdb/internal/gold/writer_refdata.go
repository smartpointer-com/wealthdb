package gold

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The reference records' inserts (canonical/refdata.go). Each table is
// windowed by as_of, which the load's windowed delete has cleared first,
// so every one is a plain insert.

// ReferenceTables are the tables the reference records load into, every
// one keyed by silver_source_id and windowed by as_of: a load's windowed
// delete and a reset reach them by those two columns.
var ReferenceTables = []string{
	"fund_compositions", "fund_lines", "fund_attributes",
	"fund_counterparties", "security_identities", "security_facts",
	"fund_risk", "fund_characteristics", "security_period_facts",
	"rate_observations",
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
