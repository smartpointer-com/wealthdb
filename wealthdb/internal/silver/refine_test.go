package silver

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
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
		{"Swisscanto (CH) Index Bond Fund Placeholder CHF", canonical.AssetClassFixedIncome},
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
