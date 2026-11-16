package spending

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// The deterministic enrichment pass. It runs after every load, over the
// whole history, and re-derives every verdict it can reach.
// docs/SPENDING.md §3, "The full re-assert", says why it is not
// incremental.

// ProvenanceSignatureOnly tags a row the pass could reach but not
// categorise. It is not a failure: it is how the model tier finds its
// backlog, and it is why a signature is recorded even when no category
// can be.
const ProvenanceSignatureOnly = "signature-only"

// ProvenanceManual tags a verdict the pins ledger placed: the
// holder's own word about one transaction, and the top of the
// precedence lattice. The pass owns these rows exactly as it owns the
// derived ones — the ledger is config-sourced, so every pass re-stamps
// it, and removing a pin removes its effect on the next pass.
const ProvenanceManual = "manual"

// Options are the pass's inputs: the account scope to stamp into gold,
// the matcher's knobs, and the two config-sourced correction surfaces.
// Everything else it needs it reads from gold.
type Options struct {
	// Include and Exclude are the config-declared account-scope
	// overrides, keyed by silver_source_id. Include pulls an account
	// into the population; Exclude fences an account out. They are
	// stamped into spend_account_scope at the start of
	// the pass, so the report macros need no runtime config injection.
	Include map[string][]string
	Exclude map[string][]string

	// MatchWindowDays and MatchTolerancePct are the internal-transfer
	// matcher's knobs, already defaulted by the caller.
	MatchWindowDays   int
	MatchTolerancePct float64
	// MatchNames say which narratives name which account as the other side
	// of a movement (CounterpartyName).
	MatchNames []CounterpartyName

	// Rules are the config's compiled `spending.rules`: narratives
	// that name the holder's own destinations at institutions gold does
	// not track, and what such a row is. A match is placed by the rule
	// tier, after the built-in rules and below the matcher. Nil marks
	// nothing.
	Rules []Rule

	// TransferOverrides are the rows of the
	// `spending.transfer_overrides` ledger: the holder's manual match /
	// unmatch decisions for the internal-transfer matcher, resolved
	// against the matcher pool at pass time, and the reimburse lines,
	// resolved against gold's transactions (resolveReimbursements). A
	// line that names no row is reported rather than silently ignored.
	// Nil overrides nothing.
	TransferOverrides []gold.TransferOverrideRule

	// Pins are the rows of the `spending.pins` ledger. Each is stamped
	// onto every transaction it describes, with provenance `manual`,
	// after every other tier. Nil pins nothing.
	Pins []Pin

	// Income is the income family's own config: its account scope, its
	// rules and its pins. The matcher knobs and the transfer-override
	// ledger are deliberately absent — there is one matcher, and both
	// families read its verdicts (docs/INCOME.md, decision 8).
	Income IncomeOptions

	// Cashflow is the `cashflow` config block. It has no rules and no
	// pins, because cashflow places no verdicts: it reads the two
	// families' and folds them into a statement. What the pass does for
	// it is stamp the household boundary into gold, the same way it
	// stamps a family's account scope.
	Cashflow CashflowOptions

	// Now overrides the assignment timestamp. Zero means time.Now();
	// tests set it so a pass's output is byte-comparable across runs.
	Now int64
}

// IncomeOptions is the income family's half of Options. Same shapes as
// the spending fields above and the same meanings, read against the
// income vocabulary: a rule or a pin here places an income_detailed
// value, and the account scope is income's own.
type IncomeOptions struct {
	Include map[string][]string
	Exclude map[string][]string
	Rules   []Rule
	Pins    []Pin
}

