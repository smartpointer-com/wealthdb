package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/spending"
)

// neighbourWindowDays is how far either side of a transaction the
// `transaction` context level looks for company. One day: a purchase's
// useful neighbours are the ones on the same outing, and a wider band
// just dilutes the signal with unrelated merchants.
const neighbourWindowDays = 1

// maxNeighbours caps the nearby signatures shown per sample.
const maxNeighbours = 3

// merchantSample is one transaction standing behind a candidate
// signature, carrying the material the wider context levels send.
// Descriptor is the RAW narrative and is fenced before it is ever
// written into a prompt.
type merchantSample struct {
	Source      string
	AccountKind string
	Day         int64
	Currency    string
	Amount      float64
	Descriptor  string
	Neighbours  []string
}

// merchantCandidate is one merchant signature offered to the model,
// with the counts that let the run report say how much of the ledger
// each verdict covers.
type merchantCandidate struct {
	Signature string
	Txns      int
	PerSource map[string]int
	Samples   []merchantSample
}

// DominantSource is the source contributing the most transactions to
// this signature — the stratification key for the leftover sample, so
// a merchant met by more than one source is attributed to the one that
// met it most.
func (c merchantCandidate) DominantSource() string {
	best, bestN := "", -1
	for _, s := range sortedKeys(c.PerSource) {
		if n := c.PerSource[s]; n > bestN {
			best, bestN = s, n
		}
	}
	return best
}

// skippedSignatures counts what candidacy refused, by reason. Each is
// a count of DISTINCT signatures, not rows, and each is reported so
// a gate that fired is visible rather than leaving it unclear why
// a row stayed uncategorised.
type skippedSignatures struct {
	Fenced        int // transfer-shaped: a person or an account where a merchant would be
	PersonShaped  int // a bare person's name on a non-card account, when the option is on
	Uninformative int // nothing to name: no word at all, or nothing but the provider's own filing
	// PersonFenceOffKey is the config key that turned the person-shape
	// arm off, or empty when it is on. The run report prints it so a
	// reader can see which policy produced the list rather than infer
	// it from a zero — and prints the key the running family actually
	// resolved, which for an income run that inherited is spending's
	// and for one with a block of its own is income's.
	PersonFenceOffKey string
}

// merchantAnchor is one verdict already in the store, shown to the
// model as an in-context example. Anchors exist to steer the model
// towards its OWN prior vocabulary: left to itself it will place one
// coffee chain under FOOD_AND_DRINK_COFFEE and the next under
// FOOD_AND_DRINK_RESTAURANT, and a report that splits one habit across
// two categories is worse than one that puts it in the wrong single
// category.
type merchantAnchor struct {
	Signature string
	Name      string
	Detailed  string
}

