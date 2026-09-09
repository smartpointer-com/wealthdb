package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/spending"
)

// Every merchant, signature and narrative in this file is invented.
// The model tier's whole privacy story is about what does and does not
// leave the machine, and a fixture that looked like a real payee would
// undercut the tests that assert it.

// ---- the validation gauntlet -------------------------------------------------

func testCandidateSet() map[string]bool {
	return map[string]bool{
		"BLUE HARBOUR CAFE":  true,
		"NORTHWIND HARDWARE": true,
	}
}

func TestParseAndValidateCategorizationsHappyPath(t *testing.T) {
	body := `BLUE HARBOUR CAFE,Blue Harbour Cafe,FOOD_AND_DRINK_COFFEE
NORTHWIND HARDWARE,Northwind Hardware,HOME_IMPROVEMENT_HARDWARE`
	valid, invalid := parseAndValidateCategorizations(body, testCandidateSet())
	if len(invalid) != 0 {
		t.Fatalf("want 0 invalid, got %d (%v)", len(invalid), invalid)
	}
	if len(valid) != 2 {
		t.Fatalf("want 2 valid, got %d", len(valid))
	}
	if valid[0].MerchantName != "Blue Harbour Cafe" || valid[0].Detailed != "FOOD_AND_DRINK_COFFEE" {
		t.Errorf("row 0 = %+v", valid[0])
	}
}

// TestParseAndValidateCategorizationsRejectsDeltas is the load-bearing
// gauntlet test. A merchant-keyed internal_transfer verdict would
// remove every transaction of that merchant from spending, globally and
// silently, so every delta value must be refused however it is spelled
// — and refused with the specific reason, so the retry tells the model
// what it did wrong rather than "not a category". The deltas are read
// from the taxonomy, so a delta added there is covered here without
// this test changing; the explicit check on `investment` pins that the
// list is non-empty and really comes from the taxonomy.
func TestParseAndValidateCategorizationsRejectsDeltas(t *testing.T) {
	var deltas []string
	for _, c := range canonical.DeltaSpendCategories() {
		deltas = append(deltas, c.Detailed)
	}
	if !slices.Contains(deltas, canonical.SpendDetailedInvestment) {
		t.Fatalf("deltas = %v, want investment among them", deltas)
	}
	for _, d := range deltas {
		for _, spelling := range []string{d, strings.ToUpper(d), " " + d + " "} {
			body := fmt.Sprintf("BLUE HARBOUR CAFE,Blue Harbour Cafe,%s", spelling)
			valid, invalid := parseAndValidateCategorizations(body, testCandidateSet())
			if len(valid) != 0 {
				t.Fatalf("%q: a delta must never be stored, got %+v", spelling, valid)
			}
			if len(invalid) != 1 {
				t.Fatalf("%q: want 1 invalid, got %d", spelling, len(invalid))
			}
			if !strings.Contains(invalid[0].Reason, "never by a model") {
				t.Errorf("%q: reason = %q, want the delta-specific rejection", spelling, invalid[0].Reason)
			}
		}
	}
}

func TestParseAndValidateCategorizationsGauntlet(t *testing.T) {
	cases := []struct {
		name, body, wantReason string
	}{
		{
			"signature not in the candidate set",
			`SOUTHPORT LAUNDRY,Southport Laundry,PERSONAL_CARE_LAUNDRY_AND_DRY_CLEANING`,
			"was not in the candidate set",
		},
		{
			"category outside the taxonomy",
			`BLUE HARBOUR CAFE,Blue Harbour Cafe,COFFEE_AND_SNACKS`,
			"not a value of the taxonomy",
		},
		{
			"a primary is not a detailed value",
			`BLUE HARBOUR CAFE,Blue Harbour Cafe,FOOD_AND_DRINK`,
			"not a value of the taxonomy",
		},
		{
			"empty merchant name",
			`BLUE HARBOUR CAFE,,FOOD_AND_DRINK_COFFEE`,
			"merchant_name is empty",
		},
		{
			"merchant name echoes the signature",
			`BLUE HARBOUR CAFE,BLUE HARBOUR CAFE,FOOD_AND_DRINK_COFFEE`,
			"echoed input",
		},
		{
			"too few columns",
			`BLUE HARBOUR CAFE,FOOD_AND_DRINK_COFFEE`,
			"expected exactly 3 columns",
		},
		{
			"padded row cannot be disambiguated",
			`BLUE HARBOUR CAFE,4,Blue Harbour Cafe,FOOD_AND_DRINK_COFFEE`,
			"expected exactly 3 columns",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			valid, invalid := parseAndValidateCategorizations(c.body, testCandidateSet())
			if len(valid) != 0 {
				t.Fatalf("want 0 valid, got %+v", valid)
			}
			if len(invalid) != 1 {
				t.Fatalf("want 1 invalid, got %d (%v)", len(invalid), invalid)
			}
			if !strings.Contains(invalid[0].Reason, c.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", invalid[0].Reason, c.wantReason)
			}
		})
	}
}

// TestParseAndValidateCategorizationsTolerances covers the response
// hygiene the precedent established: fenced bodies parse, a shouted or
// whispered category is folded to the taxonomy's casing, and a garbage
// line does not abort the rows around it.
func TestParseAndValidateCategorizationsTolerances(t *testing.T) {
	body := "```csv\nBLUE HARBOUR CAFE,Blue Harbour Cafe,food_and_drink_coffee\n" +
		"this is not csv at all\n" +
		"NORTHWIND HARDWARE,Northwind Hardware,home_improvement_hardware\n```"
	valid, invalid := parseAndValidateCategorizations(body, testCandidateSet())
	if len(valid) != 2 {
		t.Fatalf("want 2 valid, got %d (%v)", len(valid), invalid)
	}
	if valid[0].Detailed != "FOOD_AND_DRINK_COFFEE" {
		t.Errorf("lower-case category should fold up, got %q", valid[0].Detailed)
	}
	if len(invalid) != 1 {
		t.Errorf("the garbage line should be the only rejection, got %d", len(invalid))
	}
}

func TestIsDeltaSpendCategory(t *testing.T) {
	for _, s := range []string{"internal_transfer", "CASH_WITHDRAWAL", " other ", "investment", "Investment"} {
		if !isDeltaSpendCategory(s) {
			t.Errorf("%q should be recognised as a delta", s)
		}
	}
	for _, s := range []string{"", "FOOD_AND_DRINK_COFFEE", "food_and_drink_coffee", "NOT_A_CATEGORY"} {
		if isDeltaSpendCategory(s) {
			t.Errorf("%q should NOT be recognised as a delta", s)
		}
	}
}

// ---- the retry loop ----------------------------------------------------------

// scriptedLLM stands in for the model endpoint: it hands back a
// prepared response per attempt and records the prompt it was given,
// so a test can assert both the loop's behaviour and what the retry
// actually told the model.
type scriptedLLM struct {
	responses []string
	err       error
	prompts   []string
}

func (s *scriptedLLM) call(_ context.Context, _, user string) (string, error) {
	s.prompts = append(s.prompts, user)
	if s.err != nil {
		return "", s.err
	}
	i := len(s.prompts) - 1
	if i >= len(s.responses) {
		i = len(s.responses) - 1
	}
	return s.responses[i], nil
}

func twoCandidates() []merchantCandidate {
	return []merchantCandidate{
		{Signature: "BLUE HARBOUR CAFE", Txns: 4, PerSource: map[string]int{"bank": 4}},
		{Signature: "NORTHWIND HARDWARE", Txns: 2, PerSource: map[string]int{"bank": 2}},
	}
}

