package gold

import (
	"context"
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
}

// InsertReference writes every reference record of a batch.
func (w *Writer) InsertReference(ctx context.Context, b *canonical.SnapshotBatch) error {
	for _, insert := range []func() error{
		func() error { return w.insertFundCompositions(ctx, b.FundCompositions) },
		func() error { return w.insertFundLines(ctx, b.FundLines) },
		func() error { return w.insertFundAttributes(ctx, b.FundAttributes) },
		func() error { return w.insertFundCounterparties(ctx, b.FundCounterparties) },
		func() error { return w.insertSecurities(ctx, b.Securities) },
		func() error { return w.insertSecurityFacts(ctx, b.SecurityFacts) },
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
			return fmt.Errorf("InsertFundCompositions row %d: invalid basis %q", i, batch[i].Basis)
		}
	}
	const head = `
INSERT INTO fund_compositions (
    silver_source_id, fund_ref, as_of, basis, provider, source_kind, fund_name,
    currency, net_assets, line_count, weight_sum, units_outstanding, fetched_at, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "InsertFundCompositions", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf, string(r.Basis), r.Provider,
				nullableEnumValue(r.SourceKind), nullableString(r.FundName),
				nullableString(r.Currency), nullableDecimal(r.NetAssets), r.LineCount,
				nullableDecimal(r.WeightSum), nullableDecimal(r.UnitsOutstanding),
				nullableInstant(r.FetchedAt), nullableJSON(r.Payload))
		})
}

func (w *Writer) insertFundLines(ctx context.Context, batch []canonical.FundLineChange) error {
	if len(batch) == 0 {
		return nil
	}
	for i := range batch {
		if !batch[i].Basis.Valid() {
			return fmt.Errorf("InsertFundLines row %d: invalid basis %q", i, batch[i].Basis)
		}
	}
	const head = `
INSERT INTO fund_lines (
    silver_source_id, fund_ref, as_of, basis, line_no, security_key,
    isin, cusip, figi, ticker, exchange, name, asset_type,
    weight, quantity, market_value, currency, country, risk_country,
    sector, industry, maturity, coupon,
    long_currency, long_amount, short_currency, short_amount, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "InsertFundLines", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf, string(r.Basis), r.LineNo,
				nullableEnumValue(r.SecurityKey),
				nullableString(r.ISIN), nullableString(r.CUSIP), nullableString(r.FIGI),
				nullableString(r.Ticker), nullableString(r.Exchange), nullableString(r.Name),
				nullableString(r.AssetType),
				nullableDecimal(r.Weight), nullableDecimal(r.Quantity), nullableDecimal(r.MarketValue),
				nullableString(r.Currency), nullableString(r.Country), nullableString(r.RiskCountry),
				nullableString(r.Sector), nullableString(r.Industry),
				nullableTime(r.Maturity), nullableDecimal(r.Coupon),
				nullableString(r.LongCurrency), nullableDecimal(r.LongAmount),
				nullableString(r.ShortCurrency), nullableDecimal(r.ShortAmount),
				nullableJSON(r.Payload))
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
	return InsertChunked(ctx, w.tx, "InsertFundAttributes", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf,
				nullableString(r.Domicile), nullableString(r.LegalForm), nullableBool(r.UCITS),
				nullableString(r.Replication), nullableString(r.Distribution), nullableDecimal(r.TER),
				nullableString(r.HedgedCurrency), nullableBool(r.Lending),
				nullableString(r.IndexName), nullableString(r.Depositary), nullableJSON(r.Payload))
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
	return InsertChunked(ctx, w.tx, "InsertFundCounterparties", head,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.FundRef, r.AsOf, r.Role, r.Name,
				nullableString(r.LEI), nullableDecimal(r.Share), nullableJSON(r.Payload))
		})
}

func (w *Writer) insertSecurities(ctx context.Context, batch []canonical.SecurityChange) error {
	if len(batch) == 0 {
		return nil
	}
	const head = `
INSERT INTO security_identities (
    silver_source_id, security_key, as_of, provider, isin, cusip, figi, lei,
    ticker, exchange, name, security_type, listing_currency, payload
) VALUES `
	return InsertChunked(ctx, w.tx, "InsertSecurities", head,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.SecurityKey, r.AsOf, r.Provider,
				nullableString(r.ISIN), nullableString(r.CUSIP), nullableString(r.FIGI),
				nullableString(r.LEI), nullableString(r.Ticker), nullableString(r.Exchange),
				nullableString(r.Name), nullableString(r.SecurityType),
				nullableString(r.ListingCurrency), nullableJSON(r.Payload))
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
	return InsertChunked(ctx, w.tx, "InsertSecurityFacts", head,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, len(batch),
		func(i int, args []any) []any {
			r := &batch[i]
			return append(args,
				r.SilverSourceID, r.SecurityKey, r.Field, r.AsOf, r.Provider,
				r.Value, nullableString(r.Scheme), nullableJSON(r.Payload))
		})
}
