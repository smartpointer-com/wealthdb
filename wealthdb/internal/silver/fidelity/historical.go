package fidelity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Historical-snapshot reader for the silver table
// `historical_position_snapshots` (fidelity-web migration 0004):
// holdings parsed from statement PDFs — fidelity-web's 529 and
// supplied statements, and the svb statement archives.
//
// The table's primary key is (as_of_date, account_external_id,
// description). `instrument_key` is in the same identity space as
// the live `positions` table — a ticker, CUSIP or OCC code — so no
// bridge map is needed (unlike schwab-web, where the api hashValue
// has to translate to the web suffix). A row without one takes the
// key the rest of the silver names its security by (histlink.go),
// else a stable identity synthesised from its description so gold
// still has an instrument to hang it on.

// historicalSnapshotTimes returns the distinct as_of_date values
// in `historical_position_snapshots` that fall inside the window,
// so the byTime dispatch in Snapshots() carries a batch slot per
// historical date too.
func (c *Connection) historicalSnapshotTimes(ctx context.Context, w canonical.Window) ([]int64, error) {
	if !c.schema.historical {
		return nil, nil
	}
	return silver.SnapshotTimes(ctx, c.db, w, nil, `
SELECT DISTINCT as_of_date
  FROM historical_position_snapshots
 WHERE as_of_date BETWEEN ? AND ?`,
		w.Start, w.End)
}

