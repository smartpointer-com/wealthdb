package spending

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
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
	// of a non-spending kind in; Exclude fences a cash or card account
	// out. They are stamped into spend_account_scope at the start of
	// the pass, so the report macros need no runtime config injection.
	Include map[string][]string
	Exclude map[string][]string

	// MatchWindowDays and MatchTolerancePct are the internal-transfer
	// matcher's knobs, already defaulted by the caller.
	MatchWindowDays   int
	MatchTolerancePct float64

	// Rules are the config's compiled `spending.rules`: narratives
	// that name the holder's own destinations at institutions gold does
	// not track, and what such a row is. A match is placed by the rule
	// tier, after the built-in rules and below the matcher. Nil marks
	// nothing.
	Rules []Rule

	// Pins are the rows of the `spending.pins` ledger. Each is stamped
	// onto every transaction it describes, with provenance `manual`,
	// after every other tier. Nil pins nothing.
	Pins []Pin

	// Now overrides the assignment timestamp. Zero means time.Now();
	// tests set it so a pass's output is byte-comparable across runs.
	Now int64
}

// Result reports what the pass did. The counts are the pass's own
// observability: a build that stops reaching rows it used to reach
// shows up here before it shows up in a chart.
type Result struct {
	// ScopeRows is the number of spend_account_scope overrides stamped.
	ScopeRows int
	// Population is the number of rows in the enrichment population.
	Population int
	// Enriched is the number of enrichment rows written: the
	// population, plus the matched legs and pinned rows that sit
	// outside it (a card payment is an own-account move but never a
	// spending line).
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
	// `spending.accounts` entries naming an account gold does not
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
	// RekeyedMerchants counts merchant verdicts carried forward onto a
	// new signature by a SignatureVersion bump.
	RekeyedMerchants int
	// SplitMerchants counts older-version merchant verdicts a
	// SignatureVersion bump left behind: the old signature's rows now
	// land on several new signatures, so the verdict was about a shape
	// several merchants shared rather than about any one of them, and
	// it is carried onto none. Those merchants are back in the backlog.
	SplitMerchants int
}

// RunDeterministicPass recomputes every deterministic spend verdict in
// gold, in one transaction, and returns what it did.
//
// The order of the phases is load-bearing. The account scope is
// stamped FIRST because every population macro reads it, so a scope
// edit takes effect in the same pass that applies it. The old
// signatures are captured BEFORE the delete, because the
// signature-version re-key needs to know where a verdict used to hang.
// The matcher runs over its own broader pool: every account in gold,
// and the income-side kinds the spending population excludes. Without
// the income side a card payment's funding leg has nothing to pair
// with; without the unscoped accounts, neither does a transfer into an
// investment account. Pins are resolved against the whole of
// `transactions`, because a pin describes a row by what a statement
// shows and owes nothing to any population.
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
	res.ScopeRows, res.UnresolvedScopeAccounts, err =
		syncAccountScope(ctx, tx, opts.Include, opts.Exclude)
	if err != nil {
		return nil, err
	}

	kinds, err := loadSilverKinds(ctx, tx)
	if err != nil {
		return nil, err
	}
	population, err := loadPopulation(ctx, tx)
	if err != nil {
		return nil, err
	}
	res.Population = len(population)

	legs, poolNarratives, err := loadMatcherPool(ctx, tx)
	if err != nil {
		return nil, err
	}
	matched := matchInternalTransfers(legs, opts.MatchWindowDays, opts.MatchTolerancePct)

	pinned, unmatched, err := resolvePins(ctx, tx, opts.Pins)
	if err != nil {
		return nil, err
	}
	res.UnmatchedPins = unmatched

	oldSignatures, err := loadExistingSignatures(ctx, tx)
	if err != nil {
		return nil, err
	}
	merchants, err := loadMerchantStore(ctx, tx)
	if err != nil {
		return nil, err
	}

	if err := deleteEnrichmentRows(ctx, tx); err != nil {
		return nil, err
	}

	rows := assignCategories(population, poolNarratives, matched, kinds, opts.Rules, pinned, res)
	if err := insertEnrichment(ctx, tx, rows, now); err != nil {
		return nil, err
	}
	res.Enriched = len(rows)

	res.RekeyedMerchants, res.SplitMerchants, err = rekeyMerchants(ctx, tx, rows, oldSignatures, merchants, now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("spending: commit: %w", err)
	}
	committed = true
	return res, nil
}