func TestCategorizeWithLLMRetriesWithFeedback(t *testing.T) {
	llm := &scriptedLLM{responses: []string{
		// A good row and a delta the gauntlet must refuse.
		"BLUE HARBOUR CAFE,Blue Harbour Cafe,FOOD_AND_DRINK_COFFEE\n" +
			"NORTHWIND HARDWARE,Northwind Hardware,internal_transfer",
		// The retry drops the delta and re-answers. It also re-emits the
		// first merchant with a different category, which must NOT
		// displace the answer already accepted.
		"BLUE HARBOUR CAFE,Blue Harbour Cafe,FOOD_AND_DRINK_RESTAURANT\n" +
			"NORTHWIND HARDWARE,Northwind Hardware,HOME_IMPROVEMENT_HARDWARE",
	}}

	var out, errOut bytes.Buffer
	valid, attempts, totalInvalid, err := categorizeWithLLM(context.Background(), llm.call,
		splitBatches(twoCandidates(), 10), nil, config.SpendContextMerchant, 3, 30, false, nil, &out, &errOut)
	if err != nil {
		t.Fatalf("categorizeWithLLM: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if totalInvalid != 1 {
		t.Errorf("totalInvalid = %d, want 1", totalInvalid)
	}
	if len(valid) != 2 {
		t.Fatalf("valid = %d, want 2", len(valid))
	}
	byName := map[string]string{}
	for _, v := range valid {
		byName[v.Signature] = v.Detailed
	}
	if byName["BLUE HARBOUR CAFE"] != "FOOD_AND_DRINK_COFFEE" {
		t.Errorf("the first accepted verdict must win, got %q", byName["BLUE HARBOUR CAFE"])
	}
	if byName["NORTHWIND HARDWARE"] != "HOME_IMPROVEMENT_HARDWARE" {
		t.Errorf("retry verdict = %q", byName["NORTHWIND HARDWARE"])
	}
	if len(llm.prompts) != 2 {
		t.Fatalf("prompts = %d, want 2", len(llm.prompts))
	}
	if !strings.Contains(llm.prompts[1], "rows that I rejected") ||
		!strings.Contains(llm.prompts[1], "never by a model") {
		t.Errorf("the retry prompt must carry the rejection reason:\n%s", llm.prompts[1])
	}
	if strings.Contains(llm.prompts[0], "rows that I rejected") {
		t.Error("the first prompt must carry no feedback")
	}
}

func TestCategorizeWithLLMStopsAtMaxAttempts(t *testing.T) {
	llm := &scriptedLLM{responses: []string{
		"BLUE HARBOUR CAFE,Blue Harbour Cafe,FOOD_AND_DRINK_COFFEE\n" +
			"NORTHWIND HARDWARE,Northwind Hardware,NOT_A_CATEGORY",
	}}
	var out, errOut bytes.Buffer
	valid, attempts, totalInvalid, err := categorizeWithLLM(context.Background(), llm.call,
		splitBatches(twoCandidates(), 10), nil, config.SpendContextMerchant, 2, 30, false, nil, &out, &errOut)
	if err != nil {
		t.Fatalf("categorizeWithLLM: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want the cap of 2", attempts)
	}
	if totalInvalid != 2 {
		t.Errorf("totalInvalid = %d, want 2 (one per attempt)", totalInvalid)
	}
	if len(valid) != 1 {
		t.Errorf("valid = %d, want the single good row kept once", len(valid))
	}
}

func TestCategorizeWithLLMPropagatesCallErrors(t *testing.T) {
	llm := &scriptedLLM{err: errors.New("connection refused")}
	var out, errOut bytes.Buffer
	if _, _, _, err := categorizeWithLLM(context.Background(), llm.call,
		splitBatches(twoCandidates(), 10), nil, config.SpendContextMerchant, 3, 30, false, nil, &out, &errOut); err == nil {
		t.Fatal("a failing endpoint must surface as an error, not an empty run")
	}
}

// ---- batching ----------------------------------------------------------------

// The prompt's block headers, used to read back which signatures a
// prompt actually carried as candidates and which as anchors.
const (
	candidateBlockHeader = "Merchants to categorise — merchant_signature,transaction_count:"
	anchorBlockHeader    = "INPUT rows:"
)

// promptBlock returns the first CSV column of the lines that follow
// header in prompt, up to the next blank line.
func promptBlock(prompt, header string) []string {
	i := strings.Index(prompt, header)
	if i < 0 {
		return nil
	}
	rest := strings.TrimPrefix(prompt[i+len(header):], "\n")
	var out []string
	for _, line := range strings.Split(rest, "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		out = append(out, strings.SplitN(line, ",", 2)[0])
	}
	return out
}

func joined(ss []string) string { return strings.Join(ss, " | ") }

func fiveCandidates() []merchantCandidate {
	var out []merchantCandidate
	for _, sig := range []string{
		"ASHGROVE BAKERY", "BLUE HARBOUR CAFE", "CEDAR POINT PHARMACY", "DUNMORE BOOKS", "ELMFIELD GARAGE",
	} {
		out = append(out, merchantCandidate{Signature: sig, Txns: 1, PerSource: map[string]int{"bank": 1}})
	}
	return out
}

// answerEverything is a stub that answers every candidate it is shown
// with a valid row and records each prompt — enough to drive the batch
// loop without scripting a response per call.
func answerEverything(prompts *[]string) llmCall {
	return func(_ context.Context, _, user string) (string, error) {
		*prompts = append(*prompts, user)
		var b strings.Builder
		for _, sig := range promptBlock(user, candidateBlockHeader) {
			fmt.Fprintf(&b, "%s,%s,GENERAL_MERCHANDISE_PET_SUPPLIES\n", sig, strings.ToLower(sig))
		}
		return b.String(), nil
	}
}

// TestCategorizeWithLLMBatchesInOrder is the load-bearing batching
// test: a candidate set larger than one batch produces one call per
// batch, each carrying exactly its own slice, in order — and each
// later batch's anchors are the verdicts accepted before it, newest
// first and capped.
func TestCategorizeWithLLMBatchesInOrder(t *testing.T) {
	batches := splitBatches(fiveCandidates(), 2)
	if len(batches) != 3 {
		t.Fatalf("batches = %d, want 3", len(batches))
	}
	var prompts []string
	var sunk []batchOutcome
	sink := func(o batchOutcome) error { sunk = append(sunk, o); return nil }
	var out, errOut bytes.Buffer
	valid, calls, totalInvalid, err := categorizeWithLLM(context.Background(), answerEverything(&prompts),
		batches, nil, config.SpendContextMerchant, 3, 3, false, sink, &out, &errOut)
	if err != nil {
		t.Fatalf("categorizeWithLLM: %v", err)
	}
	if calls != 3 || totalInvalid != 0 || len(valid) != 5 {
		t.Fatalf("calls=%d invalid=%d valid=%d, want 3/0/5", calls, totalInvalid, len(valid))
	}
	wantCandidates := [][]string{
		{"ASHGROVE BAKERY", "BLUE HARBOUR CAFE"},
		{"CEDAR POINT PHARMACY", "DUNMORE BOOKS"},
		{"ELMFIELD GARAGE"},
	}
	if len(prompts) != len(wantCandidates) {
		t.Fatalf("prompts = %d, want %d", len(prompts), len(wantCandidates))
	}
	for i, p := range prompts {
		if got := promptBlock(p, candidateBlockHeader); joined(got) != joined(wantCandidates[i]) {
			t.Errorf("batch %d candidates = %v, want %v", i+1, got, wantCandidates[i])
		}
	}
	// No anchors were seeded, so the first prompt has no example block
	// at all; the second sees the first batch's verdicts; the third
	// sees the newest three of the four accepted so far.
	if strings.Contains(prompts[0], "Reference examples") {
		t.Error("the first batch must carry no anchors when none were seeded")
	}
	if got := promptBlock(prompts[1], anchorBlockHeader); joined(got) != joined(wantCandidates[0]) {
		t.Errorf("batch 2 anchors = %v, want batch 1's verdicts", got)
	}
	if got := promptBlock(prompts[2], anchorBlockHeader); joined(got) != "CEDAR POINT PHARMACY | DUNMORE BOOKS | ASHGROVE BAKERY" {
		t.Errorf("batch 3 anchors = %v, want the newest three, capped by --max-anchors", got)
	}
	if valid[0].Signature != "ASHGROVE BAKERY" || valid[4].Signature != "ELMFIELD GARAGE" {
		t.Errorf("verdicts out of batch order: %v", valid)
	}
	if len(sunk) != 3 || sunk[0].Index != 1 || sunk[2].Index != 3 || sunk[2].Count != 3 ||
		sunk[0].Size != 2 || sunk[2].Size != 1 || len(sunk[1].Accepted) != 2 {
		t.Errorf("sink saw %+v", sunk)
	}
	for _, want := range []string{
		"categorize: batch 1/3: 2 merchant(s), 2 accepted, 0 rejected, 1 attempt(s)",
		"categorize: batch 2/3: 2 merchant(s), 2 accepted, 0 rejected, 1 attempt(s)",
		"categorize: batch 3/3: 1 merchant(s), 1 accepted, 0 rejected, 1 attempt(s)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("progress missing %q:\n%s", want, out.String())
		}
	}
}

// TestCategorizeWithLLMRetriesOnlyTheFailingBatch: a rejected row
// costs its own batch a round-trip with feedback, and no other batch
// is asked again.
func TestCategorizeWithLLMRetriesOnlyTheFailingBatch(t *testing.T) {
	llm := &scriptedLLM{responses: []string{
		"ASHGROVE BAKERY,Ashgrove Bakery,FOOD_AND_DRINK_COFFEE\n" +
			"BLUE HARBOUR CAFE,Blue Harbour Cafe,FOOD_AND_DRINK_COFFEE",
		"CEDAR POINT PHARMACY,Cedar Point Pharmacy,MEDICAL_PHARMACIES_AND_SUPPLEMENTS\n" +
			"DUNMORE BOOKS,Dunmore Books,internal_transfer",
		"CEDAR POINT PHARMACY,Cedar Point Pharmacy,MEDICAL_PHARMACIES_AND_SUPPLEMENTS\n" +
			"DUNMORE BOOKS,Dunmore Books,GENERAL_MERCHANDISE_BOOKSTORES_AND_NEWSSTANDS",
	}}
	var sunk []batchOutcome
	sink := func(o batchOutcome) error { sunk = append(sunk, o); return nil }
	var out, errOut bytes.Buffer
	valid, calls, totalInvalid, err := categorizeWithLLM(context.Background(), llm.call,
		splitBatches(fiveCandidates()[:4], 2), nil, config.SpendContextMerchant, 3, 30, false, sink, &out, &errOut)
	if err != nil {
		t.Fatalf("categorizeWithLLM: %v", err)
	}
	if calls != 3 || totalInvalid != 1 || len(valid) != 4 {
		t.Fatalf("calls=%d invalid=%d valid=%d, want 3/1/4", calls, totalInvalid, len(valid))
	}
	if strings.Contains(llm.prompts[1], "rows that I rejected") {
		t.Error("the second batch's first attempt must carry no feedback")
	}
	retry := llm.prompts[2]
	if !strings.Contains(retry, "rows that I rejected") || !strings.Contains(retry, "never by a model") {
		t.Errorf("the retry must carry the rejection reason:\n%s", retry)
	}
	if got := promptBlock(retry, candidateBlockHeader); joined(got) != "CEDAR POINT PHARMACY | DUNMORE BOOKS" {
		t.Errorf("the retry must re-ask the failing batch only, got %v", got)
	}
	if len(sunk) != 2 || sunk[0].Attempts != 1 || sunk[0].Rejected != 0 ||
		sunk[1].Attempts != 2 || sunk[1].Rejected != 1 || len(sunk[1].Accepted) != 2 {
		t.Errorf("sink saw %+v", sunk)
	}
	if !strings.Contains(out.String(), "categorize: batch 2/2: 2 merchant(s), 2 accepted, 1 rejected, 2 attempt(s)") {
		t.Errorf("progress must show the retry on its batch:\n%s", out.String())
	}
}

