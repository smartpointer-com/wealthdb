package cointracking

import (
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// returnsPolicy is the kind's ReturnsPolicy: flow_complete, so every
// capital kind counts, every other knob at its default.
//
// Fiat deposits and withdrawals are capital, and so are a coin's
// transfer_in and transfer_out, valued at the day's rate. The load's
// matcher pairs a move between two wallets (transfer_links), and such a
// pair nets in every aggregate that holds both wallets, as any source's
// transfers do. A leg with no partner is a flow, whatever moved the coin:
// a wallet nothing tracks, an airdrop or a gift (kindTable gives them all
// a transfer kind). A wallet that debuts funded by a move from another
// wallet onboards only what that move did not bring in.
func returnsPolicy() *returns.ReturnsPolicy {
	p := returns.DefaultReturnsPolicy(returns.BankFlowPolicy())
	return &p
}
