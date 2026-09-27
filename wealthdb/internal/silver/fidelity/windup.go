package fidelity

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A wind-up moves a closing account's holdings into a sibling, and the
// feed books it on the account RECEIVING them, one row per holding:
// "TRANSFERRED FROM VS <account>-<n> <security> …". The account giving
// them up would print its side on the statement of the month it closes,
// but a closed account is no longer on that statement, and the feed had
// stopped returning it before then. Its out-legs are printed nowhere, so
// the in-legs alone read as capital arriving from outside: the returns
// engine nets a transfer at the coarse grains only against its
// counter-leg.
//
// The in-leg names the account it came from, which makes the counter-leg
// certain — the same holding, quantity and value leaving that account the
// same day. windUpCounterLegs emits it when the named account is one of
// this source's own and holds no such leg already.
var transferredFromVSRe = regexp.MustCompile(`^TRANSFERRED FROM VS (\d{3})-(\d{6})-\d+\s+(.*)$`)

// windUpInLeg is a journal row naming the sibling account it came from.
type windUpInLeg struct {
	tx   canonical.TransactionChange
	from string // the sibling's account_external_id
	rest string // the Action after the account reference
}

// asWindUpInLeg reports whether a projected row is a wind-up in-leg.
func asWindUpInLeg(tx canonical.TransactionChange, action string) (windUpInLeg, bool) {
	if tx.Kind != canonical.TxKindJournal || tx.NetAmount == nil || !tx.NetAmount.IsPositive() {
		return windUpInLeg{}, false
	}
	m := transferredFromVSRe.FindStringSubmatch(strings.TrimSpace(action))
	if m == nil || m[1]+m[2] == tx.AccountExternalID {
		return windUpInLeg{}, false
	}
	return windUpInLeg{tx: tx, from: m[1] + m[2], rest: m[3]}, true
}

// windUpCounterLegs returns the out-leg of each in-leg whose named
// account is this source's own and books nothing of that value that day.
func (c *Connection) windUpCounterLegs(ctx context.Context, legs []windUpInLeg) ([]canonical.TransactionChange, error) {
	if len(legs) == 0 {
		return nil, nil
	}
	own, err := c.ownAccountIDs(ctx)
	if err != nil {
		return nil, err
	}
	var out []canonical.TransactionChange
	for _, l := range legs {
		if !own[l.from] {
			continue
		}
		var booked int
		if err := c.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM transactions
 WHERE account_external_id = ? AND timestamp = ? AND ROUND(amount, 2) = ROUND(?, 2)`,
			l.from, l.tx.OccurredAt, l.tx.NetAmount.Neg().InexactFloat64()).Scan(&booked); err != nil {
			return nil, fmt.Errorf("fidelity wind-up counter-leg: %w", err)
		}
		if booked > 0 {
			continue
		}
		out = append(out, counterLeg(l))
	}
	return out, nil
}

// counterLeg is the in-leg seen from the account it left.
func counterLeg(l windUpInLeg) canonical.TransactionChange {
	neg := func(d *canonical.Decimal) *canonical.Decimal {
		if d == nil {
			return nil
		}
		n := d.Neg()
		return &n
	}
	descr := "TRANSFERRED TO VS " + l.tx.AccountExternalID + " " + l.rest
	payload, _ := json.Marshal(map[string]string{
		"Action":     descr,
		"counter_of": l.tx.TransactionExternalID,
	})
	return canonical.TransactionChange{
		TransactionExternalID: "windup:" + l.tx.TransactionExternalID,
		OccurredAt:            l.tx.OccurredAt,
		AccountExternalID:     l.from,
		Kind:                  canonical.TxKindJournal,
		Currency:              l.tx.Currency,
		NetAmount:             neg(l.tx.NetAmount),
		Quantity:              neg(l.tx.Quantity),
		Price:                 l.tx.Price,
		InstrumentExternalID:  l.tx.InstrumentExternalID,
		Description:           &descr,
		Payload:               payload,
	}
}

// ownAccountIDs is every account this silver knows: the live roster and
// the accounts only its historical statements carry.
func (c *Connection) ownAccountIDs(ctx context.Context) (map[string]bool, error) {
	q := `SELECT account_external_id FROM accounts`
	ok, err := c.hasHistoricalTable(ctx)
	if err != nil {
		return nil, err
	}
	if ok {
		q += ` UNION SELECT account_external_id FROM historical_position_snapshots`
	}
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("fidelity own accounts: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
