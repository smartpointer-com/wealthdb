package ubs

import (
	"log"
	"regexp"
	"strings"
	"sync"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// assetClassForInstrument classifies a PSN instrument using its
// CFI code first (ISO 10962 — the primary signal UBS surfaces for
// listed securities) and falling back to UBS's internal
// `UacAsstClsCd` when CFI is empty. CFI is empty for the
// non-listed instruments UBS holds in custody (e.g. metal-deposit receipts, private-market fund interests),
// and those rows still carry a populated UacAsstClsCd because the
// bank uses it for internal asset-allocation reporting.
//
// CFI takes precedence even when its first character is one we
// don't recognise — a known CFI is the strongest classification
// signal we have. Only fully-empty CFI falls through to UAC.
//
// ETFs (CFI group `CE`) are further refined by their underlying
// exposure from the instrument name — crypto / metal / bond ETFs
// leave the `etf` bucket per the canonical taxonomy (see
// silver.RefineETFClass).
//
// A fund's UAC code sharpens the CFI rather than merely
// backstopping it: the CFI only says "standard investment fund"
// (`CI…`) for a money-market SICAV and for a semi-liquid
// private-markets feeder alike, but UBS's own asset-allocation
// bucket pins the exposure — 0100 (Liquidity) is a money-market
// fund, 0400 (Hedge funds & private markets) is a private-market
// vehicle (private-equity, hedge-fund, or infrastructure).
func assetClassForInstrument(cfi, uacAsstClsCd, name string) canonical.AssetClass {
	var ac canonical.AssetClass
	if cfi != "" {
		ac = assetClassForCFI(cfi)
	} else {
		ac = assetClassForUacAsstCls(uacAsstClsCd)
	}
	switch {
	case ac == canonical.AssetClassETF:
		ac = silver.RefineETFClass(name)
	case ac == canonical.AssetClassFund && uacAsstClsCd == "0100":
		ac = canonical.AssetClassMoneyMarket
	case ac == canonical.AssetClassFund && uacAsstClsCd == "0400":
		ac = canonical.AssetClassPrivateFund
	}
	return ac
}

// assetClassForCFI maps an ISO 10962 CFI code's first character
// to a canonical AssetClass. The first character of the CFI code
// designates the asset category:
//
//	E - Equities                → equity
//	C - Collective investment   → fund (CE group → etf, see below)
//	D - Debt instruments        → bond
//	O - Options                 → option
//	F - Futures                 → future
//	M - Others (mostly MM)      → money_market
//	T - Structured / cash collateral → other
//	R - Entitlements (rights)   → other
//
// Within category C the second character (the CFI group) singles
// out exchange-traded funds: `CE` is the ISO 10962:2015 ETF group,
// while `CI` is the standard (vanilla) investment-fund group. Only
// `CE` maps to `etf`; every other C group (incl. hedge funds `CH`,
// REITs `CB`, funds-of-funds `CF`) stays in the coarse `fund`
// bucket until a finer canonical class is warranted.
//
// Empty CFI is *not* this function's concern — callers should go
// through assetClassForInstrument, which routes empty CFI to the
// UAC fallback.
func assetClassForCFI(cfi string) canonical.AssetClass {
	if cfi == "" {
		return canonical.AssetClassOther
	}
	switch cfi[0] {
	case 'E':
		return canonical.AssetClassEquity
	case 'C':
		if len(cfi) >= 2 && cfi[1] == 'E' {
			return canonical.AssetClassETF
		}
		return canonical.AssetClassFund
	case 'D':
		return canonical.AssetClassBond
	case 'O':
		return canonical.AssetClassOption
	case 'F':
		return canonical.AssetClassFuture
	case 'M':
		return canonical.AssetClassMoneyMarket
	default:
		// 'T' structured, 'R' rights, 'S' spot/forward FX (rare in
		// instruments), anything new.
		return canonical.AssetClassOther
	}
}

// assetClassForUacAsstCls maps UBS's internal `UacAsstClsCd`
// asset-class code to canonical AssetClass. Used as the fallback
// when CFI is empty.
//
// The UAC taxonomy is coarser than CFI:
//
//	0100 - Liquidity                       → money_market
//	0300 - Equities                        → equity
//	0400 - Hedge funds & private markets   → private_fund
//	0600 - Precious metals & commodities   → metal
//	0700 - Others                          → other
//
// The HF&PM bucket conflates hedge funds and private-market LP
// interests. The canonical taxonomy currently has no dedicated
// `hedge_fund` value, so both land in `private_fund` until the
// taxonomy grows a separate one.
func assetClassForUacAsstCls(code string) canonical.AssetClass {
	switch code {
	case "0100":
		return canonical.AssetClassMoneyMarket
	case "0300":
		return canonical.AssetClassEquity
	case "0400":
		return canonical.AssetClassPrivateFund
	case "0600":
		return canonical.AssetClassMetal
	}
	return canonical.AssetClassOther
}

// ---- 2-D taxonomy (asset_class × vehicle) ---------------------------------
//
// The functions below derive the two-dimensional taxonomy pair
// (exposure + wrapper, TAXONOMY.md) that runs BESIDE the legacy 1-D
// asset_class above during the migration. The legacy derivation is a
// control and stays untouched; this is a second, independent
// derivation the adapter double-writes (see docs/TAXONOMY-PLAN.md).

// taxPair is a 2-D taxonomy pair. It travels through the adapter
// alongside the legacy 1-D class (e.g. inside instrumentMeta) so the
// web overlay can stamp web-emitted instruments with the pair PSN
// would derive, exactly as it already does for the legacy class.
type taxPair struct {
	AssetClass canonical.AssetClass
	Vehicle    canonical.Vehicle
}

// currencyLinkedRe matches a security name that reads as a currency-
// or FX-linked product. UBS "T" (structured) CFI codes cover both
// equity-linked notes and dual-currency / currency-linked notes; the
// name is the only signal separating them. Word boundaries keep "FX"
// from matching inside a longer token.
var currencyLinkedRe = regexp.MustCompile(`(?i)\bcurrenc(?:y|ies)\b|\bFX\b|\bforex\b|foreign exchange|dual currency`)

// taxonomyPairForInstrument is the 2-D-taxonomy counterpart of
// assetClassForInstrument (its legacy 1-D sibling): it derives the
// (exposure, vehicle) pair for a PSN instrument from the same signals
// — CFI first character drives the vehicle, CFI/UAC drive the exposure
// — per TAXONOMY.md §6.
//
// CFI first character → vehicle (with its default exposure):
//
//	E → stock,   public_equity
//	C → etf (CE group) / fund (other C groups); exposure from the
//	    security name (silver.RefineETFExposure), sharpened by UAC
//	D → bond,    fixed_income
//	O → option,  public_equity (equity-option underlying)
//	F → future,  public_equity
//	R → right,   public_equity (subscription right)
//	T / other → structured_product, public_equity (equity-linked) —
//	    or foreign_exchange when the name reads currency-/FX-linked
//
// Empty CFI routes to the UAC fallback (custody items UBS surfaces
// with no CFI: money-market placements, direct equity, private-market
// LP interests, gold bars).
func taxonomyPairForInstrument(cfi, uacAsstClsCd, name string) (canonical.AssetClass, canonical.Vehicle) {
	if cfi == "" {
		return taxonomyPairForUAC(uacAsstClsCd, name)
	}
	switch cfi[0] {
	case 'E':
		return canonical.AssetClassPublicEquity, canonical.VehicleStock
	case 'C':
		// Collective vehicle: CE is the ISO 10962 ETF group; every
		// other C group is a (non-exchange-traded) fund.
		vehicle := canonical.VehicleFund
		if len(cfi) >= 2 && cfi[1] == 'E' {
			vehicle = canonical.VehicleETF
		}
		// UAC sharpens the exposure the CFI can't see: 0100 is a
		// money-market (cash) fund, 0400 a private-market vehicle.
		switch uacAsstClsCd {
		case "0100":
			return canonical.AssetClassCash, canonical.VehicleFund
		case "0400":
			return privateMarketsPair(name)
		}
		return silver.RefineETFExposure(name), vehicle
	case 'D':
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case 'O':
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	case 'F':
		return canonical.AssetClassPublicEquity, canonical.VehicleFuture
	case 'R':
		return canonical.AssetClassPublicEquity, canonical.VehicleRight
	default:
		// 'T' structured products and any unrecognised CFI: a
		// structured note. Currency-linked notes carry FX exposure;
		// everything else is equity-linked by default.
		if currencyLinkedRe.MatchString(name) {
			return canonical.AssetClassForeignExchange, canonical.VehicleStructuredProduct
		}
		return canonical.AssetClassPublicEquity, canonical.VehicleStructuredProduct
	}
}

// taxonomyPairForUAC maps UBS's internal UacAsstClsCd to a
// (exposure, vehicle) pair for custody items UBS surfaces with an
// empty CFI. Mirrors assetClassForUacAsstCls's coverage:
//
//	0100 → cash × fund           (money-market placement; the "or
//	       time_deposit" wrapper in TAXONOMY.md §4 belongs to the
//	       separate money-market-contract path, not custody items)
//	0300 → public_equity × stock (direct equity)
//	0400 → private-markets family (split by name; privateMarketsPair)
//	0600 → metal × physical      (vaulted gold bars)
//	else → other × other
func taxonomyPairForUAC(uacAsstClsCd, name string) (canonical.AssetClass, canonical.Vehicle) {
	switch uacAsstClsCd {
	case "0100":
		return canonical.AssetClassCash, canonical.VehicleFund
	case "0300":
		return canonical.AssetClassPublicEquity, canonical.VehicleStock
	case "0400":
		return privateMarketsPair(name)
	case "0600":
		return canonical.AssetClassMetal, canonical.VehiclePhysical
	}
	return canonical.AssetClassOther, canonical.VehicleOther
}

// privateMarketsPair splits UBS's conflated "hedge funds & private
// markets" UAC bucket (0400) into the three canonical exposures the
// taxonomy separates, keyed on the security name (the UAC code alone
// doesn't distinguish them). All three are held via the fund wrapper.
func privateMarketsPair(name string) (canonical.AssetClass, canonical.Vehicle) {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "infrastructure"):
		return canonical.AssetClassInfrastructure, canonical.VehicleFund
	case strings.Contains(lower, "hedge"):
		return canonical.AssetClassHedgeFund, canonical.VehicleFund
	}
	return canonical.AssetClassPrivateEquity, canonical.VehicleFund
}

