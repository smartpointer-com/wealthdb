package gold

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// AccountRow is one row of the `accounts` subcommand's output:
// the gold `accounts` row's promoted columns (everything except
// the bookkeeping first_seen_at / last_seen_at / payload) plus
// derived aggregate columns computed by FX conversion of every
// position and cash balance belonging to the account.
//
// All decimals come back as their canonical string form so the
// caller can re-render at the precision it wants. Aggregate
// pointers are nil when:
//
//   - the *Base aggregates: the account has no base_currency.
//   - the *OutCcy aggregates: no FX path resolves to the
//     requested output currency for ANY of the account's lines
//     (in practice, the same path that already powers the
//     positions table's `value_<CCY>` column).
//
// Within an aggregate, lines that fail to convert are silently
// skipped — the same per-line "best-effort, show the hole"
// semantics used by `wealthdb positions`. A partial sum is
// usually more useful than a missing one.
type AccountRow struct {
	SilverSourceID    string
	AccountExternalID string
	AccountKind       string
	DisplayName       *string
	BaseCurrency      *string
	RelationshipID    *string
	Nickname          *string
	AccountCategory   *string

	// Aggregates expressed in the account's own base_currency.
	// Nil when BaseCurrency is nil.
	PositionsValueBase *string
	CashBalanceBase      *string
	TotalValueBase     *string

	// Aggregates expressed in the user-requested output currency.
	// Nil when no FX path is available for the account at all.
	PositionsValueOutCcy *string
	CashBalanceOutCcy      *string
	TotalValueOutCcy     *string
}

// AccountsAsOf returns one AccountRow per row in the gold
// accounts table. Aggregates are computed by re-using
// PositionsAsOf + CashAsOf and converting each line into the
// account's base_currency and into outCcy via ConvertValue.
// Accounts that have no positions and no cash still appear, with
// zero aggregates (or nil when there's no base_currency).
func AccountsAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string, mode canonical.FxMode) ([]AccountRow, error) {
	accounts, err := loadAccountBase(ctx, db)
	if err != nil {
		return nil, err
	}
	positions, err := PositionsAsOf(ctx, db, asOf)
	if err != nil {
		return nil, err
	}
	cash, err := CashAsOf(ctx, db, asOf)
	if err != nil {
		return nil, err
	}

	type acctKey struct{ src, id string }
	byKey := make(map[acctKey][]lineItem, len(accounts))
	addLine := func(src, id, ccy string, valueStr *string) {
		if valueStr == nil {
			return
		}
		v, err := canonical.NewDecimalFromString(*valueStr)
		if err != nil {
			return
		}
		byKey[acctKey{src, id}] = append(byKey[acctKey{src, id}],
			lineItem{currency: ccy, amount: v})
	}
	for _, p := range positions {
		addLine(p.SilverSourceID, p.AccountExternalID, p.Currency, p.MarketValue)
	}
	// Mark which lines are "cash" vs "positions" so the two
	// aggregate columns stay separable.
	cashStart := make(map[acctKey]int, len(accounts))
	for k, lines := range byKey {
		cashStart[k] = len(lines)
	}
	for _, c := range cash {
		addLine(c.SilverSourceID, c.AccountExternalID, c.Currency, c.MarketValue)
	}

	for i := range accounts {
		a := &accounts[i]
		k := acctKey{a.SilverSourceID, a.AccountExternalID}
		lines := byKey[k]
		split := cashStart[k] // positions occupy [:split]; cash occupies [split:]

		// Compute the four aggregates per requested target ccy:
		// (positions, cash) × (base, outCcy). Each call returns
		// nil when no line converted (so a "no FX path" account
		// keeps a blank cell rather than a misleading 0).
		if a.BaseCurrency != nil && *a.BaseCurrency != "" {
			base := *a.BaseCurrency
			pos := sumConverted(ctx, db, asOf, lines[:split], base, mode)
			cv := sumConverted(ctx, db, asOf, lines[split:], base, mode)
			a.PositionsValueBase = decimalPtrString(pos)
			a.CashBalanceBase = decimalPtrString(cv)
			a.TotalValueBase = decimalPtrString(addOptional(pos, cv))
		}
		pos := sumConverted(ctx, db, asOf, lines[:split], outCcy, mode)
		cv := sumConverted(ctx, db, asOf, lines[split:], outCcy, mode)
		a.PositionsValueOutCcy = decimalPtrString(pos)
		a.CashBalanceOutCcy = decimalPtrString(cv)
		a.TotalValueOutCcy = decimalPtrString(addOptional(pos, cv))
	}
	return accounts, nil
}

// loadAccountBase pulls every account row's promoted columns.
// Ordered by (silver_source_id, account_external_id) so the
// subcommand's output is deterministic.
func loadAccountBase(ctx context.Context, db *sql.DB) ([]AccountRow, error) {
	const q = `
SELECT silver_source_id, account_external_id, account_kind,
       display_name, base_currency, relationship_id,
       nickname, account_category
  FROM accounts
 ORDER BY silver_source_id, account_external_id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("loadAccountBase: %w", err)
	}
	defer rows.Close()

	var out []AccountRow
	for rows.Next() {
		var (
			a                                                AccountRow
			displayName, baseCcy, relID, nickname, category sql.NullString
		)
		if err := rows.Scan(&a.SilverSourceID, &a.AccountExternalID, &a.AccountKind,
			&displayName, &baseCcy, &relID, &nickname, &category); err != nil {
			return nil, fmt.Errorf("loadAccountBase scan: %w", err)
		}
		a.DisplayName = nullStringToPtr(displayName)
		a.BaseCurrency = nullStringToPtr(baseCcy)
		a.RelationshipID = nullStringToPtr(relID)
		a.Nickname = nullStringToPtr(nickname)
		a.AccountCategory = nullStringToPtr(category)
		out = append(out, a)
	}
	return out, rows.Err()
}

// lineItem is one position or cash entry feeding an aggregate.
type lineItem struct {
	currency string
	amount   canonical.Decimal
}

// sumConverted converts each lineItem into target and returns the
// sum, or nil when target is "" or no line converted. Per-line
// FX failures are skipped silently.
func sumConverted(ctx context.Context, db *sql.DB, asOf int64, lines []lineItem, target string, mode canonical.FxMode) *canonical.Decimal {
	if target == "" {
		return nil
	}
	var sum canonical.Decimal
	any := false
	for _, l := range lines {
		v, err := ConvertValue(ctx, db, asOf, l.amount, l.currency, target, mode)
		if err != nil {
			if errors.Is(err, ErrNoRate) {
				continue
			}
			// Non-rate error (e.g. driver failure): bubble up by
			// treating as fatal would surprise the user; the
			// `wealthdb positions` precedent is to swallow.
			continue
		}
		sum = sum.Add(v)
		any = true
	}
	if !any {
		// Distinguish "had no lines" from "had lines but all
		// failed": both are nil. Both produce blank cells, which
		// is the honest representation.
		if len(lines) == 0 {
			zero := canonical.Decimal{}
			return &zero
		}
		return nil
	}
	return &sum
}

// addOptional adds two optional decimals. Returns nil if either is
// nil (so a partial sum doesn't masquerade as the total).
func addOptional(a, b *canonical.Decimal) *canonical.Decimal {
	if a == nil || b == nil {
		return nil
	}
	s := a.Add(*b)
	return &s
}

// decimalPtrString renders an optional Decimal as its trimmed
// canonical string (shopspring/decimal's .String() drops
// scale-padding zeros). Returns nil for nil input so the caller's
// blank-cell semantics carry through.
func decimalPtrString(d *canonical.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}
