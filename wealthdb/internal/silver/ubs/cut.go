package ubs

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// psnCut is the hard cut at each banking relationship's PSN start: a
// web row on or after it, on an account the MT940 feed speaks for, is
// PSN's to carry. Every pass that reasons about the emitted rows
// applies it, so none counts a row nothing emits.
type psnCut struct {
	startByRel   map[string]int64  // web relationship → PSN start (buildPSNStartByWebRel)
	relOfAccount map[string]string // web account → web relationship
	// coverage is which accounts the feed speaks for, and from when
	// (psnCashCoverage). The feed is delivered per account, so a
	// relationship's PSN start says nothing about an account the
	// delivery leaves out — and the cut yields a web row to the feed
	// only where the feed holds the account's bookings at all.
	coverage psnCashCoverage
}

// excludes reports whether a web row on account at `at` falls on PSN's
// side of the cut: on or after the relationship's PSN start, on an
// account the feed speaks for by that day. A relationship with no PSN
// start cuts nothing, and neither does one for an account the feed
// never reaches — the cash accounts behind a managed portfolio, mostly
// — because there is nothing on the other side to arbitrate against.
func (c psnCut) excludes(account string, at int64) bool {
	cut := c.startByRel[c.relOfAccount[account]]
	if cut <= 0 || at < cut {
		return false
	}
	return c.coverage.speaksFor(account, at)
}

// buildAccountToRelMap returns a web account_external_id → web
// banking_relationship_id lookup, taking the latest snapshot per
// account. Used by Transactions to find each row's relationship
// for the cutoff filter.
func (r *webReader) buildAccountToRelMap(ctx context.Context) (map[string]string, error) {
	const q = `
SELECT a.account_external_id, a.banking_relationship_id
  FROM accounts a
  JOIN (SELECT account_external_id, MAX(snapshot_at) AS s
          FROM accounts GROUP BY account_external_id) m
    ON a.account_external_id = m.account_external_id
   AND a.snapshot_at = m.s
 WHERE a.banking_relationship_id IS NOT NULL`
	out := make(map[string]string)
	if err := silver.ScanRows(ctx, r.db, "buildAccountToRelMap", q, func(rows *sql.Rows) error {
		var acct, rel string
		if err := rows.Scan(&acct, &rel); err != nil {
			return err
		}
		out[acct] = rel
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// buildPSNStartByWebRel resolves the PSN-start cutoff per web
// banking_relationship_id using the config relationships pairing
// and the *psnReader (if configured). Returns an empty map when
// psn is nil (degenerate splice — every web row passes).
//
// Resolution order per relationship:
//  1. RelationshipPair.PSNStartOverride if non-zero.
//  2. MIN(snapshot_at) in PSN for the paired PSNID, otherwise.
//
// A relationship with no PSN counterpart (PSNID empty, or empty
// MIN) gets cutoff=0 → no filter applied to its web rows.
func buildPSNStartByWebRel(ctx context.Context, psn *psnReader, rels []RelationshipPair) (map[string]int64, error) {
	out := make(map[string]int64, len(rels))
	if psn == nil {
		return out, nil
	}
	for _, p := range rels {
		if p.WebID == "" {
			continue
		}
		if p.PSNStartOverride > 0 {
			out[p.WebID] = p.PSNStartOverride
			continue
		}
		if p.PSNID == "" {
			continue
		}
		var minSnap sql.NullInt64
		err := psn.db.QueryRowContext(ctx, `
            SELECT MIN(snapshot_at)
              FROM cash_accounts
             WHERE relationship_id = ?`, p.PSNID).Scan(&minSnap)
		if err != nil {
			return nil, fmt.Errorf("PSN-start lookup for %q: %w", p.PSNID, err)
		}
		if minSnap.Valid {
			out[p.WebID] = minSnap.Int64
		}
	}
	return out, nil
}

// buildHistoricalCutoffs projects the per-relationship PSN-start
// cutoff onto the two key spaces the historical stream reads under.
// The historical stream is the pre-PSN backfill; without a cutoff
// its quarter-end rows collide with PSN's daily snapshot on the
// gold (source, snapshot_at, safekeeping, isin) / cash PK once PSN
// coverage laps a statement's period-end date.
//
// Portfolio side (historical_position_snapshots): the PDF row's
// portfolio_external_id is the PSN-aligned 'BBBBAAAAAAAANN' form,
// NOT the 4-char code that live web uses ('RNNN' / 'NNNN'). PSN
// silver's portfolios table owns that id space and its
// relationship_id, so the mapping comes from psn — mapping through
// live web's portfolios table misses every historical row because
// the two ID spaces don't overlap.
//
// Account side (historical_cash_balances): IBANs, which live web's
// accounts table already keys on with banking_relationship_id
// attached.
func (r *webReader) buildHistoricalCutoffs(
	ctx context.Context,
	cutoffByWebRel map[string]int64,
	psn *psnReader,
	rels []RelationshipPair,
) (portfolioCutoff, accountCutoff map[string]int64, err error) {
	portfolioCutoff = map[string]int64{}
	accountCutoff = map[string]int64{}
	if len(cutoffByWebRel) == 0 {
		return portfolioCutoff, accountCutoff, nil
	}
	if psn != nil {
		cutoffByPSNRel := make(map[string]int64, len(rels))
		for _, p := range rels {
			if p.PSNID == "" || p.WebID == "" {
				continue
			}
			if c := cutoffByWebRel[p.WebID]; c > 0 {
				cutoffByPSNRel[p.PSNID] = c
			}
		}
		if err := populateCutoffMap(ctx, psn.db,
			`SELECT DISTINCT portfolio_external_id, relationship_id
			   FROM portfolios`,
			cutoffByPSNRel, portfolioCutoff); err != nil {
			return nil, nil, fmt.Errorf("historical cutoff (psn portfolios): %w", err)
		}
	}
	if err := populateCutoffMap(ctx, r.db,
		`SELECT account_external_id, banking_relationship_id FROM accounts
		  WHERE banking_relationship_id IS NOT NULL`,
		cutoffByWebRel, accountCutoff); err != nil {
		return nil, nil, fmt.Errorf("historical cutoff (web accounts): %w", err)
	}
	return portfolioCutoff, accountCutoff, nil
}

func populateCutoffMap(
	ctx context.Context, db *sql.DB, q string,
	cutoffByRel map[string]int64, out map[string]int64,
) error {
	return silver.ScanRows(ctx, db, "", q, func(rows *sql.Rows) error {
		var id, rel string
		if err := rows.Scan(&id, &rel); err != nil {
			return err
		}
		if c := cutoffByRel[rel]; c > 0 {
			out[id] = c
		}
		return nil
	})
}
