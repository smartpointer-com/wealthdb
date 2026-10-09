package angellist

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The stamps on angellist's book value (docs/DESIGN.md §7.4).
var (
	// paidInBasis: the contributed capital the portal states, gross of
	// capital paid back, with the setup fees the portal folds into it.
	paidInBasis = canonical.Basis{
		Origin: canonical.BasisStated, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
	}
	// inKindBasis: the same capital paid in, less the basis a K-1 says
	// left the vehicle in kind.
	inKindBasis = canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
	}
)

// inKind is the basis each position distributed in kind: Schedule K-1
// Line 19(c), property distributions such as the shares an SPV hands
// out at an exit. Such a distribution moves basis out of the vehicle
// with the asset, so it reduces the book value from the K-1's period
// end on. Cash paid back (Line 19(a)) does not: the book value stays
// the capital paid in, gross.
type inKind struct {
	// byPosition holds each position's K-1s that state a property
	// distribution.
	byPosition map[string][]propertyDistribution
	// clamped collects the positions whose property distributions
	// exceed the capital the portal states paid in.
	clamped map[string]bool
}

// propertyDistribution is one K-1's Line 19(c) figure, in minor units,
// and the end of the period the K-1 covers.
type propertyDistribution struct {
	periodEnd int64
	minor     int64
}

// readInKind reads every K-1 Line 19(c) figure (silver migration 0008)
// and places it on the position the collector paired the K-1's fund to:
// the offering whose fund_name it stamped. A K-1 covers a calendar tax
// year, so its period ends on Dec 31. A fund name that two offerings
// carry names no single position, so its figures are left out rather
// than counted twice. A silver older than migration 0008 states none.
func (c *Connection) readInKind(ctx context.Context) (*inKind, error) {
	k := &inKind{byPosition: map[string][]propertyDistribution{}, clamped: map[string]bool{}}
	has, err := silver.HasColumn(ctx, c.db, "k1_capital_accounts", "property_distributions_minor")
	if err != nil || !has {
		return k, err
	}
	const q = `
SELECT o.position_external_id, k.tax_year, k.property_distributions_minor
  FROM k1_capital_accounts k
  JOIN offerings o ON o.fund_name = k.fund_name
 WHERE k.property_distributions_minor > 0
   AND (SELECT COUNT(*) FROM offerings o2 WHERE o2.fund_name = k.fund_name) = 1`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("readInKind: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			pid         string
			year, minor int64
		)
		if err := rows.Scan(&pid, &year, &minor); err != nil {
			return nil, err
		}
		end := time.Date(int(year), time.December, 31, 0, 0, 0, 0, time.UTC).Unix()
		k.byPosition[pid] = append(k.byPosition[pid], propertyDistribution{periodEnd: end, minor: minor})
	}
	return k, rows.Err()
}

// bookValue is a position's book value at t and its stamp: the capital
// the portal states paid in, less the property it distributed on K-1s
// whose period ended on or before t. Where a reduction applies, extras
// holds the two figures for the payload. A reduction never takes the
// book value below zero; logClamped reports where it would have.
func (k *inKind) bookValue(pid string, t int64, paidIn sql.NullInt64) (*canonical.Decimal, canonical.Basis, map[string]any) {
	if !paidIn.Valid {
		return nil, canonical.Basis{}, nil
	}
	var out int64
	for _, d := range k.byPosition[pid] {
		if d.periodEnd <= t {
			out += d.minor
		}
	}
	if out == 0 {
		return minorPtr(paidIn), paidInBasis, nil
	}
	left := paidIn.Int64 - out
	if left < 0 {
		left = 0
		k.clamped[pid] = true
	}
	return minorPtr(sql.NullInt64{Int64: left, Valid: true}), inKindBasis, map[string]any{
		"paid_in":              minorPtr(paidIn).String(),
		"property_distributed": minorPtr(sql.NullInt64{Int64: out, Valid: true}).String(),
	}
}

// logClamped reports the positions whose K-1s state more property
// distributed than the portal states paid in. Their book value is held
// at zero.
func (k *inKind) logClamped() {
	if n := len(k.clamped); n > 0 {
		log.Printf("angellist adapter: %d position(s) distributed more basis in kind (K-1 Line 19(c)) than the portal states paid in; book value held at 0", n)
	}
}
