package carta

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The stamps on carta's book values (docs/DESIGN.md §7.4). Neither an
// exercise, a purchase nor a conversion carries a fee.
var (
	// exerciseValueBasis: a cap-table holding with a certificate born
	// from an option exercise. Each such certificate counts at its value
	// at exercise (shares × the fair-market value per share Carta
	// states), every other line at the cash paid: the holding at its
	// value when acquired. Its lots keep the cash paid.
	exerciseValueBasis = canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodAcquisitionValue, Fees: canonical.BasisFeesNone,
	}
	// shareBasis: a cap-table holding whose every costed line is a
	// share certificate: the sum of its lots' cost, the cash paid for
	// each.
	shareBasis = canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodLots, Fees: canonical.BasisFeesNone,
	}
	// cashPaidBasis: a cap-table holding with a costed line that is no
	// lot (a convertible's principal, an award's or a warrant's cost):
	// the sum of the cash paid for every line.
	cashPaidBasis = canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesNone,
	}
	// fundBasis: a fund's book value is the capital contributed its
	// statement states, gross of capital paid back. Management fees
	// are drawn from that capital, so they are in it.
	fundBasis = canonical.Basis{
		Origin: canonical.BasisStated, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
	}
	// fundCarryBasis: before a fund's first statement, its book value
	// is the capital called so far, summed from the call notices.
	fundCarryBasis = canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
	}
)

// readInKind reads every K-1 box 19 code C figure, the property a fund
// distributed in kind, onto the fund's position (silver migrations 0004
// and 0005). Such a distribution reduces the fund's book value from the
// K-1's period end on (silver.InKind). A calendar-year K-1 states no
// period, so it ends on Dec 31 of its tax year. Two documents of one
// fund and tax year are copies of one K-1: the later document counts. A
// silver older than migration 0004 states none.
func (c *Connection) readInKind(ctx context.Context) (*silver.InKind, error) {
	k := &silver.InKind{}
	has, err := silver.HasTables(ctx, c.db, "k1_capital_accounts")
	if err != nil || !has {
		return k, err
	}
	periodEnd := `NULL`
	if has, err := silver.HasColumn(ctx, c.db, "k1_capital_accounts", "period_end"); err != nil {
		return nil, err
	} else if has {
		periodEnd = `k.period_end`
	}
	q := `
SELECT k.entity_external_id, k.tax_year, ` + periodEnd + `, k.property_distributions
  FROM k1_capital_accounts k
 WHERE k.entity_external_id IS NOT NULL
   AND k.property_distributions IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM k1_capital_accounts k2
                    WHERE k2.entity_external_id = k.entity_external_id
                      AND k2.tax_year IS k.tax_year
                      AND (COALESCE(k2.doc_id, -1), k2.content_sha256)
                          > (COALESCE(k.doc_id, -1), k.content_sha256))`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("readInKind: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			eid     int64
			year    sql.NullInt64
			end     sql.NullString
			printed string
		)
		if err := rows.Scan(&eid, &year, &end, &printed); err != nil {
			return nil, err
		}
		amount, err := canonical.NewDecimalFromString(printed)
		if err != nil {
			continue // not an amount: states none
		}
		if at, ok := k1PeriodEnd(year, end); ok {
			k.Add(positionKey(eid), at, amount)
		}
	}
	return k, rows.Err()
}

// k1PeriodEnd is the last day of the period a K-1 covers, in unix
// seconds: its fiscal period_end where it prints one, else Dec 31 of its
// tax year. False when it states neither.
func k1PeriodEnd(year sql.NullInt64, end sql.NullString) (int64, bool) {
	if end.Valid {
		if d, err := time.Parse(time.DateOnly, end.String); err == nil {
			return d.Unix(), true
		}
	}
	if year.Valid {
		return time.Date(int(year.Int64), time.December, 31, 0, 0, 0, 0, time.UTC).Unix(), true
	}
	return 0, false
}
