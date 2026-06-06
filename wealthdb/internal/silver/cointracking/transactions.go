package cointracking

import (
	"context"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// Transactions is the event-grain surface — currently a no-op for
// cointracking. The silver `transactions` table is the authoritative
// trade-history record at full per-row fidelity, and downstream
// queries can read it directly. Promoting the per-trade records
// into gold's `transactions` table would mean classifying CT's
// per-row `type` (Trade / Deposit / Withdrawal / Income / Spend /
// Lost / Stolen / Gift / Mining / Staking / Airdrop / …) into the
// canonical TxKind enum, splitting two-sided trades into a buy +
// sell pair, deriving the canonical signed amounts in the
// portfolio's quote currency, and validating sign convention
// against the running balance reconciliation. Left as a follow-up
// for when a downstream query actually needs it.
func (c *Connection) Transactions(_ context.Context, w canonical.Window) (silver.TransactionStream, error) {
	return &emptyTxStream{}, nil
}

type emptyTxStream struct{}

func (s *emptyTxStream) Next(context.Context) (canonical.TransactionBatch, bool, error) {
	return canonical.TransactionBatch{}, false, nil
}

func (s *emptyTxStream) Close() error { return nil }
