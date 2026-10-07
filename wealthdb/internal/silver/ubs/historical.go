package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Historical-snapshot reader for the ubs-web silver
// migration 0002 tables (`historical_position_snapshots`,
// `historical_cash_balances`). These tables are reconstructed
// from PDF Statements of Assets (quarterly) and Account Statements
// (monthly) — see the silver migration comment for the full
// shape. They live in PARALLEL to the live-fetch `positions` /
// `accounts` tables.
//
// Why surface them here rather than fold into the live web
// reader: the identity model differs (live web uses 4-char
// portfolio codes like RNNN/NNNN, the PDFs use PSN-aligned
// 'BBBBAAAAAAAANN'), the temporal grain differs (intra-day live
// vs end-of-period PDF), and the date range pre-dates live web.
// The merge orchestrator runs this stream first so historical
// pre-PSN-start dates are populated, then live web emits
// dimensions in the overlap, then PSN takes over facts.

// snapshotsHistorical emits one batch per distinct as_of_date in
// the window plus one batch per distinct period_end. Returns an
// empty stream when no historical rows fall inside [w.Start, w.End].
//
// Window semantics: the loader's window-DELETE deletes positions
// and cash_balances rows WHERE snapshot_at BETWEEN w.Start AND
// w.End, so any historical timestamp we emit must be inside the
// window or the next load will collide on the gold PK. The
// webReader's ChangeWindow extends Start back to MIN(historical
// times) when there's a new dump_run, so this loop's WHERE clause
// is just a defensive filter — it could equivalently read every
// historical row. Filtering keeps the read cheap when the loader
// is incrementally advancing past a single new dump_run.
// portfolioCutoff and accountCutoff (both may be nil for a
// single-subsource web-only load) hold the per-portfolio and
// per-account PSN-start dates: historical rows on or after the
// applicable cutoff are dropped so PSN's daily snapshots own the
// overlap without colliding on the gold PK. See
// webReader.buildHistoricalCutoffs.
func (r *webReader) snapshotsHistorical(
	ctx context.Context,
	w canonical.Window,
	safekeepingByPortfolio map[string]string,
	portfolioCutoff map[string]int64,
	accountCutoff map[string]int64,
) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}

	byTime := make(map[int64]*canonical.SnapshotBatch)
	getBatch := func(t int64) *canonical.SnapshotBatch {
		if b, ok := byTime[t]; ok {
			return b
		}
		b := &canonical.SnapshotBatch{}
		byTime[t] = b
		return b
	}

	if err := r.appendHistoricalSecurities(ctx, w, getBatch, safekeepingByPortfolio, portfolioCutoff); err != nil {
		return nil, err
	}
	if err := r.appendHistoricalCashBalances(ctx, w, getBatch, accountCutoff); err != nil {
		return nil, err
	}
	// Mortgages run LAST and peek at byTime directly (not getBatch)
	// so they attach to existing portfolio snapshots without
	// creating new ones — see the function comment.
	if err := r.appendHistoricalMortgages(ctx, w, byTime); err != nil {
		return nil, err
	}

	times := make([]int64, 0, len(byTime))
	for t := range byTime {
		times = append(times, t)
	}
	slices.Sort(times)

	batches := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		batches = append(batches, *byTime[t])
	}
	return silver.NewSnapshotStream(batches), nil
}

