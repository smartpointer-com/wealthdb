package ubs

import (
	"log"
	"sync"

	"github.com/ptu/wealthdb/internal/canonical"
)

// assetClassForCFI maps an ISO 10962 CFI code's first character
// to a canonical AssetClass. The first character of the CFI code
// designates the asset category:
//
//	E - Equities                → equity
//	C - Collective investment   → fund
//	D - Debt instruments        → bond
//	O - Options                 → option
//	F - Futures                 → future
//	M - Others (mostly MM)      → money_market
//	T - Structured / cash collateral → other
//	R - Entitlements (rights)   → other
//
// Empty CFI codes (UBS does ship some instruments with no CFI
// populated — typically money-market funds) fall through to other.
func assetClassForCFI(cfi string) canonical.AssetClass {
	if cfi == "" {
		return canonical.AssetClassOther
	}
	switch cfi[0] {
	case 'E':
		return canonical.AssetClassEquity
	case 'C':
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

// taxWrapperForCashAcctTp / taxWrapperForSafekeepingAcctTp map a
// PSN account's `AcctTpCd` (the UBS product code embedded in the
// SDCA payload) to the canonical TaxWrapper enum. Per the
// ubs-psn-dump DESIGN.md §1 contract:
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
// ubs-psn-dump maintainer corrected on 2026-05-24, because the
// current customer happens to hold no Swiss pension assets at
// UBS at all, so the absence of pension codes in the silver
// proves nothing about whether PSN would surface them if they
// existed.

var knownCashAcctTpCd = map[string]canonical.TaxWrapper{
	"OA155": canonical.TaxWrapperTaxablePersonal, // UBS current account
	"OA157": canonical.TaxWrapperTaxablePersonal, // investment-solutions cash
	"OA159": canonical.TaxWrapperTaxablePersonal, // Lombard limit
	"OA197": canonical.TaxWrapperTaxablePersonal, // personal account
	"OA350": canonical.TaxWrapperTaxablePersonal, // savings
	"OA352": canonical.TaxWrapperTaxablePersonal, // savings (variant)
	"OA519": canonical.TaxWrapperTaxablePersonal, // forward-contract cash
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
// AccountChange slice once to lift the mandate type the
// safekeeping account carries onto its sibling cash / overlay
// accounts that share the same portfolio_external_id and don't
// already have ManagementStyle set.
//
// UBS labels Vermögensverwaltung / Anlageberatung on the
// safekeeping account; cash accounts and forward-contract
// overlays in the same portfolio are part of the same mandate
// but the silver payload doesn't tag them. Without this pass
// every cash account renders as `self_directed` (the default-
// aware fallback) which is misleading when the portfolio is
// actually discretionary.
//
// Conflict handling: if two safekeeping accounts share a
// portfolio and disagree on style (shouldn't happen in real
// UBS data — one mandate per portfolio — but defensive), the
// first one seen wins; the propagation only fills nils, never
// overrides.
func propagateManagementStyleByPortfolio(accounts []canonical.AccountChange) {
	styleByPortfolio := make(map[string]canonical.ManagementStyle)
	for i := range accounts {
		a := &accounts[i]
		if a.AccountKind != canonical.AccountKindSafekeeping {
			continue
		}
		if a.ManagementStyle == nil || a.PortfolioExternalID == nil {
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
