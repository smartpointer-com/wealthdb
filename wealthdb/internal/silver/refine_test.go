package silver

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

func TestRefineETFClass(t *testing.T) {
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
		{"VanEck Gold Miners ETF", canonical.AssetClassETF},
		{"Global X Silver Miners ETF", canonical.AssetClassETF},
		// Bond / fixed-income ETFs.
		{"ISHARES 20+ YEAR TREASURY BOND ETF", canonical.AssetClassBondETF},
		{"Vanguard Total Bond Market ETF", canonical.AssetClassBondETF},
		{"SCHWAB US TIPS ETF", canonical.AssetClassBondETF},
		{"iShares National Muni Bond ETF", canonical.AssetClassBondETF},
		{"SPDR Bloomberg High Yield Bond ETF", canonical.AssetClassBondETF},
		// Everything else stays etf.
		{"iShares MSCI EAFE Small-Cap ETF", canonical.AssetClassETF},
		{"Vanguard Total Stock Market ETF", canonical.AssetClassETF},
		// Word boundaries: no false positives from lookalike names.
		{"Goldman Sachs ActiveBeta US Large Cap ETF", canonical.AssetClassETF},
		{"iShares MSCI Netherlands ETF", canonical.AssetClassETF},
	}
	for _, c := range cases {
		if got := RefineETFClass(c.name); got != c.want {
			t.Errorf("RefineETFClass(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRefineETFExposure(t *testing.T) {
	cases := []struct {
		name string
		want canonical.AssetClass
	}{
		{"iShares Bitcoin Trust ETF", canonical.AssetClassCrypto},
		{"SPDR Gold Shares", canonical.AssetClassMetal},
		{"VanEck Gold Miners ETF", canonical.AssetClassPublicEquity}, // miners hold stocks
		{"iShares 20+ Year Treasury Bond ETF", canonical.AssetClassFixedIncome},
		{"Vanguard Total Stock Market ETF", canonical.AssetClassPublicEquity},
		{"Swisscanto (CH) Index Bond Fund Placeholder CHF", canonical.AssetClassFixedIncome},
	}
	for _, c := range cases {
		if got := RefineETFExposure(c.name); got != c.want {
			t.Errorf("RefineETFExposure(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
