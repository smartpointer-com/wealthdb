package spending

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

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
	// as the merchant of a delta line.
	merchantLabel string
	// providerDetailed is what the issuer's own filing of this row
	// translates to, recorded whether or not the provider tier claimed
	// the row and whether or not a tier above overruled it. It is the
	// faithful record of the card provider's view — never a verdict of
	// ours — so a report can show it beside our own and the
	// disagreement is a number rather than a silent overwrite. Empty
	// where the issuer published nothing this build translates.
	providerDetailed string
	// The far side of the move, which the pass links rather than
	// writes to the overlay (writeTransferLinks).
	//
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
	// farTxID is the partner leg of a pair, and farEvidence the road
	// that named the far side of a one-legged move: a rule or the
	// source's stated counter account. A pair's evidence is its phase
	// (writeTransferLinks).
	farTxID, farEvidence string
}

// farRuleReaches reports whether one of the far-naming rules fires on
// a row, so the loop that emits such rows outside the population emits
// only those: every other pool row is some other tier's, or nobody's.
func farRuleReaches(farRules []Rule, r candidate) bool {
	_, ok := ConfigRuleCategory(farRules, r.ruleRow())
	return ok
}

// rulesWithFar is the subset of a rule list that names a far account.
func rulesWithFar(rules []Rule) []Rule {
	var out []Rule
	for _, r := range rules {
		if r.Far != "" {
			out = append(out, r)
		}
	}
	return out
}

// ruleRow is the candidate as the config rule tier reads it.
func (r candidate) ruleRow() RuleRow {
	return RuleRow{
		Counterparty: r.counterparty, Description: r.description,
		ProviderCategory: r.providerCategory,
		Source:           r.key.source, Portfolio: r.portfolio,
		Account: r.account, OccurredAt: r.occurredAt,
	}
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
// matcher paired them, a pin named them, or the other family's rule
// said where they went; nothing else has any business categorising a
// row the family does not chart. On the income side only the pin route
// exists, the matcher's legs and the far accounts being the spending
// pass's to emit.
func assignCategories(
	fam family,
	population []candidate,
	pool map[txKey]candidate,
	matched map[txKey]gold.TransferLeg,
	stated map[txKey]farAccount,
	kinds map[string]string,
	rules, farRules []Rule,
	pinned, otherPinned map[txKey]pinnedRow,
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
			if detailed, label, farClass, ok := fam.builtinRule(builtinFacts{
				kind: r.kind, accountKind: r.accountKind,
				signature: row.signature, counterparty: r.counterparty, description: r.description,
				providerCategory: r.providerCategory, filed: row.providerDetailed,
			}); ok {
				row.detailed, row.provenance = detailed, ProvenanceRule
				if fam.labelCol != "" {
					row.merchantLabel = label
				}
				if fam.linksFar {
					row.farClass, row.farEvidence = farClass, evidenceRule
				}
			} else if placement, ok := ConfigRuleCategory(rules, r.ruleRow()); ok {
				row.detailed, row.provenance = placement.Category, ProvenanceRule
				row.statedAssetClass = placement.AssetClass
				if placement.Far != "" && fam.linksFar {
					row.farSource, row.farAccount = canonical.DeclaredSourceID, placement.Far
					row.farEvidence = evidenceRule
				}
			}
		} else if placement, ok := ConfigRuleCategory(farRules, r.ruleRow()); ok {
			// The other family's word on a row outside this population:
			// the verdict is that family's to place on its own overlay,
			// and only where the money went is written here.
			row.detailed, row.provenance = placement.Category, ProvenanceRule
			row.farSource, row.farAccount = canonical.DeclaredSourceID, placement.Far
			row.farEvidence = evidenceRule
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
			if fam.linksFar {
				row.farSource, row.farAccount = partner.Group, partner.Owner
				row.farTxID = partner.ID
			}
		} else if far, ok := stated[r.key]; ok && fam.linksFar {
			// No pairing, but the source named the far account itself.
			// Only the ACCOUNT is taken: the verdict stays whatever the
			// tiers decided, because "where it went" and "what it was"
			// are different questions and this evidence answers only the
			// first. A rule's far CLASS is a stand-in for exactly this
			// and gives way to it, the way it gives way to a pairing.
			row.farSource, row.farAccount = far.source, far.account
			row.farClass, row.farEvidence = "", evidenceStatedAccount
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
		// The legs the other family's far-naming rules reach — an
		// inbound move from a declared account — for the same reason:
		// the far side is linked from this family's rows alone. A row
		// this family pinned is skipped here because the pin loop below
		// emits it: the far rules reach it there, with its own account,
		// portfolio and date, and the pin then sets the verdict. A row
		// only the other family pinned is not emitted at all: that pin
		// is the whole answer for a row a rule would otherwise call an
		// own-account move.
		if len(farRules) > 0 {
			for _, key := range sortedTxKeys(pool) {
				_, mine := pinned[key]
				_, theirs := otherPinned[key]
				if mine || theirs {
					continue
				}
				if r := pool[key]; farRuleReaches(farRules, r) {
					emit(r, false)
				}
			}
		}
	}
	// Pinned rows outside both: a pin describes a row by what a
	// statement shows, and the holder may name a row on any account.
	// Each carries the scope facts matchNaturalKeys read for it, so a
	// scoped far rule tests it as it tests any other row.
	for _, key := range sortedTxKeys(pinned) {
		emit(pinned[key].row, false)
	}
	return out
}

