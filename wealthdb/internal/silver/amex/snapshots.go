package amex

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits, in one batch: one ACCOUNT per card, a CURRENT balance from
// each card's roster figure, and the CLOSING balance series from the statement
// periods. A card carries no instrument, so nothing here is a position.
//
// The two are told apart by kind rather than by date: a CURRENT mark stamped
// at the source's latest load, and a CLOSING mark at a period's own end date.
// They can fall on the same day — the activity channel's period ends at the
// window a run fetched — and do not compete when they do.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	ccy, err := c.accountCurrencies(ctx)
	if err != nil {
		return nil, err
	}
	var batch canonical.SnapshotBatch
	if err := c.appendAccounts(ctx, w, ccy, &batch); err != nil {
		return nil, err
	}
	if err := c.appendStatementBalances(ctx, w, ccy, &batch); err != nil {
		return nil, err
	}
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
}

// appendAccounts emits one AccountChange per card from its latest snapshot,
// plus that card's CURRENT balance when the roster captured one.
//
// The CURRENT balance is stamped at the SOURCE's latest roster snapshot, not at
// the account's own. Silver content-dedups an unchanged roster row, so a quiet
// card's MAX(snapshot_at) falls behind the source's; gold's cash_chosen keeps
// only the rows carrying the source's single MAX(snapshot_at), so a card
// stamped at its own would drop out of the latest net-worth view the moment it
// stopped changing — taking its debt with it. The figure is still that card's
// latest known balance; the stamp says "as of this load", which is exactly what
// an unchanged roster row asserts.
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window,
	ccy map[string]string, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT a.account_external_id,
       COALESCE(a.display_name, ''), COALESCE(a.mask, ''),
       a.balance, a.payload,
       (SELECT MIN(snapshot_at) FROM accounts a2
         WHERE a2.account_external_id = a.account_external_id),
       a.snapshot_at,
       (SELECT MAX(snapshot_at) FROM accounts)
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(snapshot_at) FROM accounts a3
                         WHERE a3.account_external_id = a.account_external_id)
 ORDER BY a.account_external_id`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("amex appendAccounts: %w", err)
	}
	defer rows.Close()

	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	for rows.Next() {
		var (
			id, name, mask, payload           string
			bal                               sql.NullFloat64
			firstSeen, lastSeen, sourceLatest int64
		)
		if err := rows.Scan(&id, &name, &mask, &bal, &payload,
			&firstSeen, &lastSeen, &sourceLatest); err != nil {
			return err
		}
		display := name
		if display == "" {
			display = mask
		}
		currency := currencyOf(ccy, id)
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: id,
			AccountKind:       canonical.AccountKindCard,
			DisplayName:       silver.StrPtrIfNonEmpty(display),
			BaseCurrency:      &currency,
			TaxWrapper:        &wrapper,
			ManagementStyle:   &style,
			FirstSeenAt:       firstSeen,
			LastSeenAt:        lastSeen,
			Payload:           json.RawMessage(payload),
		})

		if bal.Valid && sourceLatest >= w.Start && sourceLatest <= w.End {
			batch.CashBalances = append(batch.CashBalances, cashBalance(
				sourceLatest, id, currency, canonical.BalanceKindCurrent,
				signedBalance(bal.Float64), `{"basis":"roster"}`))
		}
	}
	return rows.Err()
}

// appendStatementBalances emits one CLOSING balance at each statement period's
// end, from the figure the period stated — the only historic balance truth a
// card has, since no Amex channel carries a running balance per row.
//
// Silver records a period from whichever channel stated it (the activity JSON
// for the ~24 months it reaches, the statement PDFs for the deeper archive),
// keyed on (account, period_end) so the two converge on one row per period
// rather than competing. `transactions_covered` is deliberately not consulted:
// it records whether the period's ROWS reached silver, and the closing balance
// is asserted either way — a period whose rows were refused is precisely one
// that needs its anchor.
func (c *Connection) appendStatementBalances(ctx context.Context, w canonical.Window,
	ccy map[string]string, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT account_external_id, period_end, closing
  FROM statement_balances
 WHERE closing IS NOT NULL
   AND period_end BETWEEN ? AND ?
 ORDER BY account_external_id, period_end`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("amex appendStatementBalances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id        string
			periodEnd int64
			closing   float64
		)
		if err := rows.Scan(&id, &periodEnd, &closing); err != nil {
			return err
		}
		batch.CashBalances = append(batch.CashBalances, cashBalance(
			periodEnd, id, currencyOf(ccy, id), canonical.BalanceKindClosing,
			signedBalance(closing), `{"basis":"statement_closing"}`))
	}
	return rows.Err()
}

// signedBalance converts a silver card balance to the canonical sign. Silver
// holds every card figure the provider's way — the balance is the POSITIVE
// amount owed — and gold carries a revolving-credit liability as negative
// cash. Every account this source emits is a card, so the flip is
// unconditional, and this is the only place it happens.
//
// The collector rounds money to cents before storing, so the REAL→Decimal step
// is exact at display precision.
func signedBalance(amount float64) canonical.Decimal {
	return canonical.NewDecimalFromFloat(amount).Neg()
}

// cashBalance builds a CashBalanceChange.
func cashBalance(snap int64, accountID, currency string, kind canonical.BalanceKind,
	amount canonical.Decimal, payload string) canonical.CashBalanceChange {
	return canonical.CashBalanceChange{
		SnapshotAt:        snap,
		AccountExternalID: accountID,
		Currency:          currency,
		BalanceKind:       kind,
		Amount:            amount,
		Payload:           json.RawMessage(payload),
	}
}
