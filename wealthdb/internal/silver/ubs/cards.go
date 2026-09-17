package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Credit cards, projected from the ubs-web silver's card_* tables
// (collector migration 0007).
//
// Cards are web-only and have no PSN counterpart at all, so — like
// mortgages — they flow through regardless of the PSN cutover. The
// cutoff exists to stop web restating what PSN says better; PSN says
// nothing about cards.
//
// SIGNS. Unlike the chase silver, nothing is negated here. UBS reports a
// card negative-when-owed already: a purchase is negative, a payment
// positive, and a balance is negative while the card carries debt. That
// is canonical.AccountKindCard's own convention — a revolving-credit
// liability held as negative cash — so the figures pass through, and a
// reader who knows the chase adapter should note the difference rather
// than assume it.

// hasCardTables reports whether this silver has been migrated to the
// schema that carries cards. A DB written before collector migration
// 0007 has none, and must keep loading rather than fail.
func (r *webReader) hasCardTables(ctx context.Context) (bool, error) {
	const q = `SELECT COUNT(*) FROM sqlite_master
                WHERE type='table' AND name IN ('card_accounts','card_transactions')`
	var n int
	if err := r.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return false, fmt.Errorf("hasCardTables: %w", err)
	}
	return n == 2, nil
}

// appendWebCards emits, per snapshot in the window, one AccountChange
// per card account plus its CURRENT balance.
//
// The balance is the roster's figure as reported, and `reserved_amount`
// is NOT added to it. An account's roster balance already includes its
// authorised-but-unposted spend: it matches the sum of its cards'
// `balanceIncludingReserved`, not of their `balance`. Adding the
// reserved figure back would count that spend twice.
//
// The split is still visible: `card_accounts.reserved_amount` records
// it, and the roster node in the payload carries each card's `balance`
// beside its `balanceIncludingReserved`. Neither is read here, because
// the balance already accounts for both.
func (r *webReader) appendWebCards(ctx context.Context, w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch) error {
	ok, err := r.hasCardTables(ctx)
	if err != nil || !ok {
		return err
	}
	const q = `
SELECT snapshot_at, account_external_id, currency_iso, balance,
       product_name, account_number, payload
  FROM card_accounts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendWebCards: %w", err)
	}
	defer rows.Close()
	skipped := 0
	for rows.Next() {
		var (
			snap                     int64
			extID, payload           string
			ccy, product, acctNumber sql.NullString
			balance                  sql.NullFloat64
		)
		if err := rows.Scan(&snap, &extID, &ccy, &balance,
			&product, &acctNumber, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindCard,
			TaxWrapper:        relationshipTaxWrapper(),
			// The product line, falling back to the printed account
			// number. Neither is a card number.
			DisplayName:  silver.StrPtrIfNonEmpty(cardDisplayName(product, acctNumber)),
			BaseCurrency: silver.StrPtrIfNonEmpty(ccy.String),
			FirstSeenAt:  snap,
			LastSeenAt:   snap,
			Payload:      json.RawMessage(payload),
		})
		// A balance needs a currency to mean anything: gold keys cash
		// on (account, currency, kind), so a currencyless row is a
		// figure with no unit rather than a zero-currency one. Skipped
		// and counted, never guessed at from the account.
		currency := strings.ToUpper(strings.TrimSpace(ccy.String))
		if !balance.Valid || currency == "" {
			if balance.Valid {
				skipped++
			}
			continue
		}
		batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
			SnapshotAt:        snap,
			AccountExternalID: extID,
			Currency:          currency,
			BalanceKind:       canonical.BalanceKindCurrent,
			Amount:            canonical.NewDecimalFromFloat(balance.Float64),
			Payload:           json.RawMessage(`{"basis":"roster"}`),
		})
	}
	if skipped > 0 {
		log.Printf("ubs adapter: skipped %d card balance(s) with no currency", skipped)
	}
	return rows.Err()
}

func cardDisplayName(product, accountNumber sql.NullString) string {
	if s := strings.TrimSpace(product.String); s != "" {
		return s
	}
	return strings.TrimSpace(accountNumber.String)
}

// appendCardStatementBalances emits one CLOSING balance per billing
// period, at the period end, from the invoice's own closing figure.
//
// This is the historic series. The ledger has no running-balance column
// and the roster gives only today's figure, so without these a card
// would have exactly one balance in gold — today's — and no history at
// all. The invoice states the figure the bank billed, which is the
// authoritative one.
//
// Only periods whose four figures reconcile are emitted. The collector
// checks `balance_forward + total_debit + total_credit == due_amount`
// and records the outcome per period; a period that fails that identity
// has been mis-read somewhere, and a wrong balance is worse than a
// missing one — the carry-forward rule fills a gap from the neighbouring
// observation, but nothing corrects a figure that is present and wrong.
func (r *webReader) appendCardStatementBalances(ctx context.Context,
	w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	ok, err := r.hasCardTables(ctx)
	if err != nil || !ok {
		return err
	}
	const q = `
SELECT period_end, account_external_id, COALESCE(currency_iso, ''),
       due_amount
  FROM card_invoices
 WHERE reconciles = 1 AND due_amount IS NOT NULL
   AND period_end BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendCardStatementBalances: %w", err)
	}
	defer rows.Close()
	skipped := 0
	for rows.Next() {
		var (
			periodEnd int64
			extID     string
			ccy       string
			due       float64
		)
		if err := rows.Scan(&periodEnd, &extID, &ccy, &due); err != nil {
			return err
		}
		batch, ok := byTime[periodEnd]
		if !ok {
			// A period end is a billing date, not a dump time, so it
			// rarely coincides with a snapshot the run already has.
			// Give it its own batch — the balance belongs at the date
			// the bank drew it, never at the date it was fetched.
			batch = &canonical.SnapshotBatch{}
			byTime[periodEnd] = batch
		}
		currency := strings.ToUpper(strings.TrimSpace(ccy))
		if currency == "" {
			// Same rule as the roster balance: no unit, no row.
			skipped++
			continue
		}
		batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
			SnapshotAt:        periodEnd,
			AccountExternalID: extID,
			Currency:          currency,
			BalanceKind:       canonical.BalanceKindClosing,
			Amount:            canonical.NewDecimalFromFloat(due),
			Payload:           json.RawMessage(`{"basis":"statement_closing"}`),
		})
	}
	if skipped > 0 {
		log.Printf("ubs adapter: skipped %d statement balance(s) with no currency", skipped)
	}
	return rows.Err()
}

