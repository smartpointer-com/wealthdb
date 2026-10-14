package canonical

import (
	"sort"
	"strings"
)

// The label maps: how a provider's words for a sector, a country and an
// asset type map onto the exposure vocabularies (exposure.go). A label
// is matched lower-cased with its spaces collapsed. The generic maps
// hold the spellings issuers, filings and vendors share; a provider whose
// word means something else there gets a map of its own, which wins. A
// label no map knows stays unmapped, and the exposure report lists it.
//
// The maps name labels, never securities: no table of what a company
// is ships here.

// LabelField is what a label describes.
type LabelField string

const (
	LabelSector    LabelField = "sector"
	LabelCountry   LabelField = "country"
	LabelAssetType LabelField = "asset_type"
)

// Label is one map entry. Provider "" is the generic map.
type Label struct {
	Provider string
	Field    LabelField
	Label    string
	Value    string
}

// NormalizeLabel is the form a label is matched in: lower-cased, trimmed,
// inner whitespace collapsed to one space.
func NormalizeLabel(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

var genericSectors = map[Sector][]string{
	SectorEnergy: {"energy", "oil & gas", "oil and gas", "oil, gas and coal", "alternative energy"},
	SectorMaterials: {"materials", "basic materials", "material", "chemicals", "basic resources",
		"industrial metals and mining", "precious metals and mining", "industrial materials"},
	SectorIndustrials: {"industrials", "industrial", "industrial goods", "capital goods", "aerospace and defense",
		"construction and materials", "industrial support services", "industrial engineering",
		"general industrials", "industrial transportation", "electronic and electrical equipment",
		"industrial goods and services", "waste and disposal services"},
	SectorConsumerCyclical: {"consumer discretionary", "consumer cyclical", "consumer cyclicals", "cyclical consumer goods",
		"travel and leisure", "retailers", "retail", "consumer services", "household goods and home construction",
		"personal goods", "leisure goods", "automobiles and parts", "consumer products and services"},
	SectorConsumerStaples: {"consumer staples", "consumer defensive", "consumer non-cyclicals", "non-cyclical consumer goods",
		"tobacco", "beverages", "food producers", "personal care, drug and grocery stores",
		"food, beverage and tobacco"},
	SectorHealthCare: {"health care", "healthcare", "health", "pharmaceuticals and biotechnology",
		"medical equipment and services", "health care providers"},
	SectorFinancials: {"financials", "financial", "financial services", "finance", "banks", "insurance",
		"closed end investments", "life insurance", "non-life insurance", "investment banking and brokerage services",
		"finance and credit services", "mortgage real estate investment trusts"},
	SectorTechnology: {"information technology", "technology", "info tech", "it", "software and computer services",
		"technology hardware and equipment"},
	SectorCommunication: {"communication services", "communication", "communications", "telecommunication services",
		"telecommunications", "telecom", "media", "telecommunications service providers",
		"telecommunications equipment"},
	SectorUtilities: {"utilities", "utility", "electricity", "gas, water and multi-utilities"},
	SectorRealEstate: {"real estate", "reits", "reit", "property", "real estate investment trusts",
		"real estate investment and services"},
	SectorGovernment:  {"government", "treasury", "treasuries", "sovereign", "government related", "agency", "supranational"},
	SectorMunicipal:   {"municipal", "municipals", "munis"},
	SectorCorporate:   {"corporate", "corporates", "corporate bonds", "credit"},
	SectorSecuritised: {"securitized", "securitised", "mortgage backed", "mortgage-backed", "asset backed", "asset-backed", "mbs", "abs", "covered"},
	SectorCash:        {"cash", "cash and/or derivatives", "cash & equivalents", "cash and equivalents", "money market"},
}

// The asset types an issuer file or a filing labels a line with, and the
// asset class each counts in. N-PORT's assetCat codes are among them, and
// the sections a fund report lists its holdings under, in English and
// German. A derivative stays a leaf of its own kind: a currency forward
// counts in foreign_exchange, any other derivative in other.
var genericAssetTypes = map[AssetClass][]string{
	AssetClassPublicEquity: {"equity", "equities", "stock", "stocks", "common stock", "common", "preferred stock",
		"preferred", "ordinary shares", "ec", "ep", "adr", "gdr", "american depository receipt",
		"american depositary receipt", "global depositary receipt", "global depository receipt",
		"common stocks", "preferred stocks", "bearer shares", "registered shares", "participation certificates",
		"other shares", "stammaktien", "vorzugsaktien", "inhaberaktien", "namenaktien", "partizipationsscheine",
		"aktien und sonstige beteiligungswertpapiere und -rechte", "equity_ordinary", "equity_preferred"},
	AssetClassFixedIncome: {"fixed income", "bond", "bonds", "debt", "dbt", "fixed-income", "government bond",
		"corporate bond", "note", "notes", "abs-mbs", "abs-cbdo", "abs-o", "abs-app", "sn", "lon", "loan",
		"corporate bonds", "government bonds", "asset and mortgage backed"},
	AssetClassCash: {"cash", "cash and/or derivatives", "money market", "cash collateral and margins",
		"cash & equivalents", "cash equivalents", "stiv", "ra", "repo", "repurchase agreement", "deposit", "currency"},
	AssetClassRealEstate:      {"real estate", "re", "reit", "real estate investment trust"},
	AssetClassForeignExchange: {"fx", "forward", "forwards", "fx forward", "currency forward", "dfe", "foreign exchange"},
	AssetClassOther: {"futures", "future", "swap", "swaps", "option", "options", "derivative", "derivatives",
		"dco", "dcr", "de", "dir", "do", "comm", "commodity", "other", "rights", "warrants", "bezugsrechte",
		"optionsscheine"},
	// A fund line looks through when a composition describes it; one that
	// does not counts as multi asset.
	AssetClassMultiAsset: {"fund", "funds", "etf", "etfs", "investment company", "mutual fund", "investment funds",
		"investmentfonds"},
}

// Country spellings beside the canonical names and the codes themselves.
var countryAliases = map[string][]string{
	"US": {"united states of america", "usa", "u.s.", "u.s.a.", "us"},
	"GB": {"uk", "u.k.", "great britain", "britain", "england"},
	"KR": {"south korea", "korea (south)", "korea", "republic of korea", "korea, south"},
	"KP": {"north korea", "korea (north)"},
	"TW": {"taiwan, province of china", "chinese taipei"},
	"HK": {"hong kong sar", "hong kong sar china", "hong kong, china"},
	"MO": {"macau", "macao sar"},
	"RU": {"russia"},
	"GG": {"c.i. guernsey", "guernsey, c.i."},
	"JE": {"c.i. jersey", "jersey, c.i."},
	"CZ": {"czech republic"},
	"VN": {"vietnam"},
	"IR": {"iran, islamic republic of"},
	"VE": {"venezuela, bolivarian republic of"},
	"BO": {"bolivia, plurinational state of"},
	"TZ": {"tanzania, united republic of"},
	"SY": {"syria"},
	"LA": {"laos"},
	"MD": {"moldova, republic of"},
	"CD": {"congo (kinshasa)", "democratic republic of the congo", "dr congo"},
	"CG": {"congo (brazzaville)", "republic of the congo"},
	"CI": {"ivory coast", "côte d'ivoire"},
	"CW": {"curaçao"},
	"TR": {"türkiye", "turkiye"},
	"NL": {"the netherlands", "holland"},
	"AE": {"uae"},
	"MK": {"macedonia"},
	"SZ": {"swaziland"},
	"VG": {"british virgin islands"},
	"VI": {"us virgin islands", "u.s. virgin islands"},
	"EU": {"european union", "supranational", "euro area"},
}

// ExposureLabels is every map entry, in a stable order: the generic
// sectors, asset types and countries (each country's code, its name and
// its other spellings).
func ExposureLabels() []Label {
	var out []Label
	sectors := map[string]bool{}
	for s, labels := range genericSectors {
		for _, l := range labels {
			// Issuers write "and" and "&" alike.
			for _, v := range []string{l, strings.ReplaceAll(l, " and ", " & "), strings.ReplaceAll(l, " & ", " and ")} {
				if !sectors[v] {
					sectors[v] = true
					out = append(out, Label{Field: LabelSector, Label: v, Value: string(s)})
				}
			}
		}
	}
	for a, labels := range genericAssetTypes {
		for _, l := range labels {
			out = append(out, Label{Field: LabelAssetType, Label: l, Value: string(a)})
		}
	}
	seen := map[string]bool{}
	add := func(label, code string) {
		l := NormalizeLabel(label)
		if seen[l] {
			return
		}
		seen[l] = true
		out = append(out, Label{Field: LabelCountry, Label: l, Value: code})
	}
	for _, c := range Countries() {
		add(c.Code, c.Code)
		add(c.Name, c.Code)
	}
	for code, labels := range countryAliases {
		for _, l := range labels {
			add(l, code)
		}
	}
	out = append(out, edgarLabels()...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		return a.Label < b.Label
	})
	return out
}

