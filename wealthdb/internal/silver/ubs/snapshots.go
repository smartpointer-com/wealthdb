package ubs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

func (c *psnReader) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}

	times, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	byTime := make(map[int64]*canonical.SnapshotBatch, len(times))
	for _, t := range times {
		byTime[t] = &canonical.SnapshotBatch{}
	}

	// Instruments first — appendHoldings joins against them via
	// an in-memory map for asset_class and currency.
	instrMap, err := c.appendInstruments(ctx, w, byTime)
	if err != nil {
		return nil, err
	}
	if err := c.appendCashAccounts(ctx, w, byTime); err != nil {
		return nil, err
	}
	mandatePortfolios, err := c.appendSafekeepingAccounts(ctx, w, byTime)
	if err != nil {
		return nil, err
	}
	// Propagate the safekeeping account's management_style to its
	// sibling cash / overlay accounts within the same portfolio,
	// gated on the portfolio being a "named mandate" (i.e. its
	// safekeeping carries a non-empty AcctDesc — strategy name
	// like "EMERGING MARKETS ASIA" or "PRIVATE MARKETS").
	// Portfolios whose safekeeping has no AcctDesc are residuals
	// in general-banking portfolios; the cash there is personal
	// banking and should stay self_directed, not inherit the
	// safekeeping's advisory tag. See propagateManagementStyle
	// ByPortfolio for the discriminator rationale.
	for snap, batch := range byTime {
		propagateManagementStyleByPortfolio(batch.Accounts, mandatePortfolios[snap])
	}
	if err := c.appendPortfolios(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendHoldings(ctx, w, byTime, instrMap); err != nil {
		return nil, err
	}
	if err := c.appendCashBalances(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendFxRates(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendForwardContracts(ctx, w, byTime); err != nil {
		return nil, err
	}

	batches := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		batches = append(batches, *byTime[t])
	}
	return silver.NewSnapshotStream(batches), nil
}

// snapshotTimesInWindow returns the union of distinct snapshot_at
// values across dump_runs and every PSN content table whose
// snapshot_at column the adapter reads. ubs-psn promotes
// snapshot_at on content tables to the business-date midnight
// (UTC) of the dump's effective as-of date, which differs from
// the dump's wall-clock run time recorded in dump_runs. So
// content rows never match a dump_runs timestamp; without the
// union, the byTime dispatch in Snapshots() silently drops
// every content row.
func (c *psnReader) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `
SELECT DISTINCT snapshot_at FROM (
    SELECT snapshot_at FROM dump_runs            WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL
    SELECT snapshot_at FROM cash_accounts        WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL
    SELECT snapshot_at FROM safekeeping_accounts WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL
    SELECT snapshot_at FROM portfolios           WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL
    SELECT snapshot_at FROM holdings             WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL
    SELECT snapshot_at FROM cash_balances        WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL
    SELECT snapshot_at FROM instruments          WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL
    SELECT snapshot_at FROM fx_rates             WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL
    SELECT snapshot_at FROM forward_contracts    WHERE snapshot_at BETWEEN ? AND ?
)
ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q,
		w.Start, w.End, w.Start, w.End, w.Start, w.End,
		w.Start, w.End, w.Start, w.End, w.Start, w.End,
		w.Start, w.End, w.Start, w.End, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("snapshotTimesInWindow: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- accounts ------------------------------------------------------------

// cashAccountPayload covers the UBS SDCA fields the adapter still
// reads out of payload JSON. AcctCcyIsoCd (currency), AcctTpCd
// (product code), and AcctTpDesc (category) aren't promoted in
// silver, so they stay here. PrtflId moved to a promoted column
// in silver migration 0002 — read directly from the SELECT.
type cashAccountPayload struct {
	AcctCcyIsoCd string `json:"AcctCcyIsoCd"`
	AcctTpCd     string `json:"AcctTpCd"`
	AcctTpDesc   string `json:"AcctTpDesc"`
}

func (c *psnReader) appendCashAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, relationship_id, account_external_id,
       portfolio_external_id, payload
  FROM cash_accounts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendCashAccounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap        int64
			relID       string
			extID       string
			portfolioID sql.NullString
			payload     string
		)
		if err := rows.Scan(&snap, &relID, &extID, &portfolioID, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var p cashAccountPayload
		_ = json.Unmarshal([]byte(payload), &p) // best-effort

		// We leave DisplayName nil so the user-facing positions
		// output falls back to the IBAN (already human-
		// readable), rather than showing the less-informative
		// AcctTpDesc like "Private" or "Custody". The AcctTpDesc
		// itself is forwarded as AccountCategory.
		change := canonical.AccountChange{
			AccountExternalID:   extID,
			AccountKind:         canonical.AccountKindCash,
			BaseCurrency:        silver.StrPtrIfNonEmpty(p.AcctCcyIsoCd),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID),
			AccountCategory:     silver.StrPtrIfNonEmpty(p.AcctTpDesc),
			PortfolioExternalID: silver.StrPtrIfNonEmpty(portfolioID.String),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		}
		if w := taxWrapperForCashAcctTp(p.AcctTpCd, p.AcctTpDesc); w != "" {
			change.TaxWrapper = &w
		}
		batch.Accounts = append(batch.Accounts, change)
	}
	return rows.Err()
}

type safekeepingPayload struct {
	InvstmtCcyIsoCd string `json:"InvstmtCcyIsoCd"`
	AcctTpCd        string `json:"AcctTpCd"`
	AcctTpDesc      string `json:"AcctTpDesc"`
	AcctSubTypeDesc string `json:"AcctSubTypeDesc"`
	// AcctDesc is the strategy / mandate name UBS attaches to
	// safekeeping accounts that hold a dedicated investment
	// mandate (e.g. "EMERGING MARKETS ASIA", "PRIVATE MARKETS",
	// "ADVICE HEDGE FUND"). Empty when the safekeeping is a
	// residual sitting in a general-banking portfolio rather
	// than a named mandate. Used by the cash-propagation pass
	// to decide whether the sibling cash accounts in this
	// portfolio inherit the safekeeping's management_style.
	AcctDesc string `json:"AcctDesc"`
}

// appendSafekeepingAccounts also returns a per-snapshot set of
// "mandate portfolios" — those whose safekeeping carries a
// non-empty AcctDesc (strategy name). Used by the cash-
// propagation pass to gate which cash accounts inherit their
// portfolio's safekeeping mandate.
func (c *psnReader) appendSafekeepingAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) (map[int64]map[string]bool, error) {
	const q = `
SELECT snapshot_at, relationship_id, account_external_id,
       portfolio_external_id, payload
  FROM safekeeping_accounts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("appendSafekeepingAccounts: %w", err)
	}
	defer rows.Close()
	mandatePortfolios := map[int64]map[string]bool{}
	for rows.Next() {
		var (
			snap         int64
			relID, extID string
			portfolioID  sql.NullString
			payload      string
		)
		if err := rows.Scan(&snap, &relID, &extID, &portfolioID, &payload); err != nil {
			return nil, err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var p safekeepingPayload
		_ = json.Unmarshal([]byte(payload), &p)

		change := canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindSafekeeping,
			// DisplayName left nil; see appendCashAccounts. The
			// AcctTpDesc plus AcctSubTypeDesc when present (the
			// sub-type sharpens "Custody" / "Cust Strap." into
			// "Custody / Cash-Custody", "Custody / Personal
			// Cust.", etc.) goes into AccountCategory.
			BaseCurrency:        silver.StrPtrIfNonEmpty(p.InvstmtCcyIsoCd),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID),
			AccountCategory:     silver.StrPtrIfNonEmpty(joinSafekeepingCategory(p.AcctTpDesc, p.AcctSubTypeDesc)),
			PortfolioExternalID: silver.StrPtrIfNonEmpty(portfolioID.String),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		}
		if w := taxWrapperForSafekeepingAcctTp(p.AcctTpCd, p.AcctTpDesc); w != "" {
			change.TaxWrapper = &w
		}
		if s := managementStyleForSafekeepingSubType(p.AcctSubTypeDesc); s != "" {
			change.ManagementStyle = &s
		}
		batch.Accounts = append(batch.Accounts, change)

		if p.AcctDesc != "" && portfolioID.Valid && portfolioID.String != "" {
			if mandatePortfolios[snap] == nil {
				mandatePortfolios[snap] = map[string]bool{}
			}
			mandatePortfolios[snap][portfolioID.String] = true
		}
	}
	return mandatePortfolios, rows.Err()
}

