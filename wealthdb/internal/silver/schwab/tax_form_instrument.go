package schwab

import "regexp"

// The 1099-B feed is a lot-disposal record: every row of it is a SALE,
// so whatever it cannot resolve shows up on the sell side alone. The
// asymmetry is the feed, not the direction.
//
// It carries no ticker and no CUSIP — the parser says so itself
// (`"instrument_key": None, # name-only; gold resolves by name`) — and
// the resolution it was left for was never built. What it does carry is
// `security_name`, in exactly two shapes:
//
//	EXA                         a plain ticker
//	EXA 01/20/2023 400.00 C     an option on one
//
// occOptionRe matches the second: an underlying, an expiry, a strike and
// a put/call letter. Anchored whole, so a name that merely opens with a
// ticker cannot be read as an option.
var occOptionRe = regexp.MustCompile(
	`^([A-Z][A-Z./]{0,8})\s+\d{2}/\d{2}/\d{4}\s+[\d.]+\s+[CP]$`)

// plainTickerRe is the other shape. `/` is allowed because a share
// class is spelled into the ticker (a class-B line reads `XXX/B`), and
// refusing it would drop a real holding for punctuation.
var plainTickerRe = regexp.MustCompile(`^[A-Z][A-Z./]{0,8}$`)

// securityNameInstrument reads what a 1099-B row traded: the symbol to
// resolve against the instrument dimension, and whether the row is an
// OPTION on that symbol rather than the symbol itself.
//
// An option resolves to its UNDERLYING deliberately. The exposure a
// trade touched is the underlying's, and saying so is what moves the
// row out of the untracked-destination class; that it was held through
// an option is the VEHICLE, which the caller stamps beside it. Keeping
// the two apart is the same 2-D split `positions` uses, and the reason
// an option never becomes an asset class.
//
// ok is false for a name of neither shape, and the row then states no
// instrument at all rather than a guessed one.
func securityNameInstrument(securityName string) (symbol string, isOption, ok bool) {
	if m := occOptionRe.FindStringSubmatch(securityName); m != nil {
		return m[1], true, true
	}
	if plainTickerRe.MatchString(securityName) {
		return securityName, false, true
	}
	return "", false, false
}
