package carta

import "github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"

// capTableAssetClass classifies a cap-table position from the security types it
// aggregates. A holding that is PURELY convertible instruments (SAFEs /
// convertible notes still pre-conversion) is a convertible_note — carried at
// principal and kept distinct from equity until it converts, mirroring the
// manual collector's convertible notes. Anything with real equity (shares,
// options, RSUs/RSAs, SARs, PIUs, warrants, equity grants) — including a
// convertible that has partly converted into shares — is private_equity: all
// illiquid private-company stakes in one bucket, the security type staying
// queryable in the position payload. Fund LP interests are classified
// separately (private_fund), off the fund path.
func capTableAssetClass(hasEquity bool) canonical.AssetClass {
	if hasEquity {
		return canonical.AssetClassPrivateEquity
	}
	return canonical.AssetClassConvertibleNote
}