func (c *psnReader) appendPortfolios(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	// base_currency is promoted in silver migration 0002 — read
	// directly. The payload still carries the full PrtflKey (with
	// PrtflElmntData, performance, etc.) for forensics.
	const q = `
SELECT snapshot_at, relationship_id, portfolio_external_id,
       base_currency, payload
  FROM portfolios
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendPortfolios: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap         int64
			relID, extID string
			baseCcy      sql.NullString
			payload      string
		)
		if err := rows.Scan(&snap, &relID, &extID, &baseCcy, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		batch.Portfolios = append(batch.Portfolios, canonical.PortfolioChange{
			PortfolioExternalID: extID,
			BaseCurrency:        silver.StrPtrIfNonEmpty(baseCcy.String),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// ---- instruments + holdings ----------------------------------------------

// instrumentMeta is the lookup we build from `instruments` so
// appendHoldings can populate position.asset_class and currency
// without re-parsing instrument payloads on every holding.
type instrumentMeta struct {
	AssetClass canonical.AssetClass
	Currency   string
}

type instrumentPayload struct {
	InstrCtgyCFI        string          `json:"InstrCtgyCFI"`
	InstrCtgyCFIDesc    string          `json:"InstrCtgyCFIDesc"`
	InstrNm             instrumentNames `json:"InstrNm"`
	GacInstrRskCcyIsoCd string          `json:"GacInstrRskCcyIsoCd"`
	// UacAsstClsCd is UBS's internal asset-class code, used as a
	// classification fallback when InstrCtgyCFI is empty — that
	// happens for non-listed custody items like gold-deposit
	// receipts and private-market vehicles. See classmap.go.
	UacAsstClsCd string `json:"UacAsstClsCd"`
}

// instrumentNames is the UBS InstrNm object — a multi-language
// envelope carrying long and short names per locale. We pick
// LngNmEnglish for the canonical Name; ShrtNmEnglish is a useful
// fallback when the long name is empty.
type instrumentNames struct {
	LngNmEnglish  string `json:"LngNmEnglish"`
	ShrtNmEnglish string `json:"ShrtNmEnglish"`
}

func (n instrumentNames) Best() string {
	if n.LngNmEnglish != "" {
		return n.LngNmEnglish
	}
	return n.ShrtNmEnglish
}

// appendInstruments emits InstrumentChange for any instruments
// row in the window AND returns a (isin → meta) lookup populated
// from the **most-recent** instrument row across the whole silver
// DB (not just the window). UBS silver's loader applies content-
// dedup, so most snapshots don't carry a fresh instruments row;
// looking up by (snapshot_at, isin) would fail for the holding's
// snapshot most of the time. Latest-known-per-ISIN is the right
// resolution — instrument metadata is functionally immutable
// (name, asset class) and stale-by-one-snapshot is harmless.
func (c *psnReader) appendInstruments(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) (map[string]instrumentMeta, error) {
	// ORDER BY snapshot_at ASC so the map[isin] write inside the
	// loop ends up holding the LATEST row's meta (later writes
	// overwrite earlier).
	const q = `
SELECT snapshot_at, isin, payload
  FROM instruments
 ORDER BY snapshot_at ASC`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("appendInstruments: %w", err)
	}
	defer rows.Close()

	lookup := make(map[string]instrumentMeta)
	for rows.Next() {
		var (
			snap    int64
			isin    string
			payload string
		)
		if err := rows.Scan(&snap, &isin, &payload); err != nil {
			return nil, err
		}
		var p instrumentPayload
		_ = json.Unmarshal([]byte(payload), &p)
		ac := assetClassForInstrument(p.InstrCtgyCFI, p.UacAsstClsCd, p.InstrNm.Best())
		lookup[isin] = instrumentMeta{
			AssetClass: ac,
			Currency:   p.GacInstrRskCcyIsoCd,
		}

		// Only emit InstrumentChange for instruments INSIDE the
		// window. Gold's upsert guard would no-op the older rows
		// anyway, but skipping them here avoids the round-trip.
		if snap < w.Start || snap > w.End {
			continue
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: isin,
			AssetClass:           ac,
			ISIN:                 &isin,
			Name:                 silver.StrPtrIfNonEmpty(p.InstrNm.Best()),
			Currency:             silver.StrPtrIfNonEmpty(p.GacInstrRskCcyIsoCd),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
			Payload:              json.RawMessage(payload),
		})
	}
	return lookup, rows.Err()
}

// holdingsPayloadShape mirrors the relevant slice of the MT535
// payload — the two SWIFT tags we extract (19A monetary amounts,
// 93B quantities). Everything else stays in the raw payload for
// forensics.
type holdingsPayloadShape struct {
	Fields struct {
		Tag19A []string `json:"19A"`
		Tag93B []string `json:"93B"`
	} `json:"fields"`
}

// appendHoldings projects MT535 securities holdings into
// PositionChange. Quantity comes from the AGGR 93B subfield;
// market_value comes from the HOLD 19A subfield whose currency
// matches the instrument's natural currency (falls back to first
// HOLD entry when no exact match exists). See mt535.go for the
// parser and docs/adapters/ubs.md §4 for the rationale.
//
// As of silver migration 0002 both holdings and safekeeping_
// accounts use the same AcctId form for the safekeeping ID
// (e.g. BBBBxxxxxxxxxxS1); no cross-table translation is needed.
func (c *psnReader) appendHoldings(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, instr map[string]instrumentMeta) error {
	const q = `
SELECT snapshot_at, safekeeping_external_id, isin, payload
  FROM holdings
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHoldings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			snap          int64
			safekeepingID string
			isin          string
			payload       string
		)
		if err := rows.Scan(&snap, &safekeepingID, &isin, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}

		// Resolve instrument metadata via the latest-known lookup
		// built in appendInstruments (UBS silver dedups
		// instruments per content, so most snapshots don't carry
		// a fresh row). Fall back to AssetClass=other / blank
		// currency when the ISIN has never been seen — the
		// currency-from-HOLD step below typically rescues us.
		meta := instrumentMeta{AssetClass: canonical.AssetClassOther}
		if m, ok := instr[isin]; ok {
			meta = m
		}

		// MT535 SWIFT-tag parsing: pull aggregate quantity from
		// the 93B array and pick a 19A:HOLD entry for market
		// value. The HOLD entry's currency is the truthiest
		// source of "the currency this position is reported in" —
		// UBS publishes one HOLD per (currency the bank books
		// this in), and the instrument's natural-currency entry
		// is preferred when known. See mt535.go and
		// docs/adapters/ubs.md §4.
		var hp holdingsPayloadShape
		_ = json.Unmarshal([]byte(payload), &hp) // best-effort; bad payloads leave both NULL
		amounts := parse19A(hp.Fields.Tag19A)
		qtys := parse93B(hp.Fields.Tag93B)

		var quantity, marketValue *canonical.Decimal
		if q, ok := findQuantity(qtys); ok {
			qq := q
			quantity = &qq
		}
		mvAmt, mvCcy, mvOk := findHoldEntry(amounts, meta.Currency)
		if mvOk {
			mv := mvAmt
			marketValue = &mv
		}

		// Position currency MUST equal the currency the stored
		// market_value is denominated in — i.e. the currency of the
		// chosen 19A:HOLD leg (mvCcy). The instrument's
		// GacInstrRskCcyIsoCd is the issuer's *domicile / risk*
		// currency, which diverges from the holding's quotation
		// currency for cross-listed names: Cayman- or PRC-
		// incorporated, HK-listed shares book in HKD but carry a
		// domicile risk currency (KYD / CNY); US-listed ADRs of
		// Asian issuers book in USD but carry a domicile risk
		// currency (TWD / KRW / INR). Tagging the HOLD amount with
		// the domicile currency made gold convert it at the wrong
		// FX rate — a large over- or under-statement depending on
		// the peg. So mvCcy wins; the instrument's risk currency is
		// only a fallback for the no-HOLD case (where market_value
		// is nil anyway), and "XXX" is the last resort.
		positionCcy := mvCcy
		if positionCcy == "" {
			positionCcy = meta.Currency
		}
		if positionCcy == "" {
			positionCcy = "XXX"
		}

		isinCopy := isin
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    safekeepingID,
			PositionKey:          isin,
			InstrumentExternalID: &isinCopy,
			AssetClass:           meta.AssetClass,
			Currency:             positionCcy,
			Quantity:             quantity,
			MarketValue:          marketValue,
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// ---- cash_balances / fx_rates / forwards ---------------------------------

