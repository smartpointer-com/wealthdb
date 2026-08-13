package chase

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits, in one batch: one cash ACCOUNT per deposit account, a
// CURRENT cash balance from each account's roster balance, and a CLOSING cash
// balance per day the balance moved, valued at the transaction ledger's
// end-of-day running balance. Cash is modelled as cash_balances, never a
// position/instrument; gold's report_cash macro synthesises the read-time cash
// position.
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
	if err := c.appendRunningBalances(ctx, w, ccy, &batch); err != nil {
		return nil, err
	}
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
}

// appendAccounts emits one AccountChange (AccountKind cash) per deposit account
// from its latest roster snapshot, plus a CURRENT cash balance when the roster
// captured a live balance.
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window,
	ccy map[string]string, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT a.account_external_id,
       COALESCE(a.nickname, ''), COALESCE(a.mask, ''),
       a.balance,
       a.snapshot_at,
       (SELECT MIN(snapshot_at) FROM accounts a2 WHERE a2.account_external_id = a.account_external_id),
       (SELECT MAX(snapshot_at) FROM accounts a3 WHERE a3.account_external_id = a.account_external_id),
       a.payload
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(snapshot_at) FROM accounts a4
                         WHERE a4.account_external_id = a.account_external_id)
 ORDER BY a.account_external_id`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("chase appendAccounts: %w", err)
	}
	defer rows.Close()

	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	for rows.Next() {
		var (
			id, nickname, mask, payload string
			bal                         sql.NullFloat64
			snap, firstSeen, lastSeen   int64
		)
		if err := rows.Scan(&id, &nickname, &mask, &bal, &snap, &firstSeen, &lastSeen, &payload); err != nil {
			return err
		}
		display := nickname
		if display == "" {
			display = mask
		}
		currency := currencyOf(ccy, id)
		acc := canonical.AccountChange{
			AccountExternalID: id,
			AccountKind:       canonical.AccountKindCash,
			DisplayName:       silver.StrPtrIfNonEmpty(display),
			Nickname:          silver.StrPtrIfNonEmpty(nickname),
			BaseCurrency:      &currency,
			TaxWrapper:        &wrapper,
			ManagementStyle:   &style,
			FirstSeenAt:       firstSeen,
			LastSeenAt:        lastSeen,
			Payload:           json.RawMessage(payload),
		}
		batch.Accounts = append(batch.Accounts, acc)

		if bal.Valid && snap >= w.Start && snap <= w.End {
			batch.CashBalances = append(batch.CashBalances, cashBalance(
				snap, id, currency, canonical.BalanceKindCurrent,
				canonical.NewDecimalFromFloat(bal.Float64), `{"basis":"roster"}`))
		}
	}
	return rows.Err()
}

// appendRunningBalances emits one CLOSING cash balance per day the balance
// moved, valued at that day's end-of-day running balance from the transaction
// ledger — the exact cash time series, back to the ledger's start. This, not
// the statements, is the source of the historic cash marks: the running balance
// carries the balance at every date, whereas the collector records statement
// dates without their parsed balances (and only for recent years). The
// statement dates are a subset of these, so an as-of query at any statement
// date returns that statement's closing balance.
//
// posted_at is day-granular, so a day with several transactions has one
// end-of-day balance; the ledger has no intra-day order, so the fitid-highest
// row of the day is taken as its representative (the imprecision is bounded by
// same-day activity and resolves by the next day).
func (c *Connection) appendRunningBalances(ctx context.Context, w canonical.Window,
	ccy map[string]string, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT account_external_id, posted_at, balance FROM (
    SELECT account_external_id, posted_at, balance,
           ROW_NUMBER() OVER (PARTITION BY account_external_id, posted_at
                              ORDER BY fitid DESC) AS rn
      FROM transactions
     WHERE balance IS NOT NULL AND posted_at BETWEEN ? AND ?
) WHERE rn = 1
 ORDER BY account_external_id, posted_at`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("chase appendRunningBalances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id       string
			postedAt int64
			bal      float64
		)
		if err := rows.Scan(&id, &postedAt, &bal); err != nil {
			return err
		}
		batch.CashBalances = append(batch.CashBalances, cashBalance(
			postedAt, id, currencyOf(ccy, id), canonical.BalanceKindClosing,
			canonical.NewDecimalFromFloat(bal), `{"basis":"running_balance"}`))
	}
	return rows.Err()
}

// cashBalance builds a CashBalanceChange. The collector rounds money to cents
// before storing, so the REAL→Decimal step is exact at display precision.
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
