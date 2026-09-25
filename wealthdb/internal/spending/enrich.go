package spending

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// The deterministic enrichment pass.
//
// It runs after every load, over the whole history, and re-asserts
// every verdict it is capable of reaching. That is a deliberate choice
// over an incremental pass, for three reasons a cheaper design would
// have to give up:
//
//   - LATE ARRIVALS. A provider's category can appear on a row days
//     after the row itself; a counter-leg loaded from a different
//     source turns a withdrawal that looked like spending last night
//     into an own-account move today. Only re-deriving both sides
//     catches either.
//   - IDEMPOTENCE. A full re-assert has no accumulated state to be
//     wrong about, so a re-run after a crash, a partial load, or a
//     reset produces exactly what an unbroken run would have.
//   - EVOLUTION. When a rule or the normalisation changes, the whole
//     history moves to the new answer at once instead of leaving a
//     stratum of rows enriched by whichever build happened to see them
//     first.
//
// FALLBACK IF IT EVER MEASURES BADLY: narrow the recompute to the
// sources touched by the load plus every source holding a potential
// counter-leg within the matcher's day window, and keep the full pass
// as an explicit flag. At personal scale the full pass costs a fraction
// of the load it follows, so the complexity is not yet earned.

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
	// already-included account in; Exclude fences an account
	// out. They are stamped into spend_account_scope at the start of
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
	// unmatch decisions for the internal-transfer matcher. Resolved
	// against the matcher pool at pass time, so a row the pool does not
	// hold is reported rather than silently ignored. Nil overrides
	// nothing.
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
	// The spending family's counters, embedded so that every caller
	// and test that read them before the income family existed reads
	// them unchanged.
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

	// The matcher runs ONCE, before either family is enriched, and both
	// read its verdicts. A movement is own-account or it is not, and two
	// matchers with two bandings would call the same wire internal on
	// one side and external on the other.
	legs, poolNarratives, ambiguousRefs, err := loadMatcherPool(ctx, tx, opts.MatchNames)
	if err != nil {
		return nil, err
	}
	overrides, unresolvedOverrides, err := gold.ResolveTransferOverrides(opts.TransferOverrides, legs)
	if err != nil {
		return nil, fmt.Errorf("spending: transfer overrides: %w", err)
	}
	res.UnmatchedTransferOverrides = len(unresolvedOverrides)
	pairs := matchTransferPairs(legs, opts.MatchWindowDays, opts.MatchTolerancePct, overrides)
	matched := matchedPartners(pairs)
	res.Cashflow.ReferencePairs = countPairsBy(pairs, gold.MatchedByReference)
	res.Cashflow.StatedCounterPairs = countPairsBy(pairs, gold.MatchedByStatedCounter)
	res.Cashflow.NamedPairs = countPairsBy(pairs, gold.MatchedByName)
	res.Cashflow.AmbiguousReferences = ambiguousRefs

	// Read once and shared, for the matcher's reason: where a movement
	// went is one fact, and two families reading it apart could disagree.
	stated, err := loadStatedCounterAccounts(ctx, tx)
	if err != nil {
		return nil, err
	}

	if err := enrichFamily(ctx, tx, spendingFamily, familyInput{
		include: opts.Include, exclude: opts.Exclude,
		rules: opts.Rules, pins: opts.Pins,
		kinds: kinds, matched: matched, stated: stated, pool: poolNarratives, now: now,
	}, &res.FamilyResult); err != nil {
		return nil, err
	}

	if err := enrichFamily(ctx, tx, incomeFamily, familyInput{
		include: opts.Income.Include, exclude: opts.Income.Exclude,
		rules: opts.Income.Rules, pins: opts.Income.Pins,
		kinds: kinds, matched: matched, stated: stated, pool: poolNarratives, now: now,
	}, &res.Income); err != nil {
		return nil, err
	}

	// The cash flow statement's boundary, last: it is a stamp rather
	// than an enrichment, and it reads the accounts dimension the two
	// families have already been scoped against.
	if err := stampCashflowBoundary(ctx, tx, opts.Cashflow, &res.Cashflow); err != nil {
		return nil, err
	}
	// Carried across from the spending family, which is where the far
	// columns live: the boundary block is where a reader looks to learn
	// whether the pass knows enough to resolve an own-account move.
	res.Cashflow.StatedFarAccounts = res.FamilyResult.StatedFarAccounts

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
// needs to know where a verdict used to hang. Pins are resolved
// against the whole of `transactions`, because a pin describes a row
// by what a statement shows and owes nothing to any population.
func enrichFamily(ctx context.Context, tx *sql.Tx, fam family, in familyInput, out *FamilyResult) error {
	var err error
	out.ScopeRows, out.UnresolvedScopeAccounts, err =
		syncAccountScope(ctx, tx, fam.name, fam.scopeTable, in.include, in.exclude)
	if err != nil {
		return err
	}

	population, err := loadPopulation(ctx, tx, fam)
	if err != nil {
		return err
	}
	out.Population = len(population)

	pinned, unmatched, err := resolvePins(ctx, tx, fam.name, in.pins)
	if err != nil {
		return err
	}
	out.UnmatchedPins = unmatched

	oldSignatures, err := loadExistingSignatures(ctx, tx, fam)
	if err != nil {
		return err
	}
	verdicts, err := loadVerdictStore(ctx, tx, fam)
	if err != nil {
		return err
	}

	if err := deleteEnrichmentRows(ctx, tx, fam); err != nil {
		return err
	}

	rows := assignCategories(fam, population, in.pool, in.matched, in.stated, in.kinds, in.rules, pinned, out)
	if err := insertEnrichment(ctx, tx, fam, rows, in.now); err != nil {
		return err
	}
	out.Enriched = len(rows)

	out.RekeyedVerdicts, out.SplitVerdicts, err =
		rekeyStore(ctx, tx, fam, rows, oldSignatures, verdicts, in.now)
	return err
}