// TestCategorizeWithLLMKeepsEarlyBatchesWhenALaterOneFails covers the
// reason the sink exists: what an early batch bought is handed over
// before the next batch is asked, whether the later one merely runs
// out of attempts or the endpoint dies under it.
func TestCategorizeWithLLMKeepsEarlyBatchesWhenALaterOneFails(t *testing.T) {
	batch1 := "ASHGROVE BAKERY,Ashgrove Bakery,FOOD_AND_DRINK_COFFEE\n" +
		"BLUE HARBOUR CAFE,Blue Harbour Cafe,FOOD_AND_DRINK_COFFEE"
	badBatch2 := "CEDAR POINT PHARMACY,Cedar Point Pharmacy,MEDICAL_PHARMACIES_AND_SUPPLEMENTS\n" +
		"DUNMORE BOOKS,Dunmore Books,NOT_A_CATEGORY"

	t.Run("later batch exhausts its attempts", func(t *testing.T) {
		llm := &scriptedLLM{responses: []string{batch1, badBatch2, badBatch2}}
		var sunk []batchOutcome
		var callsAtSink []int
		sink := func(o batchOutcome) error {
			sunk = append(sunk, o)
			callsAtSink = append(callsAtSink, len(llm.prompts))
			return nil
		}
		var out, errOut bytes.Buffer
		valid, calls, totalInvalid, err := categorizeWithLLM(context.Background(), llm.call,
			splitBatches(fiveCandidates()[:4], 2), nil, config.SpendContextMerchant, 2, 30, false, sink, &out, &errOut)
		if err != nil {
			t.Fatalf("running out of attempts is not an error: %v", err)
		}
		if calls != 3 || totalInvalid != 2 || len(valid) != 3 {
			t.Fatalf("calls=%d invalid=%d valid=%d, want 3/2/3", calls, totalInvalid, len(valid))
		}
		if len(sunk) != 2 || len(sunk[0].Accepted) != 2 || callsAtSink[0] != 1 {
			t.Fatalf("batch 1 must reach the sink before batch 2 is asked: sunk=%+v callsAtSink=%v", sunk, callsAtSink)
		}
		if len(sunk[1].Accepted) != 1 || sunk[1].Accepted[0].Signature != "CEDAR POINT PHARMACY" || sunk[1].Attempts != 2 {
			t.Errorf("batch 2 must hand over its good row and drop the bad one: %+v", sunk[1])
		}
	})

	t.Run("endpoint dies under the later batch", func(t *testing.T) {
		n := 0
		call := func(_ context.Context, _, _ string) (string, error) {
			n++
			if n == 1 {
				return batch1, nil
			}
			return "", errors.New("read response: context deadline exceeded")
		}
		var sunk []batchOutcome
		sink := func(o batchOutcome) error { sunk = append(sunk, o); return nil }
		var out, errOut bytes.Buffer
		_, calls, _, err := categorizeWithLLM(context.Background(), call,
			splitBatches(fiveCandidates()[:4], 2), nil, config.SpendContextMerchant, 3, 30, false, sink, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "batch 2/2") {
			t.Fatalf("err = %v, want the failing batch named", err)
		}
		if calls != 2 {
			t.Errorf("calls = %d, want 2 (the run stops at the failure)", calls)
		}
		if len(sunk) != 1 || len(sunk[0].Accepted) != 2 {
			t.Errorf("batch 1's verdicts must have reached the sink before the failure: %+v", sunk)
		}
	})
}

// TestCategorizeShowPromptPrintsTheFirstBatchInFull: fifty full
// prompts would bury the one worth reading, so only the first batch's
// is printed; later ones report their size.
func TestCategorizeShowPromptPrintsTheFirstBatchInFull(t *testing.T) {
	var prompts []string
	var out, errOut bytes.Buffer
	if _, _, _, err := categorizeWithLLM(context.Background(), answerEverything(&prompts),
		splitBatches(fiveCandidates(), 3), nil, config.SpendContextMerchant, 3, 30, true, nil, &out, &errOut); err != nil {
		t.Fatalf("categorizeWithLLM: %v", err)
	}
	se := errOut.String()
	if !strings.Contains(se, "--- LLM prompt (batch 1/2, attempt 1) ---") || !strings.Contains(se, "Taxonomy") ||
		!strings.Contains(se, "ASHGROVE BAKERY") {
		t.Errorf("the first batch's prompt must print in full:\n%s", se)
	}
	if !strings.Contains(se, "--- LLM prompt (batch 2/2, attempt 1): ") || !strings.Contains(se, "chars, ≈") {
		t.Errorf("later batches must report their size:\n%s", se)
	}
	if strings.Contains(se, "ELMFIELD GARAGE") {
		t.Error("a later batch's prompt must not print in full")
	}
	if n := strings.Count(se, "--- end prompt ---"); n != 1 {
		t.Errorf("full prompts printed = %d, want 1", n)
	}
}

func TestPrintCategorizeBatchPlan(t *testing.T) {
	batches := splitBatches(fiveCandidates(), 2)
	anchors := []merchantAnchor{{Signature: "NORTHWIND HARDWARE", Name: "Northwind Hardware", Detailed: "HOME_IMPROVEMENT_HARDWARE"}}
	first := buildCategorizeUserPrompt(batches[0], anchors, config.SpendContextMerchant, nil)

	var out bytes.Buffer
	printCategorizeBatchPlan(&out, batches, 2, anchors, config.SpendContextMerchant, 3, true)
	for _, want := range []string{
		"categorize: plan",
		"merchants:      5 in 3 batch(es) of up to 2 (--batch)",
		"batch sizes:    2 × 2, 1 × 1",
		"anchors:        1 in the first batch",
		fmt.Sprintf("first prompt:   %d chars, ≈%d tokens", len(first), estimateTokens(first)),
		"model calls:    3 at best, 9 at worst (--max-attempts 3)",
		"dry run:        the model IS asked",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	printCategorizeBatchPlan(&out, batches, 2, anchors, config.SpendContextMerchant, 3, false)
	if strings.Contains(out.String(), "dry run:") {
		t.Error("a real run's plan must not carry the dry-run line")
	}
	if len(splitBatches(nil, 2)) != 0 {
		t.Error("no candidates, no batches")
	}
	if got := formatBatchSizes(splitBatches(make([]merchantCandidate, 2010), 40)); got != "50 × 40, 1 × 10" {
		t.Errorf("sizes = %q", got)
	}
}

// TestCategorizePromptForbidsTheDeltas guards the other half of the
// no-deltas contract: the gauntlet rejects them, and the prompt has to
// have said so, or every run pays for a wasted round-trip.
func TestCategorizePromptForbidsTheDeltas(t *testing.T) {
	p := buildCategorizeUserPrompt(twoCandidates(), nil, config.SpendContextMerchant, nil)
	for _, d := range canonical.DeltaSpendCategories() {
		if !strings.Contains(p, d.Detailed) {
			t.Errorf("the prompt must name %q as forbidden", d.Detailed)
		}
	}
	if !strings.Contains(p, "FOOD_AND_DRINK_COFFEE") {
		t.Error("the prompt must carry the vendored vocabulary")
	}
	// The vocabulary block lists the vendored values; the deltas may
	// appear only in the prohibition sentence.
	if strings.Contains(p, "\n  "+canonical.SpendDetailedInternalTransfer+"\t") {
		t.Error("a delta must never appear as a choosable taxonomy row")
	}
}

// ---- candidate collection ----------------------------------------------------

// openCategorizeGold builds a migrated in-memory gold with one card
// source, so the candidate queries run against the real macros rather
// than a hand-rolled stand-in.
func openCategorizeGold(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := gold.Open(":memory:", gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := gold.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate gold: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('bank', 'chase', '/tmp/test.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('bank', 'CASH1', 'cash', 'Everyday', 1, 1),
                    ('bank', 'CARD1', 'card', 'Card', 1, 1);
    `); err != nil {
		t.Fatalf("seed dimensions: %v", err)
	}
	return db, ctx
}

// seedSpendTxn inserts one transaction and is deliberately positional —
// the tests below read better as a table than as a struct literal per
// row.
func seedSpendTxn(t *testing.T, db *sql.DB, ctx context.Context,
	id, account, kind string, dayN int64, amount float64, counterparty, providerCategory string) {
	t.Helper()
	var pc any
	if providerCategory != "" {
		pc = providerCategory
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount,
                                  counterparty, provider_category)
             VALUES ('bank', ?, ?, ?, ?, 'USD', ?, ?, ?)`,
		id, dayN*gold.SecondsPerDay, account, kind, amount, counterparty, pc); err != nil {
		t.Fatalf("seed transaction %s: %v", id, err)
	}
}

func runEnrichment(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := spending.RunDeterministicPass(ctx, db, spending.Options{
		MatchWindowDays: 5, MatchTolerancePct: 0.5, Now: 1_700_000_000,
	}); err != nil {
		t.Fatalf("RunDeterministicPass: %v", err)
	}
}

func signaturesOf(cands []merchantCandidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Signature)
	}
	return out
}

