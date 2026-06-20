package gold

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// GlobalRow is the single-row, whole-portfolio rollup produced by
// GlobalAsOf — the ultimate level of aggregation above accounts /
// portfolios. It sums every account's output-currency aggregates and
// records the span of their snapshot dates. Computed from
// AccountsAsOf, so it reconciles exactly with the sum of
// `wealthdb accounts`.
type GlobalRow struct {
	// MinSnapshotAt / MaxSnapshotAt bound the per-account snapshot
	// dates (each account's latest snapshot ≤ the as-of date).
	// Accounts that have produced no observations at all
	// (snapshot_at 0) are excluded; both are 0 when no account has
	// any data as of the requested date.
	MinSnapshotAt int64
	MaxSnapshotAt int64

	// Output-currency totals across all accounts, as decimal
	// strings. Always non-nil: a sum is "0", not "unknown", even
	// when empty. An account whose value can't resolve an FX path
	// to outCcy contributes nothing — mirroring the per-account
	// `accounts` _<CCY> columns, which are blank in that case.
	CashBalanceOutCcy    *string
	PositionsValueOutCcy *string
	TotalValueOutCcy     *string
}

// GlobalAsOf rolls the per-account view up into one GlobalRow: it
// sums the output-currency aggregates and takes the min/max of the
// per-account snapshot dates. Deriving it from AccountsAsOf
// guarantees `global` == Σ `accounts`.
func GlobalAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string, mode canonical.FxMode) (GlobalRow, error) {
	accounts, err := AccountsAsOf(ctx, db, asOf, outCcy, mode)
	if err != nil {
		return GlobalRow{}, err
	}

	var cash, positions, total canonical.Decimal // zero value == 0
	var g GlobalRow
	for _, a := range accounts {
		if a.SnapshotAt > 0 {
			if g.MinSnapshotAt == 0 || a.SnapshotAt < g.MinSnapshotAt {
				g.MinSnapshotAt = a.SnapshotAt
			}
			if a.SnapshotAt > g.MaxSnapshotAt {
				g.MaxSnapshotAt = a.SnapshotAt
			}
		}
		if cash, err = addDecimalPtr(cash, a.CashBalanceOutCcy); err != nil {
			return GlobalRow{}, fmt.Errorf("global: cash_balance: %w", err)
		}
		if positions, err = addDecimalPtr(positions, a.PositionsValueOutCcy); err != nil {
			return GlobalRow{}, fmt.Errorf("global: positions_value: %w", err)
		}
		if total, err = addDecimalPtr(total, a.TotalValueOutCcy); err != nil {
			return GlobalRow{}, fmt.Errorf("global: total_value: %w", err)
		}
	}

	cashStr, posStr, totalStr := cash.String(), positions.String(), total.String()
	g.CashBalanceOutCcy = &cashStr
	g.PositionsValueOutCcy = &posStr
	g.TotalValueOutCcy = &totalStr
	return g, nil
}

// addDecimalPtr adds the decimal-string value at p to acc; a nil p
// contributes nothing (the account had no FX path to outCcy).
func addDecimalPtr(acc canonical.Decimal, p *string) (canonical.Decimal, error) {
	if p == nil {
		return acc, nil
	}
	d, err := canonical.NewDecimalFromString(*p)
	if err != nil {
		return acc, fmt.Errorf("parse %q: %w", *p, err)
	}
	return acc.Add(d), nil
}
