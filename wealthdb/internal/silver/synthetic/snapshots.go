package synthetic

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits one batch per distinct snapshot_at in the window, in
// ascending order. A batch carries that instant's positions, cash balances
// and fx rates, and the dimension records they reference, all seen at that
// instant: every account its positions or cash balances name, the portfolio
// each of those accounts belongs to, and every instrument its positions
// name, in the version in effect at the instant.
//
// Dimensions travel only on the snapshot stream, so an account or an
// instrument that the window's transactions reference and no snapshot in the
// window does is emitted here too, on the last batch — or on a batch of its
// own where the window holds transactions and no snapshot. Its seen range is
// the span of those transactions, and an instrument comes in the version in
// effect at the latest of them.
//
// A reference to an id its dimension table does not hold is an error: the
// silver is the canonical records themselves, so a dangling id is a defect in
// whatever wrote it, and a load that went ahead would leave gold holding a
// fact nothing describes.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	dims, err := c.readDimensions(ctx)
	if err != nil {
		return nil, err
	}

	byInstant := map[int64]*canonical.SnapshotBatch{}
	at := func(t int64) *canonical.SnapshotBatch {
		b, ok := byInstant[t]
		if !ok {
			b = &canonical.SnapshotBatch{}
			byInstant[t] = b
		}
		return b
	}
	if err := c.readPositions(ctx, w, at); err != nil {
		return nil, err
	}
	if err := c.readCashBalances(ctx, w, at); err != nil {
		return nil, err
	}
	if err := c.readFxRates(ctx, w, at); err != nil {
		return nil, err
	}

	emitted := emittedIDs{
		accounts:    map[string]bool{},
		portfolios:  map[string]bool{},
		instruments: map[string]bool{},
	}
	batches := make([]canonical.SnapshotBatch, 0, len(byInstant))
	for _, t := range slices.Sorted(maps.Keys(byInstant)) {
		b := byInstant[t]
		if err := dims.attach(b, t, emitted); err != nil {
			return nil, err
		}
		batches = append(batches, *b)
	}

	tail, err := c.transactionDimensions(ctx, w, dims, emitted)
	if err != nil {
		return nil, err
	}
	switch {
	case len(tail.Accounts)+len(tail.Portfolios)+len(tail.Instruments) == 0:
		// The snapshots already carried every dimension referenced.
	case len(batches) == 0:
		batches = append(batches, tail)
	default:
		last := &batches[len(batches)-1]
		last.Portfolios = append(last.Portfolios, tail.Portfolios...)
		last.Accounts = append(last.Accounts, tail.Accounts...)
		last.Instruments = append(last.Instruments, tail.Instruments...)
	}
	return silver.NewSnapshotStream(batches), nil
}

// emittedIDs records which dimension records the snapshot batches carry, so
// the transaction pass adds only the ones they do not.
type emittedIDs struct {
	accounts, portfolios, instruments map[string]bool
}

// attach adds to b the dimension records its facts reference, all seen at t.
func (d *dimensions) attach(b *canonical.SnapshotBatch, t int64, emitted emittedIDs) error {
	accountIDs := map[string]bool{}
	instrumentIDs := map[string]bool{}
	for _, p := range b.Positions {
		accountIDs[p.AccountExternalID] = true
		if p.InstrumentExternalID != nil {
			instrumentIDs[*p.InstrumentExternalID] = true
		}
	}
	for _, cb := range b.CashBalances {
		accountIDs[cb.AccountExternalID] = true
	}

	portfolioIDs := map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(accountIDs)) {
		a, err := d.account(id, t, t)
		if err != nil {
			return err
		}
		b.Accounts = append(b.Accounts, a)
		emitted.accounts[id] = true
		if a.PortfolioExternalID != nil {
			portfolioIDs[*a.PortfolioExternalID] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(portfolioIDs)) {
		p, err := d.portfolio(id, t, t)
		if err != nil {
			return err
		}
		b.Portfolios = append(b.Portfolios, p)
		emitted.portfolios[id] = true
	}
	for _, id := range slices.Sorted(maps.Keys(instrumentIDs)) {
		i, err := d.instrument(id, t, t, t)
		if err != nil {
			return err
		}
		b.Instruments = append(b.Instruments, i)
		emitted.instruments[id] = true
	}
	return nil
}

// seenSpan is the first and last occurred_at of the transactions that
// reference one id.
type seenSpan struct{ first, last int64 }

func (s seenSpan) widen(o seenSpan) seenSpan {
	return seenSpan{min(s.first, o.first), max(s.last, o.last)}
}

