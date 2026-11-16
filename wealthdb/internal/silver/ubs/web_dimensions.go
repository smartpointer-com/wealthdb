package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// snapshotsForOverlap emits dimensions (portfolios, accounts,
// instruments) from web's live snapshots inside the window, with
// each row filtered to snapshot_at < PSN-start for its banking
// relationship. Positions and cash flow through PSN's snapshot
// stream instead; the orchestrator wraps PSN with a fold stream
// that injects web's per-(date, key) payload into PSN's rows.
//
// This split keeps PSN's faithful safekeeping / portfolio
// structure (web silver flattens all securities under one
// PrtflId) while still preserving web-only
// fields like cost_price inside `payload.web`. Web's instrument
// descriptions are a strict upgrade over PSN's InstrNm.LngNm-
// English (which is colon-formatted and less user-readable), so
// they're emitted here and let the per-column upsert guard pick
// the latest snapshot's name.
//
// Historical PDF snapshots are handled by snapshotsHistorical
// (historical.go); transactions are handled by
// transactionsBeforePSNStart.
func (r *webReader) snapshotsForOverlap(
	ctx context.Context,
	w canonical.Window,
	cutoffByWebRel map[string]int64,
	psnTaxPair map[string]taxPair,
) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	times, err := r.dumpRunTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	byTime := silver.NewBatches(times)

	// Dimensions only — portfolios, accounts, instruments. Positions / cash
	// come from the PSN side (with web payload folded in by the orchestrator).
	if err := r.appendWebPortfolios(ctx, w, byTime, cutoffByWebRel); err != nil {
		return nil, err
	}
	if err := r.appendWebAccounts(ctx, w, byTime, cutoffByWebRel); err != nil {
		return nil, err
	}
	if err := r.appendWebInstruments(ctx, w, byTime, psnTaxPair); err != nil {
		return nil, err
	}

	// Mortgages: unique to the web side — PSN doesn't carry them
	// at all. They flow through regardless of the PSN cutoff;
	// emitted as a triple per row (account + instrument +
	// position) so they show up as a single negative-value
	// liability holding in gold positions.
	if err := r.appendWebMortgages(ctx, w, byTime); err != nil {
		return nil, err
	}

	// Cards: web-only for the same reason as mortgages — PSN carries
	// none — so they too flow through regardless of the PSN cutoff. The
	// roster gives the current balance; the invoices give the history,
	// at the billing dates the bank drew them on rather than at the
	// dates a run happened to fetch them, so appendCardStatementBalances
	// may add batch times of its own.
	if err := r.appendWebCards(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := r.appendCardStatementBalances(ctx, w, byTime); err != nil {
		return nil, err
	}

	// Every batch time streams, including a period end the statement
	// balances above added that was not a dump time.
	batches := slices.DeleteFunc(byTime.Sorted(), func(b canonical.SnapshotBatch) bool {
		return len(b.Portfolios)+len(b.Accounts)+len(b.Instruments)+
			len(b.Positions)+len(b.CashBalances) == 0
	})
	return silver.NewSnapshotStream(batches), nil
}

// appendWebInstruments emits one InstrumentChange per (snapshot,
// ISIN) in the window using the web positions.description as the
// instrument's user-facing Name. ISINs that appear multiple
// times in the same snapshot (cross-portfolio holdings) coalesce
// to one emission with the description from the first row seen —
// the descriptions don't vary by portfolio.
//
// psnTaxPair maps ISIN → PSN's CFI/UAC-derived (asset_class, vehicle)
// pair. Web doesn't know an instrument's taxonomy (no CFI), and a
// naive (other, other) emission would overwrite PSN's specific pair
// via the per-column upsert guard (web's last_seen_at is typically
// later than PSN's). Stamping PSN's pair keeps the cross-source upsert
// idempotent on asset_class / vehicle while letting web win on Name.
// ISINs PSN has never seen fall back to the description-template
// classifier (taxonomyPairForWebDescription), then (other, other).
func (r *webReader) appendWebInstruments(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, psnTaxPair map[string]taxPair) error {
	const q = `
SELECT snapshot_at, instrument_isin, currency_iso, description
  FROM positions
 WHERE instrument_isin IS NOT NULL
   AND snapshot_at BETWEEN ? AND ?
 GROUP BY snapshot_at, instrument_isin`
	return silver.ScanRows(ctx, r.db, "appendWebInstruments", q, func(rows *sql.Rows) error {
		var (
			snap        int64
			isin        string
			ccy         string
			description sql.NullString
		)
		if err := rows.Scan(&snap, &isin, &ccy, &description); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			return nil
		}
		acNew := canonical.AssetClassOther
		vehicle := canonical.VehicleOther
		if tp, ok := psnTaxPair[isin]; ok && tp.AssetClass != "" {
			acNew = tp.AssetClass
			vehicle = tp.Vehicle
		} else if ac, veh, ok := taxonomyPairForWebDescription(description.String); ok {
			acNew, vehicle = ac, veh
		}
		isinCopy := isin
		// UBS web descriptions encode the listing ticker in
		// trailing parens — e.g. "Reg.shs Example AG
		// (XMPL)". Extract it and surface as the instrument's
		// Symbol so dividend / coupon transactions joined by
		// ISIN get a populated symbol column. Descriptions
		// without a trailing (TICKER) (ETFs identified only by
		// long-form name) keep Symbol nil.
		symbol := tickerFromDescription(description.String)
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: isin,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			ISIN:                 &isinCopy,
			Symbol:               symbol,
			Name:                 silver.StrPtrIfNonEmpty(description.String),
			Currency:             silver.StrPtrIfNonEmpty(ccy),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})
		return nil
	}, w.Start, w.End)
}

