package silver

import (
	"regexp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Statement-shape regexes shared by the statement-history classifiers
// of source adapters whose PDFs carry no structured type code (today
// fidelity and schwab), so instrument-key and description shape
// heuristics are all there is. Only shapes that are byte-identical
// across those adapters live here, with the classifier tail that uses
// them (StatementShapePair); source-specific variants (e.g. a
// money-market pattern that also matches an "NET CASH POSITION" sleeve)
// stay local to the adapter that needs them.
var (
	// StmtOptionKeyRe matches an OCC option symbol: root + YYMMDD + C/P + strike.
	StmtOptionKeyRe = regexp.MustCompile(`^[A-Z.]{1,6}\d{6}[CP]\d+(\.\d+)?$`)
	// StmtOptionDescRe matches an option leg description ("CALL …" / "PUT …").
	StmtOptionDescRe = regexp.MustCompile(`^(CALL|PUT)\b`)
	// StmtCUSIPRe matches a 9-character CUSIP instrument key.
	StmtCUSIPRe = regexp.MustCompile(`^[A-Z0-9]{8}[0-9]$`)
	// StmtBondDescRe matches a coupon-bearing bond line ("… 04.12500% …" / "FIXED COUPON").
	StmtBondDescRe = regexp.MustCompile(`(?i)\b\d{1,2}\.\d{3,5}%|FIXED COUPON`)
	// StmtMoneyMktKeyRe matches a US money-market fund's 5-letter doubled-X ticker.
	StmtMoneyMktKeyRe = regexp.MustCompile(`^[A-Z]{3}XX$`)
	// StmtETFDescRe matches a description carrying the "ETF" token.
	StmtETFDescRe = regexp.MustCompile(`\bETF\b`)
	// StmtMutualFundRe matches an ordinary mutual fund's 5-letter single-trailing-X ticker.
	StmtMutualFundRe = regexp.MustCompile(`^[A-Z]{4}X$`)
	// StmtETFIssuerRe matches ETF-only issuer families whose lines often omit the "ETF" token.
	StmtETFIssuerRe = regexp.MustCompile(`(?i)^(ISHARES|SPDR|VANGUARD|XTRACKERS|PROSHARES|WISDOMTREE)\b`)
)

// StatementShapePair is the (exposure, vehicle) pair of a statement line
// the source-specific shapes did not place, from its instrument key and
// description. First match wins:
//
//   - bond (CUSIP-9 key or coupon in the description) → (fixed_income, bond).
//   - ETF by name → (RefineETFExposure(desc), etf).
//   - mutual-fund ticker → (FundExposure(desc), fund): a money fund whose
//     name the source's money-market shape missed (e.g. "… MONEY FUND")
//     still reads as cash.
//   - ETF-only issuer name → (RefineETFExposure(desc), etf).
//   - fall-through → (public_equity, stock): plain stock/ADR lines, the
//     statements' overwhelming majority.
//
// Every pair returned satisfies canonical.ValidTaxonomyPair.
func StatementShapePair(instrumentKey, description string) (canonical.AssetClass, canonical.Vehicle) {
	switch {
	case StmtCUSIPRe.MatchString(instrumentKey),
		StmtBondDescRe.MatchString(description):
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case StmtETFDescRe.MatchString(description):
		return RefineETFExposure(description), canonical.VehicleETF
	case StmtMutualFundRe.MatchString(instrumentKey):
		return FundExposure(description), canonical.VehicleFund
	case StmtETFIssuerRe.MatchString(description):
		return RefineETFExposure(description), canonical.VehicleETF
	}
	return canonical.AssetClassPublicEquity, canonical.VehicleStock
}
