package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/spending"
)

// The run report.
//
// A categorisation run's own numbers — counterparties asked, rows rejected,
// verdicts stored — say whether the model behaved. They say nothing
// about whether the PICTURE is right, and that is the question actually
// being asked. Those counters, and the stratified sample of what is
// still uncategorised, print per family.
//
// The four canaries below do not. They read the SPENDING population, so
// they are collected once and printed with the spending family; an
// income run prints its own counters and nothing here. Each is aimed at
// a way the picture can be quietly wrong:
//
//   - the per-source categorisation rate says whether one source is
//     falling behind the others, which is usually a normalisation or
//     an adapter problem rather than a model one;
//   - provider-map misses say a card issuer's categorical vocabulary
//     has moved and the map in internal/spending has not. A bank's
//     booking types are not categorical — most name a payment rail —
//     so a rail the map does not translate is not a miss;
//   - the matched-pair listing is the audit surface for the matcher,
//     the one tier that REMOVES rows from spending. Its mistakes are
//     invisible in a chart — an over-eager pair makes a month cheaper,
//     it does not make a category wrong — so both legs of every pair
//     are listed. Every pair that removed something, that is: the
//     matcher pool is every account in gold, so most of what it pairs
//     was never a spending candidate (wallet to wallet, brokerage to
//     brokerage), and those are counted on one line rather than
//     listed, so the screenful is the audit and nothing is hidden;
//   - the large unmatched legs are the mirror image: a big one-legged
//     movement is either real spending or a pair the matcher missed.
//     Cross-currency shapes get their own line because the amount pass
//     partitions by native currency and cannot pair them however obvious
//     the pairing looks to a human. What CAN pair them is a reference the
//     source stamped on both legs, so a shape listed here is one no such
//     reference reached — the pairing is not refused, the evidence for it
//     is simply absent.

// canaryListLimit caps each canary listing. The point is a signal a
// human will actually read, not a dump; every listing says how much it
// elided.
const canaryListLimit = 10

// pairListLimit caps the matched-pair listing, which is the audit
// surface and therefore worth more lines than the others.
const pairListLimit = 20

// sourceRate is one source's share of categorised spending rows.
type sourceRate struct {
	Source      string
	Total       int
	Categorised int
}

// providerMiss is one value a source with a categorical provider
// vocabulary published that this build does not translate.
type providerMiss struct {
	Source   string
	Category string
	Count    int
}

// crossCurrencyShape is a pair of unmatched legs that look like two
// halves of one movement but differ in native currency — the shape the
// amount pass cannot pair, and that no shared reference reached.
type crossCurrencyShape struct {
	Debit  spending.Leg
	Credit spending.Leg
}

// spendCanaries is everything the run report knows that the run itself
// did not produce.
type spendCanaries struct {
	Rates          []sourceRate
	ProviderMisses []providerMiss
	// Pairs are the matched pairs with at least one leg in the
	// spending population — what the matcher removed from spending.
	// PairsOutsideSpending counts the rest, which removed nothing and
	// are not listed.
	Pairs                []spending.Pair
	PairsOutsideSpending int
	Unmatched            []spending.Leg
	CrossCurrency        []crossCurrencyShape
}

// collectSpendCanaries reads gold for everything the report prints
// beside the run's own counters.
func collectSpendCanaries(ctx context.Context, db *sql.DB, cfg *config.Config) (*spendCanaries, error) {
	out := &spendCanaries{}
	var err error
	if out.Rates, err = collectSourceRates(ctx, db); err != nil {
		return nil, err
	}
	if out.ProviderMisses, err = collectProviderMisses(ctx, db); err != nil {
		return nil, err
	}
	m := cfg.SpendMatching()
	overrideRules, err := gold.ParseTransferOverrideLedger(cfg.SpendTransferOverrides())
	if err != nil {
		return nil, err
	}
	pairs, unmatched, err := spending.MatchedPairs(ctx, db, m.Window(), m.Tolerance(), matchNames(m), overrideRules)
	if err != nil {
		return nil, fmt.Errorf("categorize: matcher audit: %w", err)
	}
	out.Pairs, out.PairsOutsideSpending = splitPairsBySpending(pairs)
	out.Unmatched = unmatched
	out.CrossCurrency = findCrossCurrencyShapes(out.Unmatched, m.Window())
	return out, nil
}