type cashBalancePayload struct {
	Amount      canonical.Decimal `json:"amount"`
	CreditDebit string            `json:"credit_debit"`
	CurrencyISO string            `json:"currency_iso"`
}

// As of silver migration 0002 cash_balances.account_external_id
// is the IBAN, matching cash_accounts.account_external_id; no
// cross-table translation is needed.
func (c *psnReader) appendCashBalances(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, account_external_id, balance_kind, currency_iso, payload
  FROM cash_balances
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendCashBalances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                            int64
			extID, balanceKind, currencyISO string
			payload                         string
		)
		if err := rows.Scan(&snap, &extID, &balanceKind, &currencyISO, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}

		var p cashBalancePayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return fmt.Errorf("cash_balances payload (snap=%d): %w", snap, err)
		}

		amount := p.Amount
		if strings.ToUpper(p.CreditDebit) == "D" || strings.ToUpper(p.CreditDebit) == "DBIT" || strings.ToUpper(p.CreditDebit) == "DEBIT" {
			amount = amount.Neg()
		}

		bk := canonicalBalanceKind(balanceKind)
		if !bk.Valid() {
			continue
		}
		batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
			SnapshotAt:        snap,
			AccountExternalID: extID,
			Currency:          currencyISO,
			BalanceKind:       bk,
			Amount:            amount,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