// transactionDimensions is the batch of dimension records the window's
// transactions reference and the snapshot batches did not emit.
func (c *Connection) transactionDimensions(ctx context.Context, w canonical.Window,
	d *dimensions, emitted emittedIDs) (canonical.SnapshotBatch, error) {
	var tail canonical.SnapshotBatch
	accounts, err := c.transactionSpans(ctx, w, "account_id")
	if err != nil {
		return tail, err
	}
	instruments, err := c.transactionSpans(ctx, w, "instrument_id")
	if err != nil {
		return tail, err
	}

	portfolios := map[string]seenSpan{}
	for _, id := range slices.Sorted(maps.Keys(accounts)) {
		if emitted.accounts[id] {
			continue
		}
		span := accounts[id]
		a, err := d.account(id, span.first, span.last)
		if err != nil {
			return tail, err
		}
		tail.Accounts = append(tail.Accounts, a)
		if pid := a.PortfolioExternalID; pid != nil && !emitted.portfolios[*pid] {
			if prev, ok := portfolios[*pid]; ok {
				span = span.widen(prev)
			}
			portfolios[*pid] = span
		}
	}
	for _, id := range slices.Sorted(maps.Keys(portfolios)) {
		span := portfolios[id]
		p, err := d.portfolio(id, span.first, span.last)
		if err != nil {
			return tail, err
		}
		tail.Portfolios = append(tail.Portfolios, p)
	}
	for _, id := range slices.Sorted(maps.Keys(instruments)) {
		if emitted.instruments[id] {
			continue
		}
		span := instruments[id]
		i, err := d.instrument(id, span.last, span.first, span.last)
		if err != nil {
			return tail, err
		}
		tail.Instruments = append(tail.Instruments, i)
	}
	return tail, nil
}

// transactionSpans groups the window's transactions by one reference
// column, skipping rows that leave it empty.
func (c *Connection) transactionSpans(ctx context.Context, w canonical.Window, column string) (map[string]seenSpan, error) {
	q := fmt.Sprintf(`
SELECT %[1]s, MIN(occurred_at), MAX(occurred_at)
  FROM transactions
 WHERE occurred_at BETWEEN ? AND ?
   AND %[1]s IS NOT NULL AND %[1]s <> ''
 GROUP BY %[1]s`, column)
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("synthetic transaction %s references: %w", column, err)
	}
	defer rows.Close()
	out := map[string]seenSpan{}
	for rows.Next() {
		var id string
		var span seenSpan
		if err := rows.Scan(&id, &span.first, &span.last); err != nil {
			return nil, err
		}
		out[id] = span
	}
	return out, rows.Err()
}

// ---- facts ------------------------------------------------------------------

func (c *Connection) readPositions(ctx context.Context, w canonical.Window,
	at func(int64) *canonical.SnapshotBatch) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT snapshot_at, account_id, position_key, instrument_id, asset_class, vehicle,
       currency, quantity, market_value, book_value, accrued_interest,
       acquisition_date, payload
  FROM positions
 WHERE snapshot_at BETWEEN ? AND ?
 ORDER BY snapshot_at, account_id, position_key`, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("synthetic positions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                   int64
			account, key, assetClass, vehicle, ccy string
			payload                                string
			instrument, quantity, marketValue      sql.NullString
			bookValue, accrued, acquired           sql.NullString
		)
		if err := rows.Scan(&snap, &account, &key, &instrument, &assetClass, &vehicle,
			&ccy, &quantity, &marketValue, &bookValue, &accrued, &acquired, &payload); err != nil {
			return err
		}
		var extra annotations
		ac, veh := taxonomyPair(assetClass, vehicle, &extra)
		b := at(snap)
		b.Positions = append(b.Positions, canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    account,
			PositionKey:          key,
			InstrumentExternalID: silver.StrPtrIfNonEmpty(instrument.String),
			AssetClass:           ac,
			Vehicle:              veh,
			Currency:             ccy,
			Quantity:             silver.DecimalPtrOrNil(quantity),
			MarketValue:          silver.DecimalPtrOrNil(marketValue),
			BookValue:            silver.DecimalPtrOrNil(bookValue),
			AccruedInterest:      silver.DecimalPtrOrNil(accrued),
			AcquisitionDate:      calendarDate(acquired),
			Payload:              payloadWith(payload, extra),
		})
	}
	return rows.Err()
}

// readCashBalances reads the window's cash balances. The amount is the one
// value a balance cannot do without, so an unparseable one fails the load
// rather than landing as zero.
func (c *Connection) readCashBalances(ctx context.Context, w canonical.Window,
	at func(int64) *canonical.SnapshotBatch) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT snapshot_at, account_id, currency, balance_kind, amount, payload
  FROM cash_balances
 WHERE snapshot_at BETWEEN ? AND ?
 ORDER BY snapshot_at, account_id, currency, balance_kind`, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("synthetic cash balances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                int64
			account, ccy, kind, amount, payload string
		)
		if err := rows.Scan(&snap, &account, &ccy, &kind, &amount, &payload); err != nil {
			return err
		}
		amt, err := canonical.NewDecimalFromString(amount)
		if err != nil {
			return fmt.Errorf("synthetic cash balance (%d, %s, %s, %s): amount %q: %w",
				snap, account, ccy, kind, amount, err)
		}
		var extra annotations
		bk := canonical.BalanceKind(kind)
		if !bk.Valid() {
			extra.keep("balance_kind", kind)
			bk = canonical.BalanceKindClosing
		}
		b := at(snap)
		b.CashBalances = append(b.CashBalances, canonical.CashBalanceChange{
			SnapshotAt:        snap,
			AccountExternalID: account,
			Currency:          ccy,
			BalanceKind:       bk,
			Amount:            amt,
			Payload:           payloadWith(payload, extra),
		})
	}
	return rows.Err()
}

