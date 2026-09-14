package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// candidateHeaderFor is the prompt's candidate block header for a
// family. It was a spending-literal constant until the prompt learned
// this family's nouns; a test that reads back what a prompt carried has
// to ask the same question the prompt answered.
func candidateHeaderFor(fam categorizeFamily) string {
	plural := fam.plural()
	return fmt.Sprintf("%s to categorise — %s_signature,transaction_count:",
		strings.ToUpper(plural[:1])+plural[1:], fam.counterparty)
}

// familyAwareAnswers is a scripted endpoint that answers every
// candidate it is shown with a value of the family that asked.
//
// It decides which family a prompt came from by its own candidate
// header, which is the point: a prompt that announced one family's
// columns and asked for another's would get an answer the gauntlet
// rejects, and the run would end with an empty store rather than a
// wrong one.
type familyAwareAnswers struct {
	// value is the emittable value each family is answered with.
	value map[string]string
	// seen records, per family, the prompts it was sent.
	seen map[string][]string
	// storeCountsAt records the row count of each verdict store at the
	// moment a family's FIRST prompt arrives — the probe that says
	// whether the previous family flushed before this one started.
	storeCountsAt map[string]map[string]int
	goldPath      string
}

// call is ONE endpoint for the whole run, which is what lets the test
// drive both families through a single runCategorizeFamilies call and
// so exercise the loop's own close-and-reopen. It works out which
// family is asking from the prompt's candidate header — a prompt that
// announced neither family's columns is a failure, not a fallback.
func (f *familyAwareAnswers) call(t *testing.T) llmCall {
	return func(_ context.Context, _, user string) (string, error) {
		for _, fam := range categorizeFamilies {
			if !strings.Contains(user, candidateHeaderFor(fam)) {
				continue
			}
			if len(f.seen[fam.name]) == 0 {
				f.storeCountsAt[fam.name] = f.storeRowCounts(t)
			}
			f.seen[fam.name] = append(f.seen[fam.name], user)
			var b strings.Builder
			for _, sig := range promptBlock(user, candidateHeaderFor(fam)) {
				fmt.Fprintf(&b, "%s,Example %s,%s\n", sig, fam.counterparty, f.value[fam.name])
			}
			return b.String(), nil
		}
		return "", fmt.Errorf("the prompt carries no family's candidate header:\n%s", user)
	}
}