// ---- phase 1: the account scope ------------------------------------------

// syncAccountScope replaces one account-scope table with what the
// config declares, on the SetFxPriorities precedent: configuration is stamped
// into gold so the SQL layer can honour it without a runtime injection
// point, and a whole re-stamp means removing an entry from the config
// removes it from gold rather than leaving it behind to haunt a later
// report. It returns the rows stamped and how many of them name an
// account gold does not hold.
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
func syncAccountScope(ctx context.Context, tx *sql.Tx, name, table string, include, exclude map[string][]string) (stamped, unresolved int, err error) {
	known, err := loadAccountIDs(ctx, tx, name)
	if err != nil {
		return 0, 0, err
	}
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

// loadAccountIDs reads the account ids gold holds, per source — the
// set a configured scope entry has to hit to fence anything.
//
// It reads the `accounts` table whole and is scope-blind, but it is
// called once per scope table, so its errors carry that table's name:
// a failure on an income run reported as `spending:` sends a reader to
// the wrong block of the config.
func loadAccountIDs(ctx context.Context, tx querier, name string) (map[string]map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT DISTINCT silver_source_id, account_external_id FROM accounts`)
	if err != nil {
		return nil, fmt.Errorf("%s: read account ids: %w", name, err)
	}
	defer rows.Close()
	out := map[string]map[string]struct{}{}
	for rows.Next() {
		var source, id string
		if err := rows.Scan(&source, &id); err != nil {
			return nil, fmt.Errorf("%s: scan account ids: %w", name, err)
		}
		if out[source] == nil {
			out[source] = map[string]struct{}{}
		}
		out[source][id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate account ids: %w", name, err)
	}
	return out, nil
}

// ---- phase 2: reading the populations -------------------------------------

// candidate is one transaction the pass may write a verdict for,
// carrying only the fields a verdict is derived from. It serves both
// populations: rows from the enrichment population arrive with a
// provider category, rows read from the matcher pool leave it empty
// because nothing but the matcher may categorise them.
type candidate struct {
	key         txKey
	accountKind string
	// kind is the transaction kind, which the income family's built-in
	// rule is gated on: a narrative may only speak for the one admitted
	// kind the floor does not answer.
	kind string
	// The provider's vocabulary is per PRODUCT, not per source: a bank
	// files an account's rows by booking type and a card's by merchant
	// category, and only the account kind tells the provider tier which
	// of the two it is reading.
	counterparty     string
	description      string
	providerCategory string
	// Scope facts: what an optional `spending.rules[].scope` narrows a
	// rule by. Carried on every candidate because the rule tier cannot
	// know in advance which dimension a rule will name.
	account    string
	portfolio  string
	occurredAt int64
}

// farAccount is an account gold holds, named as the other side of a
// movement.
type farAccount struct{ source, account string }

// loadStatedCounterAccounts reads, per transaction, the OWN account the
// source itself named as the other side of the movement.
//
// It is the far account arriving by the road that needs only ONE leg. The
// matcher's road needs two however its phases join them — on amount and day,
// on a reference the source stamped on both, or on the other leg it described
// — and where the product does not collect the far side at all, none of them
// has anything to walk on. This one is the source's own
// statement of where the money went: a counter account in the row's payload,
// put there by whichever adapter or collector can read one (CASHFLOW.md §4).
//
// The join is what makes it safe. Only a counter account gold ALREADY HOLDS
// resolves; every other stated account — the overwhelming majority, third
// parties being what most payments are for — returns nothing and the row is
// left exactly as it was. The row's own source is searched first, and another
// source's account is taken only when no account of the row's own source
// answers and exactly one account elsewhere does: one collector can write
// several sources (the svb collector writes a lender's loan statements to a
// source apart from the deposit ledger that pays them), and an id two
// sources both hold says nothing about which one was meant. A row naming its
// own account is refused too: a movement is not its own counterparty, and a
// source that echoes the debited account into the field would otherwise pair
// a row with itself.
//
// It places no verdict. What a row IS stays the tiers' question — the
// evidence here answers only where it went, which is the one thing a tier
// below the matcher has never been able to say.
func loadStatedCounterAccounts(ctx context.Context, tx querier) (map[txKey]farAccount, error) {
	rows, err := tx.QueryContext(ctx, `
        WITH stated AS (
            SELECT t.silver_source_id, t.transaction_external_id,
                   upper(replace(t.account_external_id, ' ', '')) AS own,
                   upper(replace(json_extract_string(t.payload, '$.counter_account'), ' ', '')) AS named
              FROM transactions t
             WHERE json_extract_string(t.payload, '$.counter_account') IS NOT NULL
               AND json_extract_string(t.payload, '$.counter_account') <> ''
        ),
        held AS (
            SELECT s.silver_source_id, s.transaction_external_id,
                   a.silver_source_id AS far_source, a.account_external_id AS far_account,
                   a.silver_source_id = s.silver_source_id AS same_source
              FROM stated s
              JOIN accounts a ON upper(replace(a.account_external_id, ' ', '')) = s.named
             WHERE s.named <> s.own
        ),
        counted AS (
            SELECT *,
                   COUNT(*) FILTER (WHERE same_source)
                       OVER (PARTITION BY silver_source_id, transaction_external_id) AS n_same,
                   COUNT(*) OVER (PARTITION BY silver_source_id, transaction_external_id) AS n_all
              FROM held
        )
        SELECT silver_source_id, transaction_external_id, far_source, far_account
          FROM counted
         WHERE (same_source AND n_same = 1)
            OR (n_same = 0 AND n_all = 1)`)
	if err != nil {
		return nil, fmt.Errorf("spending: read stated counter accounts: %w", err)
	}
	defer rows.Close()
	out := map[txKey]farAccount{}
	for rows.Next() {
		var k txKey
		var far farAccount
		if err := rows.Scan(&k.source, &k.txID, &far.source, &far.account); err != nil {
			return nil, fmt.Errorf("spending: scan stated counter accounts: %w", err)
		}
		out[k] = far
	}
	return out, rows.Err()
}

// loadMovementReferences reads, per transaction, the reference the source
// stamped on BOTH halves of one money movement — the bank's own transaction
// number, written once and printed on the debit and on the credit alike.
//
// The far account arrives by two roads (CASHFLOW.md §4): the matcher's, which
// is a pairing, and the source's, which is the counter account it states
// outright (loadStatedCounterAccounts). This feeds the first, and of the ways
// that road joins two legs it is the strongest short of the holder's own
// word — the only one that is an identity rather than an inference. The
// source names the MOVEMENT, and because it names it rather than describing
// it, the join holds across a currency conversion, where the two legs carry
// different figures and the amount phase, which partitions by native currency
// on purpose, can never bring them together.
//
// The key is `payload.$.bank_ref`, and reading ONE key is the whole reason
// the adapter writes it. A bank states its reference in as many places as it
// has feeds — a statement column here, an MT940 field there, an id
// composition somewhere else — and an adapter is the layer that knows which.
// Decoding an id's composition HERE would put one source's private shape into
// a reader that serves every source, and would mis-fire on the next source
// whose ids happen to look similar.
//
// THE UNIQUENESS TEST IS THE SAFETY, and it is why this is a query rather
// than a column. A reference is trustworthy exactly insofar as the source
// mints one per movement, so a reference carried by anything other than two
// rows of one source is not offered at all: a reference the bank also stamped
// on a charge booked beside the payment, on a correction, or on every row of
// a batch names no single movement, and the matcher must be given no chance
// to guess which pair it meant. The census is taken over the WHOLE of
// `transactions` rather than over the matcher's pool, deliberately: a
// reference shared with a row the pool cannot see — a fee, an FX leg, a
// booking of a kind no leg is made from — is exactly the case the pool's own
// census is blind to, and the case that would otherwise pair two legs whose
// reference belongs to something larger than they are.
//
// Scoping by silver source is the second half of the same argument. A
// reference is an identity only inside one source's id space; two banks can
// mint the same string, and a pair drawn across that seam would be a
// coincidence wearing the clothes of a fact.
func loadMovementReferences(ctx context.Context, tx querier) (map[txKey]string, int, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id, ref, rows_under_ref
          FROM (
            SELECT silver_source_id, transaction_external_id, ref,
                   COUNT(*) OVER (PARTITION BY silver_source_id, ref) AS rows_under_ref
              FROM (
                SELECT silver_source_id, transaction_external_id,
                       json_extract_string(payload, '$.bank_ref') AS ref
                  FROM transactions
              )
             WHERE ref IS NOT NULL AND ref <> ''
          )
         ORDER BY silver_source_id, ref, transaction_external_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("spending: read movement references: %w", err)
	}
	defer rows.Close()
	out := map[txKey]string{}
	ambiguous := map[string]bool{}
	for rows.Next() {
		var k txKey
		var ref string
		var under int
		if err := rows.Scan(&k.source, &k.txID, &ref, &under); err != nil {
			return nil, 0, fmt.Errorf("spending: scan movement references: %w", err)
		}
		if under != 2 {
			// More than two rows under one reference is the case worth
			// counting: there the source HAS named something, and what it
			// named is not one movement, so a pairing had to be refused.
			// A few are ordinary — a bank books a charge under the
			// reference of the payment it belongs to — but the number
			// climbing with the archive is how a reference space that is
			// per-day or per-batch rather than per-movement announces
			// itself, and nothing else downstream would say so.
			//
			// One row under a reference is NOT counted. It is the ordinary
			// shape of most rows, whose other half belongs to a third
			// party and was never gold's to hold; there is nothing to
			// refuse, and counting it would bury the signal above under
			// the whole archive.
			if under > 2 {
				ambiguous[k.source+"\x00"+ref] = true
			}
			continue
		}
		out[k] = ref
	}
	return out, len(ambiguous), rows.Err()
}

// statedCounterLeg is the other leg of a movement as the source describes it:
// the currency the money became, or came from, and the figure.
type statedCounterLeg struct {
	currency string
	amount   float64
}

// loadStatedCounterLegs reads, per transaction, the OTHER LEG the source
// itself described — `payload.$.counter_currency` and `$.counter_amount`.
//
// It is the third thing a source can say about where a movement went, and the
// weakest of the three. A reference NAMES the movement, so two legs carrying
// it are one movement and nothing more need be shown. A counter account names
// the far ACCOUNT, which gold either holds or does not. A described leg names
// neither: it says only "the other side of this booking was CCY 1234.56", and
// something still has to find the row that answers.
//
// Its worth is that it reaches what neither of the others can. A bank
// converting between two accounts of one holder may stamp no shared reference
// — a statement reconstructed from a printed page carries none at all — and
// name no counter account, while the two figures differ by the rate, so no
// tolerance can bring them together. The sentence in the narrative is then
// the only link between the two rows there is.
//
// UNLIKE the reference phase, THE CENSUS IS NOT HERE. The reference query
// refuses a reference the source did not mint per movement, because a leg
// carrying an over-used reference must reach the matcher looking like a leg
// carrying none. A description is different: the matcher needs to know a leg
// was described EVEN WHEN THE DESCRIPTION RESOLVES TO NOTHING, because the
// fact that some narrative places this leg's other half on another account,
// in another currency, is itself a reason to keep it away from the credit
// sitting on its own account. Filtered here, that fact would be gone before
// the matcher could use it, and an unresolvable description would silently
// become a same-account round trip. So this reads the description as written
// and matchStatedCounters does all the counting, over the legs it was
// actually offered.
func loadStatedCounterLegs(ctx context.Context, tx querier) (map[txKey]statedCounterLeg, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id,
               json_extract_string(payload, '$.counter_currency'),
               TRY_CAST(json_extract_string(payload, '$.counter_amount') AS DOUBLE)
          FROM transactions
         WHERE json_extract_string(payload, '$.counter_currency') IS NOT NULL
           AND json_extract_string(payload, '$.counter_currency') <> ''
           AND TRY_CAST(json_extract_string(payload, '$.counter_amount') AS DOUBLE) IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("spending: read stated counter legs: %w", err)
	}
	defer rows.Close()
	out := map[txKey]statedCounterLeg{}
	for rows.Next() {
		var k txKey
		var leg statedCounterLeg
		if err := rows.Scan(&k.source, &k.txID, &leg.currency, &leg.amount); err != nil {
			return nil, fmt.Errorf("spending: scan stated counter legs: %w", err)
		}
		out[k] = leg
	}
	return out, rows.Err()
}

// loadPopulation reads one family's candidates. The two populations
// project the same columns by construction — the income one adds
// `instrument_external_id`, which the pass has no use for and does not
// select — so one query shape serves both.
func loadPopulation(ctx context.Context, tx querier, fam family) ([]candidate, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id,
               COALESCE(account_kind, ''), kind,
               COALESCE(counterparty, ''), COALESCE(description, ''),
               COALESCE(provider_category, ''),
               COALESCE(account_external_id, ''),
               COALESCE(portfolio_external_id, ''), occurred_at
          FROM `+fam.populationMacro+`(?, ?)
         ORDER BY silver_source_id, transaction_external_id`, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, fmt.Errorf("%s: read enrichment population: %w", fam.name, err)
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var r candidate
		if err := rows.Scan(&r.key.source, &r.key.txID, &r.accountKind, &r.kind,
			&r.counterparty, &r.description, &r.providerCategory,
			&r.account, &r.portfolio, &r.occurredAt); err != nil {
			return nil, fmt.Errorf("%s: scan enrichment population: %w", fam.name, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadMatcherPool reads the matcher's legs and, alongside them, the
// narrative of each leg and the count of source references it had to
// refuse. A matched leg outside the spending population still gets an
// enrichment row, and that row needs a signature like any other.
//
// The references are read HERE, rather than beside the pool at the call
// site, because this is the one place a leg is built. Both callers — the
// pass and the audit surface — take their legs from it, and a reference
// fetched at one call site and not the other would let the two disagree
// about which movements paired, which is the drift matchTransferPairs
// exists to prevent for the options.
//
// Most of the pool is outside the population — the pool spans every
// account and admits the income kinds — and the enrichment table takes
// those rows without complaint: it is keyed by transaction alone and
// carries no FK to any population. Such a row is invisible to every
// spending surface, which reaches the overlay only THROUGH
// spend_enrichment_population, and visible to `wealthdb transactions`,
// which reads spend_txn_categories() directly and so can name the leg
// for what it is. Nothing leaks and nothing is orphaned: the next
// pass deletes every derived row before re-asserting.
func loadMatcherPool(ctx context.Context, tx querier, names []CounterpartyName) ([]gold.TransferLeg, map[txKey]candidate, int, error) {
	refs, ambiguous, err := loadMovementReferences(ctx, tx)
	if err != nil {
		return nil, nil, 0, err
	}
	counters, err := loadStatedCounterLegs(ctx, tx)
	if err != nil {
		return nil, nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `
        SELECT silver_source_id, account_external_id, transaction_external_id,
               occurred_at, currency, CAST(net_amount AS DOUBLE),
               COALESCE(counterparty, ''), COALESCE(description, '')
          FROM spend_matcher_pool(?, ?)
         ORDER BY silver_source_id, transaction_external_id`, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("spending: read matcher pool: %w", err)
	}
	defer rows.Close()

	var legs []gold.TransferLeg
	narratives := map[txKey]candidate{}
	for rows.Next() {
		var (
			leg        gold.TransferLeg
			occurredAt int64
			amount     sql.NullFloat64
			row        candidate
		)
		if err := rows.Scan(&leg.Group, &leg.Owner, &leg.ID, &occurredAt,
			&leg.Ccy, &amount, &row.counterparty, &row.description); err != nil {
			return nil, nil, 0, fmt.Errorf("spending: scan matcher pool: %w", err)
		}
		if !amount.Valid {
			// A leg with no amount cannot be oriented, so it can neither
			// fund nor be funded.
			continue
		}
		leg.Day = gold.EpochDay(occurredAt)
		leg.Amt = amount.Float64
		leg.Rail, leg.RailPartner = legRail(row.counterparty, row.description)
		leg.Signature = Normalize(row.counterparty, row.description)
		leg.Reversal = reversalRe.MatchString(row.counterparty + " " + row.description)
		leg.Names = legNames(names, leg.Group, leg.Owner, row.counterparty, row.description)
		row.key = txKey{leg.Group, leg.ID}
		leg.Ref = refs[row.key]
		if c, ok := counters[row.key]; ok {
			leg.CounterCcy, leg.CounterAmt = c.currency, c.amount
		}
		legs = append(legs, leg)
		narratives[row.key] = row
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	return legs, narratives, ambiguous, nil
}

func loadSilverKinds(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT silver_source_id, silver_kind FROM silver_sources`)
	if err != nil {
		return nil, fmt.Errorf("spending: read silver kinds: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, fmt.Errorf("spending: scan silver kinds: %w", err)
		}
		out[id] = kind
	}
	return out, rows.Err()
}

// signatureAt is where a verdict used to hang: the signature a
// previous pass computed for a transaction, and the version of the
// rules that computed it.
type signatureAt struct {
	signature string
	version   int
}

func loadExistingSignatures(ctx context.Context, tx *sql.Tx, fam family) (map[txKey]signatureAt, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id, `+fam.signatureCol+`, signature_version
          FROM `+fam.overlayTable+`
         WHERE `+fam.signatureCol+` IS NOT NULL AND `+fam.signatureCol+` <> ''`)
	if err != nil {
		return nil, fmt.Errorf("%s: read existing signatures: %w", fam.name, err)
	}
	defer rows.Close()
	out := map[txKey]signatureAt{}
	for rows.Next() {
		var (
			k txKey
			s signatureAt
		)
		if err := rows.Scan(&k.source, &k.txID, &s.signature, &s.version); err != nil {
			return nil, fmt.Errorf("%s: scan existing signatures: %w", fam.name, err)
		}
		out[k] = s
	}
	return out, rows.Err()
}

// storedVerdict is one row of a family's global verdict store — a
// merchant's on the spending side, a payer's on the income side — read
// for the signature-version re-key.
type storedVerdict struct {
	name      string
	detailed  string
	version   int
	modelName string
}

func loadVerdictStore(ctx context.Context, tx *sql.Tx, fam family) (map[string]storedVerdict, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT `+fam.storeSignatureCol+`, `+fam.storeNameCol+`, `+fam.storeDetailedCol+`,
               signature_version, model_name
          FROM `+fam.storeTable)
	if err != nil {
		return nil, fmt.Errorf("%s: read verdict store: %w", fam.name, err)
	}
	defer rows.Close()
	out := map[string]storedVerdict{}
	for rows.Next() {
		var (
			sig string
			v   storedVerdict
		)
		if err := rows.Scan(&sig, &v.name, &v.detailed, &v.version, &v.modelName); err != nil {
			return nil, fmt.Errorf("%s: scan verdict store: %w", fam.name, err)
		}
		out[sig] = v
	}
	return out, rows.Err()
}

// ---- phase 3: the re-assert -----------------------------------------------

// deleteEnrichmentRows clears the whole overlay, full stop.
//
// This pass is the only writer of every provenance in it, and it
// re-derives all of them every time — the four derived ones from gold,
// `manual` from the pins ledger — so anything it leaves behind is by
// definition stale. Scoping the delete to the current population
// would be narrower but wrong at the edges: an account fenced out of
// the scope, or a transaction a re-projection dropped, leaves rows the
// pass can no longer reach and therefore can no longer clean up.
// Deleting what it owns and re-asserting it is the version with no
// orphan case to reason about.
//
// `manual` is no exception. A hand-made verdict looks like the one
// thing gold cannot re-derive, but the decision is not in gold: it is
// in the pins ledger, which is config, and config is re-stamped whole
// so that removing an entry removes its effect (the
// spend_account_scope precedent). A preserved manual row would outlive
// the pin that made it.
func deleteEnrichmentRows(ctx context.Context, tx *sql.Tx, fam family) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+fam.overlayTable); err != nil {
		return fmt.Errorf("%s: clear enrichment rows: %w", fam.name, err)
	}
	return nil
}

// enrichmentRow is one row about to be written.
type enrichmentRow struct {
	key        txKey
	signature  string
	detailed   string // empty when nothing could place the row
	provenance string
	// merchantLabel is the issuer a card bill was paid to, and is
	// empty everywhere else. The built-in card rule is its only
	// source: no other tier writes it, and a tier that overrules the
	// rule clears it, because the label describes the verdict the card
	// rule placed rather than the row. spend_txn_categories() reads it
	// as the merchant of a delta line (migration 0052).
	merchantLabel string
	// providerDetailed is what the issuer's own filing of this row
	// translates to, recorded whether or not the provider tier claimed
	// the row and whether or not a tier above overruled it. It is the
	// faithful record of the card provider's view — never a verdict of
	// ours — so a report can show it beside our own and the
	// disagreement is a number rather than a silent overwrite. Empty
	// where the issuer published nothing this build translates.
	providerDetailed string
	// farSource and farAccount identify the account on the OTHER side
	// of an own-account move: the matcher's partner leg, and the one
	// thing the matcher has always known and never written down. They
	// are a record of what was SEEN rather than a verdict, so they stay
	// on a row a later tier re-categorises — nothing reads them unless
	// the surviving verdict says the money stayed the holder's.
	farSource, farAccount string
	// farClass is the cashflow class a rule stands for where it placed
	// an own-account move without pairing anything — the mortgage
	// rule's alone today. It describes the verdict the rule placed
	// rather than the row, exactly as merchantLabel does, so a tier
	// above the rule clears it with the category it replaces.
	farClass string
	// statedAssetClass is what the holder said the capital went INTO,
	// carried beside an investing verdict by a config rule or a pin
	// (migration 0102). Like farClass and merchantLabel it describes
	// the VERDICT rather than the row, so it travels with the verdict:
	// written by the tier that placed one, cleared when the matcher
	// replaces the verdict with `internal_transfer` — an exposure on an
	// own-account move means nothing — and replaced, with a value or
	// with nothing, by a pin above a rule.
	//
	// It is NOT cleared where the source merely states a far account:
	// that road answers where the money went and never what the
	// movement was, so the verdict it describes is still standing.
	statedAssetClass string
}

// assignCategories applies the deterministic tiers to every reachable
// row.
//
// PRECEDENCE is pin > matcher > rule > provider, expressed as writes
// in the reverse of that order so the last one wins. It reads as
// weakest-first on purpose:
//
//   - the PROVIDER is the weakest signal. It is a third party's
//     opinion about a row, published without any knowledge of which
//     other accounts the product tracks. Weakest in precedence, not
//     in vocabulary: where the provider's own word names the movement
//     — an ATM withdrawal, a conversion between the holder's own
//     currency accounts, a bill paid to a card — the tier places the
//     matching delta, and every tier above still overrules it.
//   - a RULE beats it because a rule encodes something structural
//     about the product's own account graph — that the mortgage being
//     paid is itself tracked, that cash out of an ATM is
//     unattributable — which no issuer can know. The config's rules
//     sit inside this tier, AFTER the built-ins: a config rule is
//     consulted only for a row no built-in rule placed, so it can mark
//     own-money movement and capital deployed but never re-label what
//     the engine already knows (an ATM withdrawal that happens to
//     carry the holder's name is still cash out). The built-ins read
//     the description's narrative only — a memo, the payer's own
//     words, fires none of them — while a config rule reads the memo
//     as well (rules.go).
//   - the MATCHER beats those because it is not an inference at all:
//     it has SEEN both legs of the movement. A provider that labels a
//     card payment "Shopping" is simply wrong, and evidence outranks
//     opinion.
//   - a PIN beats everything, because it is the holder's own word
//     about one transaction, written for exactly the row where every
//     tier below has nothing to go on — or got it wrong.
//
// Rows outside a family's population get a verdict only when the
// matcher paired them or a pin named them; nothing else has any
// business categorising a row the family does not chart. On the income
// side only the pin route exists, the matcher's legs being the spending
// pass's to emit.
func assignCategories(
	fam family,
	population []candidate,
	pool map[txKey]candidate,
	matched map[txKey]gold.TransferLeg,
	stated map[txKey]farAccount,
	kinds map[string]string,
	rules []Rule,
	pinned map[txKey]pinnedRow,
	counts *FamilyResult,
) []enrichmentRow {
	out := make([]enrichmentRow, 0, len(population)+len(matched)+len(pinned))
	seen := make(map[txKey]bool, len(population)+len(matched)+len(pinned))

	emit := func(r candidate, inPopulation bool) {
		if seen[r.key] {
			return
		}
		seen[r.key] = true

		row := enrichmentRow{
			key:        r.key,
			signature:  Normalize(r.counterparty, r.description),
			provenance: ProvenanceSignatureOnly,
		}
		if inPopulation {
			if detailed, ok, drift := fam.providerCategory(kinds[r.key.source], r.accountKind, r.providerCategory); ok {
				// Recorded either way: this is the issuer's own view of the
				// row, and it is kept whether or not it decides anything.
				row.providerDetailed = detailed
				// ...but claimed only where the value is a verdict. A
				// catch-all from a card issuer is not one: it says only
				// "somewhere in this primary", and claiming would
				// pre-empt the model tier, which reads the merchant
				// name the issuer never saw. A catch-all from a bank's
				// booking type IS one — the bank is naming the
				// movement, there is no merchant to read, and the row
				// is fenced out of model candidacy anyway.
				// ProviderCategoryClaims holds that distinction, beside
				// the vocabularies it turns on.
				if fam.providerClaims(kinds[r.key.source], r.accountKind, detailed) {
					row.detailed, row.provenance = detailed, ProvenanceProvider
				}
			} else if drift {
				counts.UnmappedProviderCategories++
			}
			if detailed, label, farClass, ok := fam.builtinRule(r.kind, row.signature, r.counterparty, r.description, r.providerCategory); ok {
				row.detailed, row.provenance = detailed, ProvenanceRule
				if fam.labelCol != "" {
					row.merchantLabel = label
				}
				if len(fam.farCols) > 0 {
					row.farClass = farClass
				}
			} else if placement, ok := ConfigRuleCategory(rules, RuleRow{
				Counterparty: r.counterparty, Description: r.description,
				ProviderCategory: r.providerCategory,
				Source:           r.key.source, Portfolio: r.portfolio,
				Account: r.account, OccurredAt: r.occurredAt,
			}); ok {
				row.detailed, row.provenance = placement.Category, ProvenanceRule
				row.statedAssetClass = placement.AssetClass
			}
		}
		// A tier above the rule clears the label and the far class with
		// the verdict it replaces. The label says which issuer a card
		// bill was paid to, which is only true of a row this pass filed
		// as a card bill: on a bill whose card the matcher paired, or
		// on a row the holder pinned as something else, it would name
		// an issuer on a line that is no longer a card bill at all. The
		// far class is the same shape of claim — a rule's word for
		// where an unpaired move went — and a row the matcher DID pair
		// has the far account itself, which is better than any stand-in.
		if partner, ok := matched[r.key]; ok {
			row.detailed = internalTransfer
			row.provenance = ProvenanceMatcher
			row.merchantLabel = ""
			row.farClass = ""
			row.statedAssetClass = ""
			if len(fam.farCols) > 0 {
				row.farSource, row.farAccount = partner.Group, partner.Owner
			}
		} else if far, ok := stated[r.key]; ok && len(fam.farCols) > 0 {
			// No pairing, but the source named the far account itself.
			// Only the ACCOUNT is taken: the verdict stays whatever the
			// tiers decided, because "where it went" and "what it was"
			// are different questions and this evidence answers only the
			// first. A rule's far CLASS is a stand-in for exactly this
			// and gives way to it, the way it gives way to a pairing.
			row.farSource, row.farAccount = far.source, far.account
			row.farClass = ""
			counts.StatedFarAccounts++
		}
		if p, ok := pinned[r.key]; ok {
			row.detailed = p.detailed
			row.provenance = ProvenanceManual
			row.merchantLabel = ""
			row.farClass = ""
			row.statedAssetClass = p.assetClass
		}
		out = append(out, row)

		// Counted off the FINISHED row, never at the tier that wrote it:
		// the matcher clears an exposure a rule placed, and a pin may
		// replace one with nothing, so a tier-time tally would count a
		// word the overlay does not hold. Both surfaces refuse an
		// exposure beside anything but this family's investing verdict,
		// so the two arms are the two halves of one population.
		if row.statedAssetClass != "" {
			counts.StatedExposures++
		} else if row.detailed == fam.investingValue {
			counts.UnstatedInvesting++
		}
		switch row.provenance {
		case ProvenanceManual:
			counts.PinRows++
		case ProvenanceMatcher:
			counts.MatcherRows++
		case ProvenanceRule:
			counts.RuleRows++
		case ProvenanceProvider:
			counts.ProviderRows++
		default:
			counts.SignatureOnlyRows++
		}
	}

	for _, r := range population {
		emit(r, true)
	}
	// Matched legs outside the population — a card payment, the deposit
	// side of a funding wire — are marked too, where the family asks
	// for it. Both halves of a pair carry the verdict, so a later
	// report that widens the population cannot start counting one of
	// them as real money movement. Income asks for none of this: its
	// half of a pair is already in its population, and the other half
	// is spending's row to write.
	if fam.emitOutsidePopulation {
		for _, key := range sortedTxKeys(matched) {
			if r, ok := pool[key]; ok {
				emit(r, false)
			}
		}
	}
	// Pinned rows outside both: a pin describes a row by what a
	// statement shows, and the holder may name a row on any account.
	for _, key := range sortedTxKeys(pinned) {
		emit(pinned[key].row, false)
	}
	return out
}

// insertEnrichment writes the overlay: the whole population, plus the
// pinned rows outside it and — on the family that emits them — the
// matched legs, through gold.InsertChunked
// — the same multi-row VALUES batching gold's writer gives the fact
// tables. This is the pass's one bulk write and it runs after every
// load, so the shape matters: DuckDB's per-statement cost dwarfs its
// per-row cost, and a statement per row is the slowest way to feed it.
//
// An error names a row range rather than a transaction: assignCategories
// emits each key once and the table is emptied first, so a primary-key
// collision is not among the ways this can fail.
func insertEnrichment(ctx context.Context, tx *sql.Tx, fam family, rows []enrichmentRow, now int64) error {
	cols := []string{
		"silver_source_id", "transaction_external_id", fam.signatureCol,
		"signature_version", fam.detailedCol, "provenance",
	}
	if fam.labelCol != "" {
		cols = append(cols, fam.labelCol)
	}
	cols = append(cols, fam.farCols...)
	cols = append(cols, "stated_asset_class", fam.providerCol, "assigned_at")

	head := "INSERT INTO " + fam.overlayTable + " (" + strings.Join(cols, ", ") + ") VALUES "
	placeholders := "(" + strings.Repeat("?, ", len(cols)-1) + "?)"

	return gold.InsertChunked(ctx, tx, fam.name+": write enrichment", head,
		placeholders, len(rows),
		func(i int, args []any) []any {
			r := &rows[i]
			args = append(args, r.key.source, r.key.txID, nullableString(r.signature),
				SignatureVersion, nullableString(r.detailed), r.provenance)
			if fam.labelCol != "" {
				args = append(args, nullableString(r.merchantLabel))
			}
			if len(fam.farCols) > 0 {
				args = append(args, nullableString(r.farSource),
					nullableString(r.farAccount), nullableString(r.farClass))
			}
			args = append(args, nullableString(r.statedAssetClass))
			return append(args, nullableString(r.providerDetailed), now)
		})
}

// ---- phase 4: the signature-version re-key ---------------------------------

// rekeyStore carries paid-for verdicts across a normalisation change (a
// SignatureVersion bump), for whichever family's store it is given, so the
// model tier does not buy the same answers twice.
//
// A verdict is carried forward when all five hold: the row's signature
// actually moved, EVERY row that carried the old signature now carries the
// same new one, the store holds a verdict at the OLD signature, that verdict
// predates the current SignatureVersion, and the NEW signature has no
// verdict yet. An old key whose rows split across several new keys was a
// shape several creditors shared, not a merchant, so its verdict stays
// behind and is counted; an existing verdict at a new key is never
// overwritten; model_name is preserved verbatim. docs/SPENDING.md §4 argues
// each condition. The rows arrive sorted, so when two old signatures
// collapse onto one new one the first wins, whatever the map order.
func rekeyStore(
	ctx context.Context,
	tx *sql.Tx,
	fam family,
	rows []enrichmentRow,
	old map[txKey]signatureAt,
	verdicts map[string]storedVerdict,
	now int64,
) (carried, split int, err error) {
	// Where each old signature's rows landed under the current rules.
	// An old signature with more than one destination has split, and
	// nothing is carried from it. An empty destination counts: a row
	// the current rules cannot sign at all did not land with the rest.
	landed := map[string]map[string]bool{}
	for _, r := range rows {
		if prev, ok := old[r.key]; ok {
			if landed[prev.signature] == nil {
				landed[prev.signature] = map[string]bool{}
			}
			landed[prev.signature][r.signature] = true
		}
	}
	for sig, dests := range landed {
		if len(dests) < 2 {
			continue
		}
		if verdict, ok := verdicts[sig]; ok && verdict.version < SignatureVersion {
			split++
		}
	}

	type carry struct {
		signature string
		verdict   storedVerdict
	}
	var (
		plan    []carry
		claimed = map[string]bool{}
	)
	for _, r := range rows {
		prev, ok := old[r.key]
		if !ok || r.signature == "" || prev.signature == r.signature {
			continue
		}
		if len(landed[prev.signature]) > 1 {
			continue // a split: the old key was an artefact, see above
		}
		if _, exists := verdicts[r.signature]; exists || claimed[r.signature] {
			continue
		}
		verdict, ok := verdicts[prev.signature]
		if !ok || verdict.version >= SignatureVersion {
			continue
		}
		claimed[r.signature] = true
		plan = append(plan, carry{r.signature, verdict})
	}
	if len(plan) == 0 {
		return 0, split, nil
	}

	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO `+fam.storeTable+` (
            `+fam.storeSignatureCol+`, `+fam.storeNameCol+`, `+fam.storeDetailedCol+`,
            signature_version, assigned_at, model_name
        ) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: prepare verdict re-key: %w", fam.name, err)
	}
	defer stmt.Close()
	for _, p := range plan {
		if _, err := stmt.ExecContext(ctx, p.signature, p.verdict.name, p.verdict.detailed,
			SignatureVersion, now, p.verdict.modelName); err != nil {
			return 0, 0, fmt.Errorf("%s: re-key verdict onto %q: %w", fam.name, p.signature, err)
		}
	}
	return len(plan), split, nil
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