// insertEnrichment writes the overlay: the whole population, plus the
// pinned rows outside it and, on the family that emits them, the
// matched legs. It goes through gold.InsertChunked, the same multi-row
// VALUES batching gold's writer gives the fact tables. This is the
// pass's largest bulk write and it runs after every load, so the shape
// matters: DuckDB's per-statement cost dwarfs its per-row cost, and a
// statement per row is the slowest way to feed it.
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
			args = append(args, nullableString(r.statedAssetClass))
			return append(args, nullableString(r.providerDetailed), now)
		})
}

// writeTransferLinks rewrites transfer_links from the spending family's
// rows: one link per row whose far account or far class is known. A
// leg of a pair links to its partner leg, with the pair's phase as its
// evidence; any other row links to its far account alone.
//
// It goes through the same gold.InsertChunked batching as
// insertEnrichment.
func writeTransferLinks(ctx context.Context, tx *sql.Tx, rows []enrichmentRow, pairs []gold.TransferMatchPair) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM transfer_links`); err != nil {
		return fmt.Errorf("spending: clear transfer links: %w", err)
	}
	phases := make(map[txKey]gold.TransferMatchPhase, len(pairs)*2)
	for _, p := range pairs {
		phases[txKey{p.Debit.Group, p.Debit.ID}] = p.By
		phases[txKey{p.Credit.Group, p.Credit.ID}] = p.By
	}
	links := make([]*enrichmentRow, 0, len(pairs)*2)
	for i := range rows {
		if rows[i].farAccount != "" || rows[i].farClass != "" {
			links = append(links, &rows[i])
		}
	}
	return gold.InsertChunked(ctx, tx, "spending: write transfer links", `
        INSERT INTO transfer_links (silver_source_id, transaction_external_id,
                                    far_silver_source_id, far_account_external_id,
                                    far_transaction_external_id, far_class,
                                    evidence, measure) VALUES `,
		"(?, ?, ?, ?, ?, ?, ?, ?)", len(links),
		func(i int, args []any) []any {
			r := links[i]
			evidence := r.farEvidence
			if r.farTxID != "" {
				evidence = phaseEvidence[phases[r.key]]
			}
			return append(args, r.key.source, r.key.txID,
				nullableString(r.farSource), nullableString(r.farAccount),
				nullableString(r.farTxID), nullableString(r.farClass),
				evidence, measureCash)
		})
}

// countLinkedPairs reads the load summary's pair counters off
// transfer_links: the pairs each phase the summary names formed. A pair
// is two links, one per leg, with one evidence. What the counters are
// for is the summary: a road whose traffic nobody can see is a road
// nobody can tell has stopped carrying any.
func countLinkedPairs(ctx context.Context, tx querier, out *CashflowResult) error {
	rows, err := tx.QueryContext(ctx, `
        SELECT evidence, COUNT(*) // 2
          FROM transfer_links
         WHERE far_transaction_external_id IS NOT NULL
         GROUP BY evidence`)
	if err != nil {
		return fmt.Errorf("spending: count linked pairs: %w", err)
	}
	defer rows.Close()
	counters := map[gold.TransferMatchPhase]*int{
		gold.MatchedByReference:     &out.ReferencePairs,
		gold.MatchedByStatedCounter: &out.StatedCounterPairs,
		gold.MatchedByName:          &out.NamedPairs,
		gold.MatchedByReversal:      &out.ReversalPairs,
		gold.MatchedByConversion:    &out.ConvertedPairs,
	}
	for rows.Next() {
		var evidence string
		var n int
		if err := rows.Scan(&evidence, &n); err != nil {
			return fmt.Errorf("spending: scan linked pairs: %w", err)
		}
		if c, ok := counters[evidencePhase[evidence]]; ok {
			*c = n
		}
	}
	return rows.Err()
}
