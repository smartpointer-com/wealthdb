package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// The command's paths end to end, stdout pinned line for line where the
// output is the contract: the header, the per-batch lines, the summary,
// the persisted block, and each early exit.

func TestResolveSymbolsReportsEveryStage(t *testing.T) {
	srv, _ := standInModel(t)
	cfgPath, _ := resolveSymbolsFixture(t, srv.URL, 3, []any{
		map[string]any{"silver_source_id": "schwab-test", "lookup_kind": "instrument_external_id",
			"lookup_value": "EX00000009", "symbol": "PINNED"},
	})
	out, errOut, code := run(t, "-c", cfgPath, "resolve-symbols", "--batch", "2")
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	want := `resolve-symbols: synced cfg overrides — 1 upserted, 0 suppressed (delete:true), 0 stale manual-override rows purged
resolve-symbols: 3 candidates (schwab-test=3; by-kind by-id=3), 0 anchors, 2 batch(es) of up to 2, model stand-in
resolve-symbols: attempt 1: 99 chars of CSV response (after stripping reasoning)
resolve-symbols: batch 1/2: 2 of 2 resolved
resolve-symbols: attempt 1: 49 chars of CSV response (after stripping reasoning)
resolve-symbols: batch 2/2: 1 of 1 resolved
resolve-symbols: summary
  candidates sent:    3 (schwab-test=3)
  LLM attempts:       2
  hallucinated rows:  0 (rejected by validation across all attempts)
  resolved:           3 (schwab-test=3; by-kind by-id=3)
  unresolved:         0 ()
  resolution rate:    schwab-test=100.0% (3/3)
resolve-symbols: persisted to gold:
  schwab-test: 3 rows upserted
resolve-symbols: total symbol_resolutions rows in gold now 4
`
	if out != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want nothing", errOut)
	}
}

func TestResolveSymbolsOverridesOnly(t *testing.T) {
	cfgPath, goldPath := resolveSymbolsFixture(t, "http://127.0.0.1:1", 2, []any{
		map[string]any{"silver_source_id": "schwab-test", "lookup_kind": "instrument_external_id",
			"lookup_value": "EX00000001", "symbol": "PINNED"},
	})
	out, errOut, code := run(t, "-c", cfgPath, "resolve-symbols", "--overrides-only")
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	want := `resolve-symbols: synced cfg overrides — 1 upserted, 0 suppressed (delete:true), 0 stale manual-override rows purged
resolve-symbols: overrides-only mode; total symbol_resolutions rows now 1
`
	if out != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
	if got := storedSymbols(t, goldPath); got["EX00000001"][0] != "PINNED" {
		t.Errorf("stored = %v, want the pin", got)
	}
}

