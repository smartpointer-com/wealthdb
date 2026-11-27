package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// fxFixture is a setup that reports in CHF with one loaded source whose
// position is in USD, a cash balance in USD beside it, and no exchange
// rate: its config, which is not the default one, and its gold. Gold
// also holds balances in three tokens with no price, which are no
// currencies of money: no line of any command names them.
func fxFixture(t *testing.T) (cfg, goldPath string) {
	t.Helper()
	cfg = setupCLITest(t)
	goldPath = goldPathFromCfg(cfg)
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "CHF",
        "silver_sources": [{"id":"schwab-test","kind":"schwab","path":%q}],
        "web": {"enabled": false}, "mcp": {"enabled": false}}`,
		goldPath, filepath.Join(filepath.Dir(cfg), "schwab.db"))
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, se, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatalf("init: %s", se)
	}
	if so, se, code := run(t, "-c", cfg, "load", "-a"); code != 0 {
		t.Fatalf("load -a: exit %d\n%s%s", code, so, se)
	}
	// A source of its own holds what the test adds, so a reload of the
	// configured source leaves it.
	execGold(t, goldPath, `INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
	     VALUES ('bank', 'synthetic', '/x/bank.db', -1, 0, 0)`)
	execGold(t, goldPath, `INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency, first_seen_at, last_seen_at)
	     VALUES ('bank', 'ACC-1', 'checking', 'Checking', 'USD', 0, 0)`)
	for _, ccy := range append([]string{"USD"}, fxTokens...) {
		addCash(t, goldPath, ccy)
	}
	return cfg, goldPath
}

// fxTokens are the token codes the fixture holds balances in.
var fxTokens = []string{"ETK", "DTK", "XTK"}

// addCash books a cash balance in ccy on the fixture's bank account, at
// the snapshot of the loaded position.
func addCash(t *testing.T, goldPath, ccy string) {
	t.Helper()
	execGold(t, goldPath, `INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
	     SELECT 'bank', max(snapshot_at), 'ACC-1', ?, 'current', 250 FROM positions`, ccy)
}

// addRate stores one exchange rate between the two currencies.
func addRate(t *testing.T, goldPath, base, quote string) {
	t.Helper()
	execGold(t, goldPath, `INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
	     VALUES ('bank', 0, ?, ?, 0.9)`, base, quote)
}

// fxSentence is the notice for the config at cfg, as every command says
// it after its own prefix: missing is "USD" or "EUR and USD", and first
// the first of them.
func fxSentence(cfg, missing, first string) string {
	other := "or report in " + first + " with -x " + first
	if missing != first {
		other = "or report in one of them, such as " + first + " with -x " + first
	}
	return "no exchange rate from " + missing + " to CHF: amounts in " + missing + " are missing from every figure in CHF. " +
		"Add exchange rates with 'wealthdb -c " + shellWord(cfg) + " config banks' (FRED exchange and interest rates), " + other + ".\n"
}

// reportCommands are one view of every report family that prints
// amounts in an output currency, as the CLI names it.
func reportCommands(t *testing.T) [][]string {
	t.Helper()
	var out [][]string
	for _, fam := range reportFamilies {
		if !fam.currency || fam == marksFamily {
			continue
		}
		args := []string{fam.name}
		for i := range fam.views {
			if v := fam.views[i]; v.ownCurrency == "" {
				if !fam.oneShape() {
					args = append(args, v.name)
				}
				break
			}
		}
		out = append(out, args)
	}
	var names []string
	for _, c := range out {
		names = append(names, c[0])
	}
	slices.Sort(names)
	if want := []string{"cashflow", "exposure", "gains", "holdings", "income", "returns", "spending", "transactions", "yield"}; !slices.Equal(names, want) {
		t.Fatalf("the report families with a currency are %v, want %v", names, want)
	}
	return out
}

// TestReportsSayWhatDoesNotConvert: with amounts in USD, reports in CHF
// and no exchange rate, every report in an output currency says so on
// stderr, once, naming the setup command of the config it read; status,
// load, reload and web-materialize say the same sentence. With the rate
// there, or with the report in USD, nothing is said.
func TestReportsSayWhatDoesNotConvert(t *testing.T) {
	t.Parallel()
	cfg, goldPath := fxFixture(t)
	notice := fxSentence(cfg, "USD", "USD")
	said := func(text string) int { return strings.Count(text, "no exchange rate") }

	// reports runs every report and the status overview in one state of
	// gold. inUSD says a report in USD converts everything in it.
	reports := func(state, want string, inUSD bool) {
		t.Helper()
		n := 0
		if want != "" {
			n = 1
		}
		for _, args := range reportCommands(t) {
			so, se, code := runToday(t, append([]string{"-c", cfg}, args...)...)
			if code != 0 || said(se) != n || (want != "" && !strings.Contains(se, "wealthdb: "+want)) {
				t.Errorf("%s: %v: exit %d, stderr says it %d time(s), want %d of %q:\n%s", state, args, code, said(se), n, want, se)
			}
			if said(so) != 0 {
				t.Errorf("%s: %v: the notice is on stdout:\n%s", state, args, so)
			}
			if !inUSD {
				continue
			}
			if _, se, code := runToday(t, append(append([]string{"-c", cfg}, args...), "-x", "USD")...); code != 0 || said(se) != 0 {
				t.Errorf("%s: %v -x USD: exit %d, stderr:\n%s", state, args, code, se)
			}
		}
		// status --marks prints two columns in the output currency.
		if _, se, code := run(t, "-c", cfg, "status", "--marks"); code != 0 || said(se) != n || (want != "" && !strings.Contains(se, "wealthdb: "+want)) {
			t.Errorf("%s: status --marks: exit %d, want %q on stderr:\n%s", state, code, want, se)
		}
		// A view in each document's own currency converts nothing.
		if _, se, code := runToday(t, "-c", cfg, "returns", "stated"); code != 0 || said(se) != 0 {
			t.Errorf("%s: returns stated: exit %d, stderr:\n%s", state, code, se)
		}
		// status: the last line of the overview, and none in one source's
		// block.
		so, se, code := run(t, "-c", cfg, "status")
		if code != 0 || said(so) != n || (want != "" && !strings.HasSuffix(so, "\nstatus: "+want)) || said(se) != 0 {
			t.Errorf("%s: status: exit %d, want %q as the last line:\n%s%s", state, code, want, so, se)
		}
		if so, _, _ = run(t, "-c", cfg, "status", "schwab-test"); said(so) != 0 {
			t.Errorf("%s: status schwab-test says it:\n%s", state, so)
		}
	}
	// materialize runs web-materialize: the line is on stderr, in a run
	// that rewrites the tables and in one that finds them up to date.
	materialize := func(state, want string, unchanged bool) {
		t.Helper()
		so, se, code := runToday(t, "-c", cfg, "web-materialize")
		if code != 0 || (want == "") != (said(se) == 0) || (want != "" && strings.Count(se, "wealthdb: "+want) != 1) || said(so) != 0 ||
			strings.Contains(se, "inputs unchanged") != unchanged {
			t.Errorf("%s: web-materialize (unchanged %v): exit %d, want %q on stderr:\n%s%s", state, unchanged, code, want, so, se)
		}
	}
	// loads runs a load and a reload: the line is the last of the load,
	// beside its pointers. A load of one source says nothing: the source
	// of exchange rates may be the next one loaded.
	loads := func(state, want string) {
		t.Helper()
		for _, args := range [][]string{{"load", "-a"}, {"reload", "schwab-test"}} {
			so, se, code := run(t, append([]string{"-c", cfg}, args...)...)
			if code != 0 || (want == "") != (said(so) == 0) || (want != "" && !strings.HasSuffix(so, "\n"+args[0]+": "+want)) || said(se) != 0 {
				t.Errorf("%s: %v: exit %d, want %q as the last line:\n%s%s", state, args, code, want, so, se)
			}
		}
		if so, se, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 || said(so+se) != 0 {
			t.Errorf("%s: load schwab-test: exit %d, and it says the line:\n%s%s", state, code, so, se)
		}
	}

	// The tokens have no rate in any state below, and gold.Unconvertible
	// lists them; the notice is about currencies of money alone.
	if got := unconvertibleIn(t, goldPath, "CHF"); !slices.Equal(got, []string{"DTK", "ETK", "USD", "XTK"}) {
		t.Fatalf("gold.Unconvertible = %v, want the tokens and USD", got)
	}
	reports("no rate", notice, true)
	materialize("no rate", notice, false)
	materialize("no rate", notice, true)
	// A second currency with no rate: both are named.
	addCash(t, goldPath, "EUR")
	reports("two currencies", fxSentence(cfg, "EUR and USD", "EUR"), false)
	// A rate for one of the two leaves the other.
	addRate(t, goldPath, "CHF", "EUR")
	reports("a rate for EUR", notice, false)
	// A rate between USD and CHF connects everything, EUR to USD through
	// CHF too. No load ran since the tables were written, so
	// web-materialize finds them up to date.
	addRate(t, goldPath, "USD", "CHF")
	if got := unconvertibleIn(t, goldPath, "CHF"); !slices.Equal(got, []string{"DTK", "ETK", "XTK"}) {
		t.Fatalf("gold.Unconvertible = %v, want the tokens alone", got)
	}
	// The tokens alone: no command says anything.
	reports("every rate", "", true)
	materialize("every rate", "", true)
	loads("every rate", "")
	// A report in a token is a report in no currency of money: the line
	// on exchange rates has nothing to say of it.
	if _, se, code := runToday(t, "-c", cfg, "holdings", "global", "-x", "XTK"); code != 0 || said(se) != 0 {
		t.Errorf("holdings global -x XTK: exit %d, stderr:\n%s", code, se)
	}
	// Without that rate again, a load says what its gold cannot convert.
	execGold(t, goldPath, `DELETE FROM fx_rates WHERE base_currency = 'USD'`)
	loads("the rate gone", notice)
}

// unconvertibleIn is gold.Unconvertible on the gold file at goldPath,
// opened read-only.
func unconvertibleIn(t *testing.T, goldPath, target string) []string {
	t.Helper()
	db, err := gold.Open(goldPath, gold.ModeReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := gold.Unconvertible(context.Background(), db, target)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestNoticeLeavesTheReportAlone: an amount in a currency that does not
// convert is missing from the figures, so a report prints the same
// stdout and exits the same with it in gold, and says so on stderr.
func TestNoticeLeavesTheReportAlone(t *testing.T) {
	t.Parallel()
	cfg, goldPath := fxFixture(t)
	for _, args := range [][]string{{"holdings", "global", "-x", "USD"}, {"holdings", "global", "-x", "USD", "-f", "csv"},
		{"holdings", "sources", "-x", "USD", "-f", "json"}} {
		argv := append([]string{"-c", cfg}, args...)
		before, se, code := runToday(t, argv...)
		if code != 0 || se != "" || before == "" {
			t.Fatalf("%v: exit %d, stderr %q", args, code, se)
		}
		addCash(t, goldPath, "JPY")
		after, se, code2 := runToday(t, argv...)
		want := "wealthdb: no exchange rate from JPY to USD: amounts in JPY are missing from every figure in USD. " +
			"Add exchange rates with 'wealthdb -c " + cfg + " config banks' (FRED exchange and interest rates), or report in JPY with -x JPY.\n"
		if se != want {
			t.Errorf("%v: stderr = %q, want %q", args, se, want)
		}
		if after != before || code2 != code {
			t.Errorf("%v: the notice changed the report: exit %d, was %d\n%s\nwas\n%s", args, code2, code, after, before)
		}
		execGold(t, goldPath, `DELETE FROM cash_balances WHERE currency = 'JPY'`)
	}
}

// TestNoticeNamesTheSetup: the sentence names the setup command of the
// config, with -c for a setup of its own, quoted for a shell, and plain
// for the default config.
func TestNoticeNamesTheSetup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x")
	const rest = " (FRED exchange and interest rates), or report in USD with -x USD."
	for path, command := range map[string]string{
		"/x/wealthdb.cfg":     "'wealthdb config banks'",
		"/x/try/wealthdb.cfg": "'wealthdb -c /x/try/wealthdb.cfg config banks'",
		// A quoted path nests no quotes in the quoted command.
		"/x/my setup/wealthdb.cfg": `'wealthdb -c "/x/my setup/wealthdb.cfg" config banks'`,
	} {
		want := "no exchange rate from USD to CHF: amounts in USD are missing from every figure in CHF. Add exchange rates with " + command + rest
		if got := fxNotice(path, []string{"USD"}, "CHF"); got != want {
			t.Errorf("fxNotice(%q) = %q, want %q", path, got, want)
		}
	}
	if got := fxNotice("/x/wealthdb.cfg", nil, "CHF"); got != "" {
		t.Errorf("nothing missing: %q", got)
	}
	want := "no exchange rate from EUR, GBP and USD to CHF: amounts in EUR, GBP and USD are missing from every figure in CHF. " +
		"Add exchange rates with 'wealthdb config banks' (FRED exchange and interest rates), or report in one of them, such as EUR with -x EUR."
	if got := fxNotice("/x/wealthdb.cfg", []string{"EUR", "GBP", "USD"}, "CHF"); got != want {
		t.Errorf("three currencies: %q, want %q", got, want)
	}
}

// TestMCPHeaderSaysWhatDoesNotConvert: a report tool's header carries
// the same fact, in the words of a call, and nothing once the rate is
// there or the call is in a currency of the amounts.
func TestMCPHeaderSaysWhatDoesNotConvert(t *testing.T) {
	t.Parallel()
	src, _ := fxFixture(t)
	cfg := copyFixture(t, src)
	s := onFixtureToday(newTestMCP(cfg, false))
	const note = " · no exchange rate from USD to CHF: amounts in USD are missing from every figure in CHF (currency=USD shows them)"
	header := func(tool string, args map[string]any) string {
		t.Helper()
		out, err := callTool(t, s, tool, args)
		if err != nil {
			t.Fatalf("%s %v: %v", tool, args, err)
		}
		head, _, _ := strings.Cut(out, "\n")
		return head
	}
	for tool, args := range map[string]map[string]any{
		"holdings": {"view": "global"}, "returns": {"view": "global"}, "gains": {}, "exposure": {}, "yield": {},
		"spending": {}, "income": {}, "cashflow": {}, "transactions": {},
	} {
		if head := header(tool, args); strings.Count(head, note) != 1 {
			t.Errorf("%s: the header does not say it once: %s", tool, head)
		}
		in := map[string]any{"currency": "USD"}
		for k, v := range args {
			in[k] = v
		}
		if head := header(tool, in); strings.Contains(head, "exchange rate") {
			t.Errorf("%s in USD: the header says it: %s", tool, head)
		}
	}
	// A view in its own currencies converts nothing.
	if head := header("returns", map[string]any{"view": "stated"}); strings.Contains(head, "exchange rate") {
		t.Errorf("returns stated: the header says it: %s", head)
	}
	addCash(t, goldPathFromCfg(cfg), "EUR")
	const two = " · no exchange rate from EUR and USD to CHF: amounts in EUR and USD are missing from every figure in CHF " +
		"(a call in one of them, such as currency=EUR, shows its amounts)"
	if head := header("holdings", map[string]any{"view": "global"}); !strings.Contains(head, two) {
		t.Errorf("two currencies: %s", head)
	}
	addRate(t, goldPathFromCfg(cfg), "CHF", "EUR")
	addRate(t, goldPathFromCfg(cfg), "USD", "CHF")
	if head := header("holdings", map[string]any{"view": "global"}); strings.Contains(head, "exchange rate") {
		t.Errorf("with the rates: %s", head)
	}
}

// TestSetupReadsWhatDoesNotConvert: the setup's banks section names the
// currencies the database holds with no rate, read through the action
// the config command gives it, and its to-do block the banks command of
// the config it read.
func TestSetupReadsWhatDoesNotConvert(t *testing.T) {
	t.Parallel()
	cfg, goldPath := fxFixture(t)
	if got, err := unconvertibleCurrencies(context.Background(), goldPath, "CHF"); err != nil || !slices.Equal(got, []string{"USD"}) {
		t.Fatalf("unconvertibleCurrencies = %v, %v", got, err)
	}
	banks := func() string {
		t.Helper()
		var so, se bytes.Buffer
		if code := Run([]string{"-c", cfg, "config", "banks"}, strings.NewReader("\n"), &so, &se); code != 0 {
			t.Fatalf("config banks: exit %d: %s", code, se.String())
		}
		return strings.Join(strings.Fields(so.String()), " ")
	}
	hint := "The reports are in CHF, and the database holds amounts in USD with no exchange rate to CHF. " +
		"Add FRED exchange and interest rates so the reports can convert them."
	step := "wealthdb -c " + cfg + " config banks Adds FRED exchange and interest rates, so that the reports can convert amounts in another currency."
	// The hint names USD, and none of the tokens gold holds beside it.
	if out := banks(); strings.Count(out, hint) != 1 || !strings.Contains(out, step) {
		t.Errorf("the section does not name the currency from the data, or the block the banks command:\n%s", out)
	}
	// With the rate there, the tokens alone are left, and the database
	// says nothing.
	addRate(t, goldPath, "USD", "CHF")
	if out := banks(); strings.Contains(out, "the database holds") || strings.Contains(out, "config banks Adds") {
		t.Errorf("with the rate, the section still reads the database's verdict:\n%s", out)
	}
}
