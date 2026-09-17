package ubs

import (
	"log"
	"regexp"
	"strings"
	"sync"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// ---- 2-D taxonomy (asset_class × vehicle) ---------------------------------
//
// The functions below derive the two-dimensional taxonomy pair
// (exposure + wrapper, TAXONOMY.md) emitted to gold from UBS's PSN
// instrument signals — the ISO 10962 CFI code first, its internal
// UacAsstClsCd when CFI is empty.

// taxPair is a 2-D taxonomy pair. Its components travel through the
// adapter inside instrumentMeta so the web overlay can stamp web-
// emitted instruments with the (asset_class, vehicle) pair PSN would
// derive, keeping web and PSN instruments on the same taxonomy.
type taxPair struct {
	AssetClass canonical.AssetClass
	Vehicle    canonical.Vehicle
}

// currencyLinkedRe matches a security name that reads as a currency-
// or FX-linked product. A structured participation certificate (CFI
// group EY) can track an equity basket or an FX / dual-currency
// payoff; the name is the only signal separating them. Word
// boundaries keep "FX" from matching inside a longer token.
var currencyLinkedRe = regexp.MustCompile(`(?i)\bcurrenc(?:y|ies)\b|\bFX\b|\bforex\b|foreign exchange|dual currency`)

// taxonomyPairForInstrument derives the (exposure, vehicle) pair a PSN
// instrument reaches gold with, from its ISO 10962 CFI code (the
// primary signal) and UBS's internal UacAsstClsCd (the fallback when
// CFI is empty). It is the only instrument classifier the UBS adapter
// emits — TAXONOMY.md §6.
//
// The CFI first character is the ISO 10962 category; each maps to a
// vehicle and a default exposure, a few sharpened by the CFI group
// (second character), the UAC code, or the security name:
//
//	E  Equities              → stock, public_equity. Group EY
//	                           ("structured participation instruments"
//	                           — tracker / AMC certificates) is a
//	                           structured_product instead, with FX
//	                           exposure when the name reads currency-
//	                           /FX-linked.
//	C  Collective investment → etf (group CE) / fund (other groups);
//	                           exposure from the name (RefineETFExposure),
//	                           sharpened by UAC (0100 cash, 0400 private).
//	D  Debt                  → bond, fixed_income.
//	R  Entitlements (rights) → right, public_equity.
//	O  Listed / H non-listed
//	   & complex options     → option, public_equity.
//	F  Futures               → future, public_equity.
//	J  Forwards              → forward, foreign_exchange (the forward
//	                           wrapper only pairs with FX in the
//	                           taxonomy, and these feeds carry FX
//	                           forwards).
//	S swaps · I spot · K strategies · L financing · T referential
//	  (currencies / indices / rates — reference data, not a position) ·
//	  M others · and any unrecognised or future ISO category
//	                         → other, other. wealthdb does not model
//	                           these as custody holdings; an honest
//	                           "other" beats forcing an equity or
//	                           structured-product guess that would
//	                           misstate exposure.
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
		// Equities. CFI group `EY` is ISO 10962 "structured
		// participation instruments" — tracker / actively-managed
		// certificates (AMCs): equity participation held as a
		// structured product, not a share. A currency-/FX-linked one
		// carries FX exposure. Every other E group (ES shares, ED
		// depository receipts, …) is a stock.
		if len(cfi) >= 2 && cfi[1] == 'Y' {
			if currencyLinkedRe.MatchString(name) {
				return canonical.AssetClassForeignExchange, canonical.VehicleStructuredProduct
			}
			return canonical.AssetClassPublicEquity, canonical.VehicleStructuredProduct
		}
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
	case 'R':
		return canonical.AssetClassPublicEquity, canonical.VehicleRight
	case 'O', 'H':
		// Listed (O) and non-listed / complex (H) options.
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	case 'F':
		return canonical.AssetClassPublicEquity, canonical.VehicleFuture
	case 'J':
		// Forwards — FX forwards in practice; the forward wrapper
		// pairs only with foreign_exchange in the taxonomy.
		return canonical.AssetClassForeignExchange, canonical.VehicleForward
	default:
		// S swaps, I spot, K strategies, L financing, T referential
		// (currencies / indices / rates — reference data, not a
		// holding), M others, and any unrecognised or future ISO
		// category. Not modelled as custody holdings: classify as
		// other rather than a misleading equity / structured-product
		// guess.
		return canonical.AssetClassOther, canonical.VehicleOther
	}
}

