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
		// Fixed income abbreviated, as a broker's short name prints it.
		{"EXAMPLE FUND A FXD INC", canonical.AssetClassFixedIncome},
		{"Example Fund B Fixed Inc", canonical.AssetClassFixedIncome},
		{"Example Fund C Fxd Income", canonical.AssetClassFixedIncome},
		// Everything else defaults to public equity.
		{"iShares MSCI EAFE Small-Cap ETF", canonical.AssetClassPublicEquity},
		{"Vanguard Total Stock Market ETF", canonical.AssetClassPublicEquity},
		// Word boundaries: no false positives from lookalike names.
		{"Goldman Sachs ActiveBeta US Large Cap ETF", canonical.AssetClassPublicEquity},
		{"iShares MSCI Netherlands ETF", canonical.AssetClassPublicEquity},
		{"Example Affxd Income Leaders ETF", canonical.AssetClassPublicEquity},
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

// RefineClass reads OpenFIGI's type over the pair an adapter's type and
// the name give an instrument: a bond's or a share's type sets the
// pair; a fund's keeps an ETF or a fund and makes anything else a fund,
// its exposure read from the name; any other type, and a pair the
// taxonomy refuses, leaves the pair. Names are invented.
func TestRefineClassTakesTheLookupsTypeFirst(t *testing.T) {
	pe, fi := canonical.AssetClassPublicEquity, canonical.AssetClassFixedIncome
	cases := []struct {
		figi    string
		ac      canonical.AssetClass
		v       canonical.Vehicle
		name    string
		wantAC  canonical.AssetClass
		wantVeh canonical.Vehicle
	}{
		// A crypto trust an adapter types as an equity is a fund.
		{"Mutual Fund", pe, canonical.VehicleStock, "EXAMPLE BITCOIN TRUST", canonical.AssetClassCrypto, canonical.VehicleFund},
		{"ETP", pe, canonical.VehicleStock, "EXAMPLE ETHEREUM TRUST", canonical.AssetClassCrypto, canonical.VehicleETF},
		{"Closed-End Fund", pe, canonical.VehicleStock, "EXAMPLE MUNICIPAL INCOME", fi, canonical.VehicleFund},
		// An ETF an adapter types keeps its pair under OpenFIGI's fund type,
		// its exposure read from its abbreviated name.
		{"Mutual Fund", fi, canonical.VehicleETF, "EXAMPLE TOTAL BD MKT ETF", fi, canonical.VehicleETF},
		{"ETP", pe, canonical.VehicleFund, "EXAMPLE UNIT TRUST", pe, canonical.VehicleETF},
		{"Common Stock", pe, canonical.VehicleETF, "EXAMPLE HOLDINGS", pe, canonical.VehicleStock},
		{"Depositary Receipt", canonical.AssetClassOther, canonical.VehicleOther, "EXAMPLE ADR", pe, canonical.VehicleStock},
		{"Corp", pe, canonical.VehicleStock, "EXAMPLE 4% 2041", fi, canonical.VehicleBond},
		{"Option", pe, canonical.VehicleOption, "EXAMPLE CALL", pe, canonical.VehicleOption},
		{"", pe, canonical.VehicleStock, "EXAMPLE BITCOIN TRUST", pe, canonical.VehicleStock},
		// A money-market fund OpenFIGI calls an ETP stays a fund: cash is
		// no exchange-traded exposure.
		{"ETP", canonical.AssetClassCash, canonical.VehicleFund, "EXAMPLE MONEY MARKET", canonical.AssetClassCash, canonical.VehicleFund},
	}
	for _, c := range cases {
		ac, v := RefineClass(c.figi, c.ac, c.v, c.name)
		if ac != c.wantAC || v != c.wantVeh {
			t.Errorf("RefineClass(%q, %s, %s, %q) = (%s, %s), want (%s, %s)",
				c.figi, c.ac, c.v, c.name, ac, v, c.wantAC, c.wantVeh)
		}
		if again, againV := RefineClass(c.figi, ac, v, c.name); again != ac || againV != v {
			t.Errorf("RefineClass(%q) over its own (%s, %s) = (%s, %s)", c.figi, ac, v, again, againV)
		}
	}
}
