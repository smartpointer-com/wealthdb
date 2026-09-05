package main

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

func TestStripThinkingBlocks(t *testing.T) {
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
	storeErr := storeResolutions(context.Background(), goldPath, rows, "test-model", &out)
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