func TestResolveSymbolsNothingToResolve(t *testing.T) {
	cfgPath, _ := resolveSymbolsFixture(t, "http://127.0.0.1:1", 0, nil)
	out, errOut, code := run(t, "-c", cfgPath, "resolve-symbols")
	if code != 0 || out != "resolve-symbols: nothing to resolve\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// TestResolveSymbolsDryRunWritesNothing: --dry-run opens gold read-only and
// takes no lock, so it cannot sync the overrides — it says what the sync
// would do, asks the model, prints the plan, and leaves gold as it was.
func TestResolveSymbolsDryRunWritesNothing(t *testing.T) {
	srv, calls := standInModel(t)
	cfgPath, goldPath := resolveSymbolsFixture(t, srv.URL, 2, []any{
		map[string]any{"silver_source_id": "schwab-test", "lookup_kind": "instrument_external_id",
			"lookup_value": "EX00000001", "symbol": "PINNED"},
	})
	out, errOut, code := run(t, "-c", cfgPath, "resolve-symbols", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	for _, line := range []string{
		"resolve-symbols: dry-run; the sync would apply cfg overrides — 1 upserted, 0 suppressed (delete:true), 0 stale manual-override rows purged\n",
		"--- dry-run plan (no rows written) ---\n  schwab-test [instrument_external_id] EX00000002 → TK2\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("stdout lacks %q:\n%s", line, out)
		}
	}
	if len(*calls) != 1 {
		t.Errorf("model calls = %d, want 1", len(*calls))
	}
	if got := storedSymbols(t, goldPath); len(got) != 0 {
		t.Errorf("a dry run stored %v", got)
	}
}

func TestResolveSymbolsDryRunOverridesOnlyWritesNothing(t *testing.T) {
	cfgPath, goldPath := resolveSymbolsFixture(t, "http://127.0.0.1:1", 1, []any{
		map[string]any{"silver_source_id": "schwab-test", "lookup_kind": "instrument_external_id",
			"lookup_value": "EX00000001", "symbol": "PINNED"},
	})
	out, errOut, code := run(t, "-c", cfgPath, "resolve-symbols", "-n", "--overrides-only")
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	want := `resolve-symbols: dry-run; the sync would apply cfg overrides — 1 upserted, 0 suppressed (delete:true), 0 stale manual-override rows purged
resolve-symbols: overrides-only mode; nothing written (dry-run)
`
	if out != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
	if got := storedSymbols(t, goldPath); len(got) != 0 {
		t.Errorf("a dry run stored %v", got)
	}
}

func TestResolveSymbolsFlagErrors(t *testing.T) {
	cfgPath, _ := resolveSymbolsFixture(t, "http://127.0.0.1:1", 1, nil)
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"-h"}, 0},
		{[]string{"--bogus"}, 2},
		{[]string{"extra"}, 2},
		{[]string{"--batch", "0"}, 2},
	} {
		_, errOut, code := run(t, append([]string{"-c", cfgPath, "resolve-symbols"}, c.args...)...)
		if code != c.code {
			t.Errorf("%v: exit %d, want %d", c.args, code, c.code)
		}
		if !strings.Contains(errOut, "usage: wealthdb resolve-symbols") {
			t.Errorf("%v: stderr lacks the usage:\n%s", c.args, errOut)
		}
	}
}

// TestResolveSymbolsNeedsAModelUnlessOverridesOnly: the model block is
// required for a model pass and not for the config-only sync.
func TestResolveSymbolsNeedsAModelUnlessOverridesOnly(t *testing.T) {
	cfgPath, _ := resolveSymbolsFixture(t, "http://127.0.0.1:1", 1, nil)
	var cfg map[string]any
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg["symbol_resolution"].(map[string]any), "model")
	raw, _ = json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, errOut, code := run(t, "-c", cfgPath, "resolve-symbols"); code != 2 ||
		!strings.Contains(errOut, "symbol_resolution.model is not set") {
		t.Errorf("without a model: exit %d, stderr %q", code, errOut)
	}
	if _, errOut, code := run(t, "-c", cfgPath, "resolve-symbols", "--overrides-only"); code != 0 {
		t.Errorf("--overrides-only without a model: exit %d, stderr %q", code, errOut)
	}
}

// TestResolveSymbolsStopsAtAFailedBatchKeepingTheOthers: a model call that
// fails ends the run, and the batches answered before it are stored.
func TestResolveSymbolsStopsAtAFailedBatchKeepingTheOthers(t *testing.T) {
	good, _ := standInModel(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy, err := http.NewRequest(r.Method, good.URL+r.URL.Path, r.Body)
		if err != nil {
			t.Fatal(err)
		}
		proxy.Header = r.Header
		resp, err := http.DefaultClient.Do(proxy)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		var body json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&body)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	cfgPath, goldPath := resolveSymbolsFixture(t, srv.URL, 3, nil)

	out, _, code := run(t, "-c", cfgPath, "resolve-symbols", "--batch", "2", "--max-attempts", "1")
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "resolve-symbols: stopped in batch 2 of 2; the 2 resolution(s) answered so far are stored\n") {
		t.Errorf("stdout lacks the stop line:\n%s", out)
	}
	if got := storedSymbols(t, goldPath); len(got) != 2 {
		t.Errorf("stored = %v, want the first batch's 2", got)
	}
}
