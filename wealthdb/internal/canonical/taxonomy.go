package canonical

import "sort"

// This file carries the two-dimensional instrument taxonomy:
// exposure (the asset class — what moves the value) and vehicle (the
// wrapper — how the exposure is held). See docs/TAXONOMY.md for
// definitions. The exposure dimension reuses the AssetClass type
// declared in enums.go; the values enumerated here are the ones that
// reach gold's `asset_class` column, and Valid gates them. Vehicle
// (below) is the wrapper dimension.

// Asset-class (exposure) values written to gold. Some strings are
// shared with the adapters' intermediate 1-D vocabulary in enums.go
// (PrivateEquity, RealEstate, Metal, Crypto, Other) and keep the same
// meaning in both roles; the rest are exposure-only.
const (
	AssetClassPublicEquity    AssetClass = "public_equity"
	AssetClassFixedIncome     AssetClass = "fixed_income"
	AssetClassPrivateDebt     AssetClass = "private_debt"
	AssetClassInfrastructure  AssetClass = "infrastructure"
	AssetClassCash            AssetClass = "cash"
	AssetClassForeignExchange AssetClass = "foreign_exchange"
	AssetClassHedgeFund       AssetClass = "hedge_fund"
	AssetClassMultiAsset      AssetClass = "multi_asset"
	// AssetClassPrivateEquity, AssetClassRealEstate, AssetClassMetal,
	// AssetClassCrypto and AssetClassOther are declared in enums.go and
	// reused verbatim here.
)

// assetClassValues is the exposure dimension of the taxonomy —
// TAXONOMY.md §2. The adapters' intermediate-only labels (equity,
// etf, bond_etf, fund, bond, option, future, fx_forward, fx_option,
// money_market, otc_derivative, spv, private_fund, convertible_note,
// mortgage) are deliberately absent: they are wrappers or
// wrapper+exposure blends, not pure exposures, and reach gold as
// (asset_class, vehicle) pairs.
var assetClassValues = map[AssetClass]struct{}{
	AssetClassPublicEquity: {}, AssetClassPrivateEquity: {},
	AssetClassFixedIncome: {}, AssetClassPrivateDebt: {},
	AssetClassRealEstate: {}, AssetClassInfrastructure: {},
	AssetClassMetal: {}, AssetClassCrypto: {}, AssetClassCash: {},
	AssetClassForeignExchange: {}, AssetClassHedgeFund: {},
	AssetClassMultiAsset: {}, AssetClassOther: {},
}