func canonicalBalanceKind(silverKind string) canonical.BalanceKind {
	switch silverKind {
	case "opening":
		return canonical.BalanceKindOpening
	case "closing":
		return canonical.BalanceKindClosing
	case "available":
		return canonical.BalanceKindAvailable
	default:
		return ""
	}
}

type fxRatePeriod struct {
	MiddleRate canonical.Decimal `json:"MiddleRate"`
}

// fxRatePayload — UBS ships ForeignExchangeRatePeriodData as
// either a single-rate object (~65% of rows in observed real data)
// OR an array of period entries (~35%). We capture the raw bytes
// and split based on the leading byte; see firstFxPeriod.
type fxRatePayload struct {
	ForeignExchangeRatePeriodData json.RawMessage `json:"ForeignExchangeRatePeriodData"`
}

// firstFxPeriod returns the first (or only) period entry, handling
// both the object and array shapes of the upstream JSON. Returns
// ok=false when the raw JSON is empty or an empty array.
func firstFxPeriod(raw json.RawMessage) (fxRatePeriod, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return fxRatePeriod{}, false, nil
	}
	switch trimmed[0] {
	case '{':
		var p fxRatePeriod
		if err := json.Unmarshal(trimmed, &p); err != nil {
			return fxRatePeriod{}, false, err
		}
		return p, true, nil
	case '[':
		var arr []fxRatePeriod
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return fxRatePeriod{}, false, err
		}
		if len(arr) == 0 {
			return fxRatePeriod{}, false, nil
		}
		return arr[0], true, nil
	default:
		return fxRatePeriod{}, false, fmt.Errorf("unexpected ForeignExchangeRatePeriodData shape")
	}
}