// seedBacklogGold builds the five cases the candidate rules turn on:
// an untouched merchant, one the provider map already placed, one the
// merchant store already answers, one the transfer fence refuses, and
// one whose narrative is a bare booking code with nothing to name.
func seedBacklogGold(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, ctx := openCategorizeGold(t)
	seedSpendTxn(t, db, ctx, "T-BACKLOG", "CARD1", "purchase", 10, -12.50, "Blue Harbour Cafe", "")
	seedSpendTxn(t, db, ctx, "T-PROVIDER", "CARD1", "purchase", 11, -80, "Orchard Lane Market", "Groceries")
	seedSpendTxn(t, db, ctx, "T-STORED", "CARD1", "purchase", 12, -40, "Northwind Hardware", "")
	seedSpendTxn(t, db, ctx, "T-FENCED", "CASH1", "withdrawal", 13, -200, "Zelle Payment To Jordan Rivers", "")
	seedSpendTxn(t, db, ctx, "T-BARE", "CASH1", "withdrawal", 14, -20, "KH", "")
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name)
             VALUES ('NORTHWIND HARDWARE', 'Northwind Hardware', 'HOME_IMPROVEMENT_HARDWARE',
                     1, 100, 'test-model')`); err != nil {
		t.Fatalf("seed merchant store: %v", err)
	}
	runEnrichment(t, db, ctx)
	return db, ctx
}

func TestCollectMerchantCandidatesBacklogOnly(t *testing.T) {
	db, ctx := seedBacklogGold(t)

	cands, skipped, err := collectMerchantCandidates(ctx, db, config.SpendContextMerchant, 3, backlogUnplaced)
	if err != nil {
		t.Fatalf("collectMerchantCandidates: %v", err)
	}
	got := signaturesOf(cands)
	if len(got) != 1 || got[0] != "BLUE HARBOUR CAFE" {
		t.Errorf("default candidates = %v, want just BLUE HARBOUR CAFE "+
			"(a provider verdict and a stored verdict are both already answered; a bare code has nothing to name)", got)
	}
	if skipped.Fenced != 1 {
		t.Errorf("fenced = %d, want 1", skipped.Fenced)
	}
	if skipped.Uninformative != 1 {
		t.Errorf("uninformative = %d, want 1", skipped.Uninformative)
	}
	if cands[0].Txns != 1 || cands[0].PerSource["bank"] != 1 {
		t.Errorf("counts = %d / %v", cands[0].Txns, cands[0].PerSource)
	}
}

func TestCollectMerchantCandidatesAll(t *testing.T) {
	db, ctx := seedBacklogGold(t)

	cands, skipped, err := collectMerchantCandidates(ctx, db, config.SpendContextMerchant, 3, backlogAll)
	if err != nil {
		t.Fatalf("collectMerchantCandidates: %v", err)
	}
	got := signaturesOf(cands)
	want := []string{"BLUE HARBOUR CAFE", "NORTHWIND HARDWARE", "ORCHARD LANE MARKET"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("--all candidates = %v, want %v", got, want)
	}
	if skipped.Fenced != 1 {
		t.Errorf("fenced = %d, want 1 — --all widens the backlog, never the fence", skipped.Fenced)
	}
	if skipped.Uninformative != 1 {
		t.Errorf("uninformative = %d, want 1 — --all widens the backlog, never the word gate", skipped.Uninformative)
	}
}

// TestCollectMerchantCandidatesFenceHoldsAtEveryContext is the privacy
// invariant: the context level says how much of a candidate is
// described, never which signatures are candidates. A P2P narrative
// carries a person's name where a merchant would be, and it must be
// out of the set at the widest setting exactly as at the narrowest.
func TestCollectMerchantCandidatesFenceHoldsAtEveryContext(t *testing.T) {
	db, ctx := seedBacklogGold(t)

	var first []string
	for _, level := range []string{
		config.SpendContextMerchant,
		config.SpendContextDescriptor,
		config.SpendContextTransaction,
	} {
		cands, skipped, err := collectMerchantCandidates(ctx, db, level, 3, backlogAll)
		if err != nil {
			t.Fatalf("%s: collectMerchantCandidates: %v", level, err)
		}
		got := signaturesOf(cands)
		for _, sig := range got {
			if spending.TransferShaped(sig) {
				t.Errorf("%s: transfer-shaped signature %q became a candidate", level, sig)
			}
		}
		if skipped.Fenced != 1 {
			t.Errorf("%s: fenced = %d, want 1", level, skipped.Fenced)
		}
		if first == nil {
			first = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Errorf("%s: candidate set = %v, want the same set as the merchant level (%v)", level, got, first)
		}
	}
}

// TestCollectMerchantCandidatesSkipsUninformativeAtEveryContext is the
// word gate's counterpart to the fence test above. A signature with no
// word in it — what an MT940 narrative reduces to when the bank wrote
// nothing but its own tag — is refused at every context level, under
// the default backlog and under --all alike, and counted once per
// distinct signature; the real merchant beside it stays a candidate.
// With the spending.Uninformative check removed from
// collectMerchantCandidates, every level fails here on the candidate
// set and on the count, so the test is not vacuous.
func TestCollectMerchantCandidatesSkipsUninformativeAtEveryContext(t *testing.T) {
	db, ctx := openCategorizeGold(t)
	seedSpendTxn(t, db, ctx, "T-REAL", "CARD1", "purchase", 10, -12.50, "Blue Harbour Cafe", "")
	seedSpendTxn(t, db, ctx, "T-CODE-1", "CASH1", "withdrawal", 10, -20, "ZV01", "")
	seedSpendTxn(t, db, ctx, "T-CODE-2", "CASH1", "withdrawal", 11, -20, "ZV01", "")
	seedSpendTxn(t, db, ctx, "T-NUMBER", "CASH1", "withdrawal", 12, -35, "42", "")
	runEnrichment(t, db, ctx)

	for _, all := range []bool{false, true} {
		for _, level := range []string{
			config.SpendContextMerchant,
			config.SpendContextDescriptor,
			config.SpendContextTransaction,
		} {
			name := fmt.Sprintf("%s/all=%v", level, all)
			cands, skipped, err := collectMerchantCandidates(ctx, db, level, 3, backlogOf(all, false))
			if err != nil {
				t.Fatalf("%s: collectMerchantCandidates: %v", name, err)
			}
			for _, c := range cands {
				if spending.Uninformative(c.Signature) {
					t.Errorf("%s: uninformative signature %q became a candidate", name, c.Signature)
				}
			}
			if got := signaturesOf(cands); len(got) != 1 || got[0] != "BLUE HARBOUR CAFE" {
				t.Errorf("%s: candidates = %v, want just BLUE HARBOUR CAFE (a bare code has nothing to name)", name, got)
			}
			if skipped.Uninformative != 2 {
				t.Errorf("%s: uninformative = %d, want 2 distinct signatures over three rows", name, skipped.Uninformative)
			}
			if skipped.Fenced != 0 {
				t.Errorf("%s: fenced = %d, want 0 — a bare code is not a transfer", name, skipped.Fenced)
			}
		}
	}
}

// TestPrintNeverSent pins the plan lines the two gates print: one
// per gate, only when it fired, in the wording the operator greps for.
func TestPrintNeverSent(t *testing.T) {
	var quiet bytes.Buffer
	printNeverSent(&quiet, skippedSignatures{})
	if quiet.Len() != 0 {
		t.Errorf("gates that did not fire must print nothing, got:\n%s", quiet.String())
	}

	var out bytes.Buffer
	printNeverSent(&out, skippedSignatures{Fenced: 1, Uninformative: 2})
	want := "categorize: 1 signature(s) fenced as transfer-shaped and never sent\n" +
		"categorize: 2 signature(s) uninformative and never sent\n"
	if out.String() != want {
		t.Errorf("printNeverSent =\n%s\nwant\n%s", out.String(), want)
	}
}

// TestCollectMerchantCandidatesContextDepth pins what each level
// actually adds, since that is the whole difference between them.
func TestCollectMerchantCandidatesContextDepth(t *testing.T) {
	db, ctx := openCategorizeGold(t)
	seedSpendTxn(t, db, ctx, "T1", "CARD1", "purchase", 10, -12.50, "Blue Harbour Cafe", "")
	seedSpendTxn(t, db, ctx, "T2", "CARD1", "purchase", 10, -60, "Orchard Lane Market", "")
	runEnrichment(t, db, ctx)

	merchant, _, err := collectMerchantCandidates(ctx, db, config.SpendContextMerchant, 3, backlogUnplaced)
	if err != nil {
		t.Fatalf("merchant level: %v", err)
	}
	for _, c := range merchant {
		if len(c.Samples) != 0 {
			t.Errorf("the merchant level must carry no narratives, got %+v", c.Samples)
		}
	}
	if p := buildCategorizeUserPrompt(merchant, nil, config.SpendContextMerchant, nil); strings.Contains(p, "Blue Harbour Cafe") {
		t.Error("the merchant level must not send the raw narrative")
	}

	descriptor, _, err := collectMerchantCandidates(ctx, db, config.SpendContextDescriptor, 3, backlogUnplaced)
	if err != nil {
		t.Fatalf("descriptor level: %v", err)
	}
	p := buildCategorizeUserPrompt(descriptor, nil, config.SpendContextDescriptor, nil)
	if !strings.Contains(p, "Blue Harbour Cafe") {
		t.Error("the descriptor level must send the raw narrative")
	}
	if strings.Contains(p, formatEpochDay(10)) || strings.Contains(p, "-12.50") {
		t.Error("the descriptor level must not send dates or amounts")
	}

	transaction, _, err := collectMerchantCandidates(ctx, db, config.SpendContextTransaction, 3, backlogUnplaced)
	if err != nil {
		t.Fatalf("transaction level: %v", err)
	}
	p = buildCategorizeUserPrompt(transaction, nil, config.SpendContextTransaction, nil)
	if !strings.Contains(p, "-12.50") || !strings.Contains(p, "card") {
		t.Errorf("the transaction level must send amount and account kind:\n%s", p)
	}
	// Same day, same source: each merchant is the other's company.
	if !strings.Contains(p, "ORCHARD LANE MARKET") {
		t.Error("the transaction level must send nearby signatures")
	}
}

// TestCollectMerchantCandidatesFencesTheRawNarrative covers the second
// place the fence runs. A signature is a folded, capped view of the
// narrative it came from, so a rail token past the cap survives in the
// narrative and not in the key. Candidacy is unaffected — that stays
// identical across levels — but the narrative itself must not be sent.
func TestCollectMerchantCandidatesFencesTheRawNarrative(t *testing.T) {
	const long = "Northwind Hardware Supply Depot International Trading Company Wire"
	db, ctx := openCategorizeGold(t)
	seedSpendTxn(t, db, ctx, "T1", "CARD1", "purchase", 10, -40, long, "")
	runEnrichment(t, db, ctx)

	cands, skipped, err := collectMerchantCandidates(ctx, db, config.SpendContextDescriptor, 3, backlogUnplaced)
	if err != nil {
		t.Fatalf("collectMerchantCandidates: %v", err)
	}
	if skipped.Fenced != 0 || len(cands) != 1 {
		t.Fatalf("candidates = %v (fenced %d); the signature itself is not transfer-shaped",
			signaturesOf(cands), skipped.Fenced)
	}
	if spending.TransferShaped(cands[0].Signature) {
		t.Fatal("fixture is wrong: the signature should have dropped the rail token past the cap")
	}
	if len(cands[0].Samples) != 1 || cands[0].Samples[0].Descriptor != "" {
		t.Errorf("the raw narrative must be fenced away, got %q", cands[0].Samples[0].Descriptor)
	}
	if p := buildCategorizeUserPrompt(cands, nil, config.SpendContextDescriptor, nil); strings.Contains(p, "Wire") {
		t.Errorf("a fenced narrative must never reach the prompt:\n%s", p)
	}
}

// TestCollectMerchantAnchorsExcludesDeltasAndCandidates guards the
// in-context examples: an anchor carrying a delta would teach the model
// exactly the vocabulary the gauntlet then rejects.
func TestCollectMerchantAnchorsExcludesDeltasAndCandidates(t *testing.T) {
	db, ctx := openCategorizeGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('NORTHWIND HARDWARE', 'Northwind Hardware', 'HOME_IMPROVEMENT_HARDWARE', 1, 300, 'm'),
            ('SOUTHPORT LAUNDRY',  'Southport Laundry',  'internal_transfer',         1, 200, 'm'),
            ('BLUE HARBOUR CAFE',  'Blue Harbour Cafe',  'FOOD_AND_DRINK_COFFEE',     1, 100, 'm')`); err != nil {
		t.Fatalf("seed merchant store: %v", err)
	}

	anchors, err := collectMerchantAnchors(ctx, db, 10, map[string]bool{"BLUE HARBOUR CAFE": true})
	if err != nil {
		t.Fatalf("collectMerchantAnchors: %v", err)
	}
	if len(anchors) != 1 || anchors[0].Signature != "NORTHWIND HARDWARE" {
		t.Errorf("anchors = %+v, want only NORTHWIND HARDWARE "+
			"(the delta row and the current candidate are both excluded)", anchors)
	}

	none, err := collectMerchantAnchors(ctx, db, 0, nil)
	if err != nil || len(none) != 0 {
		t.Errorf("--max-anchors 0 must ask for nothing, got %+v (%v)", none, err)
	}
}

