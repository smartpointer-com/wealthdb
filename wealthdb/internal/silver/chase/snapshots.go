package chase

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits, in one batch: one ACCOUNT per roster account (cash for a
// deposit account, card for a credit card), a CURRENT balance from each
// account's roster figure, and the CLOSING balance series. Cash is modelled as
// cash_balances, never a position/instrument; gold's report_cash macro
// synthesises the read-time cash position.
//
// THREE SOURCES FEED THE BALANCE SERIES, and they are mutually exclusive by
// construction so that no account ever gets two marks of one kind on one day:
//
//   - the transaction ledger's running balance (appendRunningBalances), one
//     mark per day the balance moved. It is the only source for deposits, and
//     for a card it covers the export era, where the loader reconstructs the
//     running balance neither card export carries;
//   - a card statement's printed closing figure (appendStatementBalances),
//     one mark at each period_end — but only for the periods the
//     reconstruction does not reach, which is what keeps the two apart;
//   - the roster, which is a CURRENT mark, not a closing one, and is stamped
//     at the load rather than at a ledger day.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	facts, err := c.accountFactsByID(ctx)
	if err != nil {
		return nil, err
	}
	var batch canonical.SnapshotBatch
	if err := c.appendAccounts(ctx, w, facts, &batch); err != nil {
		return nil, err
	}
	if err := c.appendRunningBalances(ctx, w, facts, &batch); err != nil {
		return nil, err
	}
	if err := c.appendStatementBalances(ctx, w, facts, &batch); err != nil {
		return nil, err
	}
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
}

// appendAccounts emits one AccountChange per roster account from its latest
// snapshot, plus that account's CURRENT balance when the roster captured one.
//
// The CURRENT balance is stamped at the SOURCE's latest roster snapshot, not
// at the account's own. Silver content-dedups an unchanged roster row, so a
// quiet account's MAX(snapshot_at) falls behind the source's; gold's
// cash_chosen keeps only the rows carrying the source's single
// MAX(snapshot_at), so an account stamped at its own would drop out of the
// latest net-worth view the moment it stopped changing — silently, and a card
// would take its debt with it. The figure is still that account's latest known
// balance; the stamp says "as of this load", which is exactly what a roster
// row that did not change asserts.
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window,
	facts map[string]accountFacts, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT a.account_external_id,
       COALESCE(a.nickname, ''), COALESCE(a.mask, ''),
       a.balance, a.payload,
       (SELECT MIN(snapshot_at) FROM accounts a2 WHERE a2.account_external_id = a.account_external_id),
       a.snapshot_at,
       (SELECT MAX(snapshot_at) FROM accounts)
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(snapshot_at) FROM accounts a3
                         WHERE a3.account_external_id = a.account_external_id)
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
			id, nickname, mask, payload       string
			bal                               sql.NullFloat64
			firstSeen, lastSeen, sourceLatest int64
		)
		if err := rows.Scan(&id, &nickname, &mask, &bal, &payload,
			&firstSeen, &lastSeen, &sourceLatest); err != nil {
			return err
		}
		display := nickname
		if display == "" {
			display = mask
		}
		card := isCard(facts, id)
		kind := canonical.AccountKindCash
		if card {
			kind = canonical.AccountKindCard
		}
		currency := currencyOf(facts, id)
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: id,
			AccountKind:       kind,
			DisplayName:       silver.StrPtrIfNonEmpty(display),
			Nickname:          silver.StrPtrIfNonEmpty(nickname),
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
				signedBalance(bal.Float64, card), `{"basis":"roster"}`))
		}
	}
	return rows.Err()
}