// ---- phase 1: the account scope ------------------------------------------

// syncAccountScope replaces spend_account_scope with what the config
// declares, on the SetFxPriorities precedent: configuration is stamped
// into gold so the SQL layer can honour it without a runtime injection
// point, and a whole re-stamp means removing an entry from the config
// removes it from gold rather than leaving it behind to haunt a later
// report. It returns the rows stamped and how many of them name an
// account gold does not hold.
//
// The key is an account id, never a nickname or a display name
// (docs/DESIGN.md §5.1), and `spend_scoped_accounts()` joins the
// stamped row to `accounts` on exactly that id: an entry naming
// anything else matches no account and so widens or fences nothing.
// Every entry is stamped whatever it resolves to — the scope table is
// the config's whole state, and dropping an entry here would hide the
// typo instead of surfacing it — and the unresolved ones are counted
// for the caller to report, because a scope knob that silently does
// nothing is the wrong failure mode for the surface that decides which
// accounts the model tier may ever see.
func syncAccountScope(ctx context.Context, tx *sql.Tx, include, exclude map[string][]string) (stamped, unresolved int, err error) {
	known, err := loadAccountIDs(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM spend_account_scope`); err != nil {
		return 0, 0, fmt.Errorf("spending: clear account scope: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO spend_account_scope (silver_source_id, account_external_id, mode)
        VALUES (?, ?, ?)`)
	if err != nil {
		return 0, 0, fmt.Errorf("spending: prepare account scope: %w", err)
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
					return 0, 0, fmt.Errorf("spending: stamp account scope %s/%s: %w", source, id, err)
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
func loadAccountIDs(ctx context.Context, tx querier) (map[string]map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT DISTINCT silver_source_id, account_external_id FROM accounts`)
	if err != nil {
		return nil, fmt.Errorf("spending: read account ids: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]struct{}{}
	for rows.Next() {
		var source, id string
		if err := rows.Scan(&source, &id); err != nil {
			return nil, fmt.Errorf("spending: scan account ids: %w", err)
		}
		if out[source] == nil {
			out[source] = map[string]struct{}{}
		}
		out[source][id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("spending: iterate account ids: %w", err)
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
	key              txKey
	counterparty     string
	description      string
	providerCategory string
}

func loadPopulation(ctx context.Context, tx querier) ([]candidate, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id,
               COALESCE(counterparty, ''), COALESCE(description, ''),
               COALESCE(provider_category, '')
          FROM spend_enrichment_population(?, ?)
         ORDER BY silver_source_id, transaction_external_id`, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, fmt.Errorf("spending: read enrichment population: %w", err)
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var r candidate
		if err := rows.Scan(&r.key.source, &r.key.txID,
			&r.counterparty, &r.description, &r.providerCategory); err != nil {
			return nil, fmt.Errorf("spending: scan enrichment population: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadMatcherPool reads the matcher's legs and, alongside them, the
// narrative of each leg. A matched leg outside the spending population
// still gets an enrichment row, and that row needs a signature like
// any other.
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
func loadMatcherPool(ctx context.Context, tx querier) ([]gold.TransferLeg, map[txKey]candidate, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT silver_source_id, account_external_id, transaction_external_id,
               occurred_at, currency, CAST(net_amount AS DOUBLE),
               COALESCE(counterparty, ''), COALESCE(description, '')
          FROM spend_matcher_pool(?, ?)
         ORDER BY silver_source_id, transaction_external_id`, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, nil, fmt.Errorf("spending: read matcher pool: %w", err)
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
			return nil, nil, fmt.Errorf("spending: scan matcher pool: %w", err)
		}
		if !amount.Valid {
			// A leg with no amount cannot be oriented, so it can neither
			// fund nor be funded.
			continue
		}
		leg.Day = gold.EpochDay(occurredAt)
		leg.Amt = amount.Float64
		legs = append(legs, leg)
		row.key = txKey{leg.Group, leg.ID}
		narratives[row.key] = row
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return legs, narratives, nil
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

func loadExistingSignatures(ctx context.Context, tx *sql.Tx) (map[txKey]signatureAt, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id, merchant_signature, signature_version
          FROM spend_txn_enrichment
         WHERE merchant_signature IS NOT NULL AND merchant_signature <> ''`)
	if err != nil {
		return nil, fmt.Errorf("spending: read existing signatures: %w", err)
	}
	defer rows.Close()
	out := map[txKey]signatureAt{}
	for rows.Next() {
		var (
			k txKey
			s signatureAt
		)
		if err := rows.Scan(&k.source, &k.txID, &s.signature, &s.version); err != nil {
			return nil, fmt.Errorf("spending: scan existing signatures: %w", err)
		}
		out[k] = s
	}
	return out, rows.Err()
}

// merchantVerdict is one row of the global merchant store, read for
// the signature-version re-key.
type merchantVerdict struct {
	name      string
	detailed  string
	version   int
	modelName string
}

func loadMerchantStore(ctx context.Context, tx *sql.Tx) (map[string]merchantVerdict, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT merchant_signature, merchant_name, spend_detailed, signature_version, model_name
          FROM spend_merchant_categories`)
	if err != nil {
		return nil, fmt.Errorf("spending: read merchant store: %w", err)
	}
	defer rows.Close()
	out := map[string]merchantVerdict{}
	for rows.Next() {
		var (
			sig string
			v   merchantVerdict
		)
		if err := rows.Scan(&sig, &v.name, &v.detailed, &v.version, &v.modelName); err != nil {
			return nil, fmt.Errorf("spending: scan merchant store: %w", err)
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
func deleteEnrichmentRows(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM spend_txn_enrichment`); err != nil {
		return fmt.Errorf("spending: clear enrichment rows: %w", err)
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
// Rows outside the spending population get a verdict only when the
// matcher paired them or a pin named them; nothing else has any
// business categorising a deposit.
func assignCategories(
	population []candidate,
	pool map[txKey]candidate,
	matched map[txKey]bool,
	kinds map[string]string,
	rules []Rule,
	pinned map[txKey]pinnedRow,
	res *Result,
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
			if detailed, ok, drift := ProviderCategory(kinds[r.key.source], r.providerCategory); ok {
				row.detailed, row.provenance = detailed, ProvenanceProvider
			} else if drift {
				res.UnmappedProviderCategories++
			}
			if detailed, label, ok := RuleCategory(row.signature, r.counterparty, r.description, r.providerCategory); ok {
				row.detailed, row.provenance = detailed, ProvenanceRule
				row.merchantLabel = label
			} else if detailed, ok := ConfigRuleCategory(rules, r.counterparty, r.description); ok {
				row.detailed, row.provenance = detailed, ProvenanceRule
			}
		}
		// A tier above the rule clears the label with the verdict it
		// replaces. The label says which issuer a card bill was paid
		// to, which is only true of a row this pass filed as a card
		// bill: on a bill whose card the matcher paired, or on a row
		// the holder pinned as something else, it would name an issuer
		// on a line that is no longer a card bill at all.
		if matched[r.key] {
			row.detailed = canonical.SpendDetailedInternalTransfer
			row.provenance = ProvenanceMatcher
			row.merchantLabel = ""
		}
		if p, ok := pinned[r.key]; ok {
			row.detailed = p.detailed
			row.provenance = ProvenanceManual
			row.merchantLabel = ""
		}
		out = append(out, row)

		switch row.provenance {
		case ProvenanceManual:
			res.PinRows++
		case ProvenanceMatcher:
			res.MatcherRows++
		case ProvenanceRule:
			res.RuleRows++
		case ProvenanceProvider:
			res.ProviderRows++
		default:
			res.SignatureOnlyRows++
		}
	}

	for _, r := range population {
		emit(r, true)
	}
	// Matched legs outside the population — a card payment, the deposit
	// side of a funding wire — are marked too. Both halves of a pair
	// carry the verdict, so a later report that widens the population
	// cannot start counting one of them as real money movement.
	for _, key := range sortedTxKeys(matched) {
		if r, ok := pool[key]; ok {
			emit(r, false)
		}
	}
	// Pinned rows outside both: a pin describes a row by what a
	// statement shows, and the holder may name a row on any account.
	for _, key := range sortedTxKeys(pinned) {
		emit(pinned[key].row, false)
	}
	return out
}

// insertEnrichment writes the overlay: the whole population plus the
// matched legs and pinned rows outside it, through gold.InsertChunked
// — the same multi-row VALUES batching gold's writer gives the fact
// tables. This is the pass's one bulk write and it runs after every
// load, so the shape matters: DuckDB's per-statement cost dwarfs its
// per-row cost, and a statement per row is the slowest way to feed it.
//
// An error names a row range rather than a transaction: assignCategories
// emits each key once and the table is emptied first, so a primary-key
// collision is not among the ways this can fail.
func insertEnrichment(ctx context.Context, tx *sql.Tx, rows []enrichmentRow, now int64) error {
	const head = `INSERT INTO spend_txn_enrichment (
            silver_source_id, transaction_external_id, merchant_signature,
            signature_version, spend_detailed, provenance, merchant_label,
            assigned_at
        ) VALUES `
	return gold.InsertChunked(ctx, tx, "spending: write enrichment", head,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, len(rows),
		func(i int, args []any) []any {
			r := &rows[i]
			return append(args, r.key.source, r.key.txID, nullableString(r.signature),
				SignatureVersion, nullableString(r.detailed), r.provenance,
				nullableString(r.merchantLabel), now)
		})
}

// ---- phase 4: the signature-version re-key ---------------------------------

// rekeyMerchants carries paid-for merchant verdicts across a
// normalisation change.
//
// The merchant store is keyed by signature alone, and a model verdict
// in it cost real money. When Normalize starts producing a different
// string for the same merchant — a SignatureVersion bump — every one
// of those verdicts would otherwise be orphaned behind a key nothing
// will ever compute again, and the model tier would buy the same
// answers a second time.
//
// A verdict is carried forward when all five hold: the row's signature
// actually moved, EVERY row that carried the old signature now carries
// the same new one, the store holds a verdict at the OLD signature,
// that verdict predates the current SignatureVersion, and the NEW
// signature has no verdict yet.
//
// The one-to-one condition is what keeps a carry from spreading an
// artefact. A refinement of a genuine merchant's signature — a store
// number dropped, a processor prefix stripped — moves all of its rows
// together, and a verdict about that merchant is as true at the new
// key as at the old. When the old signature's rows SPLIT across
// several new keys, the old key never was a merchant: it was a shape
// several creditors shared (the direct-debit notice version 2 strips
// covers a card issuer and a telecom alike; the e-bill marker version
// 4 strips, a utility and an insurer), and the verdict bought at it
// describes the shape, not any creditor now visible. Carried by old
// key alone it would file the telecom under whatever the model made
// of the notice. Such a verdict is left where it is, unreachable, and
// counted, so those merchants show up in the backlog and are re-asked.
// A partial move — some rows stay at the old key while others leave —
// is a split too: the rows that stayed keep the verdict at the key it
// was bought at, and the rows that left get nothing.
//
// The last condition (no verdict at the new key yet) is what makes
// this safe to run every pass: an existing verdict at the new key is
// never overwritten, so a real re-categorisation is not undone by a
// stale copy. model_name is preserved verbatim — a carried verdict
// must still say which model produced the answer, not claim to be new
// work.
//
// Ordering is deterministic (the rows arrive sorted), so when two old
// signatures collapse onto one new signature the first wins and the
// result does not depend on map iteration.
func rekeyMerchants(
	ctx context.Context,
	tx *sql.Tx,
	rows []enrichmentRow,
	old map[txKey]signatureAt,
	merchants map[string]merchantVerdict,
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
		if verdict, ok := merchants[sig]; ok && verdict.version < SignatureVersion {
			split++
		}
	}

	type carry struct {
		signature string
		verdict   merchantVerdict
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
		if _, exists := merchants[r.signature]; exists || claimed[r.signature] {
			continue
		}
		verdict, ok := merchants[prev.signature]
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
        INSERT INTO spend_merchant_categories (
            merchant_signature, merchant_name, spend_detailed,
            signature_version, assigned_at, model_name
        ) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, 0, fmt.Errorf("spending: prepare merchant re-key: %w", err)
	}
	defer stmt.Close()
	for _, p := range plan {
		if _, err := stmt.ExecContext(ctx, p.signature, p.verdict.name, p.verdict.detailed,
			SignatureVersion, now, p.verdict.modelName); err != nil {
			return 0, 0, fmt.Errorf("spending: re-key merchant verdict onto %q: %w", p.signature, err)
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