// AssetClasses returns the exposure dimension — every asset class that
// reaches gold — sorted, so a caller walking it is byte-stable across
// runs. A copy, for the reason the taxonomy accessors hand one out.
func AssetClasses() []AssetClass {
	out := make([]AssetClass, 0, len(assetClassValues))
	for a := range assetClassValues {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Valid reports whether the receiver is a recognised exposure value.
// The gold writer and config validation call it before an
// asset_class reaches gold. The adapters' intermediate 1-D labels
// (enums.go) are not exposures and are not accepted here.
func (a AssetClass) Valid() bool {
	_, ok := assetClassValues[a]
	return ok
}

// Vehicle is the wrapper dimension of the 2-D taxonomy — how an
// exposure is held (TAXONOMY.md §3). Orthogonal to AssetClass:
// a stock and an ETF can both be public_equity; a fund can hold
// any exposure.
type Vehicle string

const (
	VehicleStock             Vehicle = "stock"
	VehicleETF               Vehicle = "etf"
	VehicleFund              Vehicle = "fund"
	VehicleSPV               Vehicle = "spv"
	VehicleBond              Vehicle = "bond"
	VehicleConvertibleNote   Vehicle = "convertible_note"
	VehicleLoan              Vehicle = "loan"
	VehicleOption            Vehicle = "option"
	VehicleFuture            Vehicle = "future"
	VehicleForward           Vehicle = "forward"
	VehicleTimeDeposit       Vehicle = "time_deposit"   // term-locked cash placement
	VehicleDemandDeposit     Vehicle = "demand_deposit" // at-sight account cash
	VehiclePhysical          Vehicle = "physical"       // bullion, wallet coins, direct property
	VehicleStructuredProduct Vehicle = "structured_product"
	VehicleRight             Vehicle = "right" // subscription / entitlement right
	VehicleMortgage          Vehicle = "mortgage"
	VehicleEscrow            Vehicle = "escrow"
	VehicleOther             Vehicle = "other"
)

var vehicleValues = map[Vehicle]struct{}{
	VehicleStock: {}, VehicleETF: {}, VehicleFund: {}, VehicleSPV: {},
	VehicleBond: {}, VehicleConvertibleNote: {}, VehicleLoan: {},
	VehicleOption: {}, VehicleFuture: {}, VehicleForward: {},
	VehicleTimeDeposit: {}, VehicleDemandDeposit: {}, VehiclePhysical: {},
	VehicleStructuredProduct: {}, VehicleRight: {}, VehicleMortgage: {},
	VehicleEscrow: {}, VehicleOther: {},
}

// Valid reports whether the receiver is a recognised Vehicle value.
func (v Vehicle) Valid() bool {
	_, ok := vehicleValues[v]
	return ok
}

// TaxonomyPair is a validated (exposure, vehicle) combination. Not
// every cross-product is meaningful; ValidTaxonomyPair enumerates the
// combinations the adapters may emit, and the verification harness
// rejects anything outside it (a novel pair must be added here first,
// with a taxonomy-doc update). Keyed by exposure → set of vehicles.
var validTaxonomyPairs = map[AssetClass]map[Vehicle]struct{}{
	AssetClassPublicEquity: setOf(VehicleStock, VehicleETF, VehicleFund,
		VehicleOption, VehicleFuture, VehicleRight, VehicleStructuredProduct),
	AssetClassPrivateEquity: setOf(VehicleStock, VehicleSPV, VehicleFund,
		VehicleOption, VehicleETF, VehicleConvertibleNote),
	AssetClassFixedIncome: setOf(VehicleBond, VehicleETF, VehicleFund,
		VehicleStructuredProduct),
	AssetClassPrivateDebt:     setOf(VehicleConvertibleNote, VehicleLoan, VehicleEscrow),
	AssetClassRealEstate:      setOf(VehiclePhysical, VehicleFund, VehicleETF, VehicleMortgage),
	AssetClassInfrastructure:  setOf(VehicleFund, VehicleETF),
	AssetClassMetal:           setOf(VehiclePhysical, VehicleETF, VehicleFund),
	AssetClassCrypto:          setOf(VehiclePhysical, VehicleETF, VehicleFund),
	AssetClassCash:            setOf(VehicleDemandDeposit, VehicleTimeDeposit, VehicleFund),
	AssetClassForeignExchange: setOf(VehicleForward, VehicleOption, VehicleStructuredProduct),
	AssetClassHedgeFund:       setOf(VehicleFund),
	AssetClassMultiAsset:      setOf(VehicleFund, VehicleETF),
	AssetClassOther:           setOf(VehicleOther, VehicleStructuredProduct),
}

func setOf(vs ...Vehicle) map[Vehicle]struct{} {
	m := make(map[Vehicle]struct{}, len(vs))
	for _, v := range vs {
		m[v] = struct{}{}
	}
	return m
}

// ValidTaxonomyPair reports whether (exposure, vehicle) is a
// combination the taxonomy admits. Used by the writer and the
// verification harness. An unmeaningful pair (e.g. crypto × mortgage)
// is rejected even though both dimensions are individually valid.
func ValidTaxonomyPair(a AssetClass, v Vehicle) bool {
	vs, ok := validTaxonomyPairs[a]
	if !ok {
		return false
	}
	_, ok = vs[v]
	return ok
}