// SICRange maps a range of SEC SIC codes onto a sector. The ranges are
// coarse on purpose: SIC is the fallback for an SEC filer no fund file
// and no vendor describes. A range with no sector holds codes that say
// nothing of the business, such as the code EDGAR files a depositary
// receipt's registrant under: a reader takes no sector from it.
type SICRange struct {
	Lo, Hi int
	Sector Sector
}

// SICRanges is the SIC map, the narrower ranges first so a reader takes
// the first range that holds a code. Every code a company files under
// maps somewhere.
var SICRanges = []SICRange{
	{8880, 8888, ""},               // depositary receipts, foreign governments
	{9995, 9995, SectorFinancials}, // non-operating shells and blank checks
	{2830, 2836, SectorHealthCare}, // pharmaceuticals
	{3570, 3579, SectorTechnology}, // computers
	{3630, 3639, SectorConsumerCyclical},
	{3710, 3716, SectorConsumerCyclical}, // motor vehicles
	{3840, 3851, SectorHealthCare},       // medical instruments
	{5400, 5499, SectorConsumerStaples},  // food stores
	{6798, 6798, SectorRealEstate},       // REITs
	{7370, 7379, SectorTechnology},       // software and data services
	{100, 999, SectorConsumerStaples},    // agriculture
	{1000, 1099, SectorMaterials},        // metal mining
	{1200, 1399, SectorEnergy},           // coal, oil and gas
	{1400, 1499, SectorMaterials},
	{1500, 1799, SectorIndustrials}, // construction
	{2000, 2199, SectorConsumerStaples},
	{2200, 2399, SectorConsumerCyclical},
	{2400, 2499, SectorMaterials},
	{2500, 2599, SectorConsumerCyclical},
	{2600, 2699, SectorMaterials},
	{2700, 2799, SectorCommunication}, // publishing
	{2800, 2899, SectorMaterials},     // chemicals
	{2900, 2999, SectorEnergy},        // refining
	{3000, 3399, SectorMaterials},
	{3400, 3599, SectorIndustrials},
	{3600, 3699, SectorTechnology},
	{3700, 3799, SectorIndustrials},
	{3800, 3899, SectorTechnology},
	{3900, 3999, SectorConsumerCyclical},
	{4000, 4799, SectorIndustrials}, // transportation
	{4800, 4899, SectorCommunication},
	{4900, 4999, SectorUtilities},
	{5000, 5199, SectorIndustrials}, // wholesale
	{5200, 5999, SectorConsumerCyclical},
	{6000, 6499, SectorFinancials},
	{6500, 6599, SectorRealEstate},
	{6700, 6799, SectorFinancials},
	{7000, 7299, SectorConsumerCyclical},
	{7300, 7399, SectorIndustrials},
	{7500, 7599, SectorConsumerCyclical}, // automotive repair and rental
	{7600, 7699, SectorIndustrials},      // miscellaneous repair
	{7800, 7899, SectorCommunication},
	{7900, 7999, SectorConsumerCyclical},
	{8000, 8099, SectorHealthCare},
	{8100, 8999, SectorIndustrials},
}