// taxWrapperForCashAcctTp / taxWrapperForSafekeepingAcctTp map a
// PSN account's `AcctTpCd` (the UBS product code embedded in the
// SDCA payload) to the canonical TaxWrapper enum. Per the
// ubs-psn DESIGN.md §1 contract:
//
//   - Known codes map explicitly (currently all → taxable_personal;
//     every observed PSN code so far is a private-banking
//     product).
//   - Unknown codes return "" so the caller leaves TaxWrapper nil,
//     AND log a one-shot WARN to stderr so the operator notices.
//     This is the alarm bell for "PSN started surfacing a code we
//     haven't classified" — the primary case being a pension
//     product if PSN's coverage ever widens beyond pure private
//     banking.
//   - Empty code (silver payload missing AcctTpCd) is silent.
//
// The tables are deliberately not "default everything to
// taxable_personal" — overreaching that way is what the
// ubs-psn maintainer corrected on 2026-05-24, because the
// current customer happens to hold no Swiss pension assets at
// UBS at all, so the absence of pension codes in the silver
// proves nothing about whether PSN would surface them if they
// existed.

var knownCashAcctTpCd = map[string]canonical.TaxWrapper{
	"OA155": canonical.TaxWrapperTaxablePersonal, // UBS current account for private clients
	"OA157": canonical.TaxWrapperTaxablePersonal, // UBS current account for private clients (variant)
	"OA159": canonical.TaxWrapperTaxablePersonal, // Cash Account for investment solutions (mandate cash)
	"OA197": canonical.TaxWrapperTaxablePersonal, // Limit Account Lombard (credit line)
	"OA350": canonical.TaxWrapperTaxablePersonal, // UBS personal account
	"OA352": canonical.TaxWrapperTaxablePersonal, // UBS savings account
	"OA519": canonical.TaxWrapperTaxablePersonal, // Forward Contract Account AG. For Curr.
}

