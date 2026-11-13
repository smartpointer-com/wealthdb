package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// The golden prompts: every word the two model commands send, pinned to
// the byte. A prompt is tuned text, and a refactoring that moved one
// space would change what a model answers. The spending and
// resolve-symbols goldens each cover two batches, and income one; every
// batch's first attempt draws a rejection, so the anchors, the candidate
// blocks at their widest context level and the feedback block all
// appear. Every name in the fixtures is invented. -update-prompts
// rewrites the files:
//
//	wealthdb/wealthdb-test -run '^TestGolden' ./cmd/wealthdb -args -update-prompts

var updatePrompts = flag.Bool("update-prompts", false, "rewrite testdata/prompts/*.txt")

// promptLog records the conversation a scripted endpoint was sent, one
// system and one user message per call.
type promptLog struct {
	mu    sync.Mutex
	calls [][2]string
}

func (l *promptLog) add(system, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, [2]string{system, user})
}

func (l *promptLog) String() string {
	var b strings.Builder
	for i, c := range l.calls {
		fmt.Fprintf(&b, "=== call %d: system ===\n%s\n=== call %d: user ===\n%s\n", i+1, c[0], i+1, c[1])
	}
	return b.String()
}

// checkGoldenPrompt compares got with testdata/prompts/<name>.txt, or
// rewrites the file under -update-prompts.
func checkGoldenPrompt(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "prompts", name+".txt")
	if *updatePrompts {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (create it with wealthdb/wealthdb-test -run '^TestGolden' ./cmd/wealthdb -args -update-prompts): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("the prompts differ from %s; diff the two, and rewrite it with "+
			"wealthdb/wealthdb-test -run '^TestGolden' ./cmd/wealthdb -args -update-prompts:\n%s", path, got)
	}
}