// cardTxKind classifies one card ledger row.
//
// A card ledger has no booking-type column — the cash surface's
// `description_kind` has no card equivalent — so the kind comes from the
// sign plus the settlement descriptors. That is the faithful reading:
// on a card, direction *is* the classification. Money off the card is
// spend; money onto it is either the bill being settled or a merchant
// giving some back, and only the descriptor tells those apart.
//
// The settlement descriptors are the two UBS books a bill under
// (DESIGN.md §5.6). They are matched on the whole descriptor, folded and
// space-collapsed, so a merchant whose name merely contains the words is
// not mistaken for a settlement.
func cardTxKind(amount float64, merchant string) canonical.TxKind {
	switch {
	case amount < 0:
		return canonical.TxKindPurchase
	case amount > 0:
		if isCardSettlement(merchant) {
			return canonical.TxKindCardPayment
		}
		return canonical.TxKindRefund
	default:
		// A zero-amount row is neither spend nor credit. `other` keeps
		// it queryable and out of the spending population, which is
		// what an unclassifiable row should cost.
		return canonical.TxKindOther
	}
}

// cardSettlementDescriptors are the descriptors UBS books a card bill's
// settlement under — the Swiss direct-debit rail, its SWIFT variant, and
// a transfer from an account. Written in the folded form isCardSettlement
// compares against: upper case, single-spaced, no parentheses.
var cardSettlementDescriptors = []string{
	"DIRECT DEBIT",
	"DIRECT DEBIT SWIFT",
	"TRANSFER FROM ACCOUNT",
}

// isCardSettlement reports whether a descriptor is one of the settlement
// rails, whole. Case, spacing and the parentheses UBS puts around a rail
// qualifier are folded away; nothing else is, so the comparison stays an
// equality against the whole descriptor rather than a substring search —
// a merchant whose name merely contains the words is not a settlement.
func isCardSettlement(descriptor string) bool {
	folded := strings.ToUpper(descriptor)
	folded = strings.NewReplacer("(", " ", ")", " ").Replace(folded)
	folded = strings.Join(strings.Fields(folded), " ")
	for _, d := range cardSettlementDescriptors {
		if folded == d {
			return true
		}
	}
	return false
}