// appendRunningBalances emits one CLOSING balance per day the balance moved,
// valued at that day's end-of-day running balance from the transaction ledger
// — the exact balance time series, back to the ledger's start. On a deposit
// account this, not the statements, is the source of the historic cash marks:
// the running balance carries the balance at every date, whereas the collector
// records statement dates without their parsed balances (and only for recent
// years). The statement dates are a subset of these, so an as-of query at any
// statement date returns that statement's closing balance.
//
// On a card the same column carries the loader's reconstruction, which neither
// card export supplies: it rolls the posted ledger between the statement
// closing anchors, and a span that fails to land on its anchor keeps no
// balance at all. The statement era carries none by design. Those gaps are
// what appendStatementBalances fills, at period granularity.
//
// posted_at is day-granular, so a day with several transactions has one
// end-of-day balance, and the fitid-highest row of the day is taken as its
// representative. On a card that is exact — the reconstruction rolls forward
// in (posted_at, fitid) order, so the fitid-highest row of a day IS the one
// carrying its end-of-day balance. On a deposit account the balance is the
// provider's own column and the ledger has no intra-day order, so the pick is
// arbitrary among that day's rows; the imprecision is bounded by same-day
// activity and resolves by the next day.
func (c *Connection) appendRunningBalances(ctx context.Context, w canonical.Window,
	facts map[string]accountFacts, batch *canonical.SnapshotBatch) error {
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
			postedAt, id, currencyOf(facts, id), canonical.BalanceKindClosing,
			signedBalance(bal, isCard(facts, id)), `{"basis":"running_balance"}`))
	}
	return rows.Err()
}

// appendStatementBalances emits one CLOSING balance at a card statement's
// period_end, from the figure the statement printed — the balance truth for
// the era the reconstruction cannot reach.
//
// It is product-guarded to cards: the table is written by the card statement
// pass only, and a deposit account's historic marks come from its export's own
// running-balance column at day density.
//
// The NOT EXISTS clause is what keeps this source and appendRunningBalances
// off the same day. A period contributes its anchor only when NO reconstructed
// balance falls anywhere inside [period_start, period_end] — and period_end is
// inside its own period, so a collision is impossible. Suppressing the whole
// period rather than just the day is also the right reading: the
// reconstruction is anchored TO these closing figures, so where it runs it
// already carries them, at balance-move density instead of monthly.
//
// `transactions_covered` is deliberately not consulted. It records whether the
// period's TRANSACTIONS reached silver; the printed closing balance is
// asserted by the statement either way, and a period whose rows were refused
// is precisely one that needs its anchor.
func (c *Connection) appendStatementBalances(ctx context.Context, w canonical.Window,
	facts map[string]accountFacts, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT sb.account_external_id, sb.period_end, sb.closing
  FROM statement_balances sb
 WHERE sb.closing IS NOT NULL
   AND sb.period_end BETWEEN ? AND ?
   AND sb.account_external_id IN (SELECT account_external_id FROM accounts
                                   WHERE product = ?)
   AND NOT EXISTS (SELECT 1 FROM transactions t
                    WHERE t.account_external_id = sb.account_external_id
                      AND t.balance IS NOT NULL
                      AND t.posted_at BETWEEN sb.period_start AND sb.period_end)
 ORDER BY sb.account_external_id, sb.period_end`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End, productCard)
	if err != nil {
		return fmt.Errorf("chase appendStatementBalances: %w", err)
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
		// Every row here is a card's by the product guard above, so the
		// printed owed-positive figure is unconditionally negated.
		batch.CashBalances = append(batch.CashBalances, cashBalance(
			periodEnd, id, currencyOf(facts, id), canonical.BalanceKindClosing,
			signedBalance(closing, true), `{"basis":"statement_closing"}`))
	}
	return rows.Err()
}

// signedBalance converts a silver balance to the canonical sign. Silver stores
// every card figure the provider's way — the balance is the POSITIVE amount
// owed — and gold carries a revolving-credit liability as negative cash, so a
// card figure is negated here and a deposit figure passes through. This is the
// only place the flip happens.
//
// The collector rounds money to cents before storing, so the REAL→Decimal step
// is exact at display precision.
func signedBalance(amount float64, card bool) canonical.Decimal {
	d := canonical.NewDecimalFromFloat(amount)
	if card {
		return d.Neg()
	}
	return d
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
