package equityzen

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"

// taxonomyForKind maps the silver offerings.kind to the taxonomy pair
// (silver.PrivateFundTaxonomy). load.py derives the kind from EquityZen's
// assetClass: ASSET_COMPANY is spv, ASSET_MULTI_COMPANY_FUND private_fund.
var taxonomyForKind = silver.PrivateFundTaxonomy
