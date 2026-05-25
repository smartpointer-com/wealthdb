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
