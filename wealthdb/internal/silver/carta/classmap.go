package carta

import "github.com/ptu/wealthdb/internal/canonical"

// assetClassFor maps a carta entity to its canonical AssetClass.
//
// isFund discriminates the two holder families:
//   - fund LP interest          → AssetClassPrivateFund
//   - cap-table private equity  → AssetClassPrivateEquity
//
// Every cap-table security type (share, option, rsu, rsa, sar, piu,
// equity_grant, warrant, convertible) folds into the single
// private_equity bucket: they're all illiquid private-company
// stakes, and separating, say, a private ESO from a private share
// into the public-market `option` / `equity` classes would conflate
// them with listed instruments in portfolio queries. The security
// type stays queryable in the position payload.
func assetClassFor(isFund bool) canonical.AssetClass {
	if isFund {
		return canonical.AssetClassPrivateFund
	}
	return canonical.AssetClassPrivateEquity
}
