package plaid

import (
	"regexp"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// security is one row of silver's `securities` table, as the projection
// reads it. priced is true when a holding or a buy or sell of it states a
// price other than 0 or 1. bond is readsAsBond's verdict on it, read once
// (readSecurities).
type security struct {
	id, name, ticker, typ, currency, cusip, isin, cfi string
	payload                                           string
	priced, bond                                      bool
}

// instrumentKey is the gold instrument id of a security: its CUSIP, else its
// ISIN, else its ticker, else Plaid's own security id. Plaid leaves CUSIP
// and ISIN empty for most customers, and gives some funds no ticker. Holdings
// and the investment ledger key their instruments through this one function,
// so a trade lands on the instrument its holding made.
func instrumentKey(s security) string {
	for _, k := range []string{s.cusip, s.isin, s.ticker} {
		if k = strings.TrimSpace(k); k != "" {
			return k
		}
	}
	return "plaid:" + s.id
}

// isCash reports whether a security is cash itself: Plaid's `cash` type
// with no ticker, a currency code as its ticker ("USD"), or a `CUR:`
// ticker. Cash is a balance of its account, not a position. A cash-type
// security with a ticker of its own is a money market fund, and stays a
// position.
//
// Cash is worth one per unit. Plaid can type a bond as cash, with no
// ticker, and a price then gives it away. Such a security is a position,
// and its type says nothing (pairFor).
//
// A security that reads as a bond (readsAsBond) is not cash either,
// priced or not.
func isCash(s security, currency string) bool {
	if norm(s.typ) != "cash" {
		return false
	}
	t := strings.ToUpper(strings.TrimSpace(s.ticker))
	if t == "" {
		return !s.priced && !s.bond
	}
	return strings.HasPrefix(t, "CUR:") ||
		t == strings.ToUpper(currency) || t == strings.ToUpper(s.currency)
}

var (
	bondCouponRe   = regexp.MustCompile(`\d(\.\d+)?\s*%|\b\d{2}\.\d{5}\b`)
	bondMaturityRe = regexp.MustCompile(`\b\d{1,2}[/-]\d{1,2}[/-]\d{2,4}\b`)
	bondPaperRe    = regexp.MustCompile(`\b(BDS?|BONDS)\b`)
	bondSettledRe  = regexp.MustCompile(`\(CASH\)\s*$`)
	bondNotBondRe  = regexp.MustCompile(`\b(PRF|PFD|PREF|PREFERRED|DEP SHS|BANCORP|BANCSHARES|FUNDS?|FD|ETF|MONEY MARKET|MONEY MKT|MMKT)\b`)
)

// readsAsBond reports whether a security Plaid types loosely is a bond.
// Plaid can type a municipal bond as `other`, as `cash` with a dotted
// pseudo-ticker or none, or as `equity` with no ticker. A CFI code
// (ISO 10962) decides where Plaid states one: category D is debt.
// Without one, the name reads as a bond in one of four ways:
//
//   - a coupon rate beside a maturity date;
//   - a municipal issuer's words (canonical.ReadsMunicipal), the rule
//     gold's bond description reader applies;
//   - the kind of paper (BD, BDS, BONDS), in a name with no legal form
//     only a company has;
//   - the "(Cash)" suffix of a bond settled in cash, beside a coupon rate
//     or a maturity date.
//
// A cash security with no ticker is the account's cash unless its name
// states a coupon and a maturity, or the "(Cash)" suffix beside either:
// an issuer's word alone does not make a sweep a bond. A preferred share
// can state a rate and a date too, a fund can name municipal bonds, and a
// bank holding company can carry a county's word; each stays what its
// type says.
func readsAsBond(s security) bool {
	ticker := strings.TrimSpace(s.ticker)
	switch norm(s.typ) {
	case "other", "":
	case "cash":
		// A fund's ticker or a currency code keeps a cash security what it
		// is; only a dotted pseudo-ticker, or none, can be a bond's.
		if ticker != "" && !strings.Contains(ticker, ".") {
			return false
		}
	case "equity":
		if ticker != "" {
			return false
		}
	default:
		return false
	}
	if cfi := strings.ToUpper(strings.TrimSpace(s.cfi)); cfi != "" {
		return cfi[0] == 'D'
	}
	name := strings.ToUpper(s.name)
	if bondNotBondRe.MatchString(name) {
		return false
	}
	coupon, maturity := bondCouponRe.MatchString(name), bondMaturityRe.MatchString(name)
	settled := bondSettledRe.MatchString(name) && (coupon || maturity)
	if norm(s.typ) == "cash" && ticker == "" {
		return coupon && maturity || settled
	}
	return coupon && maturity || settled || canonical.ReadsMunicipal(name) ||
		bondPaperRe.MatchString(name) && !canonical.HasCompanyForm(name)
}

// pairFor maps a security that is not cash to its (asset class, vehicle)
// pair, and reports whether Plaid's type was recognised. A type Plaid
// leaves as `other`, or one no case knows, falls back on the security's
// CFI code (ISO 10962). Category C is a collective investment vehicle: an
// ETF in group E, else a fund, with its exposure read off its name as for
// Plaid's own types. Anything else is (other, other). A security whose
// verdict is a bond (readsAsBond) is (fixed_income, bond) whatever its
// loose type, and counts as unrecognised. The caller keeps the raw type of
// an unrecognised one in the payload.
func pairFor(s security) (canonical.AssetClass, canonical.Vehicle, bool) {
	if s.bond {
		return canonical.AssetClassFixedIncome, canonical.VehicleBond, false
	}
	switch norm(s.typ) {
	case "equity":
		return canonical.AssetClassPublicEquity, canonical.VehicleStock, true
	case "etf":
		return silver.RefineETFExposure(s.name), canonical.VehicleETF, true
	case "mutual fund":
		return silver.FundExposure(s.name), canonical.VehicleFund, true
	case "cash":
		// A money market fund: cash with a ticker of its own. Without one,
		// the security is priced, and Plaid's type is wrong (isCash).
		if strings.TrimSpace(s.ticker) != "" {
			return canonical.AssetClassCash, canonical.VehicleFund, true
		}
	case "fixed income":
		return canonical.AssetClassFixedIncome, canonical.VehicleBond, true
	case "derivative":
		// Options and warrants, on an equity underlying.
		return canonical.AssetClassPublicEquity, canonical.VehicleOption, true
	case "cryptocurrency":
		// Plaid marks bitcoin as a cash equivalent; it is still crypto.
		return canonical.AssetClassCrypto, canonical.VehiclePhysical, true
	case "loan":
		// A loan the holder owns: a receivable.
		return canonical.AssetClassPrivateDebt, canonical.VehicleLoan, true
	}
	cfi := strings.ToUpper(strings.TrimSpace(s.cfi))
	switch {
	case strings.HasPrefix(cfi, "CE"):
		return silver.RefineETFExposure(s.name), canonical.VehicleETF, false
	case strings.HasPrefix(cfi, "C"):
		return silver.FundExposure(s.name), canonical.VehicleFund, false
	}
	return canonical.AssetClassOther, canonical.VehicleOther, false
}
