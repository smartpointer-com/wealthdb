package gold

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// FX rate convention (canonical, mirrors silver):
//
//   base_currency  = the currency the rate is "denominated in"
//   quote_currency = the currency the rate "prices"
//   mid_rate       = how many BASE units one QUOTE unit equals
//
// Example: (base=CHF, quote=USD, mid_rate=0.78) means "1 USD = 0.78 CHF".
// Multiply a USD amount by mid_rate to get CHF.
//
// ConvertValue handles both directions and reciprocal fallback;
// callers don't need to think about which direction a given silver
// happens to publish.

// ErrNoRate is returned by LookupRate / ConvertValue when no
// applicable rate (nor its reciprocal) exists.
var ErrNoRate = errors.New("no FX rate available")

// LookupRate returns the mid_rate for (base, quote) effective
// at `asOf` (a Unix-second epoch). Behaviour depends on the mode:
//
//   - FxModeHistoric: bracket `asOf` between the latest snapshot
//     ≤ asOf and the earliest snapshot ≥ asOf. Linear-interpolate
//     between them; flat-extrapolate when only one side exists.
//   - FxModeCurrent: ignore `asOf`; return the rate at the
//     latest available snapshot for the pair.
//
// Returns ErrNoRate (wrapped) when the (base, quote) pair has no
// fx_rates rows at all. Callers that want to try the reciprocal
// pair should use ConvertValue, which already handles that path.
func LookupRate(ctx context.Context, db *sql.DB, asOf int64, base, quote string, mode canonical.FxMode) (canonical.Decimal, error) {
	if base == quote {
		return canonical.NewDecimalFromInt(1), nil
	}

	switch mode {
	case canonical.FxModeCurrent:
		rate, ok, err := latestRate(ctx, db, base, quote)
		if err != nil {
			return canonical.Decimal{}, err
		}
		if !ok {
			return canonical.Decimal{}, fmt.Errorf("%w: %s→%s (current)", ErrNoRate, base, quote)
		}
		return rate, nil

	case canonical.FxModeHistoric, "":
		below, hasBelow, err := rateAtOrBefore(ctx, db, asOf, base, quote)
		if err != nil {
			return canonical.Decimal{}, err
		}
		above, hasAbove, err := rateAtOrAfter(ctx, db, asOf, base, quote)
		if err != nil {
			return canonical.Decimal{}, err
		}

		switch {
		case !hasBelow && !hasAbove:
			return canonical.Decimal{}, fmt.Errorf("%w: %s→%s at %d", ErrNoRate, base, quote, asOf)
		case !hasBelow:
			return above.Rate, nil // flat-extrapolate back
		case !hasAbove:
			return below.Rate, nil // flat-extrapolate forward
		case below.Snapshot == asOf:
			return below.Rate, nil // exact match
		case below.Snapshot == above.Snapshot:
			return below.Rate, nil // both ends collapse to one row
		}
		// Linear interpolation between two distinct bracketing
		// snapshots: rate = below + (above-below) * (asOf-below.t) / (above.t-below.t)
		span := above.Snapshot - below.Snapshot
		offset := asOf - below.Snapshot
		delta := above.Rate.Sub(below.Rate)
		num := delta.Mul(canonical.NewDecimalFromInt(offset))
		denom := canonical.NewDecimalFromInt(span)
		return below.Rate.Add(num.Div(denom)), nil

	default:
		return canonical.Decimal{}, fmt.Errorf("LookupRate: unknown mode %q", mode)
	}
}

// ConvertValue converts an amount from one currency to another at
// the given time + mode. Tries the direct rate first, then the
// reciprocal pair. Returns ErrNoRate when neither direction has
// any data.
func ConvertValue(ctx context.Context, db *sql.DB, asOf int64, value canonical.Decimal, from, to string, mode canonical.FxMode) (canonical.Decimal, error) {
	if from == to {
		return value, nil
	}

	// Direct: (base=to, quote=from, mid_rate=K) ⇒ "1 from = K to" ⇒ amount_to = amount * K
	if rate, err := LookupRate(ctx, db, asOf, to, from, mode); err == nil {
		return value.Mul(rate), nil
	} else if !errors.Is(err, ErrNoRate) {
		return canonical.Decimal{}, err
	}

	// Reciprocal: (base=from, quote=to, mid_rate=K) ⇒ "1 to = K from" ⇒ amount_to = amount / K
	if rate, err := LookupRate(ctx, db, asOf, from, to, mode); err == nil {
		if rate.IsZero() {
			return canonical.Decimal{}, fmt.Errorf("ConvertValue %s→%s: reciprocal rate is zero", from, to)
		}
		return value.Div(rate), nil
	} else if !errors.Is(err, ErrNoRate) {
		return canonical.Decimal{}, err
	}

	return canonical.Decimal{}, fmt.Errorf("%w: %s→%s at %d", ErrNoRate, from, to, asOf)
}

// --- internal one-shot lookups --------------------------------------------

// bracketRow is one rate observation used by the bracket
// interpolation in LookupRate.
type bracketRow struct {
	Snapshot int64
	Rate     canonical.Decimal
}

func rateAtOrBefore(ctx context.Context, db *sql.DB, asOf int64, base, quote string) (bracketRow, bool, error) {
	const q = `
SELECT snapshot_at, CAST(mid_rate AS VARCHAR)
  FROM fx_rates
 WHERE base_currency = ? AND quote_currency = ? AND snapshot_at <= ?
 ORDER BY snapshot_at DESC LIMIT 1`
	return scanBracketRow(ctx, db, q, base, quote, asOf)
}

func rateAtOrAfter(ctx context.Context, db *sql.DB, asOf int64, base, quote string) (bracketRow, bool, error) {
	const q = `
SELECT snapshot_at, CAST(mid_rate AS VARCHAR)
  FROM fx_rates
 WHERE base_currency = ? AND quote_currency = ? AND snapshot_at >= ?
 ORDER BY snapshot_at ASC LIMIT 1`
	return scanBracketRow(ctx, db, q, base, quote, asOf)
}

func latestRate(ctx context.Context, db *sql.DB, base, quote string) (canonical.Decimal, bool, error) {
	const q = `
SELECT CAST(mid_rate AS VARCHAR)
  FROM fx_rates
 WHERE base_currency = ? AND quote_currency = ?
 ORDER BY snapshot_at DESC LIMIT 1`
	var s string
	err := db.QueryRowContext(ctx, q, base, quote).Scan(&s)
	if err == sql.ErrNoRows {
		return canonical.Decimal{}, false, nil
	}
	if err != nil {
		return canonical.Decimal{}, false, err
	}
	d, err := canonical.NewDecimalFromString(s)
	if err != nil {
		return canonical.Decimal{}, false, fmt.Errorf("latestRate parse %q: %w", s, err)
	}
	return d, true, nil
}

func scanBracketRow(ctx context.Context, db *sql.DB, q string, args ...any) (bracketRow, bool, error) {
	var (
		snap int64
		s    string
	)
	err := db.QueryRowContext(ctx, q, args...).Scan(&snap, &s)
	if err == sql.ErrNoRows {
		return bracketRow{}, false, nil
	}
	if err != nil {
		return bracketRow{}, false, err
	}
	d, err := canonical.NewDecimalFromString(s)
	if err != nil {
		return bracketRow{}, false, fmt.Errorf("scanBracketRow parse %q: %w", s, err)
	}
	return bracketRow{Snapshot: snap, Rate: d}, true, nil
}