// TestCollectMerchantAnchorsFencesTransferShaped pins the fence on the
// one path that re-transmits stored keys. The merchant store is
// append-only across signature revisions and across widenings of the
// fence itself, so a verdict keyed on a transfer-shaped signature
// survives indefinitely; without this filter it would be emitted
// verbatim into every prompt, long after no transaction carries it.
func TestCollectMerchantAnchorsFencesTransferShaped(t *testing.T) {
	db, ctx := openCategorizeGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('NORTHWIND HARDWARE',       'Northwind Hardware', 'HOME_IMPROVEMENT_HARDWARE', 1, 300, 'm'),
            ('ZELLE SAMPLE PAYEE ZZ',    'Sample Payee',       'FOOD_AND_DRINK_COFFEE',     1, 200, 'm')`); err != nil {
		t.Fatalf("seed merchant store: %v", err)
	}

	anchors, err := collectMerchantAnchors(ctx, db, 10, nil)
	if err != nil {
		t.Fatalf("collectMerchantAnchors: %v", err)
	}
	for _, a := range anchors {
		if a.Signature == "ZELLE SAMPLE PAYEE ZZ" {
			t.Fatalf("a transfer-shaped signature reached the anchor set: %+v", anchors)
		}
	}
	if len(anchors) != 1 {
		t.Fatalf("anchors = %+v, want only the merchant one", anchors)
	}

	prompt := buildCategorizeUserPrompt([]merchantCandidate{
		{Signature: "NORTHWIND HARDWARE", Txns: 1, PerSource: map[string]int{"bank": 1}},
	}, anchors, config.SpendContextMerchant, nil)
	if strings.Contains(prompt, "ZELLE") {
		t.Error("the fenced signature reached the prompt text")
	}
}

// TestCollectSourceRatesCountsResolvedCategories pins the counting
// rule, not one spelling of it: a row counts as categorised when the
// resolved category is non-NULL, whichever scope answered. Reading
// spend_txn_categories() and spelling the same COALESCE over the two
// scopes inline resolve identically — the macro is that COALESCE over
// those joins — so this holds either way and is a behaviour pin rather
// than a regression test for which one the reader uses. Over the
// backlog fixture the categorised rows are the provider-placed one and
// the one the merchant store answers; the untouched merchant, the
// fenced narrative and the bare code are the remainder.
func TestCollectSourceRatesCountsResolvedCategories(t *testing.T) {
	db, ctx := seedBacklogGold(t)

	rates, err := collectSourceRates(ctx, db)
	if err != nil {
		t.Fatalf("collectSourceRates: %v", err)
	}
	if len(rates) != 1 || rates[0].Source != "bank" {
		t.Fatalf("rates = %+v, want one row for the fixture's source", rates)
	}
	if rates[0].Total != 5 || rates[0].Categorised != 2 {
		t.Errorf("rate = %d/%d, want 2/5", rates[0].Categorised, rates[0].Total)
	}
}

// ---- summary counters --------------------------------------------------------

func TestPrintCategorizeSummaryCounters(t *testing.T) {
	candidates := []merchantCandidate{
		{Signature: "BLUE HARBOUR CAFE", Txns: 4, PerSource: map[string]int{"bank": 4}},
		{Signature: "NORTHWIND HARDWARE", Txns: 2, PerSource: map[string]int{"bank": 2}},
		{Signature: "SOUTHPORT LAUNDRY", Txns: 7, PerSource: map[string]int{"other-bank": 7}},
	}
	valid := []categorization{
		{Signature: "BLUE HARBOUR CAFE", MerchantName: "Blue Harbour Cafe", Detailed: "FOOD_AND_DRINK_COFFEE"},
	}
	leftovers := uncategorisedCandidates(candidates, valid)
	canaries := &spendCanaries{
		Rates: []sourceRate{
			{Source: "bank", Total: 10, Categorised: 5},
			{Source: "other-bank", Total: 4, Categorised: 4},
		},
		ProviderMisses: []providerMiss{{Source: "bank", Category: "Seasonal Offers", Count: 3}},
		Pairs: []spending.Pair{{
			Debit:  spending.Leg{Source: "bank", TxID: "T-OUT", Account: "CASH1", Day: 20, Amount: -400, Currency: "USD", Signature: "AUTOPAY"},
			Credit: spending.Leg{Source: "bank", TxID: "T-IN", Account: "CARD1", Day: 20, Amount: 400, Currency: "USD", Signature: "CARD PAYMENT"},
		}},
		Unmatched: []spending.Leg{
			{Source: "bank", TxID: "T-LONE", Account: "CASH1", Day: 30, Amount: -9000, Currency: "USD", Signature: "WIRE OUT"},
		},
		CrossCurrency: []crossCurrencyShape{{
			Debit:  spending.Leg{Source: "bank", TxID: "A", Day: 40, Amount: -1000, Currency: "USD"},
			Credit: spending.Leg{Source: "bank", TxID: "B", Day: 41, Amount: 950, Currency: "CHF"},
		}},
	}

	var out bytes.Buffer
	printCategorizeSummary(&out, candidates, valid, leftovers, 1, 2, 3, canaries)
	got := out.String()

	for _, want := range []string{
		"merchants asked:    3 (13 transaction(s))",
		"LLM attempts:       2 over 1 batch(es)",
		"rejected rows:      3",
		"categorised:        1 merchant(s), covering 4 transaction(s)",
		"left uncategorised: 2 merchant(s)",
		"categorisation rate by source: bank=50.0% (5/10), other-bank=100.0% (4/4)",
		`provider-map misses: 1 value(s) over 3 row(s)`,
		`"Seasonal Offers"`,
		"internal-transfer pairs excluded from spending: 1",
		"AUTOPAY",
		"CARD PAYMENT",
		"largest unmatched transfer legs (1 unpaired in total",
		"WIRE OUT",
		"cross-currency near-pairs (1)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
	// Both legs of every pair, or the listing cannot answer the question
	// it exists for.
	if !strings.Contains(got, "CASH1") || !strings.Contains(got, "CARD1") {
		t.Errorf("the pair listing must show both legs:\n%s", got)
	}
}

// TestSplitPairsBySpending pins the listing's scope: a pair is listed
// when either leg was a spending candidate, in the order the matcher
// reported it, and a pair formed entirely outside the population is
// counted rather than shown. With splitPairsBySpending returning every
// pair and a zero count, both halves fail, so the test is not vacuous.
func TestSplitPairsBySpending(t *testing.T) {
	in := func(id string) spending.Leg { return spending.Leg{TxID: id, InPopulation: true} }
	out := func(id string) spending.Leg { return spending.Leg{TxID: id} }
	pairs := []spending.Pair{
		{Debit: in("A-OUT"), Credit: out("A-IN")},  // cash to card: the withdrawal was spending
		{Debit: out("B-OUT"), Credit: out("B-IN")}, // wallet to wallet
		{Debit: out("C-OUT"), Credit: in("C-IN")},  // the credit side is the spending row
		{Debit: out("D-OUT"), Credit: out("D-IN")}, // brokerage to brokerage
	}
	listed, outside := splitPairsBySpending(pairs)
	got := make([]string, 0, len(listed))
	for _, p := range listed {
		got = append(got, p.Debit.TxID)
	}
	if want := []string{"A-OUT", "C-OUT"}; !slices.Equal(got, want) {
		t.Errorf("listed = %v, want %v (either leg in the population, matcher order kept)", got, want)
	}
	if outside != 2 {
		t.Errorf("outside = %d, want 2", outside)
	}

	if listed, outside := splitPairsBySpending(nil); len(listed) != 0 || outside != 0 {
		t.Errorf("splitPairsBySpending(nil) = (%v, %d), want nothing", listed, outside)
	}
}

// TestPrintSpendCanariesCountsPairsOutsideSpending pins the one line
// that keeps the filtered listing honest — and its absence when there
// is nothing to count, so a run with no out-of-population pairs does
// not read as if something were withheld.
func TestPrintSpendCanariesCountsPairsOutsideSpending(t *testing.T) {
	pair := spending.Pair{
		Debit:  spending.Leg{Source: "bank", TxID: "T-OUT", Account: "CASH1", Day: 20, Amount: -400, Currency: "USD", Signature: "AUTOPAY", InPopulation: true},
		Credit: spending.Leg{Source: "bank", TxID: "T-IN", Account: "CARD1", Day: 20, Amount: 400, Currency: "USD", Signature: "CARD PAYMENT"},
	}
	const counter = "    3 pair(s) matched outside the spending population, not listed\n"

	var with bytes.Buffer
	printSpendCanaries(&with, &spendCanaries{Pairs: []spending.Pair{pair}, PairsOutsideSpending: 3})
	got := with.String()
	if !strings.Contains(got, "internal-transfer pairs excluded from spending: 1\n") {
		t.Errorf("the headline counts the listed pairs only:\n%s", got)
	}
	if !strings.Contains(got, counter) {
		t.Errorf("summary missing %q:\n%s", counter, got)
	}
	if strings.Index(got, "AUTOPAY") > strings.Index(got, counter) {
		t.Errorf("the counter line belongs after the listing:\n%s", got)
	}

	var without bytes.Buffer
	printSpendCanaries(&without, &spendCanaries{Pairs: []spending.Pair{pair}})
	if strings.Contains(without.String(), "outside the spending population") {
		t.Errorf("nothing to count must print no counter line:\n%s", without.String())
	}
}

func TestStratifiedMerchantSample(t *testing.T) {
	items := []merchantCandidate{
		{Signature: "A1", PerSource: map[string]int{"bank": 3}},
		{Signature: "A2", PerSource: map[string]int{"bank": 3}},
		{Signature: "A3", PerSource: map[string]int{"bank": 3}},
		{Signature: "B1", PerSource: map[string]int{"other-bank": 3}},
	}
	out := stratifiedSample(items, 2, merchantCandidate.DominantSource)
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	sources := map[string]bool{}
	for _, c := range out {
		sources[c.DominantSource()] = true
	}
	if len(sources) != 2 {
		t.Errorf("the sample must span sources, got %v", sources)
	}
	if all := stratifiedSample(items, 10, merchantCandidate.DominantSource); len(all) != len(items) {
		t.Errorf("a cap above the input size must return everything, got %d", len(all))
	}
}

func TestDominantSource(t *testing.T) {
	c := merchantCandidate{PerSource: map[string]int{"bank": 2, "other-bank": 5}}
	if got := c.DominantSource(); got != "other-bank" {
		t.Errorf("DominantSource = %q, want other-bank", got)
	}
	// A tie resolves alphabetically, so a listing does not reshuffle
	// between runs.
	tie := merchantCandidate{PerSource: map[string]int{"zeta": 3, "alpha": 3}}
	if got := tie.DominantSource(); got != "alpha" {
		t.Errorf("tie broke to %q, want alpha", got)
	}
}

// ---- the CLI surface ----------------------------------------------------------

// enableCategorization rewrites the harness config with a
// spending.categorization block. The endpoint points at a port nothing
// listens on: these tests must never reach a model, and a regression
// that tried would fail immediately rather than hang.
func enableCategorization(t *testing.T, cfgPath string) {
	t.Helper()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read cfg: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse cfg: %v", err)
	}
	m["spending"] = map[string]any{"categorization": map[string]any{
		"model": map[string]any{
			"baseUrl": "http://127.0.0.1:1/v1",
			"api":     "openai-completions",
			"name":    "test-model",
		},
	}}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode cfg: %v", err)
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
}

// seedGhostEnrichment writes a derived enrichment row for a
// transaction that does not exist. The deterministic pass deletes every
// derived row it owns, so this row's survival is a direct read of
// whether the pass ran.
func seedGhostEnrichment(t *testing.T, goldPath string) {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatalf("open gold to seed: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
             VALUES ('ghost-source', 'G1', 'GHOST SIGNATURE', 1, NULL, 'signature-only', 100)`); err != nil {
		t.Fatalf("seed ghost enrichment: %v", err)
	}
}

