package ubs

import (
	"log"
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
func assetClassForInstrument(cfi, uacAsstClsCd, name string) canonical.AssetClass {
	var ac canonical.AssetClass
	if cfi != "" {
		ac = assetClassForCFI(cfi)
	} else {
		ac = assetClassForUacAsstCls(uacAsstClsCd)
	}
	if ac == canonical.AssetClassETF {
		ac = silver.RefineETFClass(name)
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