// collectMerchantCandidates reads the model tier's backlog.
//
// DEFAULT: every counterparty signature with at least one row whose
// RESOLVED category is still NULL — read from the family's resolution
// macro, the lattice's one definition (SPENDING.md §3), so this command
// cannot drift from what a report would call categorised. A resolved
// category covers both halves at once: a signature the store already
// answers is paid for, and a signature every one of whose rows a rule
// or the provider map placed has nothing left to ask about.
//
// --all: every signature the family's candidacy admits, store row or
// not, deterministic verdict or not. That is what re-asks a
// counterparty after a taxonomy revision or a model change. It lifts
// the backlog filter and nothing else — the kind gate
// (fam.candidateKinds) and the fence still apply, so on the income
// side it re-asks deposits and never reaches a floor-placed row.
//
// EITHER WAY the transfer fence runs, and a fenced signature is
// excluded from candidacy — not merely described less. The fence is
// read over the whole ROW (spending.RowTransferShaped): the key alone
// under-fences, because a mobile person-to-person rail leads the
// narrative and names no payee, so a reduction that prefers the
// counterparty can leave a key that is a private individual's name and
// nothing else. So is a signature with no word in it
// (spending.Uninformative), and one that is nothing but the provider's
// own filing of the row (spending.FilingOnly): there is nothing to
// name, and a round-trip spent on it ends in the gauntlet every time.
// Both counts are returned so the run report can show each gate doing
// something rather than silently doing nothing.
func collectMerchantCandidates(ctx context.Context, db *sql.DB, fam categorizeFamily, level string, samples int, which backlog, fencePersonNames bool, czKey string) ([]merchantCandidate, skippedSignatures, error) {
	// The backlog is the RESOLVED value being NULL, which on the income
	// side is the difference between a few hundred payers and several
	// thousand pointless questions: a dividend the kind floor placed at
	// query time has a signature and no stored verdict, and asking a
	// model about it would be work with a known answer.
	backlogFilter := `
   AND c.` + fam.valueColumn + ` IS NULL`
	switch which {
	case backlogAll:
		backlogFilter = ""
	case backlogRefine:
		backlogFilter = `
   AND c.provenance = 'model'
   AND EXISTS (SELECT 1 FROM spend_categories sc
                WHERE sc.spend_detailed = c.` + fam.valueColumn + ` AND sc.catch_all)`
	}
	// The kind gate, which --all does NOT lift. The backlog filter is
	// about what is still unanswered; this is about what may be asked
	// at all, so the two are ANDed rather than alternated: on the
	// income side `--all` means every DEPOSIT signature including the
	// answered ones, never every signature in the population.
	q := `
SELECT c.` + fam.signatureColumn + `,
       p.silver_source_id, p.account_kind, p.occurred_at, p.currency,
       COALESCE(CAST(p.net_amount AS DOUBLE), 0),
       COALESCE(p.counterparty, ''), COALESCE(p.description, ''),
       COALESCE(p.provider_category, '')
  FROM ` + fam.resolutionMacro + ` c
  JOIN ` + fam.populationMacro + `(?, ?) p
    ON p.silver_source_id        = c.silver_source_id
   AND p.transaction_external_id = c.transaction_external_id
 WHERE c.` + fam.signatureColumn + ` IS NOT NULL
   AND c.` + fam.signatureColumn + ` <> ''` + fam.candidateKindFilter() + backlogFilter + `
 ORDER BY c.` + fam.signatureColumn + `, p.occurred_at, p.transaction_external_id`

	// The row fence, read once over the whole population rather than
	// per row here. Two reasons it cannot be a test on the row in
	// hand: this scan sees only the backlog by default, so a signature
	// carried by both a placed row and an unplaced one would be
	// admitted on the unplaced row while the rail sits on the placed
	// one the query never reaches; and even over the same rows, a key
	// admitted from a clean row before its fenced sibling arrives
	// would already be in the candidate set.
	refused, err := rowFencedSignatures(ctx, db, fam, fencePersonNames)
	if err != nil {
		return nil, skippedSignatures{}, err
	}

	rows, err := db.QueryContext(ctx, q, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, skippedSignatures{}, fmt.Errorf("categorize: read %s candidates: %w", fam.counterparty, err)
	}
	defer rows.Close()

	bySig := map[string]*merchantCandidate{}
	fencedSigs := map[string]bool{}
	personSigs := map[string]bool{}
	uninformativeSigs := map[string]bool{}
	wantSamples := level != config.SpendContextMerchant && samples > 0

	for rows.Next() {
		var (
			sig, source, kind, ccy, counterparty, description, providerCategory string
			occurredAt                                                          int64
			amount                                                              float64
		)
		if err := rows.Scan(&sig, &source, &kind, &occurredAt, &ccy, &amount,
			&counterparty, &description, &providerCategory); err != nil {
			return nil, skippedSignatures{}, fmt.Errorf("categorize: scan %s candidate: %w", fam.counterparty, err)
		}
		// The fence gates candidacy, identically at every context
		// level. A person-bearing narrative is not a merchant, and the
		// reading is over the whole row (rowFencedSignatures): a rail
		// the reduction dropped is still written in the provider's
		// filing and in the raw narrative.
		if refused.Transfer[sig] {
			fencedSigs[sig] = true
			continue
		}
		// The person-shape arm, which fires on a bare name that no
		// rail, IBAN or masked number accompanies — the one signature
		// shape that is PII by itself. It is read over the whole
		// population too (refused.Person), and for the same reason:
		// the account kind that exempts a card row belongs to the row,
		// and a key is what leaves the machine.
		if refused.Person[sig] {
			personSigs[sig] = true
			continue
		}
		// So does the word gate, at the same place and every level.
		// A bare code has nothing to name; the model would only echo
		// it and the gauntlet would only reject the echo. Neither has
		// a signature that is nothing but the bank's own booking type
		// — the bank filed the row and wrote nothing else — which
		// names how the row was booked, not whom it paid, and would
		// buy one verdict for every row filed that way; nor one that
		// names only how the money arrived.
		if spending.Uninformative(sig) || spending.FilingOnly(sig, providerCategory) ||
			spending.MechanismOnly(sig) {
			uninformativeSigs[sig] = true
			continue
		}
		c, ok := bySig[sig]
		if !ok {
			c = &merchantCandidate{Signature: sig, PerSource: map[string]int{}}
			bySig[sig] = c
		}
		c.Txns++
		c.PerSource[source]++
		if wantSamples && len(c.Samples) < samples {
			descriptor := strings.TrimSpace(counterparty)
			if descriptor == "" {
				// The description's memo — the payer's own words,
				// after canonical.DescriptionMemoSeparator — names no
				// merchant and is not sent.
				narrative, _ := canonical.SplitDescriptionMemo(description)
				descriptor = strings.TrimSpace(narrative)
			}
			// The same fence, applied a second time to the RAW
			// narrative. A signature is a folded, truncated view of
			// the narrative it came from, so a narrative can carry an
			// account number or a person in its tail that the
			// signature dropped. Fencing the sample drops the
			// narrative, never the candidate — candidacy stays
			// identical across context levels, which is what makes the
			// knob a description setting rather than a scope setting.
			if spending.TransferShaped(descriptor) {
				descriptor = ""
			}
			c.Samples = append(c.Samples, merchantSample{
				Source:      source,
				AccountKind: kind,
				Day:         gold.EpochDay(occurredAt),
				Currency:    ccy,
				Amount:      amount,
				Descriptor:  descriptor,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, skippedSignatures{}, err
	}

	out := make([]merchantCandidate, 0, len(bySig))
	for _, c := range bySig {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Signature < out[j].Signature })

	if level == config.SpendContextTransaction {
		if err := attachNeighbours(ctx, db, fam, out, refused); err != nil {
			return nil, skippedSignatures{}, err
		}
	}
	off := ""
	if !fencePersonNames {
		off = czKey
	}
	return out, skippedSignatures{
		Fenced:            len(fencedSigs),
		PersonShaped:      len(personSigs),
		Uninformative:     len(uninformativeSigs),
		PersonFenceOffKey: off,
	}, nil
}

// printNeverSent reports what candidacy refused, one line per gate
// that fired. It prints beside the plan, so why a
// row will stay uncategorised before deciding to wait on the run.
func printNeverSent(w io.Writer, skipped skippedSignatures) {
	if skipped.Fenced > 0 {
		fmt.Fprintf(w, "categorize: %d signature(s) fenced as transfer-shaped and never sent\n", skipped.Fenced)
	}
	if skipped.PersonShaped > 0 {
		fmt.Fprintf(w, "categorize: %d signature(s) fenced as person-shaped and never sent\n", skipped.PersonShaped)
	}
	// A policy line rather than a count. With the arm off there is
	// nothing to count, and a reader of the report would otherwise
	// have to know the config to tell "nothing looked like a person"
	// from "nobody was looking".
	if skipped.PersonFenceOffKey != "" {
		fmt.Fprintf(w, "categorize: person-shape fence off (%s.fence_person_names)\n",
			skipped.PersonFenceOffKey)
	}
	if skipped.Uninformative > 0 {
		fmt.Fprintf(w, "categorize: %d signature(s) uninformative and never sent\n", skipped.Uninformative)
	}
}

// fencedSignatures is what the row fence refused, split by arm so the
// run report can name each. Both are sets of SIGNATURES, and a
// signature in either never leaves the machine.
type fencedSignatures struct {
	// Transfer is the rail, IBAN and masked-contact arm
	// (spending.RowTransferShaped), always read.
	Transfer map[string]bool
	// Person is the bare-name arm (spending.PersonShaped), read only
	// when spending.categorization.fence_person_names is on, and only
	// over rows on non-card accounts. Empty when the option is off.
	Person map[string]bool
}

// Refuses reports whether a signature is fenced by either arm. The
// three sites a signature leaves the machine ask this question and
// nothing finer; only the counters care which arm fired.
func (f fencedSignatures) Refuses(sig string) bool {
	return f.Transfer[sig] || f.Person[sig]
}

// rowFencedSignatures reads the fence over the WHOLE of a family's
// population — every row, its provider filing, its narrative and its
// account kind — and returns every signature at least one row refuses.
//
// It is a signature-level answer to a row-level question, and
// deliberately so. The reduction is many-to-one: several narratives
// share one key, and a key is sent to the model, never a row. If one
// row under a key was booked on a person-to-person rail, that key can
// be a private individual's name — so the key is refused whichever
// other row would have carried it in. The same set answers all three
// places a signature leaves the machine: the candidate list, the
// neighbour lists and the anchor block.
//
// The PERSON arm is the same construction over a different question,
// and it is read here rather than at the candidate scan for the same
// reason: it is exempt on CARD rows, the account kind belongs to the
// row, and a key carried by both a card row and a bank row must be
// refused on the bank row it also has. Reading it at the candidate
// scan would consult only the backlog's rows and miss the bank row
// sitting under a key some other row already placed.
//
// Why card rows are exempt: most card merchants are two or three plain
// words with no legal form and no trade word — a name-shaped arm over
// them would refuse half the spending model tier to fence a shape that
// does not arrive there. An inbound credit transfer's narrative IS the
// sender, which is why the arm exists and why it is the bank rows it
// reads.
//
// The population read here is exactly the one the candidate scan and
// the neighbour scan read, so a single pass covers all of them rather
// than each site re-testing its own rows and missing what is written
// on the rows it does not see.
func rowFencedSignatures(ctx context.Context, db *sql.DB, fam categorizeFamily, fencePersonNames bool) (fencedSignatures, error) {
	out := fencedSignatures{Transfer: map[string]bool{}, Person: map[string]bool{}}
	rows, err := db.QueryContext(ctx, `
SELECT c.`+fam.signatureColumn+`, p.account_kind,
       COALESCE(p.provider_category, ''), COALESCE(p.description, '')
  FROM `+fam.populationMacro+`(?, ?) p
  JOIN `+fam.resolutionMacro+` c
    ON c.silver_source_id        = p.silver_source_id
   AND c.transaction_external_id = p.transaction_external_id
 WHERE c.`+fam.signatureColumn+` IS NOT NULL
   AND c.`+fam.signatureColumn+` <> ''`, int64(0), gold.MaxEpoch)
	if err != nil {
		return fencedSignatures{}, fmt.Errorf("categorize: read the row fence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sig, accountKind, providerCategory, description string
		if err := rows.Scan(&sig, &accountKind, &providerCategory, &description); err != nil {
			return fencedSignatures{}, fmt.Errorf("categorize: scan the row fence: %w", err)
		}
		if !out.Transfer[sig] && spending.RowTransferShaped(sig, providerCategory, description) {
			out.Transfer[sig] = true
		}
		if fencePersonNames && !out.Person[sig] &&
			accountKind != string(canonical.AccountKindCard) && spending.PersonShaped(sig) {
			out.Person[sig] = true
		}
	}
	return out, rows.Err()
}

// attachNeighbours fills in the nearby-transaction context the
// `transaction` level sends: for each sample, the signatures of what
// else the same source booked within a day. It is what lets a model
// place an otherwise opaque counterparty from its company — a name
// that means nothing between an airline and a hotel means something.
//
// Per family: a run asks about one vocabulary and must send only that
// family's neighbours.
//
// Neighbours are fenced like everything else — on the row, through the
// set rowFencedSignatures read over this same population, so a rail
// written only in a neighbour's provider filing or raw narrative
// fences it here too, and the key-only reading needs no second call:
// the set already refuses everything it would. A neighbour equal to
// the sample's own signature is dropped as uninformative.
func attachNeighbours(ctx context.Context, db *sql.DB, fam categorizeFamily, cands []merchantCandidate, refused fencedSignatures) error {
	type dayedSig struct {
		day int64
		sig string
	}
	// This family's own population and its own signatures. Reading the
	// spending side here during an income run would be a privacy
	// defect rather than a cosmetic one: the fence set handed in is
	// built over the INCOME population, so a spending signature drawn
	// from a different population would be checked against a set that
	// never saw its row and could leave the machine unfenced.
	//
	// The family's KIND gate applies here too, and for the same reason
	// it applies to the candidate list: a neighbour is a signature that
	// LEAVES THE MACHINE, and a kind the family never asks about has no
	// business leaving it as context for one it does. Without this, a
	// deposit's neighbour list at the `transaction` level would carry
	// the signatures of every dividend booked within a day of it —
	// which on the income side is the holdings list, and is exactly
	// what the candidate gate exists to keep out of a prompt.
	rows, err := db.QueryContext(ctx, `
SELECT p.silver_source_id, p.occurred_at, c.`+fam.signatureColumn+`
  FROM `+fam.populationMacro+`(?, ?) p
  JOIN `+fam.resolutionMacro+` c
    ON c.silver_source_id        = p.silver_source_id
   AND c.transaction_external_id = p.transaction_external_id
 WHERE c.`+fam.signatureColumn+` IS NOT NULL
   AND c.`+fam.signatureColumn+` <> ''`+fam.candidateKindFilter()+`
 ORDER BY p.silver_source_id, p.occurred_at`, int64(0), gold.MaxEpoch)
	if err != nil {
		return fmt.Errorf("categorize: read neighbour context: %w", err)
	}
	defer rows.Close()

	bySource := map[string][]dayedSig{}
	for rows.Next() {
		var source, sig string
		var occurredAt int64
		if err := rows.Scan(&source, &occurredAt, &sig); err != nil {
			return fmt.Errorf("categorize: scan neighbour context: %w", err)
		}
		if refused.Refuses(sig) {
			continue
		}
		bySource[source] = append(bySource[source], dayedSig{gold.EpochDay(occurredAt), sig})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range cands {
		for j := range cands[i].Samples {
			s := &cands[i].Samples[j]
			seen := map[string]bool{cands[i].Signature: true}
			for _, n := range bySource[s.Source] {
				if n.day < s.Day-neighbourWindowDays || n.day > s.Day+neighbourWindowDays {
					continue
				}
				if seen[n.sig] {
					continue
				}
				seen[n.sig] = true
				s.Neighbours = append(s.Neighbours, n.sig)
				if len(s.Neighbours) == maxNeighbours {
					break
				}
			}
		}
	}
	return nil
}

// collectMerchantAnchors samples verdicts already in the store.
//
// Newest first, because the vocabulary worth converging on is the one
// most recently used. Signatures currently up for categorisation are
// skipped (under --all they are all in the store, and showing a
// candidate its own previous answer would just re-assert it), and so
// is any stored verdict outside the vendored vocabulary — an anchor
// carrying a delta value would teach the model exactly the vocabulary
// the gauntlet then rejects.
//
// The transfer fence applies here too, and this is the one path where
// it has to be re-applied rather than inherited: the store is
// append-only across signature revisions, so a verdict keyed on a
// signature the current fence would refuse outlives the transactions
// that produced it and would otherwise keep going out in every prompt.
// The fence gates what leaves the machine; the verdict itself stays
// usable locally, since the enrichment lookup applies it by signature.
//
// Two readings of it, because a stored key may have no row left in
// gold at all: the key on its own, which is all an orphaned verdict
// offers, and the row fence over the population
// (rowFencedSignatures), which catches the key whose rail is written
// in the rows rather than in the reduction.
//
// The PERSON arm is read only in the second of those, and deliberately
// so. It is exempt on card rows, and a key with no row left carries no
// account kind — so applying it to the key alone would refuse every
// two-word card merchant in the store and empty the anchor block of
// exactly the vocabulary it exists to reinforce. The residue is an
// orphaned verdict on a person-shaped key, bought before the arm
// existed or before it was turned on; `categorizations --forget` is
// what removes one.
func collectMerchantAnchors(ctx context.Context, db *sql.DB, fam categorizeFamily, maxAnchors int, exclude map[string]bool, fencePersonNames bool) ([]merchantAnchor, error) {
	if maxAnchors <= 0 {
		return nil, nil
	}
	refused, err := rowFencedSignatures(ctx, db, fam, fencePersonNames)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT `+fam.signatureColumn+`, `+fam.storeNameColumn+`, `+fam.valueColumn+`
  FROM `+fam.storeTable+`
 ORDER BY assigned_at DESC, `+fam.signatureColumn)
	if err != nil {
		return nil, fmt.Errorf("categorize: read anchors: %w", err)
	}
	defer rows.Close()
	var out []merchantAnchor
	for rows.Next() {
		var a merchantAnchor
		if err := rows.Scan(&a.Signature, &a.Name, &a.Detailed); err != nil {
			return nil, fmt.Errorf("categorize: scan anchor: %w", err)
		}
		if exclude[a.Signature] || !fam.emittable(a.Detailed) {
			continue
		}
		if refused.Refuses(a.Signature) || spending.TransferShaped(a.Signature) {
			continue
		}
		out = append(out, a)
		if len(out) == maxAnchors {
			break
		}
	}
	return out, rows.Err()
}

func candidateSignatures(cands []merchantCandidate) map[string]bool {
	m := make(map[string]bool, len(cands))
	for _, c := range cands {
		m[c.Signature] = true
	}
	return m
}

func totalCandidateTxns(cands []merchantCandidate) int {
	n := 0
	for _, c := range cands {
		n += c.Txns
	}
	return n
}

// uncategorisedCandidates returns the candidates that ended the run
// without a verdict — rows the model skipped and rows it answered
// invalidly alike. A candidate counts as categorised only if a verdict
// is about to be stored for it.
func uncategorisedCandidates(cands []merchantCandidate, valid []categorization) []merchantCandidate {
	done := make(map[string]bool, len(valid))
	for _, v := range valid {
		done[v.Signature] = true
	}
	out := make([]merchantCandidate, 0, len(cands)-len(valid))
	for _, c := range cands {
		if !done[c.Signature] {
			out = append(out, c)
		}
	}
	return out
}
