package fidelity

import "strings"

// isMoneyMarketPosition reports whether a positions row is the
// brokerage cash sweep / core money-market fund rather than a
// security holding. Fidelity tags these with a trailing "**" on
// `instrument_key` and a description of "HELD IN MONEY MARKET";
// either signal suffices.
//
// Money-market positions route to gold's cash_balances table
// instead of positions + instruments, matching the convention
// the other adapters use for cash-equivalent holdings.
func isMoneyMarketPosition(instrumentKey, description string) bool {
	if strings.HasSuffix(instrumentKey, "**") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(description), "HELD IN MONEY MARKET")
}

// canonicalInstrumentKey returns the instrument identifier with
// any trailing "**" stripped. Fidelity's CSV export adds the
// asterisks to flag the row as a money-market core position; the
// underlying ticker (FDRXX, SPAXX, ...) is the real identity and
// is what `transactions.instrument_key` references.
func canonicalInstrumentKey(instrumentKey string) string {
	return strings.TrimRight(instrumentKey, "*")
}