// dumpRunTimesInWindow returns the chronologically-sorted set of
// snapshot_at values in [w.Start, w.End] from dump_runs.
func (r *webReader) dumpRunTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `SELECT snapshot_at FROM dump_runs WHERE snapshot_at BETWEEN ? AND ? ORDER BY snapshot_at`
	return silver.Query(ctx, r.db, "ubs-web dumpRunTimesInWindow", q, func(rows *sql.Rows) (int64, error) {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return 0, err
		}
		return t, nil
	}, w.Start, w.End)
}

// appendWebPortfolios projects web silver portfolios into
// PortfolioChange. Cutoff applies via banking_relationship_id.
func (r *webReader) appendWebPortfolios(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, cutoff map[string]int64) error {
	const q = `
SELECT snapshot_at, portfolio_external_id, banking_relationship_id,
       description, payload
  FROM portfolios
 WHERE snapshot_at BETWEEN ? AND ?`
	return silver.ScanRows(ctx, r.db, "appendWebPortfolios", q, func(rows *sql.Rows) error {
		var (
			snap               int64
			extID, payload     string
			relID, description sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &relID, &description, &payload); err != nil {
			return err
		}
		if relID.Valid {
			if cut := cutoff[relID.String]; cut > 0 && snap >= cut {
				return nil
			}
		}
		batch, ok := byTime[snap]
		if !ok {
			return nil
		}
		batch.Portfolios = append(batch.Portfolios, canonical.PortfolioChange{
			PortfolioExternalID: extID,
			DisplayName:         silver.StrPtrIfNonEmpty(description.String),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID.String),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
		return nil
	}, w.Start, w.End)
}

// appendWebAccounts projects web silver accounts (cash and
// safekeeping; the schema's CHECK constraint already excludes
// card accounts). Cutoff applies via banking_relationship_id.
func (r *webReader) appendWebAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, cutoff map[string]int64) error {
	const q = `
SELECT snapshot_at, account_external_id, kind, currency_iso,
       banking_relationship_id, portfolio_external_id,
       description, payload
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`
	return silver.ScanRows(ctx, r.db, "appendWebAccounts", q, func(rows *sql.Rows) error {
		var (
			snap                                 int64
			extID, kind, payload                 string
			ccy, relID, portfolioID, description sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &kind, &ccy, &relID, &portfolioID, &description, &payload); err != nil {
			return err
		}
		if relID.Valid {
			if cut := cutoff[relID.String]; cut > 0 && snap >= cut {
				return nil
			}
		}
		batch, ok := byTime[snap]
		if !ok {
			return nil
		}
		var ak canonical.AccountKind
		switch kind {
		case "cash":
			ak = canonical.AccountKindCash
		case "safekeeping":
			ak = canonical.AccountKindSafekeeping
		default:
			ak = canonical.AccountKindOther
		}
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID:   extID,
			AccountKind:         ak,
			DisplayName:         silver.StrPtrIfNonEmpty(description.String),
			BaseCurrency:        silver.StrPtrIfNonEmpty(ccy.String),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID.String),
			PortfolioExternalID: silver.StrPtrIfNonEmpty(portfolioID.String),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
		return nil
	}, w.Start, w.End)
}

