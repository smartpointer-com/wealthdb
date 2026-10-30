package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
)

// The keys a fund and a security are known by in gold. One scheme serves
// both: a fund is a security that has a composition. The order prefers
// the identifier two sources most often share.
//
//	isin:<ISIN>      an ISIN
//	cusip:<CUSIP>    a CUSIP
//	figi:<FIGI>      an OpenFIGI identifier
//	symbol:<SYMBOL>  a ticker, without its exchange
//	name:<hash>      a hash of a normalized name, the last resort
//
// A contract a source knows by its own id alone, such as a currency
// forward, takes a name key of that id (ContractKey).
//
// A ticker key carries no exchange: providers label exchanges in their
// own words, and the key must stay stable across providers and dates.
//
// A CUSIP stays a CUSIP key. A CUSIP whose first character is a digit
// is a US or a Canadian security, and nothing in the CUSIP says which,
// so the ISIN the US rule gives (ISINFromCUSIP) is only a guess. Gold
// reads it as an alias of last resort, below any ISIN a source states
// beside the CUSIP.

// Key prefixes.
const (
	KeyISIN   = "isin:"
	KeyCUSIP  = "cusip:"
	keyFIGI   = "figi:"
	KeySymbol = "symbol:"
	keyName   = "name:"
)

// ParseFundRef reads a fund reference written by hand (a config entry):
// isin:<ISIN>, cusip:<CUSIP> or symbol:<SYMBOL>, in any case. It returns
// the reference in the form gold builds.
func ParseFundRef(s string) (string, error) {
	prefix, body, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("fund reference %q: want isin:…, cusip:… or symbol:…", s)
	}
	switch strings.ToLower(prefix) + ":" {
	case KeyISIN:
		if i := normalizeISIN(body); i != "" {
			return KeyISIN + i, nil
		}
		return "", fmt.Errorf("fund reference %q: not an ISIN", s)
	case KeyCUSIP:
		if c := normalizeID(body); len(c) == 9 {
			return KeyCUSIP + c, nil
		}
		return "", fmt.Errorf("fund reference %q: a CUSIP has nine characters", s)
	case KeySymbol:
		return KeySymbol + normalizeID(body), nil
	}
	return "", fmt.Errorf("fund reference %q: want isin:…, cusip:… or symbol:…", s)
}

// SecurityKey is the key of a security with these identifiers, in the
// order the scheme above gives; "" when every one is blank.
func SecurityKey(isin, cusip, figi, ticker, name string) string {
	if i := normalizeISIN(isin); i != "" {
		return KeyISIN + i
	}
	if c := normalizeID(cusip); c != "" {
		return KeyCUSIP + c
	}
	if f := normalizeID(figi); f != "" {
		return keyFIGI + f
	}
	if t := normalizeID(ticker); t != "" {
		return KeySymbol + t
	}
	if n := normalizeName(name); n != "" {
		h := sha256.Sum256([]byte(n))
		return keyName + hex.EncodeToString(h[:6])
	}
	return ""
}

// ContractKey is the key of a contract a source knows by its own id
// alone, such as a currency forward: a name key of the id, so no provider
// is asked for it and no fund line names it. "" for a blank id.
func ContractKey(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return SecurityKey("", "", "", "", "contract "+id)
}

// normalizeISIN is s upper-cased and trimmed when it has an ISIN's shape
// (two letters, nine alphanumerics, a digit), else "". The check digit is
// not verified: a provider's ISIN is its key even where the digit is off.
func normalizeISIN(s string) string {
	s = normalizeID(s)
	if len(s) != 12 || !isLetter(s[0]) || !isLetter(s[1]) || !isDigit(s[11]) {
		return ""
	}
	for i := 2; i < 11; i++ {
		if !isLetter(s[i]) && !isDigit(s[i]) {
			return ""
		}
	}
	return s
}

// ISINFromCUSIP is the ISIN a US CUSIP gives by rule: US, the CUSIP, and
// the ISIN check digit. A CUSIP whose first character is a letter is a
// CINS code for a security outside North America and gives none, nor
// does anything that is not nine alphanumerics. A Canadian CUSIP also
// starts with a digit and takes CA instead, so the result is a guess
// until a source confirms it.
func ISINFromCUSIP(cusip string) string {
	c := normalizeID(cusip)
	if len(c) != 9 || !isDigit(c[0]) {
		return ""
	}
	for i := 0; i < 9; i++ {
		if !isLetter(c[i]) && !isDigit(c[i]) {
			return ""
		}
	}
	body := "US" + c
	return body + string(rune('0'+ISINCheckDigit(body)))
}

// ISINCheckDigit is the check digit of the first eleven characters of an
// ISIN: letters become their numbers (A=10 … Z=35), and the Luhn
// algorithm runs over the digits.
func ISINCheckDigit(body string) int {
	var digits []int
	for i := 0; i < len(body); i++ {
		ch := body[i]
		switch {
		case isDigit(ch):
			digits = append(digits, int(ch-'0'))
		case isLetter(ch):
			v := int(ch-'A') + 10
			digits = append(digits, v/10, v%10)
		}
	}
	sum := 0
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if (len(digits)-1-i)%2 == 0 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return (10 - sum%10) % 10
}

func normalizeID(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// normalizeName folds a name to lower-case letters and digits separated
// by single spaces, so two spellings of one name hash alike.
func normalizeName(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
		} else {
			space = true
		}
	}
	return b.String()
}

func isLetter(c byte) bool { return c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
