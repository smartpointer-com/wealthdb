package spending

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

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

// passEvidence is what the sources state in their payloads about where
// each movement went (canonical/payloadkeys.go), read in one scan
// (loadPassEvidence).
type passEvidence struct {
	refs            map[txKey]string
	ambiguousRefs   int
	counterLegs     map[txKey]statedCounterLeg
	counterAccounts map[txKey]farAccount
}

// evidenceRow is one transaction's row of txn_evidence(), with its own
// account and its counter account normalised as the stated counter
// accounts compare them.
type evidenceRow struct {
	key                                 txKey
	bankRef, counterAccount, own, named sql.NullString
	counterCcy                          sql.NullString
	counterAmt                          sql.NullFloat64
}

// loadPassEvidence reads every transaction that states a reference, a
// counter account or a counter leg, in one scan of txn_evidence(), and
// finds the three kinds of evidence in them against the accounts gold
// holds (heldAccounts.byNorm).
func loadPassEvidence(ctx context.Context, tx querier, accounts map[string][]farAccount) (passEvidence, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id, bank_ref, counter_account,
               upper(replace(account_external_id, ' ', '')),
               upper(replace(counter_account, ' ', '')),
               counter_currency, counter_amount
          FROM txn_evidence()
         WHERE (bank_ref IS NOT NULL AND bank_ref <> '')
            OR (counter_account IS NOT NULL AND counter_account <> '')
            OR (counter_currency IS NOT NULL AND counter_currency <> '' AND counter_amount IS NOT NULL)`)
	if err != nil {
		return passEvidence{}, fmt.Errorf("spending: read payload evidence: %w", err)
	}
	defer rows.Close()
	var evidence []evidenceRow
	for rows.Next() {
		var r evidenceRow
		if err := rows.Scan(&r.key.source, &r.key.txID, &r.bankRef, &r.counterAccount,
			&r.own, &r.named, &r.counterCcy, &r.counterAmt); err != nil {
			return passEvidence{}, fmt.Errorf("spending: scan payload evidence: %w", err)
		}
		evidence = append(evidence, r)
	}
	if err := rows.Err(); err != nil {
		return passEvidence{}, fmt.Errorf("spending: read payload evidence: %w", err)
	}
	ev := passEvidence{
		counterLegs:     statedCounterLegs(evidence),
		counterAccounts: statedCounterAccounts(evidence, accounts),
	}
	ev.refs, ev.ambiguousRefs = movementReferences(evidence)
	return ev, nil
}

// farAccount is an account gold holds, named as the other side of a
// movement.
type farAccount struct{ source, account string }

// statedCounterAccounts finds, per transaction, the OWN account the
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
func statedCounterAccounts(rows []evidenceRow, accounts map[string][]farAccount) map[txKey]farAccount {
	out := map[txKey]farAccount{}
	for _, r := range rows {
		if !r.counterAccount.Valid || r.counterAccount.String == "" || !r.own.Valid || r.named.String == r.own.String {
			continue
		}
		var same, all []farAccount
		for _, a := range accounts[r.named.String] {
			if a.source == r.key.source {
				same = append(same, a)
			}
			all = append(all, a)
		}
		switch {
		case len(same) == 1:
			out[r.key] = same[0]
		case len(same) == 0 && len(all) == 1:
			out[r.key] = all[0]
		}
	}
	return out
}

// movementReferences finds, per transaction, the reference the source
// stamped on BOTH halves of one money movement — the bank's own transaction
// number, written once and printed on the debit and on the credit alike.
//
// The far account arrives by two roads (CASHFLOW.md §4): the matcher's, which
// is a pairing, and the source's, which is the counter account it states
// outright (statedCounterAccounts). This feeds the first, and of the ways
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
// THE UNIQUENESS TEST IS THE SAFETY, and it is why this is a census rather
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
func movementReferences(rows []evidenceRow) (map[txKey]string, int) {
	type sourceRef struct{ source, ref string }
	under := map[sourceRef]int{}
	for _, r := range rows {
		if r.bankRef.Valid && r.bankRef.String != "" {
			under[sourceRef{r.key.source, r.bankRef.String}]++
		}
	}
	out := map[txKey]string{}
	ambiguous := 0
	for _, r := range rows {
		if !r.bankRef.Valid || r.bankRef.String == "" {
			continue
		}
		if under[sourceRef{r.key.source, r.bankRef.String}] == 2 {
			out[r.key] = r.bankRef.String
		}
	}
	for _, n := range under {
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
		if n > 2 {
			ambiguous++
		}
	}
	return out, ambiguous
}

// statedCounterLeg is the other leg of a movement as the source describes it:
// the currency the money became, or came from, and the figure.
type statedCounterLeg struct {
	currency string
	amount   float64
}

// statedCounterLegs finds, per transaction, the OTHER LEG the source
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
// UNLIKE the reference phase, THE CENSUS IS NOT HERE. The reference census
// refuses a reference the source did not mint per movement, because a leg
// carrying an over-used reference must reach the matcher looking like a leg
// carrying none. A description is different: whether it resolves depends on
// which legs answer to it, and that is a question about the pool the matcher
// is offered, not about every row of the table. So this reads the
// description as written and matchStatedCounters does all the counting, over
// the legs it was actually offered.
func statedCounterLegs(rows []evidenceRow) map[txKey]statedCounterLeg {
	out := map[txKey]statedCounterLeg{}
	for _, r := range rows {
		if r.counterCcy.Valid && r.counterCcy.String != "" && r.counterAmt.Valid {
			out[r.key] = statedCounterLeg{r.counterCcy.String, r.counterAmt.Float64}
		}
	}
	return out
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
// narrative of each leg. A matched leg outside the spending population
// still gets an enrichment row, and that row needs a signature like any
// other.
//
// Each leg carries the reference and the counter leg its source stated
// (evidence), because this is the one place a leg is built. Its amount
// is the pool's match_amount: a coin's dust keeps its size. A leg that
// moves a coin in the coin itself is a coin leg and carries the coin's
// USD price on its day, from rates (gold.CoinUSD): its instrument is its
// currency, and the asset class and vehicle (the row's own, else its
// instrument's) are a coin's (gold.IsCoin).
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
func loadMatcherPool(ctx context.Context, tx querier, names []CounterpartyName, evidence passEvidence, rates *gold.DailyRates) ([]gold.TransferLeg, map[txKey]candidate, error) {
	// The scope facts and the provider's filing ride along with the
	// narrative: a far rule is tested against a pool row exactly as
	// against a population row (farRuleReaches), and a rule scoped to
	// an account, a portfolio or a date range, or one that matches the
	// filing, would otherwise admit nothing it reaches here.
	rows, err := tx.QueryContext(ctx, `
        SELECT p.silver_source_id, p.account_external_id, p.transaction_external_id,
               p.occurred_at, p.currency, CAST(p.match_amount AS DOUBLE),
               COALESCE(p.counterparty, ''), COALESCE(p.description, ''),
               COALESCE(p.provider_category, ''), COALESCE(a.portfolio_external_id, ''),
               COALESCE(t.instrument_external_id = p.currency, false),
               COALESCE(t.asset_class, ''), COALESCE(i.asset_class, ''),
               COALESCE(t.vehicle, ''), COALESCE(i.vehicle, '')
          FROM spend_matcher_pool(?, ?) p
          JOIN transactions t
            ON t.silver_source_id = p.silver_source_id
           AND t.transaction_external_id = p.transaction_external_id
          LEFT JOIN accounts a
            ON a.silver_source_id = p.silver_source_id
           AND a.account_external_id = p.account_external_id
          LEFT JOIN instruments i
            ON i.silver_source_id = t.silver_source_id
           AND i.instrument_external_id = t.instrument_external_id
         ORDER BY p.silver_source_id, p.transaction_external_id`, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, nil, fmt.Errorf("spending: read matcher pool: %w", err)
	}
	defer rows.Close()

	var legs []gold.TransferLeg
	narratives := map[txKey]candidate{}
	for rows.Next() {
		var (
			leg                                    gold.TransferLeg
			occurredAt                             int64
			amount                                 sql.NullFloat64
			row                                    candidate
			ownUnit                                bool
			class, instClass, vehicle, instVehicle string
		)
		if err := rows.Scan(&leg.Group, &leg.Owner, &leg.ID, &occurredAt,
			&leg.Ccy, &amount, &row.counterparty, &row.description, &row.providerCategory, &row.portfolio,
			&ownUnit, &class, &instClass, &vehicle, &instVehicle); err != nil {
			return nil, nil, fmt.Errorf("spending: scan matcher pool: %w", err)
		}
		row.account, row.occurredAt = leg.Owner, occurredAt
		if !amount.Valid {
			// A leg with no amount cannot be oriented, so it can neither
			// fund nor be funded.
			continue
		}
		leg.Day = gold.EpochDay(occurredAt)
		leg.Amt = amount.Float64
		leg.Coin = ownUnit && gold.IsCoin(cmp.Or(class, instClass), cmp.Or(vehicle, instVehicle))
		if leg.Coin {
			leg.USD = gold.CoinUSD(rates, leg.Ccy, leg.Day)
		}
		leg.Rail, leg.RailPartner = legRail(row.counterparty, row.description)
		leg.Signature = Normalize(row.counterparty, row.description)
		leg.Reversal = reversalRe.MatchString(row.counterparty + " " + row.description)
		leg.Names = legNames(names, leg.Group, leg.Owner, row.counterparty, row.description)
		row.key = txKey{leg.Group, leg.ID}
		leg.Ref = evidence.refs[row.key]
		if c, ok := evidence.counterLegs[row.key]; ok {
			leg.CounterCcy, leg.CounterAmt = c.currency, c.amount
		}
		legs = append(legs, leg)
		narratives[row.key] = row
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("spending: read matcher pool: %w", err)
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
