package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func TestStripThinkingBlocks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no thinking",
			in:   "schwab,name,FOO BAR,FB",
			want: "schwab,name,FOO BAR,FB",
		},
		{
			name: "single block stripped",
			in:   "<think>let me consider this</think>schwab,name,FOO BAR,FB",
			want: "schwab,name,FOO BAR,FB",
		},
		{
			name: "multiline block",
			in:   "<think>\nstep 1: identify\nstep 2: lookup\n</think>\nschwab,name,FOO,FB",
			want: "schwab,name,FOO,FB",
		},
		{
			name: "multiple blocks",
			in:   "<think>a</think>schwab,1\n<think>b</think>schwab,2",
			want: "schwab,1\nschwab,2",
		},
		{
			name: "no closing tag — left alone",
			in:   "<think>oops" + "\nschwab,name,FOO,FB",
			want: "<think>oops\nschwab,name,FOO,FB",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripThinkingBlocks(c.in)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestStripCodeFences(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"schwab,name,foo,FB", "schwab,name,foo,FB"},
		{"```csv\nschwab,name,foo,FB\n```", "schwab,name,foo,FB"},
		{"```\nschwab,name,foo,FB\n```", "schwab,name,foo,FB"},
		{"  ```csv\nrow1\nrow2\n```  ", "row1\nrow2"},
	}
	for _, c := range cases {
		got := stripCodeFences(c.in)
		if got != c.want {
			t.Errorf("stripCodeFences(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTickerShapeRe(t *testing.T) {
	t.Parallel()
	goodTickers := []string{"AAPL", "BRK.B", "XDEW", "TFLO", "005930", "9988", "BRK-B", "DBK.DE"}
	for _, s := range goodTickers {
		if !tickerShapeRe.MatchString(s) {
			t.Errorf("%q should be ticker-shaped", s)
		}
	}
	badTickers := []string{
		"",                   // empty
		"aapl",               // lowercase
		"BANK INT 07/30",     // spaces + slash
		"VERY-LONG-TICKER-X", // too long
		"$$$",                // junk
	}
	for _, s := range badTickers {
		if tickerShapeRe.MatchString(s) {
			t.Errorf("%q should NOT be ticker-shaped", s)
		}
	}
}

func TestIsinShapeRe(t *testing.T) {
	t.Parallel()
	isins := []string{"US0000000030", "IE0000000040", "CH0000000020", "DE0000000050"}
	for _, s := range isins {
		if !isinShapeRe.MatchString(s) {
			t.Errorf("%q should match ISIN shape", s)
		}
	}
	// "ABCD12345678" matches the surface shape even though it's
	// not a real ISIN (no real country code AB, no check digit
	// validity). The point of the regex is to catch the model
	// emitting an alphanumeric-blob-of-the-right-shape — actual
	// ISIN validity is irrelevant.
	nonIsins := []string{"AAPL", "BRK.B", "005930", "12USA1234567", "lowercase123"}
	for _, s := range nonIsins {
		if isinShapeRe.MatchString(s) {
			t.Errorf("%q should NOT match ISIN shape", s)
		}
	}
}

func TestParseAndValidate(t *testing.T) {
	t.Parallel()
	sources := map[string]bool{"schwab": true, "ubs": true}
	candKeys := map[string]bool{
		candKey("schwab", "name", "ISHARES TREASURY FLOATNGRATE BD ETF"): true,
		candKey("schwab", "name", "INVSC S P 500 EQUAL WEIGHT ETF"):      true,
		candKey("ubs", "instrument_external_id", "IE0000000040"):         true,
		candKey("ubs", "instrument_external_id", "US0000000030"):         true,
	}

	t.Run("happy path", func(t *testing.T) {
		body := `schwab,name,ISHARES TREASURY FLOATNGRATE BD ETF,TFLO
ubs,instrument_external_id,IE0000000040,XDEW`
		valid, invalid := parseAndValidate(body, candKeys, sources)
		if len(valid) != 2 {
			t.Errorf("want 2 valid, got %d", len(valid))
		}
		if len(invalid) != 0 {
			t.Errorf("want 0 invalid, got %d (%v)", len(invalid), invalid)
		}
		if valid[0].Symbol != "TFLO" || valid[1].Symbol != "XDEW" {
			t.Errorf("unexpected symbols: %+v", valid)
		}
	})

	t.Run("six-column reply (model echoed input)", func(t *testing.T) {
		// Model emitted candidate row + symbol; parser should
		// pick first 3 + last.
		body := `ubs,instrument_external_id,IE0000000040,"Example Equal-Weight UCITS ETF",USD,XDEW`
		valid, invalid := parseAndValidate(body, candKeys, sources)
		if len(valid) != 1 {
			t.Fatalf("want 1 valid, got %d; invalid=%v", len(valid), invalid)
		}
		if valid[0].Symbol != "XDEW" {
			t.Errorf("symbol = %q, want XDEW", valid[0].Symbol)
		}
	})

	t.Run("rejects unknown source", func(t *testing.T) {
		body := `binance,name,ISHARES TREASURY FLOATNGRATE BD ETF,TFLO`
		valid, invalid := parseAndValidate(body, candKeys, sources)
		if len(valid) != 0 || len(invalid) != 1 {
			t.Errorf("want 0 valid + 1 invalid, got %d + %d", len(valid), len(invalid))
		}
	})

	t.Run("rejects unknown lookup_kind", func(t *testing.T) {
		body := `schwab,cusip,037833100,AAPL`
		_, invalid := parseAndValidate(body, candKeys, sources)
		if len(invalid) != 1 {
			t.Errorf("want 1 invalid, got %d", len(invalid))
		}
	})

	t.Run("rejects hallucinated lookup_value", func(t *testing.T) {
		body := `schwab,name,VANGUARD I MADE THIS UP,VTI`
		_, invalid := parseAndValidate(body, candKeys, sources)
		if len(invalid) != 1 {
			t.Errorf("want 1 invalid (hallucinated lookup_value), got %d", len(invalid))
		}
	})

	t.Run("rejects bad ticker shape", func(t *testing.T) {
		body := `schwab,name,ISHARES TREASURY FLOATNGRATE BD ETF,this is not a ticker`
		_, invalid := parseAndValidate(body, candKeys, sources)
		if len(invalid) != 1 {
			t.Errorf("want 1 invalid (bad ticker shape), got %d", len(invalid))
		}
	})

	t.Run("rejects symbol equals lookup_value (ISIN echo)", func(t *testing.T) {
		body := `ubs,instrument_external_id,US0000000030,US0000000030`
		_, invalid := parseAndValidate(body, candKeys, sources)
		if len(invalid) != 1 {
			t.Errorf("want 1 invalid (echo), got %d", len(invalid))
		}
		if !strings.Contains(invalid[0].Reason, "echoed input") {
			t.Errorf("reason = %q, want 'echoed input' mention", invalid[0].Reason)
		}
	})

	t.Run("rejects ISIN-shaped symbol", func(t *testing.T) {
		// Model emits a different ISIN as the symbol — not a real ticker.
		body := `ubs,instrument_external_id,IE0000000040,CH0000000020`
		_, invalid := parseAndValidate(body, candKeys, sources)
		if len(invalid) != 1 {
			t.Errorf("want 1 invalid (ISIN-shaped symbol), got %d", len(invalid))
		}
	})

	t.Run("rejects CUSIP-shaped symbol", func(t *testing.T) {
		// Model copies a CUSIP out of the narrative instead of naming a ticker.
		body := `ubs,instrument_external_id,IE0000000040,00000A000`
		_, invalid := parseAndValidate(body, candKeys, sources)
		if len(invalid) != 1 || !strings.Contains(invalid[0].Reason, "CUSIP-shaped") {
			t.Errorf("want 1 invalid (CUSIP-shaped symbol), got %v", invalid)
		}
	})

	t.Run("tolerates code fences", func(t *testing.T) {
		body := "```csv\nschwab,name,ISHARES TREASURY FLOATNGRATE BD ETF,TFLO\n```"
		valid, invalid := parseAndValidate(body, candKeys, sources)
		if len(valid) != 1 || len(invalid) != 0 {
			t.Errorf("code-fenced body should parse cleanly: valid=%d invalid=%d", len(valid), len(invalid))
		}
	})

	t.Run("tolerates ragged rows (parser doesn't abort)", func(t *testing.T) {
		body := `schwab,name,ISHARES TREASURY FLOATNGRATE BD ETF,TFLO
this is not csv at all
ubs,instrument_external_id,IE0000000040,XDEW`
		valid, _ := parseAndValidate(body, candKeys, sources)
		// First and third rows valid, middle row rejected.
		if len(valid) != 2 {
			t.Errorf("want 2 valid, got %d", len(valid))
		}
	})
}

func TestStratifiedSample(t *testing.T) {
	t.Parallel()
	items := []candidate{
		{SilverSourceID: "schwab", LookupKind: "name", LookupValue: "S1"},
		{SilverSourceID: "schwab", LookupKind: "name", LookupValue: "S2"},
		{SilverSourceID: "schwab", LookupKind: "name", LookupValue: "S3"},
		{SilverSourceID: "schwab", LookupKind: "name", LookupValue: "S4"},
		{SilverSourceID: "ubs", LookupKind: "instrument_external_id", LookupValue: "U1"},
		{SilverSourceID: "ubs", LookupKind: "instrument_external_id", LookupValue: "U2"},
	}
	out := stratifiedSample(items, 4, candidateGroup)
	if len(out) != 4 {
		t.Fatalf("got %d, want 4", len(out))
	}
	// Should include rows from both (schwab,name) and (ubs,instrument_external_id)
	// groups, not all from the same group.
	groups := map[string]bool{}
	for _, c := range out {
		groups[candidateGroup(c)] = true
	}
	if len(groups) < 2 {
		t.Errorf("stratifiedSample should span groups; got only %v", groups)
	}
}

func TestStratifiedSampleSmallerThanMax(t *testing.T) {
	t.Parallel()
	items := []candidate{
		{SilverSourceID: "schwab", LookupKind: "name", LookupValue: "S1"},
		{SilverSourceID: "ubs", LookupKind: "name", LookupValue: "U1"},
	}
	out := stratifiedSample(items, 10, candidateGroup)
	if len(out) != 2 {
		t.Errorf("got %d, want 2 (full input returned)", len(out))
	}
}

func TestUnresolvedCandidates(t *testing.T) {
	t.Parallel()
	cands := []candidate{
		{SilverSourceID: "schwab", LookupKind: "name", LookupValue: "FOO"},
		{SilverSourceID: "schwab", LookupKind: "name", LookupValue: "BAR"},
		{SilverSourceID: "ubs", LookupKind: "instrument_external_id", LookupValue: "ISIN1"},
	}
	valid := []resolution{
		{SilverSourceID: "schwab", LookupKind: "name", LookupValue: "FOO", Symbol: "F"},
	}
	out := unresolvedCandidates(cands, valid)
	if len(out) != 2 {
		t.Errorf("want 2 unresolved, got %d", len(out))
	}
	for _, c := range out {
		if c.LookupValue == "FOO" {
			t.Errorf("FOO was resolved, should not be in unresolved set")
		}
	}
}

// TestStoreResolutionsSurvivesAReaderHoldingGold pins what a command
// that has already paid a model owes its answers.
//
// The handle is released across the model pass so readers get in, and
// DuckDB will not attach a file read-write while any other handle is
// open on it — so a single concurrent read command can make the
// re-open that stores the answers fail. Either outcome is acceptable;
// losing the answers silently is not. The resolutions must end up in
// gold, or, when the reader outlasts the whole backoff, on stdout in
// the plan format that can be re-applied.
func TestStoreResolutionsSurvivesAReaderHoldingGold(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	goldPath := goldPathFromCfg(cfg)

	reader, err := gold.Open(goldPath, gold.ModeReadOnly)
	if err != nil {
		t.Fatalf("open gold read-only: %v", err)
	}

	rows := []resolution{{
		SilverSourceID: "schwab-test",
		LookupKind:     "name",
		LookupValue:    "EXAMPLE GLOBAL FUND",
		Symbol:         "EXGF",
	}}
	var out bytes.Buffer
	_, _, storeErr := storeResolutions(context.Background(), goldPath, rows, "test-model", &out)
	reader.Close()

	if storeErr != nil {
		if !strings.Contains(out.String(), "EXAMPLE GLOBAL FUND → EXGF") {
			t.Errorf("the persist failed and the paid-for resolutions were not printed either:\n%s", out.String())
		}
		return
	}

	// The re-open won the race, so the rows are in gold.
	db, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("re-open gold read-only: %v", err)
	}
	defer db.Close()
	var symbol string
	if err := db.QueryRow(`SELECT symbol FROM symbol_resolutions
                            WHERE lookup_value = 'EXAMPLE GLOBAL FUND'`).Scan(&symbol); err != nil {
		t.Fatalf("read back the stored resolution: %v", err)
	}
	if symbol != "EXGF" {
		t.Errorf("stored symbol = %q, want EXGF", symbol)
	}
}

func TestAnchorsForKeepsTheBatchSources(t *testing.T) {
	t.Parallel()
	anchors := []anchor{
		{SilverSourceID: "a", InstrumentExternalID: "1"},
		{SilverSourceID: "b", InstrumentExternalID: "2"},
		{SilverSourceID: "a", InstrumentExternalID: "3"},
	}
	got := anchorsFor(anchors, []candidate{{SilverSourceID: "a"}, {SilverSourceID: "a"}})
	if len(got) != 2 || got[0].InstrumentExternalID != "1" || got[1].InstrumentExternalID != "3" {
		t.Errorf("anchors for a batch of source a = %+v, want a's two in order", got)
	}
	if got := anchorsFor(anchors, []candidate{{SilverSourceID: "c"}}); len(got) != 0 {
		t.Errorf("anchors for a source with none = %+v, want none", got)
	}
}

// standInModel serves chat completions that answer every candidate the
// prompt carries with a ticker made of its lookup value's last character,
// recording the candidate rows of each call it receives.
func standInModel(t *testing.T) (*httptest.Server, *[][][]string) {
	t.Helper()
	var calls [][][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, block, _ := strings.Cut(req.Messages[len(req.Messages)-1].Content,
			"Unresolved rows that need a ticker (same INPUT shape as above):\n")
		block, _, _ = strings.Cut(block, "\n\n")
		rows, err := csv.NewReader(strings.NewReader(block)).ReadAll()
		if err != nil {
			t.Errorf("read prompt candidates: %v", err)
		}
		calls = append(calls, rows)
		var answer strings.Builder
		for _, row := range rows {
			fmt.Fprintf(&answer, "%s,%s,%s,TK%s\n", row[0], row[1], row[2], row[2][len(row[2])-1:])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": answer.String()}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// resolveSymbolsFixture initialises a gold holding n symbol-less
// instruments of the test source, EX00000001 onwards, with the config
// pointed at the stand-in model and carrying the given overrides.
func resolveSymbolsFixture(t *testing.T, modelURL string, n int, overrides []any) (cfgPath, goldPath string) {
	t.Helper()
	cfgPath = setupCLITest(t)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["symbol_resolution"] = map[string]any{
		"model":     map[string]any{"baseUrl": modelURL + "/v1", "api": "openai-completions", "name": "stand-in"},
		"overrides": overrides,
	}
	raw, _ = json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := run(t, "-c", cfgPath, "init"); code != 0 {
		t.Fatal("init failed")
	}
	goldPath = goldPathFromCfg(cfgPath)
	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 1; i <= n; i++ {
		if _, err := db.Exec(`INSERT INTO instruments (silver_source_id, instrument_external_id,
                asset_class, name, first_seen_at, last_seen_at)
            VALUES ('schwab-test', ?, 'public_equity', ?, 0, 0)`,
			fmt.Sprintf("EX0000000%d", i), fmt.Sprintf("EXAMPLE FUND %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	return cfgPath, goldPath
}

// storedSymbols reads symbol_resolutions back as lookup value → symbol
// and model name.
func storedSymbols(t *testing.T, goldPath string) map[string][2]string {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT lookup_value, symbol, model_name FROM symbol_resolutions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][2]string{}
	for rows.Next() {
		var value, symbol, model string
		if err := rows.Scan(&value, &symbol, &model); err != nil {
			t.Fatal(err)
		}
		out[value] = [2]string{symbol, model}
	}
	return out
}

// TestResolveSymbolsAsksOneBatchPerCallAndStoresEach: five candidates at
// --batch 2 are three calls, none asked about another batch's rows, and
// every answer lands in gold.
func TestResolveSymbolsAsksOneBatchPerCallAndStoresEach(t *testing.T) {
	t.Parallel()
	srv, calls := standInModel(t)
	cfgPath, goldPath := resolveSymbolsFixture(t, srv.URL, 5, nil)

	out, errOut, code := run(t, "-c", cfgPath, "resolve-symbols", "--batch", "2")
	if code != 0 {
		t.Fatalf("resolve-symbols exit %d:\n%s\n%s", code, out, errOut)
	}
	if len(*calls) != 3 {
		t.Errorf("model calls = %d, want 3 (5 candidates in batches of 2)", len(*calls))
	}
	for i, rows := range *calls {
		if len(rows) > 2 {
			t.Errorf("call %d carried %d candidates, want at most one batch of 2", i+1, len(rows))
		}
	}
	if got := storedSymbols(t, goldPath); len(got) != 5 {
		t.Errorf("stored resolutions = %v, want all 5\n%s", got, out)
	}
}

// TestResolveSymbolsLeavesConfigDecidedKeysAlone: a key the config pins
// and a key it suppresses are never put to the model, so the model's
// answers, stored after the overrides are synced, cannot displace them.
func TestResolveSymbolsLeavesConfigDecidedKeysAlone(t *testing.T) {
	t.Parallel()
	srv, calls := standInModel(t)
	cfgPath, goldPath := resolveSymbolsFixture(t, srv.URL, 3, []any{
		map[string]any{"silver_source_id": "schwab-test", "lookup_kind": "instrument_external_id",
			"lookup_value": "EX00000001", "symbol": "PINNED"},
		map[string]any{"silver_source_id": "schwab-test", "lookup_kind": "instrument_external_id",
			"lookup_value": "EX00000002", "delete": true},
	})

	out, errOut, code := run(t, "-c", cfgPath, "resolve-symbols")
	if code != 0 {
		t.Fatalf("resolve-symbols exit %d:\n%s\n%s", code, out, errOut)
	}
	var asked []string
	for _, rows := range *calls {
		for _, row := range rows {
			asked = append(asked, row[2])
		}
	}
	if len(asked) != 1 || asked[0] != "EX00000003" {
		t.Errorf("the model was asked about %v, want only the undecided EX00000003", asked)
	}
	got := storedSymbols(t, goldPath)
	if got["EX00000001"] != [2]string{"PINNED", manualOverrideModelName} {
		t.Errorf("pinned key = %v, want the config's PINNED", got["EX00000001"])
	}
	if _, ok := got["EX00000002"]; ok {
		t.Errorf("suppressed key came back as %v", got["EX00000002"])
	}
	if got["EX00000003"][0] != "TK3" {
		t.Errorf("undecided key = %v, want the model's TK3", got["EX00000003"])
	}
}

// TestCollectAnchorsSkipsIdentifierSymbols: an instrument whose symbol
// is a CUSIP or an ISIN is not an example of a ticker, and showing it to
// the model as one teaches it to answer with identifiers. A ticker that
// is also the instrument's key is a fine example.
func TestCollectAnchorsSkipsIdentifierSymbols(t *testing.T) {
	t.Parallel()
	_, goldPath := resolveSymbolsFixture(t, "http://unused", 0, nil)
	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, row := range [][2]string{{"AAAA", "AAAA"}, {"00000A000", "00000A000"}, {"XX0000000009", "XX0000000009"}} {
		if _, err := db.Exec(`INSERT INTO instruments (silver_source_id, instrument_external_id,
                asset_class, symbol, name, first_seen_at, last_seen_at)
            VALUES ('schwab-test', ?, 'public_equity', ?, 'EXAMPLE', 0, 0)`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	anchors, err := collectAnchors(context.Background(), db, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) != 1 || anchors[0].Symbol != "AAAA" {
		t.Errorf("anchors = %+v, want only the ticker AAAA", anchors)
	}
}

// TestCollectCandidatesSkipsHintedRowsAndCash: a transaction whose
// adapter looked its instrument up and failed carries the token it
// failed on, and is config's to close by that token, not a model's to
// name; a cash-class instrument has no ticker worth guessing. Everything
// else stays a candidate.
func TestCollectCandidatesSkipsHintedRowsAndCash(t *testing.T) {
	t.Parallel()
	_, goldPath := resolveSymbolsFixture(t, "http://unused", 0, nil)
	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, row := range [][2]string{{"EX00000001", "public_equity"}, {"EX00000002", "cash"}} {
		if _, err := db.Exec(`INSERT INTO instruments (silver_source_id, instrument_external_id,
                asset_class, name, first_seen_at, last_seen_at)
            VALUES ('schwab-test', ?, ?, 'EXAMPLE', 0, 0)`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	for i, row := range []struct{ description, hint any }{
		{"DIVIDEND RECEIVED EXAMPLE COMPANY", nil},
		{"DIVIDEND RECEIVED OTHER EXAMPLE INC", "OTHEREXAMPLEINC"},
	} {
		if _, err := db.Exec(`INSERT INTO transactions (silver_source_id, transaction_external_id,
                occurred_at, account_external_id, kind, currency, net_amount, description, instrument_hint)
            VALUES ('schwab-test', ?, 0, 'ACCT1', 'dividend', 'USD', 10, ?, ?)`,
			fmt.Sprintf("T%d", i), row.description, row.hint); err != nil {
			t.Fatal(err)
		}
	}
	cands, err := collectCandidates(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cands {
		got = append(got, c.LookupKind+":"+c.LookupValue)
	}
	want := []string{"instrument_external_id:EX00000001", "name:DIVIDEND RECEIVED EXAMPLE COMPANY"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}
