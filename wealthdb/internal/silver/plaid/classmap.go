package plaid

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// security is one row of silver's `securities` table, as the projection
// reads it.
type security struct {
	id, name, ticker, typ, currency, cusip, isin, cfi string
	payload                                           string
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
func isCash(s security, holdingCurrency string) bool {
	if norm(s.typ) != "cash" {
		return false
	}
	t := strings.ToUpper(strings.TrimSpace(s.ticker))
	return t == "" || strings.HasPrefix(t, "CUR:") ||
		t == strings.ToUpper(holdingCurrency) || t == strings.ToUpper(s.currency)
}

// pairFor maps a security that is not cash to its (asset class, vehicle)
// pair, and reports whether Plaid's type was recognised. A type Plaid
// leaves as `other`, or one no case knows, falls back on the security's
// CFI code (ISO 10962). Category C is a collective investment vehicle: an
// ETF in group E, else a fund, with its exposure read off its name as for
// Plaid's own types. Anything else is (other, other). The caller keeps the
// raw type of an unrecognised one in the payload.
func pairFor(s security) (canonical.AssetClass, canonical.Vehicle, bool) {
	switch norm(s.typ) {
	case "equity":
		return canonical.AssetClassPublicEquity, canonical.VehicleStock, true
	case "etf":
		return silver.RefineETFExposure(s.name), canonical.VehicleETF, true
	case "mutual fund":
		return fundExposure(s.name), canonical.VehicleFund, true
	case "cash":
		// A money market fund: cash with a ticker of its own (isCash).
		return canonical.AssetClassCash, canonical.VehicleFund, true
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
		return fundExposure(s.name), canonical.VehicleFund, false
	}
	return canonical.AssetClassOther, canonical.VehicleOther, false
}

// fundExposure is a mutual fund's exposure: a money market fund is cash,
// and any other fund reads its exposure off its name, as an ETF does.
func fundExposure(name string) canonical.AssetClass {
	if silver.NamesMoneyMarket(name) {
		return canonical.AssetClassCash
	}
	return silver.RefineETFExposure(name)
}