// Result reports what the pass did. The counts are the pass's own
// observability: a build that stops reaching rows it used to reach
// shows up here before it shows up in a chart.
type Result struct {
	// The spending family's counters, embedded so a caller reads them
	// at the top level.
	FamilyResult

	// Income is the same counters for the income family.
	Income FamilyResult

	// Cashflow is what the boundary stamp did. Not a FamilyResult:
	// cashflow enriches nothing, so there is no population, no tier and
	// no backlog to count.
	Cashflow CashflowResult

	// UnmatchedTransferOverrides is the number of transfer-override
	// rules that named no leg in the matcher pool. It sits here rather
	// than on a family because there is one matcher and one ledger:
	// a match decision is about a PAIR, and a pair has a leg on each
	// side. Reported, never an error, and with the same force as
	// UnmatchedPins: an override is something a person wrote down
	// about a row they believe exists, so one that quietly does
	// nothing is worse than one that says so.
	UnmatchedTransferOverrides int

	// ReimbursementPairs is the number of ledger `reimburse` lines that
	// named a repayment and its outflow, UnmatchedReimbursements the lines
	// that named none, and UnlinkedReimbursements the repayments no road
	// links: each stays out of both bases, and a
	// ledger line is what links it.
	ReimbursementPairs      int
	UnmatchedReimbursements int
	UnlinkedReimbursements  int
}

// FamilyResult is what the pass did for one family. The counts are the
// pass's own observability: a build that stops reaching rows it used
// to reach shows up here before it shows up in a chart.
type FamilyResult struct {
	// ScopeRows is the number of account-scope overrides stamped.
	ScopeRows int
	// Population is the number of rows in the enrichment population.
	Population int
	// Enriched is the number of enrichment rows written.
	//
	// For SPENDING that is the population plus the matched legs and
	// pinned rows that sit outside it — a card payment is an
	// own-account move but never a spending line. For INCOME it is the
	// population plus pinned rows only: the matcher runs once, on the
	// spending side, and the income pass reads its verdicts rather than
	// re-emitting its legs (family.emitOutsidePopulation).
	Enriched int
	// Per-tier counts of the rows written, by the provenance each
	// landed with.
	MatcherRows       int
	RuleRows          int
	ProviderRows      int
	PinRows           int
	SignatureOnlyRows int
	// UnmatchedPins is the number of ledger pins that described no
	// transaction in gold. Reported, never an error: the row may
	// simply not have loaded yet.
	UnmatchedPins int
	// UnresolvedScopeAccounts is the number of configured
	// `<family>.accounts` entries naming an account gold does not
	// hold. Such an entry fences nothing — the scope table joins to
	// `accounts` on the id — so it is counted rather than silently
	// stamped and forgotten. Reported, never an error: the key is an
	// account id, and a source whose accounts have not loaded yet has
	// none to resolve against.
	UnresolvedScopeAccounts int
	// UnmappedProviderCategories counts rows whose source publishes a
	// categorical vocabulary but whose category is not in it — the
	// drift signal that says the provider's vocabulary has moved. A
	// bank's booking types are not categorical, and a rail the map
	// does not translate is not counted (see ProviderCategory).
	UnmappedProviderCategories int
	// RekeyedVerdicts counts stored verdicts — merchants on the
	// spending side, payers on the income side — carried forward onto
	// a new signature by a SignatureVersion bump.
	RekeyedVerdicts int
	// StatedExposures counts the rows on which a config rule or a pin
	// said what the capital went into, and UnstatedInvesting the rows
	// placed on an investing verdict that still say nothing. The second
	// is the one to work down: a success counter rising from zero says
	// nothing about whether the node is finally honest, and the tiers
	// this surface cannot reach — the provider tier above all — keep
	// pushing the backlog back up.
	StatedExposures   int
	UnstatedInvesting int
	// StatedFarAccounts counts the rows whose far account came from the
	// SOURCE naming it rather than from a pairing. It is the spending
	// family's alone — the income overlay has no far columns — and it is
	// the observable that says how much of "where the money went" the
	// product now knows about movements it collects only one side of.
	StatedFarAccounts int
	// SplitVerdicts counts older-version verdicts a SignatureVersion
	// bump left behind: the old signature's rows now land on several
	// new signatures, so the verdict was about a shape several
	// counterparties shared rather than about any one of them, and it
	// is carried onto none. Those are back in the backlog.
	SplitVerdicts int
}

