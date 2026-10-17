package silver

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func TestRefineETFExposure(t *testing.T) {
	cases := []struct {
		name string
		want canonical.AssetClass
	}{
		// Crypto ETFs / ETPs.
		{"iShares Bitcoin Trust ETF", canonical.AssetClassCrypto},
		{"GRAYSCALE ETHEREUM TRUST", canonical.AssetClassCrypto},
		{"21Shares Crypto Basket Index ETP", canonical.AssetClassCrypto},
		// Physical-metal ETFs / ETPs.
		{"SPDR Gold Shares", canonical.AssetClassMetal},
		{"iShares Silver Trust", canonical.AssetClassMetal},
		{"abrdn Physical Platinum Shares ETF", canonical.AssetClassMetal},
		{"Invesco DB Precious Metals Fund", canonical.AssetClassMetal},
		// Miners hold mining STOCKS — equity exposure, not metal.
		{"VanEck Gold Miners ETF", canonical.AssetClassPublicEquity},
		{"Global X Silver Miners ETF", canonical.AssetClassPublicEquity},
		// Bond / fixed-income keywords.
		{"ISHARES 20+ YEAR TREASURY BOND ETF", canonical.AssetClassFixedIncome},
		{"Vanguard Total Bond Market ETF", canonical.AssetClassFixedIncome},
		{"SCHWAB US TIPS ETF", canonical.AssetClassFixedIncome},
		{"iShares National Muni Bond ETF", canonical.AssetClassFixedIncome},
		{"SPDR Bloomberg High Yield Bond ETF", canonical.AssetClassFixedIncome},
		{"Qarvel Bond Index Sleeve CHF", canonical.AssetClassFixedIncome},
		// Everything else defaults to public equity.
		{"iShares MSCI EAFE Small-Cap ETF", canonical.AssetClassPublicEquity},
		{"Vanguard Total Stock Market ETF", canonical.AssetClassPublicEquity},
		// Word boundaries: no false positives from lookalike names.
		{"Goldman Sachs ActiveBeta US Large Cap ETF", canonical.AssetClassPublicEquity},
		{"iShares MSCI Netherlands ETF", canonical.AssetClassPublicEquity},
	}
	for _, c := range cases {
		if got := RefineETFExposure(c.name); got != c.want {
			t.Errorf("RefineETFExposure(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestFundExposure pins the fund rule: a money-market name is cash even when
// it carries a bond keyword, and any other fund reads like an ETF.
func TestFundExposure(t *testing.T) {
	for name, want := range map[string]canonical.AssetClass{
		"EXAMPLE TREASURY MONEY MARKET FUND": canonical.AssetClassCash,
		"EXAMPLE GOVERNMENT MONEY FUND":      canonical.AssetClassCash,
		"EXAMPLE TOTAL BOND MARKET FUND":     canonical.AssetClassFixedIncome,
		"EXAMPLE GROWTH FUND":                canonical.AssetClassPublicEquity,
	} {
		if got := FundExposure(name); got != want {
			t.Errorf("FundExposure(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestStatementShapePair pins the shared statement-classifier tail, first
// match winning.
func TestStatementShapePair(t *testing.T) {
	for _, c := range []struct {
		key, desc string
		ac        canonical.AssetClass
		v         canonical.Vehicle
	}{
		{"SYNCUSIP1", "EXAMPLE CORP NOTE", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		{"", "EXAMPLE CORP 04.12500% 2030", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		// A CUSIP key wins over an ETF description.
		{"SYNCUSIP1", "EXAMPLE TOTAL MARKET ETF", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		{"EXMPL", "EXAMPLE TOTAL MARKET ETF", canonical.AssetClassPublicEquity, canonical.VehicleETF},
		{"EXMPX", "EXAMPLE GOVERNMENT MONEY FUND", canonical.AssetClassCash, canonical.VehicleFund},
		{"EXMPX", "EXAMPLE GROWTH FUND", canonical.AssetClassPublicEquity, canonical.VehicleFund},
		{"AGG", "ISHARES CORE U.S. AGGREGATE BOND", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		{"EXMP", "EXAMPLE CORP COMMON", canonical.AssetClassPublicEquity, canonical.VehicleStock},
	} {
		ac, v := StatementShapePair(c.key, c.desc)
		if ac != c.ac || v != c.v {
			t.Errorf("StatementShapePair(%q, %q) = (%s, %s), want (%s, %s)", c.key, c.desc, ac, v, c.ac, c.v)
		}
		if !canonical.ValidTaxonomyPair(ac, v) {
			t.Errorf("StatementShapePair(%q, %q) = (%s, %s), not a valid pair", c.key, c.desc, ac, v)
		}
	}
}