// storeRowCounts reads both verdict stores through a handle of its own,
// opened at the moment a prompt is built and closed again immediately.
//
// It has to be a fresh handle, and it has to succeed: DuckDB refuses a
// second connection to a file under a different mode, so an open here
// succeeding is itself the proof that the run released gold before the
// model round-trip — which is the whole reason the handle is closed and
// reopened between families. An open that FAILS means the run is
// holding the file, and the counts below would be a lie, so it fails
// the test rather than returning zeroes.
func (f *familyAwareAnswers) storeRowCounts(t *testing.T) map[string]int {
	t.Helper()
	db, err := gold.Open(f.goldPath, gold.ModeReadOnly)
	if err != nil {
		t.Fatalf("a reader could not open gold during the model pass, so the handle was not released: %v", err)
	}
	defer db.Close()
	out := map[string]int{}
	for _, tbl := range []string{"spend_merchant_categories", "income_payer_categories"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}

// seedTwoFamilyGold builds a gold FILE — not :memory: — with exactly
// one unplaced candidate per family, neither of them fenced: a shop on
// a card for spending, an employer on a bank account for income. A file
// is what makes the run's close-and-reopen real, since an in-memory
// database honours no access mode and so cannot be closed against a
// reader.
func seedTwoFamilyGold(t *testing.T, ctx context.Context, goldPath string) *sql.DB {
	t.Helper()
	db, err := gold.Open(goldPath, gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	if err := gold.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
            high_watermark, first_loaded_at, last_loaded_at)
        VALUES ('bank', 'chase', '/tmp/b.db', -1, 0, 0);

        INSERT INTO accounts(silver_source_id, account_external_id, account_kind,
            first_seen_at, last_seen_at)
        VALUES ('bank', 'CASH1', 'cash', 1, 1), ('bank', 'CARD1', 'card', 1, 1);

        INSERT INTO transactions(silver_source_id, transaction_external_id, occurred_at,
            account_external_id, kind, currency, net_amount, counterparty) VALUES
            ('bank', 'T-SHOP', 1000, 'CARD1', 'purchase', 'USD', -25, 'Northwind Hardware'),
            ('bank', 'T-PAY',  1000, 'CASH1', 'deposit',  'USD', 900, 'Blue Harbour Payroll');

        INSERT INTO spend_txn_enrichment(silver_source_id, transaction_external_id,
            merchant_signature, signature_version, spend_detailed, provenance, assigned_at)
        VALUES ('bank', 'T-SHOP', 'NORTHWIND HARDWARE', 1, NULL, 'signature-only', 1);

        INSERT INTO income_txn_enrichment(silver_source_id, transaction_external_id,
            payer_signature, signature_version, income_detailed, provenance, assigned_at)
        VALUES ('bank', 'T-PAY', 'BLUE HARBOUR PAYROLL', 1, NULL, 'signature-only', 1);
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

// writeTwoFamilyConfig writes a config whose spending block carries a
// model, so both families resolve one — income inherits it — and whose
// endpoint is unreachable on purpose: every test here injects its own
// call, and a config pointing at something real would make a bug look
// like a passing test.
func writeTwoFamilyConfig(t *testing.T, dir, goldPath string) *config.Config {
	t.Helper()
	cfgPath := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(cfgPath, []byte(`{
        "gold_db": `+quoteJSON(goldPath)+`,
        "default_currency": "USD",
        "silver_sources": [{"id": "bank", "kind": "chase", "path": "/tmp/b.db"}],
        "spending": {"categorization": {"model":
            {"name": "test-model", "baseUrl": "http://127.0.0.1:1/v1", "api": "openai"}}}
    }`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// TestCategorizeRunsBothFamiliesEndToEnd drives the whole two-family
// loop — the part of `categorize` with the sequencing in it, and the
// part PLAN §7 asked to be pinned.
//
// Three things it holds, none of which any other test reaches:
//
//  1. BOTH families are run, and each family's verdicts land in its own
//     store. A loop over families[:1] passes every other test here.
//  2. The first family's verdicts are FLUSHED BEFORE the second family
//     starts. A run of two families must not leave the first one's last
//     batch unflushed while the second spends another model pass — the
//     last-batch lock hazard. The probe is the store row count read
//     from outside at the instant the second family's first prompt is
//     built.
//  3. The gold handle is RE-OPENED between families. The model pass
//     closes it so readers can use gold, and gold is a real file here
//     rather than :memory: precisely so that close is real.
func TestCategorizeRunsBothFamiliesEndToEnd(t *testing.T) {
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "gold.db")
	ctx := context.Background()

	db := seedTwoFamilyGold(t, ctx, goldPath)
	cfg := writeTwoFamilyConfig(t, dir, goldPath)

	answers := &familyAwareAnswers{
		value: map[string]string{
			"spending": "HOME_IMPROVEMENT_HARDWARE",
			"income":   "INCOME_WAGES",
		},
		seen:          map[string][]string{},
		storeCountsAt: map[string]map[string]int{},
		goldPath:      goldPath,
	}

	var out strings.Builder
	opts := categorizeRunOptions{
		maxAttempts: 2, maxAnchors: 10, batch: 40, call: answers.call(t),
	}
	db, dbOpen, err := runCategorizeFamilies(ctx, db, true, cfg,
		categorizeFamilies, opts, gold.ModeReadWrite, &out, &out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !dbOpen {
		if db, err = gold.Open(goldPath, gold.ModeReadWrite); err != nil {
			t.Fatalf("re-open after the run: %v", err)
		}
	}
	defer db.Close()

	// (1) Both families ran, and each verdict landed in its own store.
	for _, fam := range categorizeFamilies {
		if len(answers.seen[fam.name]) == 0 {
			t.Fatalf("the %s family was never asked anything", fam.name)
		}
	}
	for _, tc := range []struct{ table, sig, want string }{
		{"spend_merchant_categories", "NORTHWIND HARDWARE", "HOME_IMPROVEMENT_HARDWARE"},
		{"income_payer_categories", "BLUE HARBOUR PAYROLL", "INCOME_WAGES"},
	} {
		col := "spend_detailed"
		key := "merchant_signature"
		if tc.table == "income_payer_categories" {
			col, key = "income_detailed", "payer_signature"
		}
		var got string
		if err := db.QueryRowContext(ctx,
			"SELECT "+col+" FROM "+tc.table+" WHERE "+key+" = ?", tc.sig).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.table, err)
		}
		if got != tc.want {
			t.Errorf("%s[%s] = %q, want %q", tc.table, tc.sig, got, tc.want)
		}
	}

	// (2) Spending's verdict was already committed when income's first
	// prompt was built. Read from OUTSIDE the run, so it is the flush
	// that is under test rather than the writer's own view of it.
	atIncome := answers.storeCountsAt["income"]
	if atIncome["spend_merchant_categories"] != 1 {
		t.Errorf("spend_merchant_categories held %d row(s) when income started, want 1: "+
			"the first family's last batch must be flushed before the second spends a model pass",
			atIncome["spend_merchant_categories"])
	}
	// ...and income's own store was still empty then, which is what
	// makes the count above a statement about ORDER rather than about
	// the end state.
	if atIncome["income_payer_categories"] != 0 {
		t.Errorf("income_payer_categories held %d row(s) before the income pass began",
			atIncome["income_payer_categories"])
	}

	// (3) Each family's prompt carried its OWN nouns. A prompt read
	// back through the other family's header returns nothing, which is
	// how this test would notice the output contract drifting back to
	// one family's columns.
	for _, fam := range categorizeFamilies {
		p := answers.seen[fam.name][0]
		if len(promptBlock(p, candidateHeaderFor(fam))) == 0 {
			t.Errorf("the %s prompt carries no candidate block under its own header", fam.name)
		}
		for _, other := range categorizeFamilies {
			if other.name == fam.name {
				continue
			}
			if strings.Contains(p, candidateHeaderFor(other)) {
				t.Errorf("the %s prompt carries the %s family's candidate header", fam.name, other.name)
			}
		}
	}
	if strings.Contains(answers.seen["income"][0], "merchant") {
		t.Errorf("the income prompt says \"merchant\" somewhere:\n%s", answers.seen["income"][0])
	}

	// And the run report named both families.
	for _, want := range []string{"categorize: spending:", "categorize: income:",
		"merchants asked:", "payers asked:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the run report is missing %q:\n%s", want, out.String())
		}
	}
}

// quoteJSON renders s as a JSON string literal, for a config a test
// writes with a temp-dir path in it.
func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}