var knownSafekeepingAcctTpCd = map[string]canonical.TaxWrapper{
	"OA601": canonical.TaxWrapperTaxablePersonal, // UBS Custody Account
	"OA609": canonical.TaxWrapperTaxablePersonal, // Custody for investment solutions
	"OA621": canonical.TaxWrapperTaxablePersonal, // Custody account (non-securities)
}

// unknownAcctTpCdSeen dedups the WARN log so each unique
// (side, code) pair only logs once per process — reloading 30
// snapshots that all share an unknown code produces one WARN,
// not 30.
var unknownAcctTpCdSeen sync.Map

func taxWrapperForCashAcctTp(code, desc string) canonical.TaxWrapper {
	if w, ok := knownCashAcctTpCd[code]; ok {
		return w
	}
	if code == "" {
		return ""
	}
	if _, seen := unknownAcctTpCdSeen.LoadOrStore("cash:"+code, true); !seen {
		log.Printf("warn: ubs adapter: unknown cash AcctTpCd %q (AcctTpDesc=%q); leaving tax_wrapper unset. If this is a pension-shaped account, extend internal/silver/ubs/classmap.go.", code, desc)
	}
	return ""
}

func taxWrapperForSafekeepingAcctTp(code, desc string) canonical.TaxWrapper {
	if w, ok := knownSafekeepingAcctTpCd[code]; ok {
		return w
	}
	if code == "" {
		return ""
	}
	if _, seen := unknownAcctTpCdSeen.LoadOrStore("safekeeping:"+code, true); !seen {
		log.Printf("warn: ubs adapter: unknown safekeeping AcctTpCd %q (AcctTpDesc=%q); leaving tax_wrapper unset. If this is a pension-shaped account, extend internal/silver/ubs/classmap.go.", code, desc)
	}
	return ""
}

