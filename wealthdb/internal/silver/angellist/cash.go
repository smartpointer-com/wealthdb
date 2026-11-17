package angellist

import (
	"context"
	"database/sql"
	"log"
	"slices"

	"github.com/shopspring/decimal"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// walletTx is one funding-ledger row as the wallet's cash reads it: its
// time, its signed amount and the balance the ledger states after it.
type walletTx struct {
	at      int64
	amount  int64
	running sql.NullInt64
}

// walletDay is the wallet's cash at the end of a ledger day, in minor
// units, dated at the day's last row.
type walletDay struct {
	at      int64
	balance int64
}

// How walletDays read the balances.
const (
	// walletRunning: the running balances the ledger states.
	walletRunning = iota
	// walletAnchored: the amounts' cumulative sum, anchored so the last
	// day ends on the balance the latest dump states.
	walletAnchored
	// walletSum: the amounts' cumulative sum from an empty wallet.
	walletSum
)

// walletDays are the wallet's balances at the end of each ledger day, from
// txs in date order, and how they were read.
//
// The running balances the ledger states are read where they follow the
// signed amounts. Rows of one day carry no reliable order, so a day is
// read as a whole: the opening balance is one a running balance stated on
// the first day less that day's amounts, and under it every day's end,
// the opening balance plus the amounts so far, must be a balance one of
// that day's rows states. Where no opening balance passes, or a row
// states none, the balances are the amounts' cumulative sum. With stated,
// the balance the latest dump states, the sum is anchored to it: every
// day moves by the same amount, so the last day ends on the stated
// balance and no day steps without a flow.
func walletDays(txs []walletTx, stated sql.NullInt64) ([]walletDay, int) {
	type ledgerDay struct {
		at, sum int64
		states  []int64
		gap     bool // a row states no running balance
	}
	var ds []ledgerDay
	for i := 0; i < len(txs); {
		day := utcDay(txs[i].at)
		var d ledgerDay
		for ; i < len(txs) && utcDay(txs[i].at) == day; i++ {
			d.sum += txs[i].amount
			d.at = txs[i].at
			if txs[i].running.Valid {
				d.states = append(d.states, txs[i].running.Int64)
			} else {
				d.gap = true
			}
		}
		ds = append(ds, d)
	}
	if len(ds) == 0 {
		return nil, walletSum
	}
	days := func(opening int64) []walletDay {
		out := make([]walletDay, len(ds))
		bal := opening
		for i, d := range ds {
			bal += d.sum
			out[i] = walletDay{at: d.at, balance: bal}
		}
		return out
	}
	follows := func(opening int64) bool {
		bal := opening
		for _, d := range ds {
			bal += d.sum
			if d.gap || !slices.Contains(d.states, bal) {
				return false
			}
		}
		return true
	}
	candidates := slices.Clone(ds[0].states)
	slices.Sort(candidates)
	for _, s := range slices.Compact(candidates) {
		if opening := s - ds[0].sum; follows(opening) {
			return days(opening), walletRunning
		}
	}
	if stated.Valid {
		var total int64
		for _, d := range ds {
			total += d.sum
		}
		return days(stated.Int64 - total), walletAnchored
	}
	return days(0), walletSum
}

// fundingCash is the funding wallet's uninvested cash at the end of every
// ledger day, one CashBalanceChange per day (walletDays), so a deposit, a
// call and a payout move the cash on their own day. The balance the latest
// dump states is the wallet's cash as of that dump: it is a balance of its
// own at the dump's time where the dump comes after the last ledger day,
// else the last day's. A ledger read from its amounts, and a ledger whose
// end differs from the stated balance, are logged. A silver whose
// funding_accounts names several wallets reads its ledger as one wallet,
// and that is logged too. Nil without a ledger or an account.
func (c *Connection) fundingCash(ctx context.Context, account string) ([]canonical.CashBalanceChange, error) {
	if account == "" || !c.schema.fundingTransactions {
		return nil, nil
	}
	ccy := "USD"
	var stated, statedAt sql.NullInt64
	if c.schema.fundingAccounts {
		type wallet struct {
			balance, at sql.NullInt64
			currency    sql.NullString
		}
		wallets, err := silver.Query(ctx, c.db, "fundingAccounts", `
SELECT balance_minor, snapshot_at, currency FROM funding_accounts ORDER BY funding_account_external_id`,
			func(rows *sql.Rows) (w wallet, err error) {
				err = rows.Scan(&w.balance, &w.at, &w.currency)
				return w, err
			})
		if err != nil {
			return nil, err
		}
		if len(wallets) > 1 {
			log.Printf("angellist adapter: the silver names %d funding accounts; their ledger reads as one wallet", len(wallets))
		}
		for _, w := range wallets {
			if w.balance.Valid {
				stated = sql.NullInt64{Int64: stated.Int64 + w.balance.Int64, Valid: true}
			}
			if w.at.Valid && w.at.Int64 > statedAt.Int64 {
				statedAt = w.at
			}
			if w.currency.Valid && w.currency.String != "" {
				ccy = w.currency.String
			}
		}
	}
	const q = `
SELECT occurred_at, COALESCE(amount_minor, 0), running_balance_minor
  FROM funding_transactions
 ORDER BY occurred_at, transaction_external_id`
	txs, err := silver.Query(ctx, c.db, "fundingCash", q, func(rows *sql.Rows) (walletTx, error) {
		var tx walletTx
		err := rows.Scan(&tx.at, &tx.amount, &tx.running)
		return tx, err
	})
	if err != nil {
		return nil, err
	}
	days, how := walletDays(txs, stated)
	if len(days) == 0 {
		return nil, nil
	}
	switch how {
	case walletAnchored:
		log.Printf("angellist adapter: the funding ledger's running balances do not follow its amounts; " +
			"the wallet's cash is their cumulative sum, ending on the balance the latest dump states")
	case walletSum:
		log.Printf("angellist adapter: the funding ledger's running balances do not follow its amounts " +
			"and no dump states the wallet's balance; the wallet's cash is their cumulative sum from zero")
	}
	if last := days[len(days)-1]; stated.Valid {
		if last.balance != stated.Int64 {
			log.Printf("angellist adapter: the funding ledger ends on a balance other than the one the latest dump states; " +
				"the dump's balance holds from the dump on")
		}
		if statedAt.Valid && statedAt.Int64 > last.at {
			days = append(days, walletDay{at: statedAt.Int64, balance: stated.Int64})
		} else {
			days[len(days)-1].balance = stated.Int64
		}
	}
	out := make([]canonical.CashBalanceChange, len(days))
	for i, d := range days {
		out[i] = canonical.CashBalanceChange{
			SnapshotAt:        d.at,
			AccountExternalID: account,
			Currency:          ccy,
			BalanceKind:       canonical.BalanceKindCurrent,
			Amount:            decimal.New(d.balance, -2),
		}
	}
	return out, nil
}
