package angellist

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// paidInBasis stamps angellist's book value (docs/DESIGN.md §7.4): the
// contributed capital the portal states, gross of capital paid back,
// with the setup fees the portal folds into it. Less the basis a K-1
// says left in kind, the same figure is derived (silver.InKind).
var paidInBasis = canonical.Basis{
	Origin: canonical.BasisStated, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
}

// readInKind reads every K-1 Line 19(c) figure (silver migration 0008)
// and places it on the position the collector paired the K-1's fund to:
// the offering whose fund_name it stamped. A K-1 covers a calendar tax
// year, so its period ends on Dec 31. A fund name that no offering, or
// several, carry names no single position: its figures reduce no book
// value, and the load names those funds. A silver older than migration
// 0008 states none.
func (c *Connection) readInKind(ctx context.Context) (*silver.InKind, error) {
	k := &silver.InKind{}
	has, err := silver.HasColumn(ctx, c.db, "k1_capital_accounts", "property_distributions_minor")
	if err != nil || !has {
		return k, err
	}
	const q = `
SELECT k.fund_name, k.tax_year, k.property_distributions_minor,
       (SELECT COUNT(*) FROM offerings o WHERE o.fund_name = k.fund_name),
       (SELECT MIN(o.position_external_id) FROM offerings o WHERE o.fund_name = k.fund_name)
  FROM k1_capital_accounts k
 WHERE k.property_distributions_minor > 0`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("readInKind: %w", err)
	}
	defer rows.Close()
	dropped := map[string]int64{}
	for rows.Next() {
		var (
			fund               string
			year, minor, count int64
			pid                sql.NullString
		)
		if err := rows.Scan(&fund, &year, &minor, &count, &pid); err != nil {
			return nil, err
		}
		if count != 1 {
			dropped[fund] = count
			continue
		}
		end := time.Date(int(year), time.December, 31, 0, 0, 0, 0, time.UTC).Unix()
		k.Add(pid.String, end, decimal.New(minor, -2))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	logDropped(dropped)
	return k, nil
}

// logDropped names the K-1 funds whose property distributions reduce no
// book value, each with the number of offerings that carry its name.
func logDropped(dropped map[string]int64) {
	if len(dropped) == 0 {
		return
	}
	names := make([]string, 0, len(dropped))
	for fund, n := range dropped {
		names = append(names, fmt.Sprintf("%s (on %d offerings)", fund, n))
	}
	sort.Strings(names)
	log.Printf("angellist adapter: %d K-1 fund(s) state a property distribution (Line 19(c)) "+
		"but name no single offering, so it reduces no book value: %s",
		len(names), strings.Join(names, "; "))
}