func (c *psnReader) appendFxRates(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, base_currency_iso, quote_currency_iso, payload
  FROM fx_rates
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendFxRates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                       int64
			baseCcy, quoteCcy, payload string
		)
		if err := rows.Scan(&snap, &baseCcy, &quoteCcy, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var p fxRatePayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return fmt.Errorf("fx_rates payload (snap=%d): %w", snap, err)
		}
		period, ok, err := firstFxPeriod(p.ForeignExchangeRatePeriodData)
		if err != nil {
			return fmt.Errorf("fx_rates payload (snap=%d): %w", snap, err)
		}
		if !ok {
			continue
		}
		batch.FxRates = append(batch.FxRates, canonical.FxRateChange{
			SnapshotAt:    snap,
			BaseCurrency:  baseCcy,
			QuoteCurrency: quoteCcy,
			MidRate:       period.MiddleRate,
			Payload:       json.RawMessage(payload),
		})
	}
	return rows.Err()
}

type forwardPayload struct {
	MrktValueAmt      *canonical.Decimal `json:"MrktValueAmt"`
	MrktValueCcyIsoCd string             `json:"MrktValueCcyIsoCd"`
	PrtflId           string             `json:"PrtflId"`
}

// appendForwardContracts projects open FX-forward contracts as
// PositionChange rows with asset_class=fx_forward. UBS attributes
// these directly to the portfolio (no sub-account); we attach
// them to a synthetic per-portfolio overlay account
// ("<portfolio_id>:overlay", account_kind=overlay) so every
// position remains owned by an `accounts` row and the totals
// across `accounts` and `portfolios` tie out against `positions
// --with-cash`. See docs/adapters/ubs.md.
func (c *psnReader) appendForwardContracts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, contract_external_id, payload
  FROM forward_contracts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendForwardContracts: %w", err)
	}
	defer rows.Close()
	// Track which (snapshot, portfolio) overlay accounts we've
	// already emitted to avoid one AccountChange per forward.
	overlayEmitted := map[[2]string]bool{}
	for rows.Next() {
		var (
			snap       int64
			contractID string
			payload    string
		)
		if err := rows.Scan(&snap, &contractID, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var p forwardPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return fmt.Errorf("forward_contracts payload (snap=%d): %w", snap, err)
		}
		if p.PrtflId == "" {
			continue
		}
		ccy := p.MrktValueCcyIsoCd
		if ccy == "" {
			ccy = "XXX"
		}
		overlayID := overlayAccountID(p.PrtflId)
		key := [2]string{fmt.Sprintf("%d", snap), p.PrtflId}
		if !overlayEmitted[key] {
			overlayEmitted[key] = true
			pid := p.PrtflId
			batch.Accounts = append(batch.Accounts, canonical.AccountChange{
				AccountExternalID:   overlayID,
				AccountKind:         canonical.AccountKindOverlay,
				DisplayName:         silver.StrPtrIfNonEmpty("Portfolio overlay"),
				PortfolioExternalID: &pid,
				FirstSeenAt:         snap,
				LastSeenAt:          snap,
			})
		}
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:        snap,
			AccountExternalID: overlayID,
			PositionKey:       contractID,
			AssetClass:        canonical.AssetClassFxForward,
			Currency:          ccy,
			MarketValue:       p.MrktValueAmt,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// overlayAccountID returns the synthetic account_external_id that
// holds a portfolio's direct positions (forward contracts, MMC,
// OTC). One per portfolio.
func overlayAccountID(portfolioID string) string {
	return portfolioID + ":overlay"
}

// ---- helpers --------------------------------------------------------------

// joinSafekeepingCategory combines the safekeeping AcctTpDesc and
// AcctSubTypeDesc into a single category string for gold's
// account_category column. Returns the type alone when the sub-
// type is missing (the common case for non-Custody types) and the
// empty string when both are blank.
func joinSafekeepingCategory(typ, subtype string) string {
	typ = strings.TrimSpace(typ)
	subtype = strings.TrimSpace(subtype)
	switch {
	case typ != "" && subtype != "":
		return typ + " / " + subtype
	case typ != "":
		return typ
	case subtype != "":
		return subtype
	default:
		return ""
	}
}
