package angellist

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"

// taxonomyForKind maps the silver offerings.kind (spv | fund) to the
// taxonomy pair: an SPV or RUV is an SPV, a fund the generic private fund
// (silver.PrivateFundTaxonomy).
var taxonomyForKind = silver.PrivateFundTaxonomy
