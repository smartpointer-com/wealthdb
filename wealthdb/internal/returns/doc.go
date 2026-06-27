// Package returns computes time-weighted (TWR) and money-weighted (MWR / XIRR)
// returns for wealthdb.
//
// It is intentionally pure and DB-free: callers (internal/gold) assemble
// period-boundary values and dated external flows from the gold report macros
// and hand plain numbers here. Keeping the math in one dependency-light package
// makes the highest-risk logic — the XIRR root-find, the Modified-Dietz
// approximation, geometric chaining, the per-adapter flow policy, and the
// synthetic onboarding/closure mechanics — exhaustively unit-testable with zero
// database.
//
// Conventions:
//
//   - All math is float64. Returns are ratios and the XIRR solve is inherently
//     iterative; money amounts are assembled exactly upstream (DECIMAL) and
//     floated only at this boundary.
//   - Day arguments are epoch days (Unix seconds / 86400, UTC). Only differences
//     matter.
//   - Flow.Amount carries the canonical account-perspective sign that gold's
//     value_outccy already produces: capital INTO the entity is positive,
//     capital OUT is negative. Modified-Dietz uses Amount directly as F_i; XIRR
//     negates it to the investor cash-flow convention internally.
//
// See wealthdb/docs/RETURNS-NOTES.md for the design decisions and the places the
// implementation deviated from the rev.2 proposal.
package returns

// Flow is a dated external cash flow in the report (output) currency.
//
// Amount uses gold's value_outccy sign: deposit / transfer_in / distribution are
// positive (capital in); withdrawal / transfer_out / contribution are negative
// (capital out). This is exactly canonicalSign — see CapitalDirection.
type Flow struct {
	Day    int64
	Amount float64
}
