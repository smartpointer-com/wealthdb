package ubs

import (
	"context"
	"encoding/json"
)

// The statement rail — the MT940 feed — is delivered per account: an
// account is in scope for statements or it is not, and one that is not
// receives none on any day (docs/adapters/ubs.md §3). A current account
// typically is; the cash accounts behind a managed portfolio, one per
// currency it trades in, often are not. So every decision the
// adapter takes between the feed and another record of a booking has
// to be asked per account: whether the export's copy yields to the
// feed's (psnCut), whether a conversion the feed describes on one
// account has its other leg anywhere (conversionMirrors), and whether
// the cash a corporate action paid is in the ledger at all
// (corporateActionCashLegs).

// psnCashCoverage is, per cash account IBAN, the earliest value day the
// MT940 feed has carried a movement for it. An account absent from the
// map is one the feed has never spoken for.
type psnCashCoverage map[string]int64

// speaksFor reports whether the statement rail is the record of the
// account's bookings on the day: the feed has carried the account at
// all, from a day no later than this one. Before its first statement an
// account is as uncovered as one the feed never reaches — the first
// delivery reaches back over a few days (the seam), and anything
// earlier is on another rail or on none.
func (c psnCashCoverage) speaksFor(account string, at int64) bool {
	start, ok := c[account]
	return ok && at >= start
}

// cashCoverage reads the feed's coverage off the whole silver: whether the
// feed speaks for an account is a property of what has been delivered,
// never of which slice of time a load happens to cover.
func (r *psnReader) cashCoverage(ctx context.Context) (psnCashCoverage, error) {
	out := psnCashCoverage{}
	err := r.eachCashMovement(ctx, "ubs-psn cashCoverage", func(row psnCashRow) error {
		acct := row.account
		var p cashMovementPayload
		if err := json.Unmarshal([]byte(row.payload), &p); err == nil && p.Account != "" {
			acct = p.Account
		}
		if start, ok := out[acct]; !ok || row.at < start {
			out[acct] = row.at
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