func ghostEnrichmentRows(t *testing.T, goldPath string) int {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("open gold read-only: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM spend_txn_enrichment
                             WHERE silver_source_id = 'ghost-source'`).Scan(&n); err != nil {
		t.Fatalf("count ghost rows: %v", err)
	}
	return n
}

// TestCategorizeDryRunIsReadOnlyAndSaysSo pins the one place this
// command differs from its precedent in substance rather than wording.
// A real run re-asserts the deterministic verdicts first; a dry run
// cannot, because that is a write. So it must not do it, and it must
// label the plan it produces instead of quietly planning against a
// different world.
func TestCategorizeDryRunIsReadOnlyAndSaysSo(t *testing.T) {
	cfg := setupCLITest(t)
	enableCategorization(t, cfg)
	if _, se, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatalf("init failed: %s", se)
	}
	if _, se, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatalf("load failed: %s", se)
	}
	goldPath := goldPathFromCfg(cfg)
	seedGhostEnrichment(t, goldPath)

	so, se, code := run(t, "-c", cfg, "categorize", "-n")
	if code != 0 {
		t.Fatalf("categorize -n: exit %d, stderr=%s", code, se)
	}
	if !strings.Contains(so, "deterministic pass did NOT run") {
		t.Errorf("the dry run must say it skipped the pass:\n%s", so)
	}
	if !strings.Contains(so, "AS OF THE LAST LOAD") {
		t.Errorf("the dry run must label its plan:\n%s", so)
	}
	if strings.Contains(so, "row(s) enriched") {
		t.Errorf("the dry run must not run the pass:\n%s", so)
	}
	if n := ghostEnrichmentRows(t, goldPath); n != 1 {
		t.Errorf("ghost enrichment rows after a dry run = %d, want 1 (nothing was written)", n)
	}
	// The canaries describe gold, so they print even with an empty
	// backlog — that is what makes them an audit surface rather than a
	// run log.
	if !strings.Contains(so, "categorize: canaries") {
		t.Errorf("the canaries must print even when there is nothing to categorise:\n%s", so)
	}

	so, se, code = run(t, "-c", cfg, "categorize")
	if code != 0 {
		t.Fatalf("categorize: exit %d, stderr=%s", code, se)
	}
	if !strings.Contains(so, "row(s) enriched") {
		t.Errorf("a real run must re-assert the deterministic pass first:\n%s", so)
	}
	if n := ghostEnrichmentRows(t, goldPath); n != 0 {
		t.Errorf("ghost enrichment rows after a real run = %d, want 0 (the pass owns them)", n)
	}
}

func TestCategorizeRefusesWithoutAModel(t *testing.T) {
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	_, se, code := run(t, "-c", cfg, "categorize", "-n")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(se, "spending.categorization.model") {
		t.Errorf("stderr must point at the missing config block: %s", se)
	}
}

// seedTwoMerchantVerdicts initialises gold and writes two merchant verdicts
// into it, returning the config path: the fixture the dump and the
// --forget tests share.
func seedTwoMerchantVerdicts(t *testing.T) (cfg string) {
	t.Helper()
	cfg = setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	db, err := sql.Open("duckdb", goldPathFromCfg(cfg))
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('BLUE HARBOUR CAFE',  'Blue Harbour Cafe',  'FOOD_AND_DRINK_COFFEE',     1, 1700000000, 'test-model'),
            ('NORTHWIND HARDWARE', 'Northwind Hardware', 'HOME_IMPROVEMENT_HARDWARE', 1, 1700000000, 'test-model')`); err != nil {
		t.Fatalf("seed merchant store: %v", err)
	}
	return cfg
}