// The security-description templates taxonomyPairForWebDescription
// matches. UBS's web exports and Statement-of-Assets PDFs generate
// descriptions from a fixed per-instrument-type vocabulary, so the
// leading template words are a reliable classification signal —
// strict prefixes first (immune to a token like "Private Equity"
// appearing inside a share's company name), fuzzy tokens after.
var (
	// "Reg.shs Example AG", "Reg. shs", "Reg shs", "Shs -A- …",
	// "Shs nom.", "shs …" — ordinary/registered shares.
	webSharesRe = regexp.MustCompile(`(?i)^(reg\.?\s*)?shs\b`)
	// "Sponsored American Deposit Receipt …", "Sponsrd American
	// Depositary Receipt …", "Non-Voting Depository Receipt …",
	// "Sponsored Global Deposit Receipt …" — DRs are shares.
	webDepositReceiptRe = regexp.MustCompile(`(?i)deposit(a|o)ry receipt|deposit receipt`)
	// "Part. Cert. …", "Participation Cert …", "Dividend-right
	// certificate" — Swiss Partizipationsscheine / Genussscheine:
	// non-voting corporate equity (PSN codes the same securities
	// CFI ES…), not issuer-wrapped structured products.
	webPartCertRe = regexp.MustCompile(`(?i)^(part\.?\s*cert|participation cert|dividend-right certificate)`)
	// "SSgA SPDR ETFs Europe I Plc", "UBS (Irl) ETF plc" — plus the
	// ETF umbrellas whose names lack the token: "iShares III Plc",
	// "Xtrackers (IE) Plc", "Invesco Markets III Plc".
	webETFRe = regexp.MustCompile(`(?i)\bETFs?\b|^(ishares|xtrackers|invesco markets)\b`)
	// "… - Multi-Vintage …" — UBS's multi-vintage private-markets
	// program families.
	webMultiVintageRe = regexp.MustCompile(`(?i)\bmulti-vintage\b`)
	// "… Sicav - …", "UBS (Lux) Fund Solutions - …" — pooled funds.
	webFundRe = regexp.MustCompile(`(?i)\bsicav\b|\bfund\b`)
	// Exposure sharpeners inside the fund branch.
	webInfraRe = regexp.MustCompile(`(?i)\binfrastructure\b`)
	webPERe    = regexp.MustCompile(`(?i)\bprivate equity\b`)
	// "Precious metals & commodities" (the overview-derived
	// per-portfolio line), "Gold bar(s) fine weight …".
	webMetalRe = regexp.MustCompile(`(?i)^(precious metals|gold bar)`)
	webAMCRe   = regexp.MustCompile(`(?i)^actively managed certificate`)
	webMMRe    = regexp.MustCompile(`(?i)\bmoney market\b`)
)

// taxonomyPairForWebDescription derives the (exposure, vehicle) pair
// from a ubs-web security description, for instruments PSN has no
// CFI/UAC for — historical Statement-of-Assets securities sold before
// the PSN feed began, and web-only holdings. Returns ok=false when no
// template matches; callers keep (other, other) then rather than
// guessing. When PSN metadata exists for the ISIN it always wins —
// this fallback only fires for PSN-unknown instruments, so the two
// classifiers cannot disagree on the same gold row.
func taxonomyPairForWebDescription(desc string) (canonical.AssetClass, canonical.Vehicle, bool) {
	switch {
	case webAMCRe.MatchString(desc):
		// Actively Managed Certificate: a structured participation
		// wrapper (the web-description twin of CFI group EY).
		if currencyLinkedRe.MatchString(desc) {
			return canonical.AssetClassForeignExchange, canonical.VehicleStructuredProduct, true
		}
		return canonical.AssetClassPublicEquity, canonical.VehicleStructuredProduct, true
	case webSharesRe.MatchString(desc),
		webDepositReceiptRe.MatchString(desc),
		webPartCertRe.MatchString(desc):
		return canonical.AssetClassPublicEquity, canonical.VehicleStock, true
	case webMMRe.MatchString(desc):
		return canonical.AssetClassCash, canonical.VehicleFund, true
	case webETFRe.MatchString(desc):
		return silver.RefineETFExposure(desc), canonical.VehicleETF, true
	case webMultiVintageRe.MatchString(desc):
		return canonical.AssetClassPrivateEquity, canonical.VehicleFund, true
	case webFundRe.MatchString(desc):
		switch {
		case webInfraRe.MatchString(desc):
			return canonical.AssetClassInfrastructure, canonical.VehicleFund, true
		case webPERe.MatchString(desc):
			return canonical.AssetClassPrivateEquity, canonical.VehicleFund, true
		}
		return silver.RefineETFExposure(desc), canonical.VehicleFund, true
	case webMetalRe.MatchString(desc):
		return canonical.AssetClassMetal, canonical.VehiclePhysical, true
	}
	return canonical.AssetClassOther, canonical.VehicleOther, false
}