// appendHistoricalSecurities emits security positions from
// `historical_position_snapshots` (rows where instrument_isin IS
// NOT NULL). UBS PDFs don't surface the safekeeping account
// reliably, so silver leaves account_external_id=” on these
// rows. We attach them to the per-portfolio overlay account
// pattern PSN already uses for forward contracts (account_kind=
// 'overlay', '<portfolio>:overlay'), preserving the invariant
// that every position is owned by some account row.
//
// Cash rows (instrument_isin IS NULL) from this same table are
// intentionally skipped — they overlap with `historical_cash_
// balances` rows on the same (account, currency, period_end) and
// would collide on the gold cash_balances PK. The monthly cash-
// balances table is the richer source (opening + closing per
// month vs quarter-end only), so we use it exclusively for cash.
// safekeepingByPortfolio maps a portfolio_external_id to the PSN
// safekeeping account that holds its securities, where exactly one
// of them can be (see psnReader.safekeepingByPortfolio). When a
// portfolio is present, its historical securities attach to that
// real safekeeping account_external_id, giving account-by-account
// continuity across the web→PSN cutover. When absent (PSN not
// configured, or a portfolio whose securities two accounts could
// equally hold) the security falls back to the synthetic
// per-portfolio overlay account.
func (r *webReader) appendHistoricalSecurities(
	ctx context.Context,
	w canonical.Window,
	getBatch func(int64) *canonical.SnapshotBatch,
	safekeepingByPortfolio map[string]string,
	portfolioCutoff map[string]int64,
) error {
	// Year-end Statements of Assets print a per-instrument gold-bar
	// detail line for the precious-metals overlay portfolio that the
	// quarterly statements omit. We already synthesise a single
	// overview precious-metals position (PM-<portfolio>) for that
	// portfolio from the sibling reporting-currency statement, so
	// emitting the detail line too would double-count the metal at
	// every year-end (the two reach gold under distinct position
	// keys). Suppress the detail line whenever its overview sibling
	// exists for the same (as_of_date, portfolio): the overview row
	// is continuous across all quarters and reported in clean USD.
	pmOverview, err := r.preciousMetalsOverviewKeys(ctx, w)
	if err != nil {
		return err
	}

	const q = `
SELECT as_of_date, portfolio_external_id, instrument_isin, currency_iso,
       units, market_value, market_value_currency,
       cost_price, market_price, accrued_interest, description, payload
  FROM historical_position_snapshots
 WHERE instrument_isin IS NOT NULL
   AND as_of_date BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalSecurities: %w", err)
	}
	defer rows.Close()

	// One PortfolioChange and one AccountChange per (snapshot,
	// portfolio), not one per holding.
	emitted := map[snapshotKey]bool{}

	for rows.Next() {
		var (
			asOf                            int64
			portID, isin, ccy, mvCcy        string
			descr                           sql.NullString
			units, mv, cost, price, accrued sql.NullFloat64
			payload                         string
		)
		if err := rows.Scan(&asOf, &portID, &isin, &ccy, &units, &mv, &mvCcy,
			&cost, &price, &accrued, &descr, &payload); err != nil {
			return err
		}
		// Drop rows whose portfolio has crossed over to PSN
		// coverage; without this the quarter-end PDF row collides
		// with PSN's daily snapshot for the same safekeeping+ISIN
		// on the gold PositionChange PK. Portfolios with no PSN
		// counterpart (cutoff==0) pass through.
		if cut := portfolioCutoff[portID]; cut > 0 && asOf >= cut {
			continue
		}
		// Drop the year-end precious-metals detail line when its
		// synthetic overview sibling is present for the same
		// (as_of, portfolio) — see the function's opening comment.
		if looksLikeISIN(isin) && pmOverview[snapshotKey{asOf, portID}] &&
			isPreciousMetalsLine(descr.String, payload) {
			continue
		}
		batch := getBatch(asOf)

		// Prefer the real PSN safekeeping account so this security's
		// history is continuous with the PSN-era holdings on the
		// same account. Fall back to the synthetic overlay when no
		// unambiguous mapping exists.
		accountID, mapped := safekeepingByPortfolio[portID]
		if !mapped {
			accountID = overlayAccountID(portID)
		}
		if portKey := (snapshotKey{asOf, portID}); !emitted[portKey] {
			emitted[portKey] = true
			batch.Portfolios = append(batch.Portfolios, canonical.PortfolioChange{
				PortfolioExternalID: portID,
				BaseCurrency:        silver.StrPtrIfNonEmpty(mvCcy),
				FirstSeenAt:         asOf,
				LastSeenAt:          asOf,
			})
			pid := portID
			ac := canonical.AccountChange{
				AccountExternalID:   accountID,
				PortfolioExternalID: &pid,
				FirstSeenAt:         asOf,
				LastSeenAt:          asOf,
			}
			if mapped {
				// Real safekeeping account. Leave DisplayName nil so
				// the PSN-era AccountChange (which carries the proper
				// mandate name) wins via gold's latest-last_seen_at
				// upsert guard; our contribution just extends
				// first_seen_at back to the PDF era.
				ac.AccountKind = canonical.AccountKindSafekeeping
			} else {
				ac.AccountKind = canonical.AccountKindOverlay
				ac.DisplayName = silver.StrPtrIfNonEmpty("Portfolio overlay (historical)")
				// The synthetic overlay only. A real safekeeping
				// account's wrapper is the AcctTpCd tables' answer,
				// and stamping one here would let the PDF era supply
				// a wrapper for a product code PSN deliberately left
				// unmapped — gold's merge backfills from older
				// observations and absence never wins, so the
				// caution those tables exist for would be undone from
				// behind.
				ac.TaxWrapper = relationshipTaxWrapper()
			}
			batch.Accounts = append(batch.Accounts, ac)
		}

		// Some historical rows carry a synthetic, non-ISIN-shaped
		// instrument key — e.g. the overview-derived precious-metals
		// position for a portfolio UBS issues no per-instrument
		// Statement-of-Assets page for ("PM-<portfolio>"). It still
		// needs a stable instrument/position identity, but must not
		// claim a canonical ISIN, so leave InstrumentChange.ISIN nil
		// for those.
		isinCopy := isin
		var isinPtr *string
		if looksLikeISIN(isin) {
			isinPtr = &isinCopy
		}
		// Historical PDF securities carry no CFI/UAC, so the pair
		// comes from the description-template classifier — UBS
		// generates PDF descriptions from a fixed per-instrument-type
		// vocabulary ("Reg.shs …", "… Sicav …", "Sponsored American
		// Deposit Receipt …"), so the templates are a reliable signal.
		// Unmatched descriptions keep (other, other). Instruments that
		// later reappear in PSN converge on PSN's CFI-derived pair via
		// gold's latest-last_seen_at per-column upsert.
		acHist, vehHist, _ := taxonomyPairForWebDescription(descr.String)
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: isin,
			AssetClass:           acHist,
			Vehicle:              vehHist,
			ISIN:                 isinPtr,
			Name:                 silver.StrPtrIfNonEmpty(descr.String),
			Currency:             silver.StrPtrIfNonEmpty(ccy),
			FirstSeenAt:          asOf,
			LastSeenAt:           asOf,
		})

		positionCcy := mvCcy
		if positionCcy == "" {
			positionCcy = ccy
		}
		bookValue, posPayload := historicalBookValue(units, cost, ccy, positionCcy, payload)
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           asOf,
			AccountExternalID:    accountID,
			PositionKey:          isin,
			InstrumentExternalID: &isinCopy,
			AssetClass:           acHist,
			Vehicle:              vehHist,
			Currency:             positionCcy,
			Quantity:             silver.DecimalPtrFromNullFloat(units),
			MarketValue:          silver.DecimalPtrFromNullFloat(mv),
			BookValue:            bookValue,
			AccruedInterest:      silver.DecimalPtrFromNullFloat(accrued),
			Payload:              posPayload,
		})
	}
	return rows.Err()
}

// snapshotKey is one account or portfolio at one snapshot time.
type snapshotKey struct {
	at int64
	id string
}

// preciousMetalsOverviewKeys returns the (as_of_date, portfolio)
// pairs in the window that carry a synthetic overview precious-metals
// row (instrument_isin like 'PM-%'). Used to suppress the duplicate
// year-end gold-bar detail line for those portfolios.
func (r *webReader) preciousMetalsOverviewKeys(
	ctx context.Context, w canonical.Window,
) (map[snapshotKey]bool, error) {
	const q = `
SELECT DISTINCT as_of_date, portfolio_external_id
  FROM historical_position_snapshots
 WHERE instrument_isin LIKE 'PM-%'
   AND as_of_date BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("preciousMetalsOverviewKeys: %w", err)
	}
	defer rows.Close()
	set := map[snapshotKey]bool{}
	for rows.Next() {
		var asOf int64
		var port string
		if err := rows.Scan(&asOf, &port); err != nil {
			return nil, err
		}
		set[snapshotKey{asOf, port}] = true
	}
	return set, rows.Err()
}