// storedSignatures reads the merchant store's keys, read-only, in
// signature order.
func storedSignatures(t *testing.T, cfg string) []string {
	t.Helper()
	db, err := sql.Open("duckdb", goldPathFromCfg(cfg)+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("open gold read-only: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT merchant_signature FROM spend_merchant_categories ORDER BY 1`)
	if err != nil {
		t.Fatalf("read merchant store: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sig string
		if err := rows.Scan(&sig); err != nil {
			t.Fatalf("scan merchant store: %v", err)
		}
		out = append(out, sig)
	}
	return out
}

func TestCategorizationsDump(t *testing.T) {
	cfg := seedTwoMerchantVerdicts(t)

	so, se, code := run(t, "-c", cfg, "categorizations")
	if code != 0 {
		t.Fatalf("categorizations: exit %d, stderr=%s", code, se)
	}
	for _, want := range []string{
		"merchant_signature", "BLUE HARBOUR CAFE", "Blue Harbour Cafe",
		"FOOD_AND_DRINK_COFFEE", "test-model", "2023-11-14",
	} {
		if !strings.Contains(so, want) {
			t.Errorf("table output missing %q:\n%s", want, so)
		}
	}

	so, _, code = run(t, "-c", cfg, "categorizations", "-f", "csv", "-d", "HOME_IMPROVEMENT_HARDWARE")
	if code != 0 {
		t.Fatalf("filtered dump: exit %d", code)
	}
	if strings.Contains(so, "BLUE HARBOUR CAFE") {
		t.Errorf("the -d filter must exclude other categories:\n%s", so)
	}
	if !strings.Contains(so, "NORTHWIND HARDWARE") {
		t.Errorf("the -d filter dropped its own match:\n%s", so)
	}

	if _, _, code := run(t, "-c", cfg, "categorizations", "-f", "nope"); code != 2 {
		t.Errorf("a bad format must exit 2, got %d", code)
	}
	if _, _, code := run(t, "-c", cfg, "categorizations", "extra"); code != 2 {
		t.Errorf("a positional argument must exit 2, got %d", code)
	}
}

// TestCategorizationsForget: a present signature is removed and
// reported with its name and category; an absent one is reported and
// is not an error; every other row is untouched.
func TestCategorizationsForget(t *testing.T) {
	cfg := seedTwoMerchantVerdicts(t)
	so, se, code := run(t, "-c", cfg, "categorizations",
		"--forget", "BLUE HARBOUR CAFE", "--forget", "NO SUCH MERCHANT")
	if code != 0 {
		t.Fatalf("categorizations --forget: exit %d, stderr=%s", code, se)
	}
	for _, want := range []string{
		`forgot "BLUE HARBOUR CAFE" — Blue Harbour Cafe [FOOD_AND_DRINK_COFFEE]`,
		`no verdict stored at "NO SUCH MERCHANT"`,
		"1 verdict(s) removed, 1 not found",
	} {
		if !strings.Contains(so, want) {
			t.Errorf("stdout missing %q:\n%s", want, so)
		}
	}
	if got := storedSignatures(t, cfg); !slices.Equal(got, []string{"NORTHWIND HARDWARE"}) {
		t.Errorf("store after --forget = %v, want only NORTHWIND HARDWARE", got)
	}

	// A miss alone is still a clean exit.
	if _, se, code := run(t, "-c", cfg, "categorizations", "--forget", "BLUE HARBOUR CAFE"); code != 0 {
		t.Errorf("a miss must not fail the command: exit %d, stderr=%s", code, se)
	}
}

// TestCategorizationsForgetDryRunAndReadOnly: the dry run says what it
// would remove and writes nothing, on a read-only gold too; a real
// removal against a read-only gold is refused with the dry run named.
func TestCategorizationsForgetDryRunAndReadOnly(t *testing.T) {
	cfg := seedTwoMerchantVerdicts(t)
	both := []string{"BLUE HARBOUR CAFE", "NORTHWIND HARDWARE"}

	so, se, code := run(t, "-c", cfg, "categorizations", "--forget", "NORTHWIND HARDWARE", "-n")
	if code != 0 {
		t.Fatalf("dry run: exit %d, stderr=%s", code, se)
	}
	for _, want := range []string{
		`would forget "NORTHWIND HARDWARE" — Northwind Hardware [HOME_IMPROVEMENT_HARDWARE]`,
		"1 verdict(s) would be removed, 0 not found; nothing written",
	} {
		if !strings.Contains(so, want) {
			t.Errorf("dry-run stdout missing %q:\n%s", want, so)
		}
	}
	if got := storedSignatures(t, cfg); !slices.Equal(got, both) {
		t.Errorf("a dry run wrote: store = %v", got)
	}

	if _, se, code := run(t, "-r", "-c", cfg, "categorizations", "--forget", "NORTHWIND HARDWARE", "-n"); code != 0 {
		t.Errorf("a dry run must be allowed read-only: exit %d, stderr=%s", code, se)
	}

	_, se, code = run(t, "-r", "-c", cfg, "categorizations", "--forget", "NORTHWIND HARDWARE")
	if code != errs.ExitRWNeeded {
		t.Errorf("read-only removal: exit %d, want %d", code, errs.ExitRWNeeded)
	}
	if !strings.Contains(se, "read-only") || !strings.Contains(se, "--dry-run") {
		t.Errorf("the refusal must explain read-only and name the dry run: %s", se)
	}
	if got := storedSignatures(t, cfg); !slices.Equal(got, both) {
		t.Errorf("a refused removal wrote: store = %v", got)
	}
}

// TestCategorizationsDumpPrivacy pins the dump's -p behaviour in every
// format: the signature is a fold of the raw narrative and masks
// whole, while the merchant name — which only exists because the
// signature cleared the transfer fence, and which is what names a
// verdict to --forget — stays legible.
func TestCategorizationsDumpPrivacy(t *testing.T) {
	cfg := seedTwoMerchantVerdicts(t)

	for _, format := range []string{"table", "csv", "csv_plain", "json"} {
		so, se, code := run(t, "-c", cfg, "categorizations", "-f", format, "-p")
		if code != 0 {
			t.Fatalf("%s: exit %d, stderr=%s", format, code, se)
		}
		if strings.Contains(so, "BLUE HARBOUR CAFE") {
			t.Errorf("%s: the signature survived -p:\n%s", format, so)
		}
		if !strings.Contains(so, "***") {
			t.Errorf("%s: no free-text placeholder in the output:\n%s", format, so)
		}
		if strings.Contains(so, "Blue Harbour Cafe") {
			t.Errorf("%s: the merchant name survived -p:\n%s", format, so)
		}
		if !strings.Contains(so, "FOOD_AND_DRINK_COFFEE") {
			t.Errorf("%s: the category must stay legible:\n%s", format, so)
		}
	}

	// Without -p the dump is verbatim, which is what --forget needs.
	so, _, code := run(t, "-c", cfg, "categorizations")
	if code != 0 || !strings.Contains(so, "BLUE HARBOUR CAFE") {
		t.Errorf("the default dump must print signatures verbatim:\n%s", so)
	}
}

// TestCategorizationsForgetFlagMisuse: the dump's flags and the
// removal's do not mix, and a removal needs a signature.
func TestCategorizationsForgetFlagMisuse(t *testing.T) {
	cfg := seedTwoMerchantVerdicts(t)
	for _, args := range [][]string{
		{"--forget", "BLUE HARBOUR CAFE", "-d", "FOOD_AND_DRINK_COFFEE"},
		{"--forget", "BLUE HARBOUR CAFE", "-f", "json"},
		{"--forget", "BLUE HARBOUR CAFE", "-p"},
		{"-n"},
		{"--forget", ""},
		{"--forget", "BLUE HARBOUR CAFE", "extra"},
	} {
		if _, _, code := run(t, append([]string{"-c", cfg, "categorizations"}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if got := storedSignatures(t, cfg); len(got) != 2 {
		t.Errorf("a refused invocation wrote: store = %v", got)
	}
}

// TestCategorizeBatchMustBePositive: a batch of zero would send
// nothing forever, so it is refused at the flag, before config or
// gold are touched.
func TestCategorizeBatchMustBePositive(t *testing.T) {
	cfg := setupCLITest(t)
	for _, v := range []string{"0", "-3"} {
		_, se, code := run(t, "-c", cfg, "categorize", "-n", "--batch", v)
		if code != 2 {
			t.Errorf("--batch %s: exit = %d, want 2", v, code)
		}
		if !strings.Contains(se, "--batch must be at least 1") {
			t.Errorf("--batch %s: stderr must say why: %s", v, se)
		}
	}
}

// TestCategorizeDryRunPrintsThePlanBeforeAnyCall runs against an
// endpoint nothing listens on, so the first call fails at once. The
// plan must already be on stdout by then — that is what lets an
// operator learn what a run costs without waiting on a timeout.
func TestCategorizeDryRunPrintsThePlanBeforeAnyCall(t *testing.T) {
	cfg := setupSpendingGold(t)
	enableCategorization(t, cfg)
	so, se, code := run(t, "-c", cfg, "categorize", "-n")
	if code == 0 {
		t.Fatalf("the endpoint is unreachable; the run must fail:\n%s", so)
	}
	for _, want := range []string{
		"categorize: plan",
		"merchants:      1 in 1 batch(es) of up to 40 (--batch)",
		"batch sizes:    1 × 1",
		"anchors:        1 in the first batch",
		"first prompt:",
		"dry run:        the model IS asked",
		"categorize: stopped; nothing stored (dry run)",
	} {
		if !strings.Contains(so, want) {
			t.Errorf("stdout missing %q:\n%s", want, so)
		}
	}
	if !strings.Contains(se, "batch 1/1") {
		t.Errorf("the failure must name the batch: %s", se)
	}
}

// TestCollectMerchantCandidatesFencesTheWholeRow pins the fence's
// row-level reading at candidacy, which is the one place a key-only
// reading lets a private individual through. A mobile person-to-person
// rail leads the narrative and names no payee; the reduction prefers
// the counterparty, so the key is the payee's name and nothing else —
// not transfer-shaped, carrying a word, not the bank's filing, so
// every key-only refusal passes it. The rail survives in the provider's
// filing and in the raw narrative, which is what refuses the row.
//
// Every value is invented. The assertions are about a name reaching a
// prompt, so the fixture is written as one: a person-shaped payee
// beside an ordinary merchant on the same day and source, which is
// also what makes the neighbour list non-trivial.
func TestCollectMerchantCandidatesFencesTheWholeRow(t *testing.T) {
	const (
		payee     = "EXAMPLE, PERSON"
		narrative = "DEBIT UBS TWINT; EXAMPLE, PERSON; TWINT-EXAMPLE"
		filing    = "Debit UBS TWINT"
	)
	db, ctx := openCategorizeGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount,
                                  counterparty, description, provider_category)
             VALUES ('bank', 'T-P2P', ?, 'CASH1', 'withdrawal', 'USD', -60, ?, ?, ?)`,
		10*gold.SecondsPerDay, payee, narrative, filing); err != nil {
		t.Fatalf("seed the person-to-person row: %v", err)
	}
	seedSpendTxn(t, db, ctx, "T-SHOP", "CARD1", "purchase", 10, -12.50, "Blue Harbour Cafe", "")
	runEnrichment(t, db, ctx)

	// The key alone is exactly the payee, and every key-only refusal
	// admits it: without the row reading there is no gate left.
	const key = "EXAMPLE PERSON"
	if got := spending.Normalize(payee, narrative); got != key {
		t.Fatalf("fixture is wrong: Normalize = %q, want the payee alone (%q)", got, key)
	}
	if spending.TransferShaped(key) || spending.Uninformative(key) || spending.FilingOnly(key, filing) {
		t.Fatal("fixture is wrong: a key-only gate already refuses this key, so the test would pass vacuously")
	}

	for _, all := range []bool{false, true} {
		for _, level := range []string{
			config.SpendContextMerchant,
			config.SpendContextDescriptor,
			config.SpendContextTransaction,
		} {
			name := fmt.Sprintf("%s/all=%v", level, all)
			cands, skipped, err := collectMerchantCandidates(ctx, db, level, 3, backlogOf(all, false))
			if err != nil {
				t.Fatalf("%s: collectMerchantCandidates: %v", name, err)
			}
			if slices.Contains(signaturesOf(cands), key) {
				t.Errorf("%s: the payee became a merchant candidate: %v", name, signaturesOf(cands))
			}
			if skipped.Fenced != 1 {
				t.Errorf("%s: fenced = %d, want 1 (the refusal is counted, not silent)", name, skipped.Fenced)
			}
			for _, c := range cands {
				for _, s := range c.Samples {
					if slices.Contains(s.Neighbours, key) {
						t.Errorf("%s: the payee reached a neighbour list: %v", name, s.Neighbours)
					}
				}
			}
			if csv := formatCandidateSignatureCSV(cands); strings.Contains(csv, "EXAMPLE") {
				t.Errorf("%s: the payee reached the candidate CSV:\n%s", name, csv)
			}
			if p := buildCategorizeUserPrompt(cands, nil, level, nil); strings.Contains(p, "EXAMPLE") {
				t.Errorf("%s: the payee reached the prompt", name)
			}
		}
	}

	// The store is append-only across signature revisions, so a verdict
	// an older fence admitted at that key is still there. The row fence
	// keeps it out of the anchor block, which is prompt content too.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            (?,                    'Example Person',     'FOOD_AND_DRINK_RESTAURANT', 1, 300, 'm'),
            ('NORTHWIND HARDWARE', 'Northwind Hardware', 'HOME_IMPROVEMENT_HARDWARE', 1, 200, 'm')`,
		key); err != nil {
		t.Fatalf("seed merchant store: %v", err)
	}
	anchors, err := collectMerchantAnchors(ctx, db, 10, nil)
	if err != nil {
		t.Fatalf("collectMerchantAnchors: %v", err)
	}
	if len(anchors) != 1 || anchors[0].Signature != "NORTHWIND HARDWARE" {
		t.Errorf("anchors = %+v, want only the merchant one", anchors)
	}
	if csv := formatAnchorSignatureCSV(anchors); strings.Contains(csv, "EXAMPLE") {
		t.Errorf("the payee reached the anchor CSV:\n%s", csv)
	}
}

// TestRefineBacklogAsksOnlyWhereTheModelGaveUp pins the narrow re-ask.
//
// Three signatures with a catch-all verdict and one with a real one, each
// placed by a different tier. Only the model's catch-all may be asked
// about again: a catch-all a rule or a pin placed is a considered
// decision — the taxonomy has no word for it and one was chosen on
// purpose — and re-asking would undo deliberate work.
func TestRefineBacklogAsksOnlyWhereTheModelGaveUp(t *testing.T) {
	db, ctx := openCategorizeGold(t)
	const catchAll = "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE"
	seedSpendTxn(t, db, ctx, "T-MODEL", "CARD1", "purchase", 10, -10, "Model Shop", "")
	seedSpendTxn(t, db, ctx, "T-RULE", "CARD1", "purchase", 11, -20, "Rule Shop", "")
	seedSpendTxn(t, db, ctx, "T-PIN", "CARD1", "purchase", 12, -30, "Pin Shop", "")
	seedSpendTxn(t, db, ctx, "T-PLACED", "CARD1", "purchase", 13, -40, "Placed Shop", "")
	// A merchant the MODEL placed on a real value. Same provenance as the
	// one that must be re-asked, so only the catch_all half of the
	// predicate can tell them apart — without it this test passes with
	// that half deleted.
	seedSpendTxn(t, db, ctx, "T-MODEL-OK", "CARD1", "purchase", 14, -50, "Answered Shop", "")
	runEnrichment(t, db, ctx)

	// The model's verdict lives in the merchant store and resolves as
	// provenance `model`; the other two are stamped on the overlay row.
	for _, st := range []struct {
		id, detailed, provenance string
	}{
		{"T-MODEL", "", "signature-only"},
		{"T-RULE", catchAll, "rule"},
		{"T-PIN", catchAll, "manual"},
		{"T-PLACED", "FOOD_AND_DRINK_GROCERIES", "rule"},
		{"T-MODEL-OK", "", "signature-only"},
	} {
		var d any
		if st.detailed != "" {
			d = st.detailed
		}
		if _, err := db.ExecContext(ctx, `
            UPDATE spend_txn_enrichment SET spend_detailed = ?, provenance = ?
             WHERE transaction_external_id = ?`, d, st.provenance, st.id); err != nil {
			t.Fatalf("stage %s: %v", st.id, err)
		}
	}
	sigOf := func(id string) string {
		t.Helper()
		var sig string
		if err := db.QueryRowContext(ctx, `SELECT merchant_signature FROM spend_txn_enrichment
             WHERE transaction_external_id = ?`, id).Scan(&sig); err != nil {
			t.Fatalf("read signature for %s: %v", id, err)
		}
		return sig
	}
	sig, sigOK := sigOf("T-MODEL"), sigOf("T-MODEL-OK")
	for _, v := range []struct{ sig, name, detailed string }{
		{sig, "Model Shop", catchAll},
		{sigOK, "Answered Shop", "FOOD_AND_DRINK_COFFEE"},
	} {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO spend_merchant_categories (merchant_signature, merchant_name,
                  spend_detailed, signature_version, assigned_at, model_name)
            VALUES (?, ?, ?, 1, 1, 'test-model')`, v.sig, v.name, v.detailed); err != nil {
			t.Fatalf("seed store verdict for %s: %v", v.name, err)
		}
	}

	cands, _, err := collectMerchantCandidates(ctx, db, config.SpendContextMerchant, 0, backlogRefine)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := signaturesOf(cands)
	if len(got) != 1 || got[0] != sig {
		t.Errorf("--refine asked about %v, want only the model's catch-all (%q)", got, sig)
	}

	// The flag reaches the backlog it names, and the two widening flags
	// are mutually exclusive.
	if backlogOf(false, true) != backlogRefine || backlogOf(true, false) != backlogAll ||
		backlogOf(false, false) != backlogUnplaced {
		t.Error("backlogOf does not map the flags to the backlogs they name")
	}
}