// dafAccountIDs returns the set of account ids grouped under a
// kind='daf' portfolio in any snapshot — the Donor-Advised Fund
// domain, whose historical rows are pool holdings (see
// appendHistoricalPositions). Empty (not an error) on pre-v7 silvers
// with no daf portfolios.
func (c *Connection) dafAccountIDs(ctx context.Context) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if err := silver.ScanRows(ctx, c.db, "dafAccountIDs", `
SELECT DISTINCT a.account_external_id
  FROM accounts a JOIN portfolios p
    ON p.snapshot_at = a.snapshot_at
   AND p.portfolio_external_id = a.portfolio_external_id
 WHERE p.kind = 'daf'`, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		out[id] = struct{}{}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// heldKinds maps a vehicle the historical classifier reads off a row to
// the account kind holding it makes an account: a home loan's
// outstanding principal types a mortgage account, a deposit account's
// balance a cash one.
var heldKinds = map[canonical.Vehicle]canonical.AccountKind{
	canonical.VehicleMortgage:      canonical.AccountKindMortgage,
	canonical.VehicleDemandDeposit: canonical.AccountKindCash,
}

// heldAccountKinds returns the kind each account's historical rows type
// it as (heldKinds); applyHeldKind applies it. A loan's principal
// outranks a deposit balance, so the answer never depends on row order.
// It reads the whole silver, not the load window, so an account's kind
// never depends on which window a load happens to read. Empty on a
// silver with no historical table.
func (c *Connection) heldAccountKinds(ctx context.Context) (map[string]canonical.AccountKind, error) {
	out := map[string]canonical.AccountKind{}
	if !c.schema.historical {
		return out, nil
	}
	if err := silver.ScanRows(ctx, c.db, "heldAccountKinds", `
SELECT DISTINCT account_external_id, COALESCE(instrument_key, ''), description
  FROM historical_position_snapshots`, func(rows *sql.Rows) error {
		var id, key, desc string
		if err := rows.Scan(&id, &key, &desc); err != nil {
			return err
		}
		_, vehicle := classifyHistoricalPair(key, desc)
		if kind, ok := heldKinds[vehicle]; ok && out[id] != canonical.AccountKindMortgage {
			out[id] = kind
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// appendHistoricalAccounts projects the per-account master rows
// captured in the live `accounts` table backwards onto each
// historical as_of_date. Without this, `wealthdb holdings accounts
// --as-of <historical-date>` would show no account for a date
// the historical positions cover.
//
// Any one live `accounts` row per account_external_id serves
// (they're stable across snapshots for a given account) and copy
// nickname / portfolio / management_style verbatim. Gold's
// upsert pulls FirstSeenAt back to the historical date so the
// account's first-seen timestamp reflects the statement, not
// the toolkit-first-ran date.
func (c *Connection) appendHistoricalAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, held map[string]canonical.AccountKind) error {
	mgmtCol := "NULL"
	if c.schema.managementStyle {
		mgmtCol = "a.management_style"
	}
	// One live `accounts` row per distinct account_external_id
	// that appears in historical_position_snapshots (joined on
	// account, not date — the snapshot_at of the live row may be
	// well after the historical date).
	q := fmt.Sprintf(`
SELECT DISTINCT a.account_external_id, a.portfolio_external_id,
       a.nickname, a.payload, p.kind, COALESCE(%s, '')
  FROM historical_position_snapshots h
  JOIN accounts a
    ON a.account_external_id = h.account_external_id
  LEFT JOIN portfolios p
    ON p.snapshot_at = a.snapshot_at
   AND p.portfolio_external_id = a.portfolio_external_id
 WHERE h.as_of_date BETWEEN ? AND ?
   AND a.snapshot_at = (
       SELECT MAX(a2.snapshot_at) FROM accounts a2
        WHERE a2.account_external_id = a.account_external_id
   )`, mgmtCol)
	usd := "USD"
	type acctRow struct {
		extID, payload, silverMgmt           string
		portfolioID, nickname, portfolioKind sql.NullString
	}
	seedRows, err := silver.Query(ctx, c.db, "appendHistoricalAccounts", q, func(rows *sql.Rows) (rec acctRow, err error) {
		var r acctRow
		if err := rows.Scan(&r.extID, &r.portfolioID, &r.nickname, &r.payload,
			&r.portfolioKind, &r.silverMgmt); err != nil {
			return rec, err
		}
		return r, nil
	}, w.Start, w.End)
	if err != nil {
		return err
	}
	// For each snapshot in the window, emit one AccountChange +
	// one PortfolioChange per seed row. Without the per-date
	// emission, the byTime dispatch in Snapshots() would have no
	// master data for the historical-only timestamps; on a live
	// timestamp the seed row joins the live one.
	for snap, batch := range byTime {
		for _, r := range seedRows {
			change := canonical.AccountChange{
				AccountExternalID:   r.extID,
				AccountKind:         canonical.AccountKindBrokerage,
				BaseCurrency:        &usd,
				Nickname:            silver.NullStringPtr(r.nickname),
				PortfolioExternalID: silver.NullStringPtr(r.portfolioID),
				FirstSeenAt:         snap,
				LastSeenAt:          snap,
				Payload:             json.RawMessage(r.payload),
			}
			applyPortfolioKindTaxonomy(r.portfolioKind.String, &change)
			applyHeldKind(r.extID, held, &change)
			if r.silverMgmt != "" {
				s := canonical.ManagementStyle(r.silverMgmt)
				change.ManagementStyle = &s
			}
			batch.Accounts = append(batch.Accounts, change)
			if r.portfolioID.Valid && r.portfolioID.String != "" {
				display := portfolioDisplay(r.portfolioID.String, r.portfolioKind.String)
				batch.Portfolios = append(batch.Portfolios, canonical.PortfolioChange{
					PortfolioExternalID: r.portfolioID.String,
					DisplayName:         &display,
					BaseCurrency:        &usd,
					FirstSeenAt:         snap,
					LastSeenAt:          snap,
				})
			}
		}
	}
	return nil
}

// appendHistoricalPositions emits one InstrumentChange + one
// PositionChange per row in `historical_position_snapshots`. The
// table is positions-only (no cash-balance counterpart), so this
// path never produces CashBalanceChange rows. The statement PDFs
// carry no structured type code, so the (asset_class, vehicle)
// pair comes from the shape heuristics in classifyHistoricalPair
// (instrument-key and description shapes); the live-positions
// path still overwrites the instrument dimension whenever the same
// instrument_key reappears with a source-classified value.
//
// A row with no `instrument_key` takes the key the rest of the silver
// names its security by (historicalLinks), with its description cleaned
// of the statement's page artifacts as its name; else it is keyed by
// syntheticHistoricalInstrumentKey. A row's key is also its symbol,
// except a statement construct's (isStatementConstruct); its CUSIP is
// the one the lots state (instrumentCUSIPs). The book value
// is the cost basis the statement prints for the holding; the 529 and
// DAF layouts, the core account and the svb statements print none.
func (c *Connection) appendHistoricalPositions(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, cusips map[string]string) error {
	dafAccounts, err := c.dafAccountIDs(ctx)
	if err != nil {
		return err
	}
	// The statement's cost basis (migration 0009); NULL before it.
	costCol := "NULL"
	if c.schema.historicalCost {
		costCol = "CAST(cost_basis AS VARCHAR)"
	}
	q := fmt.Sprintf(`
SELECT as_of_date, account_external_id,
       COALESCE(instrument_key, ''),
       description,
       currency,
       CAST(quantity     AS VARCHAR),
       CAST(market_value AS VARCHAR),
       %s,
       payload
  FROM historical_position_snapshots
 WHERE as_of_date BETWEEN ? AND ?
 ORDER BY as_of_date, account_external_id, description`, costCol)
	links, err := c.historicalLinks(ctx)
	if err != nil {
		return err
	}

	if err := silver.ScanRows(ctx, c.db, "appendHistoricalPositions", q, func(rows *sql.Rows) error {
		var (
			snap                           int64
			acct, instrKey, desc, currency string
			qtyStr, valueStr, costStr      *canonical.Decimal
			payload                        string
		)
		if err := rows.Scan(&snap, &acct, &instrKey, &desc, &currency,
			silver.Dec(&qtyStr, silver.DecText), silver.Dec(&valueStr, silver.DecText), silver.Dec(&costStr, silver.DecText), &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			return nil
		}
		// Classified from the raw instrKey/desc (before the synthetic
		// key substitution below); a linked row from its key and
		// cleaned name (linkedClass). Rows on a Donor-Advised Fund
		// account are pool holdings by construction — the shape
		// heuristics don't apply to pool names ("Growth"), so they
		// get the same (multi_asset, fund) pair the live daf_pool
		// class maps to.
		assetClassNew, vehicle := classifyHistoricalPair(instrKey, desc)
		_, isDAF := dafAccounts[acct]
		name := desc
		if instrKey == "" && !isDAF && vehicle != canonical.VehicleOption && !isStatementConstruct(desc) {
			if key := links.link(snap, acct, desc); key != "" {
				instrKey, name = key, historicalDescription(desc)
				assetClassNew, vehicle = linkedClass(instrKey, name)
			}
		}
		if isDAF {
			assetClassNew, vehicle = canonical.AssetClassMultiAsset, canonical.VehicleFund
		}
		keyless := instrKey == ""
		if keyless {
			instrKey = syntheticHistoricalInstrumentKey(desc)
		}
		var symbol *string
		if !keyless || !isStatementConstruct(desc) {
			s := instrKey
			symbol = &s
		}
		ccy := currency
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: instrKey,
			AssetClass:           assetClassNew,
			Vehicle:              vehicle,
			CUSIP:                silver.StrPtrIfNonEmpty(cusips[instrKey]),
			Symbol:               symbol,
			Name:                 &name,
			Currency:             &ccy,
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})

		instrumentKey := instrKey
		pos := canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    acct,
			PositionKey:          instrKey,
			InstrumentExternalID: &instrumentKey,
			AssetClass:           assetClassNew,
			Vehicle:              vehicle,
			Currency:             currency,
			Quantity:             qtyStr,
			MarketValue:          valueStr,
			Payload:              json.RawMessage(payload),
		}
		pos.SetBookValue(costStr, lotBasis)
		batch.Positions = append(batch.Positions, pos)
		return nil
	}, w.Start, w.End); err != nil {
		return err
	}
	return nil
}

// syntheticHistoricalInstrumentKey deterministically derives a
// gold-side instrument identity from the description of a row that
// has no key — a security neither the silver cross-walk nor the link
// (histlink.go) keys, or a statement construct. "fidelity-hist:" + a
// sha prefix, stable across re-loads so the same statement → same gold
// rows.
func syntheticHistoricalInstrumentKey(description string) string {
	sum := sha256.Sum256([]byte(description))
	return "fidelity-hist:" + hex.EncodeToString(sum[:8])
}