// appendWebMortgages projects ubs-web silver `mortgages` rows
// into three canonical changes per row:
//
//   - AccountChange{Kind: mortgage}           — one liability account
//   - InstrumentChange{AssetClass: mortgage}  — synthetic instrument
//     keyed by the same external ID (UBS doesn't expose a separate
//     instrument-level identity for mortgages)
//   - PositionChange{AssetClass: mortgage,
//     MarketValue: outstanding_balance}       — already negative in
//     the silver row; flows through to net-worth roll-ups as a
//     negative contribution
//
// No PSN cutoff: PSN doesn't surface mortgages at all, so every web
// snapshot's mortgage rows pass through. A silver without the
// `mortgages` table emits none.
func (r *webReader) appendWebMortgages(ctx context.Context,
	w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch) error {
	ok, err := r.hasTable(ctx, "mortgages")
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	const q = `
SELECT snapshot_at, account_external_id, banking_relationship_id,
       portfolio_external_id, currency_iso, description, payload
  FROM mortgages
 WHERE snapshot_at BETWEEN ? AND ?`
	return silver.ScanRows(ctx, r.db, "appendWebMortgages", q, func(rows *sql.Rows) error {
		var (
			snap                      int64
			extID, currency, payload  string
			relID, portfolioID, descr sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &relID, &portfolioID,
			&currency, &descr, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			return nil
		}
		// Dimensions only: the balance and terms stay inside payload, and
		// mortgage Positions are injected by the psn-web fold stream, NOT
		// emitted here. Emitting a Position at the web dump's snapshot_at
		// would create a mortgage-only gold snapshot at that time. It would
		// report the mortgage alone, the one account still reporting there,
		// so gold's per-account reading would end every other UBS account
		// last observed beside the mortgage (DESIGN.md §10.7). The fold
		// stream injects mortgages only into PSN batches that already carry
		// Positions, keeping snapshot times aligned.
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID:   extID,
			AccountKind:         canonical.AccountKindMortgage,
			TaxWrapper:          relationshipTaxWrapper(),
			DisplayName:         silver.StrPtrIfNonEmpty(descr.String),
			BaseCurrency:        silver.StrPtrIfNonEmpty(currency),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID.String),
			PortfolioExternalID: silver.StrPtrIfNonEmpty(portfolioID.String),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: extID,
			AssetClass:           canonical.AssetClassRealEstate,
			Vehicle:              canonical.VehicleMortgage,
			Name:                 silver.StrPtrIfNonEmpty(descr.String),
			Currency:             silver.StrPtrIfNonEmpty(currency),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})
		return nil
	}, w.Start, w.End)
}

// latestMortgagePositions returns one PositionChange per mortgage
// account in web silver, populated from the row with the largest
// snapshot_at ≤ asOf for that account. Used by the fold stream to
// carry web-only mortgage data forward into each PSN snapshot:
// web dumps fire on login, PSN snapshots fire nightly. A PSN
// snapshot without the mortgage would report every account the
// mortgage was last observed beside, so gold's per-account reading
// would end the mortgage there (DESIGN.md §10.7).
//
// The returned templates have SnapshotAt = 0; the caller stamps
// the right time per batch.
func (r *webReader) latestMortgagePositions(ctx context.Context, asOf int64) ([]canonical.PositionChange, error) {
	ok, err := r.hasTable(ctx, "mortgages")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	const q = `
SELECT m.account_external_id, m.currency_iso, m.outstanding_balance, m.payload
  FROM mortgages m
  JOIN (
        SELECT account_external_id, MAX(snapshot_at) AS s
          FROM mortgages
         WHERE snapshot_at <= ?
         GROUP BY account_external_id
       ) latest
    ON latest.account_external_id = m.account_external_id
   AND latest.s                    = m.snapshot_at`
	var out []canonical.PositionChange
	if err := silver.ScanRows(ctx, r.db, "latestMortgagePositions", q, func(rows *sql.Rows) error {
		var (
			extID, currency, payload string
			outstanding              *canonical.Decimal
		)
		if err := rows.Scan(&extID, &currency, silver.Dec(&outstanding, silver.DecFloat), &payload); err != nil {
			return err
		}
		idCopy := extID
		out = append(out, canonical.PositionChange{
			AccountExternalID:    extID,
			PositionKey:          extID,
			InstrumentExternalID: &idCopy,
			AssetClass:           canonical.AssetClassRealEstate,
			Vehicle:              canonical.VehicleMortgage,
			Currency:             currency,
			MarketValue:          outstanding,
			Payload:              json.RawMessage(payload),
		})
		return nil
	}, asOf); err != nil {
		return nil, err
	}
	return out, nil
}

// buildMortgageAccountIndex maps a folded mortgage reference onto the account
// id gold holds the mortgage under.
//
// It reads the ids rather than composing them. The branch a stamp omits is
// knowable only from the account itself, and the two places silver records one
// — the live positions export and the maturity-notice PDFs — already agree on
// the spelling, deliberately (the collector re-pads the PDF form so the two
// are byte-equal). Looking the whole id up therefore guesses nothing: a stamp
// naming a mortgage silver has never seen resolves to nothing, which is the
// right answer and leaves the row exactly as it was.
func (r *webReader) buildMortgageAccountIndex(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for _, table := range []string{"mortgages", "historical_mortgages"} {
		ok, err := r.hasTable(ctx, table)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if err := silver.ScanRows(ctx, r.db, "buildMortgageAccountIndex "+table,
			`SELECT DISTINCT account_external_id FROM `+table, func(rows *sql.Rows) error {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				if k := mortgageRefKey(id, true); k != "" {
					out[k] = id
				}
				return nil
			}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// looksLikeISIN: 12 chars, first two ASCII letters (country
// code), remaining 10 ASCII alphanumerics. Strict enough to
// reject "(SCMN)"-style ticker fragments while accepting every
// real ISIN.
func looksLikeISIN(s string) bool {
	if len(s) != 12 {
		return false
	}
	if !isASCIIAlpha(s[0]) || !isASCIIAlpha(s[1]) {
		return false
	}
	for i := 2; i < 12; i++ {
		c := s[i]
		if !isASCIIAlpha(c) && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