// cardTransactions yields the booked card ledger.
//
// No PSN cutoff applies: PSN carries no card rows, so there is nothing
// for the cut to arbitrate between.
//
// The amounts are UBS's own and already canonical for a liability, but
// they still pass through ApplyCanonicalSign, because the card kinds
// have a fixed direction — a purchase is negative and a refund or card
// payment positive whatever the row said.
func (r *webReader) cardTransactions(ctx context.Context, w canonical.Window) (canonical.TransactionBatch, error) {
	var out canonical.TransactionBatch
	ok, err := r.hasCardTables(ctx)
	if err != nil || !ok {
		return out, err
	}
	const q = `
SELECT transaction_external_id, value_date, account_external_id, amount,
       COALESCE(currency_iso, ''), merchant, merchant_category, payload
  FROM card_transactions
 WHERE value_date BETWEEN ? AND ?
 ORDER BY value_date, transaction_external_id`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return out, fmt.Errorf("ubs-web card transactions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			txID, accountID, ccy, payload string
			valueDate                     int64
			amount                        float64
			merchant, category            sql.NullString
		)
		if err := rows.Scan(&txID, &valueDate, &accountID, &amount, &ccy,
			&merchant, &category, &payload); err != nil {
			return out, fmt.Errorf("ubs-web card transactions scan: %w", err)
		}
		kind := cardTxKind(amount, merchant.String)
		net := canonical.NewDecimalFromFloat(amount)
		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			// The provider's own row id is unique across the source, so
			// unlike the cash rows it needs no per-leg synthesis.
			TransactionExternalID: txID,
			OccurredAt:            valueDate,
			AccountExternalID:     accountID,
			Kind:                  kind,
			Currency:              ccy,
			NetAmount:             canonical.ApplyCanonicalSign(kind, &net),
			// The terminal descriptor, verbatim. It is the input to
			// gold's merchant signature, so any reformatting here would
			// re-key every merchant it touched.
			Description:  silver.StrPtrIfNonEmpty(merchant.String),
			Counterparty: silver.StrPtrIfNonEmpty(merchant.String),
			// The MCC description, verbatim and un-normalised — the
			// provider tier's input. Despite the API calling the field
			// `merchantName`, it names a line of business.
			ProviderCategory: silver.StrPtrIfNonEmpty(category.String),
			Payload:          json.RawMessage(payload),
		})
	}
	return out, rows.Err()
}

// cardRange is the span of every date the card projection emits at:
// the ledger's booking dates and the billing periods' ends.
//
// It widens the reader's ChangeWindow, and that is a correctness
// requirement rather than cosmetics. Gold deletes the window before
// re-inserting it, so a record emitted outside [Start, End] is inserted
// again without its predecessor being removed, and duplicates on every
// load. A card period end in particular is a billing date that need not
// fall on any dump time, so nothing else in the window computation
// reaches it.
//
// Returns (-1, -1) when there is nothing to include.
func (r *webReader) cardRange(ctx context.Context) (int64, int64, error) {
	ok, err := r.hasCardTables(ctx)
	if err != nil || !ok {
		return -1, -1, err
	}
	var (
		txMin, txMax   sql.NullInt64
		invMin, invMax sql.NullInt64
	)
	if err := r.db.QueryRowContext(ctx,
		`SELECT MIN(value_date), MAX(value_date) FROM card_transactions`,
	).Scan(&txMin, &txMax); err != nil {
		return -1, -1, fmt.Errorf("cardRange transactions: %w", err)
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT MIN(period_end), MAX(period_end) FROM card_invoices`,
	).Scan(&invMin, &invMax); err != nil {
		return -1, -1, fmt.Errorf("cardRange invoices: %w", err)
	}
	lo, hi := int64(-1), int64(-1)
	for _, n := range []sql.NullInt64{txMin, invMin} {
		if n.Valid && (lo < 0 || n.Int64 < lo) {
			lo = n.Int64
		}
	}
	for _, n := range []sql.NullInt64{txMax, invMax} {
		if n.Valid && n.Int64 > hi {
			hi = n.Int64
		}
	}
	return lo, hi, nil
}