// TestPersonFenceReachesCandidacyFromConfig pins the WIRE between the
// config field and the fence, which nothing else does.
//
// The predicate has a table test, and candidacy has a test that passes
// the flag directly — but the step between them, runCategorizeFamily
// reading cz.PersonNameFence() and handing it to the collector, is
// where a hardcoded true or false would live, and both of those pass
// every other test in this package.
//
// Driven through the whole loop with a real config file, because the
// config is the input under test.
func TestPersonFenceReachesCandidacyFromConfig(t *testing.T) {
	// A bare person's name on a bank account: fenced when the option is
	// on, a candidate when it is off, and nothing else about it changes.
	const bare = "EXAMPLE SAMPLE"

	for _, tc := range []struct {
		name       string
		fenceJSON  string
		wantAsked  bool
		wantReport string
	}{
		{"default (absent)", "", false, ""},
		{"explicit true", `, "fence_person_names": true`, false, ""},
		{"explicit false", `, "fence_person_names": false`, true,
			"person-shape fence off (spending.categorization.fence_person_names)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			goldPath := filepath.Join(dir, "gold.db")
			ctx := context.Background()

			db := seedTwoFamilyGold(t, ctx, goldPath)
			if _, err := db.ExecContext(ctx, `
                INSERT INTO transactions(silver_source_id, transaction_external_id, occurred_at,
                    account_external_id, kind, currency, net_amount, counterparty)
                VALUES ('bank', 'T-BARE', 1000, 'CASH1', 'deposit', 'USD', 300, 'Example Sample');
                INSERT INTO income_txn_enrichment(silver_source_id, transaction_external_id,
                    payer_signature, signature_version, income_detailed, provenance, assigned_at)
                VALUES ('bank', 'T-BARE', ?, 1, NULL, 'signature-only', 1);`, bare); err != nil {
				t.Fatalf("seed the bare name: %v", err)
			}

			cfgPath := filepath.Join(dir, "wealthdb.cfg")
			if err := os.WriteFile(cfgPath, []byte(`{
                "gold_db": `+quoteJSON(goldPath)+`,
                "default_currency": "USD",
                "silver_sources": [{"id": "bank", "kind": "chase", "path": "/tmp/b.db"}],
                "spending": {"categorization": {"model":
                    {"name": "test-model", "baseUrl": "http://127.0.0.1:1/v1", "api": "openai"}`+
				tc.fenceJSON+`}}
            }`), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}

			asked := map[string]bool{}
			call := func(_ context.Context, _, user string) (string, error) {
				for _, sig := range promptBlock(user, candidateHeaderFor(incomeCategorizeFamily)) {
					asked[sig] = true
				}
				return "", nil
			}
			var out strings.Builder
			db, dbOpen, err := runCategorizeFamilies(ctx, db, true, cfg,
				[]categorizeFamily{incomeCategorizeFamily},
				categorizeRunOptions{maxAttempts: 1, maxAnchors: 10, batch: 40, call: call},
				gold.ModeReadWrite, &out, &out)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out.String())
			}
			if !dbOpen {
				if db, err = gold.Open(goldPath, gold.ModeReadWrite); err != nil {
					t.Fatalf("re-open: %v", err)
				}
			}
			db.Close()

			// The employer is asked about either way, so a run that
			// asked about nothing at all cannot pass by accident.
			if !asked["BLUE HARBOUR PAYROLL"] {
				t.Fatalf("the run asked about nothing:\n%s", out.String())
			}
			if asked[bare] != tc.wantAsked {
				t.Errorf("%q asked = %v, want %v — the config did not reach the fence\n%s",
					bare, asked[bare], tc.wantAsked, out.String())
			}
			if tc.wantReport != "" && !strings.Contains(out.String(), tc.wantReport) {
				t.Errorf("the run report does not say the arm is off:\n%s", out.String())
			}
			if tc.wantReport == "" && strings.Contains(out.String(), "fence off") {
				t.Errorf("the run report says the arm is off while it is on:\n%s", out.String())
			}
		})
	}
}