// isPreciousMetalsLine reports whether a historical_position_snapshots
// row is a precious-metals detail line (e.g. the year-end "Gold bar(s)
// fine weight" Statement-of-Assets row), matched on its description and
// raw payload. The call site already gates on an overview sibling being
// present for the same portfolio, so this only ever fires inside a
// precious-metals overlay portfolio — a genuine non-metal security
// there (e.g. a money-market fund) is left untouched.
func isPreciousMetalsLine(description, payload string) bool {
	hay := strings.ToLower(description + " " + payload)
	for _, kw := range []string{
		"gold bar", "fine weight", "precious metal",
		"bullion", "silver bar", "platinum", "palladium",
	} {
		if strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

// appendHistoricalCashBalances emits opening and closing balance
// rows from `historical_cash_balances`. One CashBalanceChange per
// non-NULL balance is produced — opening at period_start, closing
// at period_end. Per-IBAN AccountChange is emitted once per
// (period_end, IBAN) so accounts the PDFs reference but live web
// hasn't observed still appear in gold.
//
// total_debits / total_credits are kept in the payload but not
// projected to gold — they're monthly aggregates, not point-in-
// time balances, so they don't map onto cash_balances semantics.
func (r *webReader) appendHistoricalCashBalances(
	ctx context.Context,
	w canonical.Window,
	getBatch func(int64) *canonical.SnapshotBatch,
	accountCutoff map[string]int64,
) error {
	const q = `
SELECT period_end, period_start, account_external_id, currency_iso,
       opening_balance, closing_balance, payload
  FROM historical_cash_balances
 WHERE period_end BETWEEN ? AND ?
    OR period_start BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalCashBalances: %w", err)
	}
	defer rows.Close()

	// One AccountChange per (snapshot, account), however many
	// balances the snapshot carries.
	accountEmitted := map[snapshotKey]bool{}

	for rows.Next() {
		var (
			periodEnd, periodStart int64
			acctID, ccy            string
			open, close            sql.NullFloat64
			payload                string
		)
		if err := rows.Scan(&periodEnd, &periodStart, &acctID, &ccy,
			&open, &close, &payload); err != nil {
			return err
		}
		// Same PSN cutover as historical securities: once the IBAN
		// is covered by PSN, PSN's daily cash rows own the (source,
		// snapshot_at, account, kind) key; drop the historical row
		// for that period. Accounts with no PSN counterpart pass
		// through.
		cut := accountCutoff[acctID]
		for _, b := range []struct {
			at      int64
			balance sql.NullFloat64
			kind    canonical.BalanceKind
		}{
			{periodStart, open, canonical.BalanceKindOpening},
			{periodEnd, close, canonical.BalanceKindClosing},
		} {
			if !b.balance.Valid || b.at < w.Start || b.at > w.End || (cut > 0 && b.at >= cut) {
				continue
			}
			batch := getBatch(b.at)
			if key := (snapshotKey{b.at, acctID}); !accountEmitted[key] {
				accountEmitted[key] = true
				c := ccy
				batch.Accounts = append(batch.Accounts, canonical.AccountChange{
					AccountExternalID: acctID,
					AccountKind:       canonical.AccountKindCash,
					BaseCurrency:      &c,
					FirstSeenAt:       b.at,
					LastSeenAt:        b.at,
				})
			}
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        b.at,
				AccountExternalID: acctID,
				Currency:          ccy,
				BalanceKind:       b.kind,
				Amount:            canonical.NewDecimalFromFloat(b.balance.Float64),
				Payload:           json.RawMessage(payload),
			})
		}
	}
	return rows.Err()
}

// appendHistoricalMortgages attaches each PDF-derived mortgage
// balance point (one per Maturity Notice "As at" date) to the
// portfolio snapshot at that same as_of_date, emitting the same
// triple as the live path — Account + Instrument + Position keyed
// by the UBS-internal mortgage account number, AccountKind /
// AssetClass = mortgage, MarketValue already negative from silver.
//
// It takes `byTime` directly (not getBatch) and PEEKS rather than
// creates: a mortgage row is only emitted when a real portfolio
// snapshot already exists at its as_of_date (a Statement-of-Assets
// securities batch or an Account-Statement cash batch). Maturity
// notices and Statement-of-Assets PDFs are both quarter-end, so in
// the normal case they line up exactly. The one that wouldn't —
// UBS issues the next interest-roll quarter's Maturity Notice
// ahead of time, so there's a future-dated mortgage row with no
// portfolio snapshot behind it — must NOT spawn a mortgage-only
// snapshot, or gold's MAX(snapshot_at)-per-source "today" query
// lands on that future date and every other position vanishes
// from the view. Peeking (and skipping unanchored dates) prevents
// that. Mortgages must therefore run AFTER securities + cash in
// snapshotsHistorical so the batches they anchor to already exist.
func (r *webReader) appendHistoricalMortgages(
	ctx context.Context,
	w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch,
) error {
	ok, err := r.hasTable(ctx, "historical_mortgages")
	if err != nil || !ok {
		return err
	}
	const q = `
SELECT as_of_date, account_external_id, currency_iso,
       outstanding_balance, product_name, rate_type,
       collateral_description, payload
  FROM historical_mortgages
 WHERE as_of_date BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalMortgages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			asOf                              int64
			extID, currency, payload          string
			outstanding                       sql.NullFloat64
			productName, rateType, collateral sql.NullString
		)
		if err := rows.Scan(&asOf, &extID, &currency, &outstanding,
			&productName, &rateType, &collateral, &payload); err != nil {
			return err
		}
		_ = rateType // surfaced via payload

		// Peek — never create. Skip mortgage rows whose as_of_date
		// has no real portfolio snapshot behind it (e.g. the
		// future-dated next-quarter Maturity Notice).
		batch, ok := byTime[asOf]
		if !ok || (len(batch.Positions) == 0 && len(batch.CashBalances) == 0) {
			continue
		}

		display := sql.NullString{
			Valid:  productName.Valid && collateral.Valid,
			String: productName.String + ", " + collateral.String,
		}
		if !display.Valid {
			display = productName
		}
		extIDCopy := extID
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindMortgage,
			TaxWrapper:        relationshipTaxWrapper(),
			DisplayName:       silver.StrPtrIfNonEmpty(display.String),
			BaseCurrency:      silver.StrPtrIfNonEmpty(currency),
			FirstSeenAt:       asOf,
			LastSeenAt:        asOf,
			Payload:           json.RawMessage(payload),
		})
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: extID,
			AssetClass:           canonical.AssetClassRealEstate,
			Vehicle:              canonical.VehicleMortgage,
			Name:                 silver.StrPtrIfNonEmpty(display.String),
			Currency:             silver.StrPtrIfNonEmpty(currency),
			FirstSeenAt:          asOf,
			LastSeenAt:           asOf,
		})
		var mv *canonical.Decimal
		if outstanding.Valid {
			d := canonical.NewDecimalFromFloat(outstanding.Float64)
			mv = &d
		}
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           asOf,
			AccountExternalID:    extID,
			PositionKey:          extID,
			InstrumentExternalID: &extIDCopy,
			AssetClass:           canonical.AssetClassRealEstate,
			Vehicle:              canonical.VehicleMortgage,
			Currency:             currency,
			MarketValue:          mv,
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// historicalRange returns the span of dates the historical tables cover, or
// (-1, -1) when the silver predates them (migration 0002; mortgages 0005)
// or they hold no rows. ChangeWindow widens Start by it so the loader's
// window-DELETE covers existing historical rows before the re-INSERT.
func (r *webReader) historicalRange(ctx context.Context) (int64, int64, error) {
	queries := []string{
		`SELECT MIN(as_of_date), MAX(as_of_date) FROM historical_position_snapshots`,
		`SELECT MIN(period_start), MAX(period_end) FROM historical_cash_balances`,
	}
	for _, table := range []string{"historical_position_snapshots", "historical_cash_balances"} {
		if ok, err := r.hasTable(ctx, table); err != nil || !ok {
			return -1, -1, err
		}
	}
	ok, err := r.hasTable(ctx, "historical_mortgages")
	if err != nil {
		return -1, -1, err
	}
	if ok {
		queries = append(queries, `SELECT MIN(as_of_date), MAX(as_of_date) FROM historical_mortgages`)
	}
	return r.span(ctx, "historicalRange", queries)
}

// historicalBookValue returns a statement row's book value and the payload
// its position carries.
//
// The statement prints the cost price in the instrument's currency, while the
// position is stated in the portfolio's base currency. units × cost_price is a
// book value only when the two currencies are the same. Otherwise the book
// value stays NULL, since the average buy FX rate that would convert it is not
// parsed, and the cost price travels in the payload with its currency.
func historicalBookValue(units, cost sql.NullFloat64, instrumentCcy, positionCcy, payload string) (*canonical.Decimal, json.RawMessage) {
	if !units.Valid || !cost.Valid {
		return nil, json.RawMessage(payload)
	}
	if instrumentCcy != "" && instrumentCcy == positionCcy {
		d := canonical.NewDecimalFromFloat(units.Float64 * cost.Float64)
		return &d, json.RawMessage(payload)
	}
	price := strconv.FormatFloat(cost.Float64, 'f', -1, 64)
	out := spliceStringField(payload, costPriceKey, price)
	return nil, spliceStringField(string(out), costCurrencyKey, instrumentCcy)
}

// The payload keys a statement row's cost price travels under when it cannot
// become a book value.
const (
	costPriceKey    = `"cost_price":`
	costCurrencyKey = `"cost_currency":`
)