// RunDeterministicPass recomputes every deterministic verdict in gold,
// both families', in one transaction, and returns what it did.
//
// What it owns is the work neither family may do twice. Silver kinds
// are read once. The matcher runs ONCE, before either family, over its
// own broader pool: every account in gold, and the income-side kinds
// the spending population excludes. Without the income side a card
// payment's funding leg has nothing to pair with; without the unscoped
// accounts, neither does a transfer into an investment account. A pair
// found there is one pair, seen from both sides. Each family's own
// phases, and the ordering they require, are enrichFamily's.
//
// The cash flow statement's boundary is stamped here too, after both
// families. It is not an enrichment — cashflow places no verdicts —
// but it is configuration that has to reach SQL, and this is the pass
// that turns configuration into tables.
func RunDeterministicPass(ctx context.Context, db *sql.DB, opts Options) (*Result, error) {
	now := opts.Now
	if now == 0 {
		now = time.Now().Unix()
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("spending: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	res := &Result{}

	kinds, err := loadSilverKinds(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Nothing the pass writes changes the accounts, so one read serves
	// the three scopes and the stated counter accounts.
	held, err := loadHeldAccounts(ctx, tx)
	if err != nil {
		return nil, err
	}

	// The matcher runs ONCE, before either family is enriched, and both
	// read its verdicts. A movement is own-account or it is not, and two
	// matchers with two bandings would call the same wire internal on
	// one side and external on the other.
	evidence, err := loadPassEvidence(ctx, tx, held.byNorm)
	if err != nil {
		return nil, err
	}
	legs, poolNarratives, err := loadMatcherPool(ctx, tx, opts.MatchNames, evidence)
	if err != nil {
		return nil, err
	}
	overrides, unresolvedOverrides, err := gold.ResolveTransferOverrides(opts.TransferOverrides, legs)
	if err != nil {
		return nil, fmt.Errorf("spending: transfer overrides: %w", err)
	}
	res.UnmatchedTransferOverrides = len(unresolvedOverrides)
	rates, err := gold.LoadDailyRates(ctx, tx)
	if err != nil {
		return nil, err
	}
	pairs := matchTransferPairs(legs, opts.MatchWindowDays, opts.MatchTolerancePct, overrides, rates)
	matched := matchedPartners(pairs)
	res.Cashflow.AmbiguousReferences = evidence.ambiguousRefs

	// Read once and shared, for the matcher's reason: where a movement
	// went is one fact, and two families reading it apart could disagree.
	stated := evidence.counterAccounts

	// Each ledger's pins are resolved once, against the whole of
	// `transactions`, because a pin describes a row by what a statement
	// shows and owes nothing to any population. A family writes its own
	// pins; the spending family steps around the income family's.
	spendPinned, spendUnmatched, err := resolvePins(ctx, tx, spendingFamily.name, opts.Pins)
	if err != nil {
		return nil, err
	}
	incomePinned, incomeUnmatched, err := resolvePins(ctx, tx, incomeFamily.name, opts.Income.Pins)
	if err != nil {
		return nil, err
	}

	spendRows, err := enrichFamily(ctx, tx, spendingFamily, familyInput{
		include: opts.Include, exclude: opts.Exclude,
		rules: opts.Rules, farRules: rulesWithFar(opts.Income.Rules),
		pinned: spendPinned, otherPinned: incomePinned, unmatchedPins: spendUnmatched,
		kinds: kinds, accounts: held.ids, matched: matched, stated: stated, pool: poolNarratives, now: now,
	}, &res.FamilyResult)
	if err != nil {
		return nil, err
	}
	// The links hold what the spending overlay's far columns hold, so
	// they are written from its rows, and the pair counters are read
	// back off them.
	if err := writeTransferLinks(ctx, tx, spendRows, pairs); err != nil {
		return nil, err
	}
	if err := writeStatedMoveLinks(ctx, tx); err != nil {
		return nil, err
	}
	if err := countLinkedPairs(ctx, tx, &res.Cashflow); err != nil {
		return nil, err
	}

	if _, err := enrichFamily(ctx, tx, incomeFamily, familyInput{
		include: opts.Income.Include, exclude: opts.Income.Exclude,
		rules:  opts.Income.Rules,
		pinned: incomePinned, unmatchedPins: incomeUnmatched,
		kinds: kinds, accounts: held.ids, matched: matched, stated: stated, pool: poolNarratives, now: now,
	}, &res.Income); err != nil {
		return nil, err
	}

	// After both families: a repayment's verdict is the income family's,
	// the outflow's category the spending family's.
	if res.ReimbursementPairs, res.UnmatchedReimbursements, err = resolveReimbursements(ctx, tx, opts.TransferOverrides); err != nil {
		return nil, err
	}
	if res.UnlinkedReimbursements, err = countUnlinkedReimbursements(ctx, tx); err != nil {
		return nil, err
	}

	// The cash flow statement's boundary, last: it is a stamp rather
	// than an enrichment, and it reads the accounts dimension the two
	// families have already been scoped against.
	if err := stampCashflowBoundary(ctx, tx, opts.Cashflow, held.ids, &res.Cashflow); err != nil {
		return nil, err
	}
	// Carried across from the spending family, which is where the far
	// columns live: the boundary block is where a reader looks to learn
	// whether the pass knows enough to resolve an own-account move.
	res.Cashflow.StatedFarAccounts = res.FamilyResult.StatedFarAccounts
	if err := countDeclaredAccounts(ctx, tx, &res.Cashflow); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("spending: commit: %w", err)
	}
	committed = true
	return res, nil
}

// enrichFamily runs one family's phases inside the pass's transaction.
//
// The order is load-bearing and is the reason this is one function
// rather than a sequence at the call site. The account scope is
// stamped FIRST because the population macro reads it, so a scope edit
// takes effect in the same pass that applies it. The old signatures
// are captured BEFORE the delete, because the signature-version re-key
// needs to know where a verdict used to hang.
//
// It returns the rows it wrote.
func enrichFamily(ctx context.Context, tx *sql.Tx, fam family, in familyInput, out *FamilyResult) ([]enrichmentRow, error) {
	var err error
	out.ScopeRows, out.UnresolvedScopeAccounts, err =
		syncAccountScope(ctx, tx, fam.name, fam.scopeTable, in.accounts, in.include, in.exclude)
	if err != nil {
		return nil, err
	}

	population, err := loadPopulation(ctx, tx, fam)
	if err != nil {
		return nil, err
	}
	out.Population = len(population)

	// A family counts its own pins that described nothing. The other
	// family's pins are only stepped around: what they say, and what
	// they missed, are that family's.
	out.UnmatchedPins = in.unmatchedPins

	oldSignatures, err := loadExistingSignatures(ctx, tx, fam)
	if err != nil {
		return nil, err
	}
	verdicts, err := loadVerdictStore(ctx, tx, fam)
	if err != nil {
		return nil, err
	}

	if err := deleteEnrichmentRows(ctx, tx, fam); err != nil {
		return nil, err
	}

	rows := assignCategories(fam, population, in.pool, in.matched, in.stated, in.kinds, in.rules, in.farRules, in.pinned, in.otherPinned, out)
	if err := insertEnrichment(ctx, tx, fam, rows, in.now); err != nil {
		return nil, err
	}
	out.Enriched = len(rows)

	out.RekeyedVerdicts, out.SplitVerdicts, err =
		rekeyStore(ctx, tx, fam, rows, oldSignatures, verdicts, in.now)
	return rows, err
}

// ---- phase 1: the account scope ------------------------------------------

// syncAccountScope replaces one account-scope table with what the
// config declares, on the SetFxPriorities precedent: configuration is stamped
// into gold so the SQL layer can honour it without a runtime injection
// point, and a whole re-stamp means removing an entry from the config
// removes it from gold rather than leaving it behind to haunt a later
// report. It returns the rows stamped and how many of them name an
// account gold does not hold (known, by source).
//
// It takes the table and the name to prefix its errors with rather
// than a family, because the cash pool has a scope table and is not a
// family: it has no vocabulary, no overlay and no tier of its own.
//
// The key is an account id, never a nickname or a display name
// (docs/DESIGN.md §5.1), and the macro that reads the stamped row
// joins it to `accounts` on exactly that id: an entry naming anything
// else matches no account and so widens or fences nothing.
// Every entry is stamped whatever it resolves to — the scope table is
// the config's whole state, and dropping an entry here would hide the
// typo instead of surfacing it — and the unresolved ones are counted
// for the caller to report, because a scope knob that silently does
// nothing is the wrong failure mode for the surface that decides which
// accounts the model tier may ever see.
func syncAccountScope(ctx context.Context, tx *sql.Tx, name, table string, known map[string]map[string]struct{},
	include, exclude map[string][]string) (stamped, unresolved int, err error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
		return 0, 0, fmt.Errorf("%s: clear account scope: %w", name, err)
	}
	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO `+table+` (silver_source_id, account_external_id, mode)
        VALUES (?, ?, ?)`)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: prepare account scope: %w", name, err)
	}
	defer stmt.Close()

	for _, mode := range []struct {
		name string
		m    map[string][]string
	}{{"include", include}, {"exclude", exclude}} {
		for _, source := range sortedKeys(mode.m) {
			ids := append([]string(nil), mode.m[source]...)
			sort.Strings(ids)
			for _, id := range ids {
				if _, err := stmt.ExecContext(ctx, source, id, mode.name); err != nil {
					return 0, 0, fmt.Errorf("%s: stamp account scope %s/%s: %w", name, source, id, err)
				}
				stamped++
				if _, ok := known[source][id]; !ok {
					unresolved++
				}
			}
		}
	}
	return stamped, unresolved, nil
}

// heldAccounts are the accounts gold holds, in the two shapes the pass
// reads them in.
type heldAccounts struct {
	// ids are the account ids per source: the set a configured scope
	// entry has to hit to fence anything.
	ids map[string]map[string]struct{}
	// byNorm are the accounts keyed by the id upper-cased with its
	// spaces removed: the form a stated counter account is compared in.
	byNorm map[string][]farAccount
}

// loadHeldAccounts reads the accounts table whole, once per pass.
func loadHeldAccounts(ctx context.Context, tx querier) (heldAccounts, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT silver_source_id, account_external_id, upper(replace(account_external_id, ' ', ''))
          FROM accounts`)
	if err != nil {
		return heldAccounts{}, fmt.Errorf("spending: read accounts: %w", err)
	}
	defer rows.Close()
	out := heldAccounts{ids: map[string]map[string]struct{}{}, byNorm: map[string][]farAccount{}}
	for rows.Next() {
		var a farAccount
		var norm string
		if err := rows.Scan(&a.source, &a.account, &norm); err != nil {
			return heldAccounts{}, fmt.Errorf("spending: scan accounts: %w", err)
		}
		if out.ids[a.source] == nil {
			out.ids[a.source] = map[string]struct{}{}
		}
		out.ids[a.source][a.account] = struct{}{}
		out.byNorm[norm] = append(out.byNorm[norm], a)
	}
	if err := rows.Err(); err != nil {
		return heldAccounts{}, fmt.Errorf("spending: read accounts: %w", err)
	}
	return out, nil
}

// ---- small helpers ---------------------------------------------------------

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedTxKeys[V any](m map[txKey]V) []txKey {
	out := make([]txKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortTxKeys(out)
	return out
}

func sortTxKeys(keys []txKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].source != keys[j].source {
			return keys[i].source < keys[j].source
		}
		return keys[i].txID < keys[j].txID
	})
}
