package returns_test

// This external test package blank-imports silver/all so silver.Register
// forwards each kind's policy (returns.RegisterPolicy), then pins the EXACT
// FlowPolicy each source resolves to (via returns.ReturnsPolicyFor(kind).Flow).
// It lives in package returns_test (not returns) to avoid an import cycle:
// silver imports internal/returns, so only an external test package may pull
// the silver adapters in alongside the package under test.

import (
	"reflect"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"

	// Every adapter, as cmd/wealthdb/main.go imports them, so registration
	// runs before the assertions.
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/all"
)

// TestRegisteredFlowPolicies pins the whole ReturnsPolicy every silver kind
// resolves to. The regime and the one kind table (canonical.CapitalKind)
// decide which kinds count and net (TestCompletenessCells), so a row states
// the regime and every knob, and nothing per kind.
func TestRegisteredFlowPolicies(t *testing.T) {
	bank := returns.DefaultReturnsPolicy(returns.BankFlowPolicy())
	with := func(p returns.ReturnsPolicy, set func(*returns.ReturnsPolicy)) returns.ReturnsPolicy {
		set(&p)
		return p
	}
	// A private-market ledger: complete on the custody account, the exit
	// ledger authoritative, and the honesty tag kept on flow-less windows.
	privateLedger := with(bank, func(p *returns.ReturnsPolicy) {
		p.CapitalCallRisk, p.ClosureScope = true, returns.ClosureLedgerExact
	})
	// The reference sources register no policy and fall to the Known=false
	// default.
	unknown := with(bank, func(p *returns.ReturnsPolicy) { p.Flow.Known = false })
	want := map[string]returns.ReturnsPolicy{
		// The bank policy: every capital kind counts, every other knob at
		// its default. The deposit banks' accounts are cash and amex's are
		// cards, which the engine hides or drops by kind on any source.
		"amex": bank, "angellist": bank, "chase": bank, "fidelity": bank, "firstcitizens": bank,
		"raiffeisen_at": bank, "relevate": bank, "schwab": bank, "swissquote": bank, "viac": bank,
		// A crypto tracker's coin transfer legs count as every source's
		// transfers do: a pair the load stored nets, an unpaired leg is a
		// flow.
		"cointracking": bank,
		// The generic kinds cannot know which institution a source models;
		// returns_policy_overrides states a source's own regime.
		"plaid": bank, "synthetic": bank,
		"carta": privateLedger, "equityzen": privateLedger,
		// No collected row counts, only the ledger's legs; a hand-dated
		// release leg on the day a claim zeroes is a real exit.
		"manual": with(bank, func(p *returns.ReturnsPolicy) {
			p.Flow = returns.NewFlowPolicy(returns.RegimeNavOnly)
			p.ClosureScope = returns.ClosureLedgerExact
		}),
		// One onboarding per entity, anchored inception, cash as conduit,
		// and only the rows the silver marks as owner capital.
		"ubs": with(bank, func(p *returns.ReturnsPolicy) {
			p.OnboardScope, p.Inception = returns.OnboardPerEntityOnce, returns.InceptionFirstRealSnapshot
			p.ConduitKinds, p.ExternalOnly = []canonical.AccountKind{canonical.AccountKindCash}, true
		}),
		"curves": unknown, "fred": unknown, "refdata": unknown,
	}
	for _, k := range silver.Kinds() {
		w, ok := want[k]
		if !ok {
			t.Errorf("%s: no expected policy; add the kind to the table", k)
			continue
		}
		if got, _ := returns.ReturnsPolicyFor(k); !reflect.DeepEqual(got, w) {
			t.Errorf("%s: policy %+v, want %+v", k, got, w)
		}
	}
}