// TestGoldenCategorizePrompts runs both families of categorize through
// runCategorizeFamilies at the transaction level, so a prompt carries
// narratives, amounts and neighbours. The stand-in answers the second
// candidate of a batch with a non-value on the first attempt and
// everything on the second.
func TestGoldenCategorizePrompts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "gold.db")
	ctx := context.Background()
	db, err := gold.OpenFresh(goldPath)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	// Each row its own second, so the neighbour scan's order has no
	// ties to break.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
            high_watermark, first_loaded_at, last_loaded_at)
        VALUES ('bank', 'chase', '/tmp/b.db', -1, 0, 0);

        INSERT INTO accounts(silver_source_id, account_external_id, account_kind,
            first_seen_at, last_seen_at)
        VALUES ('bank', 'CASH1', 'cash', 1, 1), ('bank', 'CARD1', 'card', 1, 1),
               ('bank', 'BRK1', 'brokerage', 1, 1);

        INSERT INTO transactions(silver_source_id, transaction_external_id, occurred_at,
            account_external_id, kind, currency, net_amount, counterparty) VALUES
            ('bank', 'S1', 864000, 'CARD1', 'purchase',   'USD', -8.40,  'Ashgrove Bakery'),
            ('bank', 'S2', 864060, 'CARD1', 'purchase',   'USD', -23.10, 'Cedar Point Pharmacy'),
            ('bank', 'S3', 950400, 'CARD1', 'purchase',   'USD', -15,    'Dunmore Books'),
            ('bank', 'S4', 950460, 'CASH1', 'withdrawal', 'USD', -200,   'Zelle Payment To Jordan Rivers'),
            ('bank', 'S5', 950520, 'CARD1', 'purchase',   'USD', -61.75, 'Northwind Hardware'),
            ('bank', 'S6', 1036800, 'CARD1', 'purchase',  'USD', -32,    'Elmfield Garage'),
            ('bank', 'I1', 864120, 'CASH1', 'deposit',    'USD', 900,    'Blue Harbour Studio'),
            ('bank', 'I2', 864180, 'CASH1', 'deposit',    'USD', 450,    'Example Letting Agent Ltd'),
            ('bank', 'I3', 864240, 'BRK1',  'dividend',   'USD', 40,     'Example Depot AG'),
            ('bank', 'I4', 950580, 'CASH1', 'deposit',    'USD', 120,    'Greystone Lending Club');

        INSERT INTO spend_merchant_categories(merchant_signature, merchant_name, spend_detailed,
            signature_version, assigned_at, model_name)
        VALUES ('NORTHWIND HARDWARE', 'Northwind Hardware', 'HOME_IMPROVEMENT_HARDWARE', 1, 100, 'test-model');

        INSERT INTO income_payer_categories(payer_signature, payer_name, income_detailed,
            signature_version, assigned_at, model_name)
        VALUES ('ORCHARD LANE TRUST', 'Orchard Lane Trust', 'INCOME_OTHER', 1, 100, 'test-model');
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runEnrichment(t, db, ctx)

	cfgPath := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(cfgPath, []byte(`{
        "gold_db": `+quoteJSON(goldPath)+`,
        "default_currency": "USD",
        "silver_sources": [{"id": "bank", "kind": "chase", "path": "/tmp/b.db"}],
        "spending": {"categorization": {"context": "transaction", "descriptor_samples": 2, "model":
            {"name": "test-model", "baseUrl": "http://127.0.0.1:1/v1", "api": "openai"}}}
    }`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	logs := map[string]*promptLog{}
	values := map[string]string{"spending": "FOOD_AND_DRINK_COFFEE", "income": "INCOME_SALARY"}
	call := func(_ context.Context, system, user string) (string, error) {
		for _, fam := range categorizeFamilies {
			header := candidateHeaderFor(fam)
			if !strings.Contains(user, header) {
				continue
			}
			if logs[fam.name] == nil {
				logs[fam.name] = &promptLog{}
			}
			logs[fam.name].add(system, user)
			retry := strings.Contains(user, "rows that I rejected")
			var b strings.Builder
			for i, sig := range promptBlock(user, header) {
				value := values[fam.name]
				if i == 1 && !retry {
					value = "NOT_A_CATEGORY"
				}
				fmt.Fprintf(&b, "%s,%s,%s\n", sig, strings.ToLower(sig), value)
			}
			return b.String(), nil
		}
		return "", fmt.Errorf("the prompt carries no family's candidate header:\n%s", user)
	}

	var out strings.Builder
	db, dbOpen, err := runCategorizeFamilies(ctx, db, true, cfg, categorizeFamilies,
		categorizeRunOptions{modelFlags: modelFlags{maxAttempts: 2, maxAnchors: 10, batch: 2}, call: call},
		gold.ModeReadWrite, &out, &out)
	if dbOpen {
		db.Close()
	}
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, fam := range categorizeFamilies {
		if logs[fam.name] == nil {
			t.Fatalf("the %s family asked nothing:\n%s", fam.name, out.String())
		}
		checkGoldenPrompt(t, "categorize-"+fam.name, logs[fam.name].String())
	}
}

// TestGoldenResolveSymbolsPrompt asks the model pass of resolve-symbols
// about by-id and by-name candidates, with anchors that carry a
// currency, in two batches. The stand-in answers a batch's second
// candidate under an unknown source on the first attempt.
func TestGoldenResolveSymbolsPrompt(t *testing.T) {
	t.Parallel()
	log := &promptLog{}
	call := func(_ context.Context, system, user string) (string, error) {
		log.add(system, user)
		_, block, _ := strings.Cut(user, "Unresolved rows that need a ticker (same INPUT shape as above):\n")
		block, _, _ = strings.Cut(block, "\n\n")
		rows, err := csv.NewReader(strings.NewReader(block)).ReadAll()
		if err != nil {
			return "", err
		}
		retry := strings.Contains(user, "rows that I rejected")
		var answer strings.Builder
		cw := csv.NewWriter(&answer)
		for i, row := range rows {
			source := row[0]
			if i == 1 && !retry {
				source = "nowhere"
			}
			_ = cw.Write([]string{source, row[1], row[2], "TK" + row[2][len(row[2])-1:]})
		}
		cw.Flush()
		return answer.String(), nil
	}
	anchors := []anchor{
		{SilverSourceID: "schwab-test", InstrumentExternalID: "EX00000101", Name: "EXAMPLE ANCHOR ONE", Symbol: "EXA", Currency: "USD"},
		{SilverSourceID: "schwab-test", InstrumentExternalID: "EX00000102", Name: "EXAMPLE ANCHOR, TWO", Symbol: "EXB", Currency: "EUR"},
	}
	var cands []candidate
	for i := 1; i <= 3; i++ {
		cands = append(cands, candidate{SilverSourceID: "schwab-test", LookupKind: "instrument_external_id",
			LookupValue: fmt.Sprintf("EX0000000%d", i), HintName: fmt.Sprintf("EXAMPLE FUND %d", i)})
	}
	cands = append(cands, candidate{SilverSourceID: "schwab-test", LookupKind: "name",
		LookupValue: "DIVIDEND RECEIVED EXAMPLE COMPANY", Currency: "USD"})
	sources := map[string]bool{"schwab-test": true}

	var out, errOut strings.Builder
	pass := resolvePass(call, anchors, sources, resolveSymbolsOpts{modelFlags: modelFlags{maxAttempts: 2}}, &out, &errOut)
	batches := splitBatches(cands, 2)
	for i, b := range batches {
		if _, _, _, err := pass.ask(context.Background(), b, batchAt{i + 1, len(batches)}); err != nil {
			t.Fatalf("ask: %v", err)
		}
	}
	checkGoldenPrompt(t, "resolve-symbols", log.String())
}