// readFxRates reads the window's fx rates, in gold's direction already (the
// schema's). The mid rate is required, as a cash balance's amount is.
func (c *Connection) readFxRates(ctx context.Context, w canonical.Window,
	at func(int64) *canonical.SnapshotBatch) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT snapshot_at, base_currency, quote_currency, mid_rate, bid_rate, ask_rate, payload
  FROM fx_rates
 WHERE snapshot_at BETWEEN ? AND ?
 ORDER BY snapshot_at, base_currency, quote_currency`, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("synthetic fx rates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                      int64
			base, quote, mid, payload string
			bid, ask                  sql.NullString
		)
		if err := rows.Scan(&snap, &base, &quote, &mid, &bid, &ask, &payload); err != nil {
			return err
		}
		rate, err := canonical.NewDecimalFromString(mid)
		if err != nil {
			return fmt.Errorf("synthetic fx rate (%d, %s, %s): mid_rate %q: %w",
				snap, base, quote, mid, err)
		}
		b := at(snap)
		b.FxRates = append(b.FxRates, canonical.FxRateChange{
			SnapshotAt:    snap,
			BaseCurrency:  base,
			QuoteCurrency: quote,
			MidRate:       rate,
			BidRate:       silver.DecimalPtrOrNil(bid),
			AskRate:       silver.DecimalPtrOrNil(ask),
			Payload:       payloadWith(payload, nil),
		})
	}
	return rows.Err()
}

// calendarDate reads a 'YYYY-MM-DD' column as that day's UTC midnight, the
// shape gold stores an acquisition date in. An absent or unreadable date is
// nil.
func calendarDate(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	d, err := time.Parse(time.DateOnly, s.String)
	if err != nil {
		return nil
	}
	return &d
}

// ---- dimensions -------------------------------------------------------------

// dimensions holds the three dimension tables, read once per Snapshots call
// and each row already validated into the record it becomes. A batch takes a
// copy and stamps the seen range on it.
type dimensions struct {
	portfolios  map[string]canonical.PortfolioChange
	accounts    map[string]canonical.AccountChange
	instruments map[string][]instrumentVersion // ascending valid_from
}

// instrumentVersion is one row of the instruments table: the record that
// applies from validFrom until the next version's validFrom.
type instrumentVersion struct {
	validFrom int64
	change    canonical.InstrumentChange
}

func (d *dimensions) account(id string, first, last int64) (canonical.AccountChange, error) {
	a, ok := d.accounts[id]
	if !ok {
		return a, fmt.Errorf("synthetic: account %q is referenced but has no accounts row", id)
	}
	a.FirstSeenAt, a.LastSeenAt = first, last
	return a, nil
}

func (d *dimensions) portfolio(id string, first, last int64) (canonical.PortfolioChange, error) {
	p, ok := d.portfolios[id]
	if !ok {
		return p, fmt.Errorf("synthetic: portfolio %q is referenced but has no portfolios row", id)
	}
	p.FirstSeenAt, p.LastSeenAt = first, last
	return p, nil
}

// instrument is the version of an instrument in effect at t: the one with
// the greatest valid_from at or before t. A reference earlier than every
// version gets the earliest, since the instrument existed and that is the
// oldest description of it there is.
func (d *dimensions) instrument(id string, t, first, last int64) (canonical.InstrumentChange, error) {
	versions := d.instruments[id]
	if len(versions) == 0 {
		return canonical.InstrumentChange{}, fmt.Errorf(
			"synthetic: instrument %q is referenced but has no instruments row", id)
	}
	// The first version starting after t; the one before it is in effect.
	i := sort.Search(len(versions), func(i int) bool { return versions[i].validFrom > t })
	if i > 0 {
		i--
	}
	ch := versions[i].change
	ch.FirstSeenAt, ch.LastSeenAt = first, last
	return ch, nil
}

func (c *Connection) readDimensions(ctx context.Context) (*dimensions, error) {
	d := &dimensions{
		portfolios:  map[string]canonical.PortfolioChange{},
		accounts:    map[string]canonical.AccountChange{},
		instruments: map[string][]instrumentVersion{},
	}
	if err := c.readPortfolios(ctx, d); err != nil {
		return nil, err
	}
	if err := c.readAccounts(ctx, d); err != nil {
		return nil, err
	}
	if err := c.readInstruments(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (c *Connection) readPortfolios(ctx context.Context, d *dimensions) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT portfolio_id, display_name, base_currency, nickname, payload FROM portfolios`)
	if err != nil {
		return fmt.Errorf("synthetic portfolios: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, payload         string
			name, ccy, nickname sql.NullString
		)
		if err := rows.Scan(&id, &name, &ccy, &nickname, &payload); err != nil {
			return err
		}
		d.portfolios[id] = canonical.PortfolioChange{
			PortfolioExternalID: id,
			DisplayName:         silver.StrPtrIfNonEmpty(name.String),
			BaseCurrency:        silver.StrPtrIfNonEmpty(ccy.String),
			Nickname:            silver.StrPtrIfNonEmpty(nickname.String),
			Payload:             payloadWith(payload, nil),
		}
	}
	return rows.Err()
}

