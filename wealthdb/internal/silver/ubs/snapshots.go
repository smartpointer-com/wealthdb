package ubs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// snapshotStream buffers the whole window upfront and emits one
// batch per dump_runs.snapshot_at. Same shape as the Schwab
// adapter; see docs/adapters/ubs.md §3 for the coverage matrix.
type snapshotStream struct {
	batches []canonical.SnapshotBatch
	idx     int
}

func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return &snapshotStream{}, nil
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
	if err := c.appendSafekeepingAccounts(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendPortfolios(ctx, w, byTime); err != nil {
		return nil, err
	}
	// Build the (relationship_id, suffix) → canonical safekeeping
	// ID lookup. UBS silver stores the same safekeeping account in
	// two formats — `0230-xxxxxxxx.S1` in safekeeping_accounts and
	// `023000xxxxxxxxS1` in holdings — both ending in the same
	// "S1"/"T1"/... suffix per (relationship, snapshot). Without
	// this normalisation appendHoldings would stamp the MT535
	// format onto position.account_external_id and downstream
	// joins against gold.accounts would never match.
	safekeepingLookup, err := c.safekeepingIDLookup(ctx)
	if err != nil {
		return nil, err
	}
	// Same flavour of mismatch on the cash side: cash_balances
	// records carry AcctId-style IDs ("023000xxxxxxxx010000G")
	// while cash_accounts uses the IBAN ("CH0000230230xxxxxxxx"). The
	// AcctId is also present inside cash_accounts.payload, so the
	// lookup is built from there.
	cashLookup, err := c.cashIDLookup(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.appendHoldings(ctx, w, byTime, instrMap, safekeepingLookup); err != nil {
		return nil, err
	}
	if err := c.appendCashBalances(ctx, w, byTime, cashLookup); err != nil {
		return nil, err
	}
	if err := c.appendFxRates(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendForwardContracts(ctx, w, byTime); err != nil {
		return nil, err
	}

	out := &snapshotStream{batches: make([]canonical.SnapshotBatch, 0, len(times))}
	for _, t := range times {
		out.batches = append(out.batches, *byTime[t])
	}
	return out, nil
}

func (s *snapshotStream) Next(context.Context) (canonical.SnapshotBatch, bool, error) {
	if s.idx >= len(s.batches) {
		return canonical.SnapshotBatch{}, false, nil
	}
	b := s.batches[s.idx]
	s.idx++
	return b, s.idx < len(s.batches), nil
}

func (s *snapshotStream) Close() error { return nil }

func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `SELECT snapshot_at FROM dump_runs WHERE snapshot_at BETWEEN ? AND ? ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
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

// cashAccountPayload covers the UBS SDCA fields we extract. AcctId
// (the SWIFT-flavoured cash-account ID) is also captured so
// appendCashBalances can rewrite cash_balances rows from that
// format into the canonical IBAN form held in account_external_id.
// PrtflId names the parent portfolio (when the account is part of
// a wealth-management portfolio) — forwarded as
// ParentAccountExternalID so the accounts rollup can sum component
// balances into their portfolio row.
type cashAccountPayload struct {
	AcctCcyIsoCd string `json:"AcctCcyIsoCd"`
	AcctTpDesc   string `json:"AcctTpDesc"`
	AcctId       string `json:"AcctId"`
	PrtflId      string `json:"PrtflId"`
}

func (c *Connection) appendCashAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, relationship_id, account_external_id, payload
  FROM cash_accounts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendCashAccounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap     int64
			relID    string
			extID    string
			payload  string
		)
		if err := rows.Scan(&snap, &relID, &extID, &payload); err != nil {
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
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID:       extID,
			AccountKind:             canonical.AccountKindCash,
			BaseCurrency:            strPtrIfNonEmpty(p.AcctCcyIsoCd),
			RelationshipID:          strPtrIfNonEmpty(relID),
			AccountCategory:         strPtrIfNonEmpty(p.AcctTpDesc),
			ParentAccountExternalID: strPtrIfNonEmpty(p.PrtflId),
			FirstSeenAt:             snap,
			LastSeenAt:              snap,
			Payload:                 json.RawMessage(payload),
		})
	}
	return rows.Err()
}

type safekeepingPayload struct {
	InvstmtCcyIsoCd string `json:"InvstmtCcyIsoCd"`
	AcctTpDesc      string `json:"AcctTpDesc"`
	AcctSubTypeDesc string `json:"AcctSubTypeDesc"`
	PrtflId         string `json:"PrtflId"`
}

func (c *Connection) appendSafekeepingAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, relationship_id, account_external_id, payload
  FROM safekeeping_accounts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendSafekeepingAccounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap            int64
			relID, extID    string
			payload         string
		)
		if err := rows.Scan(&snap, &relID, &extID, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var p safekeepingPayload
		_ = json.Unmarshal([]byte(payload), &p)

		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindSafekeeping,
			// DisplayName left nil; see appendCashAccounts. The
			// AcctTpDesc plus AcctSubTypeDesc when present (the
			// sub-type sharpens "Custody" / "Cust Strap." into
			// "Custody / Cash-Custody", "Custody / Personal
			// Cust.", etc.) goes into AccountCategory.
			BaseCurrency:            strPtrIfNonEmpty(p.InvstmtCcyIsoCd),
			RelationshipID:          strPtrIfNonEmpty(relID),
			AccountCategory:         strPtrIfNonEmpty(joinSafekeepingCategory(p.AcctTpDesc, p.AcctSubTypeDesc)),
			ParentAccountExternalID: strPtrIfNonEmpty(p.PrtflId),
			FirstSeenAt:             snap,
			LastSeenAt:              snap,
			Payload:                 json.RawMessage(payload),
		})
	}
	return rows.Err()
}

func (c *Connection) appendPortfolios(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, relationship_id, portfolio_external_id, payload
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
			payload      string
		)
		if err := rows.Scan(&snap, &relID, &extID, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var p portfolioPayload
		_ = json.Unmarshal([]byte(payload), &p) // best-effort
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindPortfolio,
			BaseCurrency:      strPtrIfNonEmpty(p.PrtflKey.PrtflCcyIsoCd),
			RelationshipID:    strPtrIfNonEmpty(relID),
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// portfolioPayload extracts the portfolio-level base currency
// from PrtflKey.PrtflCcyIsoCd. The rest of the portfolio payload
// (PrtflElmntData composition list, perf metrics) stays in
// AccountChange.Payload for forensics.
type portfolioPayload struct {
	PrtflKey struct {
		PrtflCcyIsoCd string `json:"PrtflCcyIsoCd"`
	} `json:"PrtflKey"`
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
	InstrCtgyCFI       string `json:"InstrCtgyCFI"`
	InstrCtgyCFIDesc   string `json:"InstrCtgyCFIDesc"`
	InstrNm            string `json:"InstrNm"`
	GacInstrRskCcyIsoCd string `json:"GacInstrRskCcyIsoCd"`
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
func (c *Connection) appendInstruments(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) (map[string]instrumentMeta, error) {
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
		ac := assetClassForCFI(p.InstrCtgyCFI)
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
			Name:                 strPtrIfNonEmpty(p.InstrNm),
			Currency:             strPtrIfNonEmpty(p.GacInstrRskCcyIsoCd),
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
// safekeepingLookup translates the MT535-flavoured
// safekeeping_external_id stored in `holdings`
// ("023000xxxxxxxxS1") into the canonical safekeeping_accounts
// format ("0230-xxxxxxxx.S1"). When a row's (relationship_id,
// suffix) pair has no entry, the raw silver value is forwarded
// — defensive against suffix-extraction edge cases (the gold-side
// join will simply miss).
func (c *Connection) appendHoldings(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, instr map[string]instrumentMeta, safekeepingLookup map[[2]string]string) error {
	const q = `
SELECT snapshot_at, relationship_id, safekeeping_external_id, isin, payload
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
			relID         string
			safekeepingID string
			isin          string
			payload       string
		)
		if err := rows.Scan(&snap, &relID, &safekeepingID, &isin, &payload); err != nil {
			return err
		}
		if canonical := safekeepingLookup[[2]string{relID, trailingSuffix(safekeepingID)}]; canonical != "" {
			safekeepingID = canonical
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

		// Position currency precedence: instrument meta wins when
		// known (matches the chosen HOLD entry anyway since we
		// preferred it); else the chosen HOLD entry's currency
		// (rescues "XXX-currency" positions whose instrument has
		// no GacInstrRskCcyIsoCd but whose holding payload is
		// reported in a real currency); else the "XXX" sentinel.
		positionCcy := meta.Currency
		if positionCcy == "" {
			positionCcy = mvCcy
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

func (c *Connection) appendCashBalances(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, cashLookup map[[2]string]string) error {
	const q = `
SELECT snapshot_at, relationship_id, account_external_id, balance_kind, currency_iso, payload
  FROM cash_balances
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendCashBalances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                   int64
			relID, extID, balanceKind, currencyISO string
			payload                                string
		)
		if err := rows.Scan(&snap, &relID, &extID, &balanceKind, &currencyISO, &payload); err != nil {
			return err
		}
		if iban := cashLookup[[2]string{relID, extID}]; iban != "" {
			extID = iban
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

func (c *Connection) appendFxRates(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
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
// PositionChange rows with asset_class=fx_forward. The portfolio
// is treated as the parent account; see docs/adapters/ubs.md.
func (c *Connection) appendForwardContracts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, contract_external_id, payload
  FROM forward_contracts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendForwardContracts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap             int64
			contractID       string
			payload          string
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
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:        snap,
			AccountExternalID: p.PrtflId,
			PositionKey:       contractID,
			AssetClass:        canonical.AssetClassFxForward,
			Currency:          ccy,
			MarketValue:       p.MrktValueAmt,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// ---- helpers --------------------------------------------------------------

func strPtrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// safekeepingIDLookup builds a (relationship_id, suffix) →
// canonical safekeeping_accounts.account_external_id map from
// silver. UBS PSN's MT535 holdings record uses a stripped/padded
// form of the safekeeping ID ("023000xxxxxxxxS1") while the
// safekeeping_accounts table uses the canonical
// ("0230-xxxxxxxx.S1") form — they share the trailing
// letter+digits suffix ("S1", "T1", ...) within a relationship.
// Without this lookup, position.account_external_id never joins
// to gold.accounts.
//
// Built once per Snapshots() call against the WHOLE silver table
// (not just the change window) so a holding's lookup never
// misses just because its safekeeping_accounts record was last
// updated in an earlier snapshot — UBS dedups by content like
// instruments do.
func (c *Connection) safekeepingIDLookup(ctx context.Context) (map[[2]string]string, error) {
	const q = `SELECT DISTINCT relationship_id, account_external_id FROM safekeeping_accounts`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("safekeepingIDLookup: %w", err)
	}
	defer rows.Close()

	out := make(map[[2]string]string)
	for rows.Next() {
		var rel, id string
		if err := rows.Scan(&rel, &id); err != nil {
			return nil, err
		}
		suffix := suffixAfterDot(id)
		if suffix == "" {
			continue
		}
		out[[2]string{rel, suffix}] = id
	}
	return out, rows.Err()
}

// cashIDLookup builds a (relationship_id, AcctId) →
// account_external_id map from silver.cash_accounts, where
// AcctId is the SWIFT-flavoured cash-account ID
// ("023000xxxxxxxx010000G") and account_external_id is the IBAN
// ("CH0000230230xxxxxxxx..."). cash_balances records reference
// accounts by AcctId, so without this rewrite the balances never
// join to gold.accounts.
//
// Built once per Snapshots() call against the whole silver table
// — same idempotency reasoning as safekeepingIDLookup.
func (c *Connection) cashIDLookup(ctx context.Context) (map[[2]string]string, error) {
	const q = `SELECT DISTINCT relationship_id, account_external_id, payload FROM cash_accounts`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("cashIDLookup: %w", err)
	}
	defer rows.Close()

	out := make(map[[2]string]string)
	for rows.Next() {
		var rel, iban, payload string
		if err := rows.Scan(&rel, &iban, &payload); err != nil {
			return nil, err
		}
		var p cashAccountPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			continue // tolerate; just no lookup entry
		}
		if p.AcctId == "" {
			continue
		}
		out[[2]string{rel, p.AcctId}] = iban
	}
	return out, rows.Err()
}

// suffixAfterDot returns whatever follows the LAST `.` in s, or
// "" if there's no dot. "0230-xxxxxxxx.S1" → "S1".
func suffixAfterDot(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return ""
}

// trailingSuffix extracts a trailing [A-Z][0-9]+ from s, or "" if
// no such suffix exists. "023000xxxxxxxxS1" → "S1"; "ABC123XY"
// → "" (no digits at end); "" → "".
func trailingSuffix(s string) string {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == 0 || i == len(s) {
		return ""
	}
	if s[i-1] < 'A' || s[i-1] > 'Z' {
		return ""
	}
	return s[i-1:]
}

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