// managementStyleForSafekeepingSubType maps the silver-side
// `safekeeping_accounts.payload.AcctSubTypeDesc` string to the
// canonical ManagementStyle. UBS's safekeeping sub-type tags the
// mandate type directly:
//
//   "managed securities account"              → discretionary
//                                               (UBS Vermögens-
//                                                verwaltung — the
//                                                bank places trades
//                                                under limited POA)
//   "securities account with dvisory agreement" → advisory
//                                                 (UBS Anlage-
//                                                  beratung; note
//                                                  the missing 'a'
//                                                  is the actual
//                                                  string in UBS's
//                                                  payload)
//   "securities account with advisory agreement" → advisory
//                                                  (if UBS ever
//                                                   fixes the typo)
//
// Unknown sub-types return "" so the caller leaves
// ManagementStyle nil; the render-time default surfaces
// self_directed. One-shot WARN per unique unknown string, same
// dedup mechanism as the AcctTpCd switch. Empty sub-type
// (cash-only safekeeping accounts that have no mandate
// designation) is silent.
//
// Cash accounts have no AcctSubTypeDesc — they're transactional
// accounts independent of any investment mandate — so the cash
// side legitimately leaves management_style nil, and the portfolio
// rollup picks up the discretionary / advisory tag from the
// safekeeping account in the same portfolio.
// propagateManagementStyleByPortfolio walks the per-snapshot
// AccountChange slice to lift the mandate type the safekeeping
// account carries onto its sibling cash / overlay accounts in
// the same portfolio — but only when the portfolio is a *named
// mandate*, gated by the `mandatePortfolios` set (built in
// appendSafekeepingAccounts from rows with a non-empty
// AcctDesc).
//
// Why the gate. UBS labels Vermögensverwaltung / Anlageberatung
// on every safekeeping via AcctSubTypeDesc, but the
// *portfolio* can still be a general-banking package even when
// its (residual) safekeeping technically has an advisory tag.
// Concretely: a "private banking" portfolio that holds personal
// chequing / savings / current accounts plus a residual
// advisory securities position carries advisory-tagged
// safekeeping rows (AcctSubTypeDesc = "securities account with
// dvisory agreement"), but the cash accounts in the portfolio
// are personal banking that the customer manages directly —
// not part of any investment mandate. Those safekeeping rows
// have an EMPTY AcctDesc (no strategy name like "EMERGING
// MARKETS ASIA" / "PRIVATE MARKETS"). Named-mandate
// safekeeping accounts always carry an AcctDesc; their cash
// siblings ARE part of the mandate (typically labelled in
// silver as "Cash Account for investment solutions"), and
// inheriting the safekeeping's style is correct. Staging cash
// in a named-mandate portfolio (e.g. USD pre-positioned to
// fund a Private Equity capital call) is also part of the
// mandate and inherits — the customer doesn't direct that cash
// independently; they fund the mandate, the mandate deploys.
//
// Conflict handling: if two safekeeping accounts share a
// portfolio and disagree on style (shouldn't happen in real
// UBS data — one mandate per portfolio — but defensive), the
// first one seen wins; the propagation only fills nils, never
// overrides.
func propagateManagementStyleByPortfolio(accounts []canonical.AccountChange, mandatePortfolios map[string]bool) {
	if len(mandatePortfolios) == 0 {
		return
	}
	styleByPortfolio := make(map[string]canonical.ManagementStyle)
	for i := range accounts {
		a := &accounts[i]
		if a.AccountKind != canonical.AccountKindSafekeeping {
			continue
		}
		if a.ManagementStyle == nil || a.PortfolioExternalID == nil {
			continue
		}
		if !mandatePortfolios[*a.PortfolioExternalID] {
			continue
		}
		if _, set := styleByPortfolio[*a.PortfolioExternalID]; !set {
			styleByPortfolio[*a.PortfolioExternalID] = *a.ManagementStyle
		}
	}
	if len(styleByPortfolio) == 0 {
		return
	}
	for i := range accounts {
		a := &accounts[i]
		if a.ManagementStyle != nil || a.PortfolioExternalID == nil {
			continue
		}
		if s, ok := styleByPortfolio[*a.PortfolioExternalID]; ok {
			s := s
			a.ManagementStyle = &s
		}
	}
}

func managementStyleForSafekeepingSubType(subType string) canonical.ManagementStyle {
	switch subType {
	case "managed securities account":
		return canonical.ManagementStyleDiscretionary
	case "securities account with dvisory agreement",
		"securities account with advisory agreement":
		return canonical.ManagementStyleAdvisory
	}
	if subType == "" {
		return ""
	}
	if _, seen := unknownAcctTpCdSeen.LoadOrStore("safekeeping-subtype:"+subType, true); !seen {
		log.Printf("warn: ubs adapter: unknown safekeeping AcctSubTypeDesc %q; leaving management_style unset. If this names a mandate type (managed / advisory / etc.), extend internal/silver/ubs/classmap.go.", subType)
	}
	return ""
}