// readAccounts reads the accounts table. The three taxonomy columns are taken
// from the row; a value outside its vocabulary falls back — account_kind to
// `other`, which the column cannot do without, and tax_wrapper /
// management_style to absent, which they can — with the raw value kept.
func (c *Connection) readAccounts(ctx context.Context, d *dimensions) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT account_id, account_kind, display_name, base_currency, nickname,
       account_category, tax_wrapper, management_style, portfolio_id, payload
  FROM accounts`)
	if err != nil {
		return fmt.Errorf("synthetic accounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, kind, payload             string
			name, ccy, nickname, category sql.NullString
			wrapper, style, portfolio     sql.NullString
		)
		if err := rows.Scan(&id, &kind, &name, &ccy, &nickname, &category,
			&wrapper, &style, &portfolio, &payload); err != nil {
			return err
		}
		var extra annotations
		ak := canonical.AccountKind(kind)
		if !ak.Valid() {
			extra.keep("account_kind", kind)
			ak = canonical.AccountKindOther
		}
		a := canonical.AccountChange{
			AccountExternalID:   id,
			AccountKind:         ak,
			DisplayName:         silver.StrPtrIfNonEmpty(name.String),
			BaseCurrency:        silver.StrPtrIfNonEmpty(ccy.String),
			Nickname:            silver.StrPtrIfNonEmpty(nickname.String),
			AccountCategory:     silver.StrPtrIfNonEmpty(category.String),
			PortfolioExternalID: silver.StrPtrIfNonEmpty(portfolio.String),
		}
		if raw := wrapper.String; raw != "" {
			if tw := canonical.TaxWrapper(raw); tw.Valid() {
				a.TaxWrapper = &tw
			} else {
				extra.keep("tax_wrapper", raw)
			}
		}
		if raw := style.String; raw != "" {
			if ms := canonical.ManagementStyle(raw); ms.Valid() {
				a.ManagementStyle = &ms
			} else {
				extra.keep("management_style", raw)
			}
		}
		a.Payload = payloadWith(payload, extra)
		d.accounts[id] = a
	}
	return rows.Err()
}

func (c *Connection) readInstruments(ctx context.Context, d *dimensions) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT instrument_id, valid_from, asset_class, vehicle,
       isin, cusip, symbol, name, currency, payload
  FROM instruments
 ORDER BY instrument_id, valid_from`)
	if err != nil {
		return fmt.Errorf("synthetic instruments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, assetClass, vehicle, payload string
			validFrom                        int64
			isin, cusip, symbol, name, ccy   sql.NullString
		)
		if err := rows.Scan(&id, &validFrom, &assetClass, &vehicle,
			&isin, &cusip, &symbol, &name, &ccy, &payload); err != nil {
			return err
		}
		var extra annotations
		ac, veh := taxonomyPair(assetClass, vehicle, &extra)
		d.instruments[id] = append(d.instruments[id], instrumentVersion{
			validFrom: validFrom,
			change: canonical.InstrumentChange{
				InstrumentExternalID: id,
				AssetClass:           ac,
				Vehicle:              veh,
				ISIN:                 silver.StrPtrIfNonEmpty(isin.String),
				CUSIP:                silver.StrPtrIfNonEmpty(cusip.String),
				Symbol:               silver.StrPtrIfNonEmpty(symbol.String),
				Name:                 silver.StrPtrIfNonEmpty(name.String),
				Currency:             silver.StrPtrIfNonEmpty(ccy.String),
				Payload:              payloadWith(payload, extra),
			},
		})
	}
	return rows.Err()
}
