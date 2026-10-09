package schwab

import (
	"regexp"
	"strings"
)

// A 1099-B lot carries no ticker and no CUSIP: the collector's parser
// leaves instrument_key NULL, and `security_name` is the lot's only
// statement of what it sold. It comes in exactly two shapes:
//
//	EXA                         a plain ticker
//	EXA 01/20/2023 400.00 C     an option contract
//
// Both are the key the statement and history feeds store for the same
// security, so the name, read in either shape, is the key itself.

// occOptionRe matches an option contract: an underlying, an expiry, a
// strike and a put/call letter, the form the statements print.
// Anchored whole, so a name that merely opens with a ticker cannot be
// read as an option.
var occOptionRe = regexp.MustCompile(
	`^[A-Z][A-Z./]{0,8}\s+\d{2}/\d{2}/\d{4}\s+[\d.]+\s+[CP]$`)

// plainTickerRe is the other shape. `/` is allowed because a share
// class is spelled into the ticker (a class-B line reads `XXX/B`), and
// refusing it would drop a real holding for punctuation.
var plainTickerRe = regexp.MustCompile(`^[A-Z][A-Z./]{0,8}$`)

// securityNameKey reads the instrument key a 1099-B security name
// states. An option keeps its contract rather than resolving to its
// underlying: a realized lot is the contract's, and a gain booked
// against the stock would mix the two. ok is false for a name of
// neither shape, which then states no instrument rather than a
// guessed one. Runs of spaces collapse to one, as the statements print
// a contract.
func securityNameKey(securityName string) (key string, ok bool) {
	name := strings.Join(strings.Fields(securityName), " ")
	if occOptionRe.MatchString(name) || plainTickerRe.MatchString(name) {
		return name, true
	}
	return "", false
}
