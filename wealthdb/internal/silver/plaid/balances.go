package plaid

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// statedBalance is a figure a cash, card or mortgage account stated: a
// cash or card balance of `kind`, or a mortgage's principal, in gold's
// sign. An investment account with no holding states its balance too
// (appendUnitemized).
type statedBalance struct {
	at       int64
	currency string
	kind     canonical.BalanceKind
	amount   canonical.Decimal
}

// statedOf is the figure an account's listing in the run at t states, if
// it states one.
func statedOf(a account, t int64) (statedBalance, bool) {
	amount := silver.DecimalPtrOrNil(a.balance)
	available := silver.DecimalPtrOrNil(a.available)
	switch {
	case a.currency == "":
	case amount != nil && (a.kind == canonical.AccountKindCard || a.kind == canonical.AccountKindMortgage):
		return statedBalance{t, a.currency, canonical.BalanceKindCurrent, amount.Neg()}, true
	case amount != nil:
		return statedBalance{t, a.currency, canonical.BalanceKindCurrent, *amount}, true
	case a.kind == canonical.AccountKindCash && available != nil:
		// Plaid leaves the current balance empty at some institutions; the
		// available balance is then all there is.
		return statedBalance{t, a.currency, canonical.BalanceKindAvailable, *available}, true
	}
	return statedBalance{}, false
}

// noteBalances records in `stated` the figures a run that carries no
// snapshots states, so a later carry restates the newest of them.
func (c *Connection) noteBalances(ctx context.Context, t int64, stated map[string]statedBalance) error {
	accounts, err := c.accountsAt(ctx, t)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if s, ok := statedOf(a, t); ok {
			stated[a.id] = s
		}
	}
	return nil
}

// unitemizedKey is the position key, and the instrument, of the value of
// an investment account Plaid lists with no holding.
const unitemizedKey = "plaid:unitemized"

// appendUnitemized adds one (other, other) position for each investment
// account the run lists with no holding and a balance other than zero,
// worth that balance. Plaid's balance of an investment account is its
// total value, so the account counts at its worth, and the payload says
// its holdings are not itemized. Where the run states no balance, the
// last one stated since the account last held something is carried,
// marked "carried": unknown is not zero. A run in which the account holds
// something states no such position, and the account's snapshot then
// replaces it whole.
func appendUnitemized(b *canonical.SnapshotBatch, t int64, accounts []account, holdings []holding,
	stated map[string]statedBalance) {
	held := map[string]bool{}
	for _, h := range holdings {
		held[h.accountID] = true
	}
	instrument := false
	for _, a := range accounts {
		if !investmentLedgerKind(a.kind) {
			continue
		}
		if held[a.id] {
			delete(stated, a.id)
			continue
		}
		payload := `{"basis":"unitemized"}`
		s, ok := statedOf(a, t)
		if ok {
			stated[a.id] = s
		} else if s, ok = stated[a.id]; ok {
			payload = fmt.Sprintf(`{"basis":"carried","stated_at":%d}`, s.at)
		}
		if !ok || s.amount.IsZero() {
			continue
		}
		key, value := unitemizedKey, s.amount
		b.Positions = append(b.Positions, canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    a.id,
			PositionKey:          key,
			InstrumentExternalID: &key,
			AssetClass:           canonical.AssetClassOther,
			Vehicle:              canonical.VehicleOther,
			Currency:             s.currency,
			MarketValue:          &value,
			Payload:              json.RawMessage(payload),
		})
		if !instrument {
			instrument = true
			name := "Unitemized holdings"
			b.Instruments = append(b.Instruments, canonical.InstrumentChange{
				InstrumentExternalID: key,
				AssetClass:           canonical.AssetClassOther,
				Vehicle:              canonical.VehicleOther,
				Name:                 &name,
				FirstSeenAt:          t,
				LastSeenAt:           t,
				Payload:              json.RawMessage(`{}`),
			})
		}
	}
}

// appendMortgage adds a mortgage's outstanding principal as a negative
// (real estate, mortgage) position, keyed by the account: one loan, one
// position, one instrument.
func appendMortgage(b *canonical.SnapshotBatch, t int64, a account, s statedBalance) {
	id := a.id
	value := s.amount
	currency := s.currency
	payload := json.RawMessage(a.payload)
	if s.at != t {
		payload = silver.PayloadWith(a.payload, map[string]any{"basis": "carried", "stated_at": s.at})
	}
	b.Instruments = append(b.Instruments, canonical.InstrumentChange{
		InstrumentExternalID: id,
		AssetClass:           canonical.AssetClassRealEstate,
		Vehicle:              canonical.VehicleMortgage,
		Name:                 silver.StrPtrIfNonEmpty(displayName(a)),
		Currency:             &currency,
		FirstSeenAt:          t,
		LastSeenAt:           t,
	})
	b.Positions = append(b.Positions, canonical.PositionChange{
		SnapshotAt:           t,
		AccountExternalID:    id,
		PositionKey:          id,
		InstrumentExternalID: &id,
		AssetClass:           canonical.AssetClassRealEstate,
		Vehicle:              canonical.VehicleMortgage,
		Currency:             currency,
		MarketValue:          &value,
		Payload:              payload,
	})
}

// appendStatementCloses adds a card's CLOSING balance at the issue date of
// each statement in the window: the balance the statement stated, negated.
// Every run restates the last statement, so the newest run that states a
// figure for a statement stands.
//
// Only a statement issued before the newest run that carries snapshots
// counts. A later one, read by a run whose holdings failed, would report
// the card alone past that run, and so end every account last observed
// beside it (DESIGN.md §10.7). It counts once a later run carries
// snapshots. A card whose newest listing names no currency closes in the
// currency of its last stated balance.
func (c *Connection) appendStatementCloses(ctx context.Context, w canonical.Window, newest int64,
	latest map[string]account, stated map[string]statedBalance,
	at func(int64) *canonical.SnapshotBatch) error {
	return silver.ScanRows(ctx, c.db, "plaid appendStatementCloses", `
SELECT account_id, last_statement_issue_date, last_statement_balance FROM (
    SELECT account_id, last_statement_issue_date, last_statement_balance,
           ROW_NUMBER() OVER (PARTITION BY account_id, last_statement_issue_date
                              ORDER BY snapshot_at DESC) AS rn
      FROM liabilities
     WHERE kind = 'credit'
       AND last_statement_balance IS NOT NULL
       AND last_statement_issue_date BETWEEN ? AND ?
       AND last_statement_issue_date < ?
) WHERE rn = 1
 ORDER BY account_id, last_statement_issue_date`, func(rows *sql.Rows) error {
		var (
			id      string
			issued  int64
			balance sql.NullString
		)
		if err := rows.Scan(&id, &issued, &balance); err != nil {
			return err
		}
		a, ok := latest[id]
		amount := silver.DecimalPtrOrNil(balance)
		currency := cmp.Or(a.currency, stated[id].currency)
		if !ok || a.kind != canonical.AccountKindCard || amount == nil || currency == "" {
			return nil
		}
		b := at(issued)
		b.Accounts = append(b.Accounts, accountChange(a, issued, issued))
		b.CashBalances = append(b.CashBalances, silver.CashBalance(issued, id, currency,
			canonical.BalanceKindClosing, amount.Neg(), `{"basis":"statement_closing"}`))
		return nil
	}, w.Start, w.End, newest)
}