// taxonomyPairForUAC maps UBS's internal UacAsstClsCd to a
// (exposure, vehicle) pair for custody items UBS surfaces with an
// empty CFI. Covers the UAC codes UBS surfaces for custody items:
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
//     AND log a one-shot WARN to stderr.
//     This is the alarm bell for "PSN started surfacing a code we
//     haven't classified" — the primary case being a pension
//     product if PSN's coverage ever widens beyond pure private
//     banking.
//   - Empty code (silver payload missing AcctTpCd) is silent.
//
// The tables are deliberately not "default everything to
// taxable_personal": every code observed so far is a
// private-banking product, and the absence of pension codes in
// observed silver proves nothing about how PSN would encode one.

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
//	"managed securities account"              → discretionary
//	                                            (UBS Vermögens-
//	                                             verwaltung — the
//	                                             bank places trades
//	                                             under limited POA)
//	"securities account with dvisory agreement" → advisory
//	                                              (UBS Anlage-
//	                                               beratung; note
//	                                               the missing 'a'
//	                                               is the actual
//	                                               string in UBS's
//	                                               payload)
//	"securities account with advisory agreement" → advisory
//	                                               (if UBS ever
//	                                                fixes the typo)
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
// Such residual safekeeping rows have an EMPTY AcctDesc (no
// strategy name), and the cash accounts sharing their portfolio
// are personal banking outside any investment mandate — they
// must not inherit the safekeeping's advisory tag. Named-mandate
// safekeeping accounts always carry an AcctDesc (a strategy
// name like "STRATEGY ALPHA" / "STRATEGY BETA"); their cash
// siblings ARE part of the mandate (typically labelled in
// silver as "Cash Account for investment solutions"), and
// inheriting the safekeeping's style is correct. Cash staged in
// a named-mandate portfolio to fund the mandate is likewise
// directed by the mandate rather than managed as a standalone
// balance, so it inherits too.
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

// relationshipTaxWrapper is the tax wrapper the account kinds this
// adapter constructs itself carry — a card, a mortgage and the
// synthetic portfolio `overlay`, none of which PSN describes with an
// AcctTpCd. The two tables above answer for the cash and safekeeping
// accounts it does.
//
// Those kinds are not a separate tax position. A card and a mortgage
// are liabilities of the relationship whose cash and custody accounts
// the AcctTpCd tables already place, and an overlay is a synthetic
// account over that relationship's own contracts, so all three take the
// relationship's wrapper. An unset wrapper is not neutral: cashflow
// reads it as household, which is right here and wrong for a pension
// account nothing mapped, and `wealthdb status -v` reports the unset
// ones as the cash flow boundary's coverage gap. Stating it removes the
// ambiguity without moving a number.
//
// It is deliberately NOT a default applied to the AcctTpCd tables'
// misses: an unknown product code may be a pension product, and
// guessing taxable there is the error those tables exist to avoid. This
// answers only for the kinds the adapter itself constructs, where the
// tax position is known because the adapter is the thing that knows it.
func relationshipTaxWrapper() *canonical.TaxWrapper {
	w := canonical.TaxWrapperTaxablePersonal
	return &w
}
