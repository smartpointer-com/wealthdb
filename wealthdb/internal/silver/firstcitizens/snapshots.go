package firstcitizens

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
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
	ccy, err := silver.ReadAccountCurrencies(ctx, c.db, "USD")
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
//
// The CURRENT balance is stamped at the SOURCE's latest roster snapshot, not
// at the account's own. Silver content-dedups an unchanged roster row, so a
// quiet account's MAX(snapshot_at) falls behind the source's; gold's
// cash_chosen keeps only the rows carrying the source's single
// MAX(snapshot_at), so an account stamped at its own would drop out of the
// latest net-worth view once another account changed. The figure is still
// that account's latest known balance, as of this load.
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window,
	ccy silver.AccountCurrencies, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT a.account_external_id,
       COALESCE(a.nickname, ''), COALESCE(a.mask, ''),
       a.balance,
       (SELECT MIN(snapshot_at) FROM accounts a2 WHERE a2.account_external_id = a.account_external_id),
       a.snapshot_at,
       (SELECT MAX(snapshot_at) FROM accounts),
       a.payload
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(snapshot_at) FROM accounts a3
                         WHERE a3.account_external_id = a.account_external_id)
 ORDER BY a.account_external_id`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("firstcitizens appendAccounts: %w", err)
	}
	defer rows.Close()

	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	for rows.Next() {
		var (
			id, nickname, mask, payload       string
			bal                               sql.NullFloat64
			firstSeen, lastSeen, sourceLatest int64
		)
		if err := rows.Scan(&id, &nickname, &mask, &bal, &firstSeen, &lastSeen, &sourceLatest, &payload); err != nil {
			return err
		}
		display := nickname
		if display == "" {
			display = mask
		}
		currency := ccy.Of(id)
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

		if bal.Valid && sourceLatest >= w.Start && sourceLatest <= w.End {
			batch.CashBalances = append(batch.CashBalances, silver.CashBalance(
				sourceLatest, id, currency, canonical.BalanceKindCurrent,
				canonical.NewDecimalFromFloat(bal.Float64), `{"basis":"roster"}`))
		}
	}
	return rows.Err()
}

// appendRunningBalances emits one CLOSING cash balance per day the balance
// moved, valued at that day's end-of-day running balance from the transaction
// ledger — the exact cash time series, back to the ledger's start. Every
// history row carries a runningBalance, so the series has no gaps.
//
// posted_at is day-granular, so a day with several transactions has one
// end-of-day balance; the ledger has no intra-day order, so the fitid-highest
// row of the day is taken as its representative (the imprecision is bounded by
// same-day activity and resolves by the next day).
func (c *Connection) appendRunningBalances(ctx context.Context, w canonical.Window,
	ccy silver.AccountCurrencies, batch *canonical.SnapshotBatch) error {
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
		return fmt.Errorf("firstcitizens appendRunningBalances: %w", err)
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
		batch.CashBalances = append(batch.CashBalances, silver.CashBalance(
			postedAt, id, ccy.Of(id), canonical.BalanceKindClosing,
			canonical.NewDecimalFromFloat(bal), `{"basis":"running_balance"}`))
	}
	return rows.Err()
}