// TestCategorizeFlushesOneFamilyBeforeTheNextSpends is the last-batch
// lock hazard PLAN §7 names, and the one property the loop test above
// cannot reach.
//
// The batch sink stores each batch as it completes, so in the ordinary
// case there is nothing left over and the family's closing retryFlush
// is a no-op — which is exactly why moving that flush out of the family
// and after the loop passes every other test. It only bites when a
// batch's persist LOSES the re-open race: the verdicts are then held in
// the store, unpaid-for work in memory, and a two-family run must land
// them before the second family spends another model pass rather than
// carrying them across it.
//
// So the race is staged. A writer's handle is held over the instant
// spending's batch persists and released again shortly after; the
// sink's re-open fails, the family's retryFlush retries into the window
// where it succeeds, and income's first prompt then finds the verdict
// already committed. With the flush moved after the loop it finds an
// empty store.
func TestCategorizeFlushesOneFamilyBeforeTheNextSpends(t *testing.T) {
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "gold.db")
	ctx := context.Background()

	db := seedTwoFamilyGold(t, ctx, goldPath)
	cfg := writeTwoFamilyConfig(t, dir, goldPath)

	var (
		mu            sync.Mutex
		spendAtIncome int
		blocker       *sql.DB
	)
	// A READ-ONLY handle, and the mode is the whole mechanism: DuckDB
	// refuses a second connection only under a DIFFERENT configuration,
	// so two writers coexist and it is a READER that makes a re-open
	// for writing fail. That is also the real hazard — the handle is
	// released for the model pass precisely so readers may use gold,
	// and a reader present when a batch persists is the race.
	//
	// Held from the moment spending's prompt is answered — so it is
	// open when the batch's persist runs — and released 1.2s later,
	// inside retryFlush's 0s / 1s / 3s backoff but after its first two
	// attempts.
	release := func() {
		mu.Lock()
		defer mu.Unlock()
		if blocker != nil {
			_ = blocker.Close()
			blocker = nil
		}
	}
	defer release()

	call := func(_ context.Context, _, user string) (string, error) {
		if strings.Contains(user, candidateHeaderFor(spendingCategorizeFamily)) {
			held, err := gold.Open(goldPath, gold.ModeReadOnly)
			if err != nil {
				return "", fmt.Errorf("stage the race: %w", err)
			}
			mu.Lock()
			blocker = held
			mu.Unlock()
			time.AfterFunc(1200*time.Millisecond, release)
			var b strings.Builder
			for _, sig := range promptBlock(user, candidateHeaderFor(spendingCategorizeFamily)) {
				fmt.Fprintf(&b, "%s,Example Merchant,HOME_IMPROVEMENT_HARDWARE\n", sig)
			}
			return b.String(), nil
		}
		if strings.Contains(user, candidateHeaderFor(incomeCategorizeFamily)) {
			spendAtIncome = countRows(t, goldPath, "spend_merchant_categories")
			var b strings.Builder
			for _, sig := range promptBlock(user, candidateHeaderFor(incomeCategorizeFamily)) {
				fmt.Fprintf(&b, "%s,Example Payer,INCOME_WAGES\n", sig)
			}
			return b.String(), nil
		}
		return "", fmt.Errorf("the prompt carries no family's candidate header")
	}

	var out strings.Builder
	db, dbOpen, err := runCategorizeFamilies(ctx, db, true, cfg, categorizeFamilies,
		categorizeRunOptions{maxAttempts: 2, maxAnchors: 10, batch: 40, call: call},
		gold.ModeReadWrite, &out, &out)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !dbOpen {
		if db, err = gold.Open(goldPath, gold.ModeReadWrite); err != nil {
			t.Fatalf("re-open: %v", err)
		}
	}
	defer db.Close()

	// The staging has to have bitten, or the assertion below is about
	// nothing: with no failed persist there is nothing for the flush to
	// carry, and the test would pass however the flush is ordered. The
	// warning the store prints when a batch cannot be stored is the
	// proof that it did.
	if !strings.Contains(out.String(), "held after a failed store") {
		t.Fatalf("no batch lost the re-open race, so this test proves nothing about the flush:\n%s",
			out.String())
	}
	if spendAtIncome != 1 {
		t.Errorf("spend_merchant_categories held %d row(s) when the income pass began, want 1: "+
			"a family's held-back verdicts must land before the next family spends a model pass\n%s",
			spendAtIncome, out.String())
	}
	// ...and both stores are complete at the end either way, so the
	// assertion above is about ORDER and not about loss.
	for tbl, want := range map[string]int{
		"spend_merchant_categories": 1, "income_payer_categories": 1,
	} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != want {
			t.Errorf("%s = %d row(s) at the end, want %d", tbl, n, want)
		}
	}
}

// countRows opens gold read-only and counts one table. It is only
// called from inside a scripted endpoint, where the run has released
// the file for the model round-trip.
func countRows(t *testing.T, path, table string) int {
	t.Helper()
	db, err := gold.Open(path, gold.ModeReadOnly)
	if err != nil {
		t.Fatalf("open %s read-only: %v", path, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