// splitPairsBySpending keeps the pairs that removed something from
// spending, in the order the matcher reported them, and counts the
// rest. The pool spans every account in gold, so most of what it pairs
// was never a spending candidate; listing those would bury the pairs
// the audit exists for, and dropping them silently would hide that the
// matcher acted at all.
func splitPairsBySpending(pairs []spending.Pair) (listed []spending.Pair, outside int) {
	for _, p := range pairs {
		if p.RemovedFromSpending() {
			listed = append(listed, p)
		} else {
			outside++
		}
	}
	return listed, outside
}

// collectSourceRates counts, per source, how much of the spending
// population has a category at all. The resolved category comes from
// spend_txn_categories(), the lattice's one definition (SPENDING.md
// §3), so the rate counts exactly what a report would call
// categorised — whichever scope answered.
func collectSourceRates(ctx context.Context, db *sql.DB) ([]sourceRate, error) {
	rows, err := db.QueryContext(ctx, `
SELECT p.silver_source_id,
       COUNT(*)                 AS total,
       COUNT(c.spend_detailed)  AS categorised
  FROM spend_enrichment_population(?, ?) p
  LEFT JOIN spend_txn_categories() c
    ON c.silver_source_id        = p.silver_source_id
   AND c.transaction_external_id = p.transaction_external_id
 GROUP BY p.silver_source_id
 ORDER BY p.silver_source_id`, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, fmt.Errorf("categorize: read categorisation rates: %w", err)
	}
	defer rows.Close()
	var out []sourceRate
	for rows.Next() {
		var r sourceRate
		if err := rows.Scan(&r.Source, &r.Total, &r.Categorised); err != nil {
			return nil, fmt.Errorf("categorize: scan categorisation rate: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// collectProviderMisses finds the values a source with a categorical
// provider vocabulary published that the map does not translate. A
// source with NO mapped vocabulary contributes nothing, and neither
// does an untranslated rail in a bank's booking types: there is
// nothing for either to be unmapped against, and counting them would
// drown the signal this canary exists for (spending.ProviderCategory
// makes the distinction).
func collectProviderMisses(ctx context.Context, db *sql.DB) ([]providerMiss, error) {
	rows, err := db.QueryContext(ctx, `
SELECT p.silver_source_id, s.silver_kind, COALESCE(p.account_kind, ''),
       p.provider_category, COUNT(*) AS n
  FROM spend_enrichment_population(?, ?) p
  JOIN silver_sources s ON s.silver_source_id = p.silver_source_id
 WHERE p.provider_category IS NOT NULL
   AND p.provider_category <> ''
 GROUP BY p.silver_source_id, s.silver_kind, p.account_kind,
          p.provider_category
 ORDER BY n DESC, p.silver_source_id, p.provider_category`, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, fmt.Errorf("categorize: read provider categories: %w", err)
	}
	defer rows.Close()
	var out []providerMiss
	for rows.Next() {
		var (
			source, kind, accountKind, category string
			n                                   int
		)
		if err := rows.Scan(&source, &kind, &accountKind, &category, &n); err != nil {
			return nil, fmt.Errorf("categorize: scan provider category: %w", err)
		}
		// The vocabulary is per product, so the account kind decides
		// which of a source's vocabularies this value is measured
		// against — a bank's rails are not drift, a card's are.
		if _, _, drift := spending.ProviderCategory(kind, accountKind, category); drift {
			out = append(out, providerMiss{Source: source, Category: category, Count: n})
		}
	}
	return out, rows.Err()
}

// findCrossCurrencyShapes pairs up unmatched legs that differ only in
// currency: opposite signs, within the matcher's day window, different
// native currencies. Amounts are deliberately NOT compared — doing so
// would need an FX rate, and the reason the amount pass cannot reach these
// is that converting would make the same movement pair differently per
// output currency.
//
// It walks the UNMATCHED legs, so a conversion the reference phase paired
// has already left this scan. What is left is the residue: a movement whose
// two legs no source stamped with one reference, and which therefore still
// needs a person to say whether either half is really spending.
//
// The scan is quadratic, so it runs over the largest legs only (the
// input arrives sorted by descending magnitude). A small cross-currency
// transfer that goes unreported here costs nothing; a large one is
// exactly what this is for.
func findCrossCurrencyShapes(unmatched []spending.Leg, windowDays int) []crossCurrencyShape {
	const scanDepth = 200
	legs := unmatched
	if len(legs) > scanDepth {
		legs = legs[:scanDepth]
	}
	var out []crossCurrencyShape
	claimed := make(map[string]bool, len(legs))
	key := func(l spending.Leg) string { return l.Source + "\x00" + l.TxID }
	for i := range legs {
		if legs[i].Amount >= 0 || claimed[key(legs[i])] {
			continue
		}
		for j := range legs {
			if legs[j].Amount <= 0 || claimed[key(legs[j])] {
				continue
			}
			if legs[i].Currency == legs[j].Currency {
				continue
			}
			if diff := legs[i].Day - legs[j].Day; diff > int64(windowDays) || diff < -int64(windowDays) {
				continue
			}
			claimed[key(legs[i])] = true
			claimed[key(legs[j])] = true
			out = append(out, crossCurrencyShape{Debit: legs[i], Credit: legs[j]})
			break
		}
		if len(out) == canaryListLimit {
			break
		}
	}
	return out
}

// ---- printing ----------------------------------------------------------------

// printCategorizeSummary writes the run's own numbers, aggregated
// across every batch, followed by the canaries. Run on both the
// dry-run and the write path, before the per-row plan / persist
// block, as resolve-symbols does.
func printCategorizeSummary(
	w io.Writer,
	fam categorizeFamily,
	candidates []merchantCandidate,
	valid []categorization,
	leftovers []merchantCandidate,
	batches, attempts, totalInvalid int,
	canaries *spendCanaries,
) {
	coveredTxns := 0
	byCategory := map[string]int{}
	done := make(map[string]bool, len(valid))
	for _, v := range valid {
		done[v.Signature] = true
		byCategory[v.Detailed]++
	}
	for _, c := range candidates {
		if done[c.Signature] {
			coveredTxns += c.Txns
		}
	}

	fmt.Fprintln(w, "categorize: summary")
	// The label column is twenty wide, so the family's own noun lines
	// up with the fixed labels under it rather than shifting the whole
	// block by the length of one word.
	fmt.Fprintf(w, "  %-20s%d (%d transaction(s))\n", fam.plural()+" asked:",
		len(candidates), totalCandidateTxns(candidates))
	fmt.Fprintf(w, "  LLM attempts:       %d over %d batch(es)\n", attempts, batches)
	fmt.Fprintf(w, "  rejected rows:      %d (failed validation across all attempts)\n", totalInvalid)
	fmt.Fprintf(w, "  categorised:        %d %s(s), covering %d transaction(s)\n", len(valid), fam.counterparty, coveredTxns)
	fmt.Fprintf(w, "  left uncategorised: %d %s(s)\n", len(leftovers), fam.counterparty)
	if len(byCategory) > 0 {
		fmt.Fprintf(w, "  distinct categories used: %d\n", len(byCategory))
	}

	if len(leftovers) > 0 {
		sample := stratifiedSample(leftovers, canaryListLimit, merchantCandidate.DominantSource)
		fmt.Fprintf(w, "  sample of uncategorised %s (%d of %d, mixed across sources):\n", fam.plural(), len(sample), len(leftovers))
		for _, c := range sample {
			fmt.Fprintf(w, "    %s [%s, %d txn(s)]\n", c.Signature, c.DominantSource(), c.Txns)
		}
		// The tail is already in memory, so it is printed rather
		// than pointed at: the sample is the readable head, and
		// everything it left out follows one signature per line.
		// No other surface lists this set — the dry-run plan walks
		// the verdicts the model ACCEPTED, and pays a full model
		// pass to do it.
		if len(leftovers) > len(sample) {
			inSample := make(map[string]bool, len(sample))
			for _, c := range sample {
				inSample[c.Signature] = true
			}
			fmt.Fprintf(w, "    ... and %d more:\n", len(leftovers)-len(sample))
			for _, c := range leftovers {
				if !inSample[c.Signature] {
					fmt.Fprintf(w, "    %s [%s, %d txn(s)]\n", c.Signature, c.DominantSource(), c.Txns)
				}
			}
		}
	}
	printSpendCanaries(w, canaries)
}

// printSpendCanaries writes the four canaries. Printed even when there
// was nothing to categorise: they describe gold, not the run — and they
// are read BEFORE the run's own verdicts are stored, so a rate here is
// the one this run started from, not the one it leaves behind.
func printSpendCanaries(w io.Writer, c *spendCanaries) {
	if c == nil {
		return
	}
	fmt.Fprintln(w, "categorize: canaries")

	if len(c.Rates) == 0 {
		fmt.Fprintln(w, "  categorisation rate: (no spending population — no account in scope, or nothing spend-side in the window)")
	} else {
		parts := make([]string, 0, len(c.Rates))
		for _, r := range c.Rates {
			pct := 0.0
			if r.Total > 0 {
				pct = 100 * float64(r.Categorised) / float64(r.Total)
			}
			parts = append(parts, fmt.Sprintf("%s=%.1f%% (%d/%d)", r.Source, pct, r.Categorised, r.Total))
		}
		fmt.Fprintf(w, "  categorisation rate by source: %s\n", strings.Join(parts, ", "))
	}

	if len(c.ProviderMisses) == 0 {
		fmt.Fprintln(w, "  provider-map misses: none")
	} else {
		total := 0
		for _, m := range c.ProviderMisses {
			total += m.Count
		}
		fmt.Fprintf(w, "  provider-map misses: %d value(s) over %d row(s) — the issuer's vocabulary has moved:\n",
			len(c.ProviderMisses), total)
		for i, m := range c.ProviderMisses {
			if i == canaryListLimit {
				fmt.Fprintf(w, "    ... and %d more\n", len(c.ProviderMisses)-canaryListLimit)
				break
			}
			fmt.Fprintf(w, "    %s: %q (%d row(s))\n", m.Source, m.Category, m.Count)
		}
	}

	fmt.Fprintf(w, "  internal-transfer pairs excluded from spending: %d\n", len(c.Pairs))
	for i, p := range c.Pairs {
		if i == pairListLimit {
			fmt.Fprintf(w, "    ... and %d more\n", len(c.Pairs)-pairListLimit)
			break
		}
		// The phase leads the pair, because it is what says how to read the
		// two lines under it: legs that disagree in amount and currency are
		// the expected shape of a pair the source asserted and the alarming
		// shape of one the amounts alone produced.
		fmt.Fprintf(w, "    (%s)\n", p.By)
		fmt.Fprintf(w, "    %s  %s %s %.2f %s [%s]\n",
			formatEpochDay(p.Debit.Day), p.Debit.Source, p.Debit.Account, p.Debit.Amount, p.Debit.Currency, p.Debit.Signature)
		fmt.Fprintf(w, "    %s  %s %s %.2f %s [%s]\n",
			formatEpochDay(p.Credit.Day), p.Credit.Source, p.Credit.Account, p.Credit.Amount, p.Credit.Currency, p.Credit.Signature)
	}
	if c.PairsOutsideSpending > 0 {
		fmt.Fprintf(w, "    %d pair(s) matched outside the spending population, not listed\n", c.PairsOutsideSpending)
	}

	if len(c.Unmatched) == 0 {
		fmt.Fprintln(w, "  largest unmatched transfer legs: none")
	} else {
		fmt.Fprintf(w, "  largest unmatched transfer legs (%d unpaired in total; each is real spending or a missed pair):\n",
			len(c.Unmatched))
		for i, l := range c.Unmatched {
			if i == canaryListLimit {
				break
			}
			fmt.Fprintf(w, "    %s  %s %s %.2f %s [%s]\n",
				formatEpochDay(l.Day), l.Source, l.Account, l.Amount, l.Currency, l.Signature)
		}
	}

	if len(c.CrossCurrency) == 0 {
		fmt.Fprintln(w, "  cross-currency near-pairs: none")
	} else {
		fmt.Fprintf(w, "  cross-currency near-pairs (%d): opposite signs within the window but different native currencies,\n", len(c.CrossCurrency))
		fmt.Fprintln(w, "    which the amount pass cannot pair and no shared reference reached — check whether either is really spending:")
		for _, s := range c.CrossCurrency {
			fmt.Fprintf(w, "    %s  %s %.2f %s  ⟷  %s  %s %.2f %s\n",
				formatEpochDay(s.Debit.Day), s.Debit.Source, s.Debit.Amount, s.Debit.Currency,
				formatEpochDay(s.Credit.Day), s.Credit.Source, s.Credit.Amount, s.Credit.Currency)
		}
	}
}
