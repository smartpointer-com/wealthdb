package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// newTestMCP is a server over the config at cfg, as `mcp-serve` builds
// one, with its log discarded.
func newTestMCP(cfg string, privacy bool) *mcpServer {
	return &mcpServer{
		configPath: cfg,
		privacy:    privacy,
		chars:      16000,
		duckdb:     map[string]string{"threads": "2"},
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:        time.Now,
	}
}

// callTool runs one tool call the way the handler does, minus the
// protocol: parse the arguments, run, return the text.
func callTool(t *testing.T, s *mcpServer, tool string, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range s.tools() {
		if spec.name != tool {
			continue
		}
		a, err := parseArgs(tool, spec.params, raw)
		if err != nil {
			return "", err
		}
		var out toolOutput
		if spec.family != nil {
			out, err = s.runFamily(context.Background(), spec, a)
		} else {
			out, err = spec.run(context.Background(), a)
		}
		return out.text, err
	}
	t.Fatalf("no tool %q", tool)
	return "", nil
}

// goldenCase is one report run through both front-ends.
type goldenCase struct {
	name string
	cli  []string       // after `-c cfg`, before `-f csv`
	tool string         // the MCP tool
	args map[string]any // the call's arguments, before format/columns/max_chars
}

// fixedColumns marks the two dumps, which print every column and take
// no -C.
func (c goldenCase) fixedColumns() bool {
	return c.tool == "categorizations" || c.tool == "resolutions"
}

// cliPrivacy is false for resolutions, which has no -p to compare a
// redacted run with.
func (c goldenCase) cliPrivacy() bool { return c.tool != "resolutions" }

// TestMCPMatchesCLI is the server's contract made executable: for
// every view of the nine report tools and for the two dumps, the
// server's csv result equals `wealthdb … -f csv -C all` cell for cell,
// over seeded gold, with and without privacy, both on fixtureToday. The
// two front-ends share the report runner and rowsToTable, so this holds
// by construction; the test keeps it holding.
func TestMCPMatchesCLI(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		name  string
		setup func(*testing.T) string
		cases []goldenCase
	}{
		{"spending", func(t *testing.T) string { return withFeeLine(t, setupSpendingGold(t)) }, append(familyCases("spending", spendingFamily.views, "2026-05-01", "2026-06-30"),
			goldenCase{"spending transactions filtered", []string{"spending", "transactions", "2026-05-01", "2026-06-30",
				"-F", "date >= 2026-05-06", "-F", "category != Restaurant"}, "spending",
				map[string]any{"view": "transactions", "from": "2026-05-01", "to": "2026-06-30",
					"filter": []any{"date >= 2026-05-06", "category != Restaurant"}}})},
		{"income", setupIncomeGold, familyCases("income", []string{"summary", "types", "transactions"}, "2026-05-01",
			"2026-06-30")},
		{"cashflow", func(t *testing.T) string { return withResolutions(t, setupCashflowGold(t)) }, append(cashflowCases(),
			goldenCase{"categorizations", []string{"categorizations"}, "categorizations", map[string]any{}},
			goldenCase{"resolutions", []string{"resolutions"}, "resolutions", map[string]any{}},
			goldenCase{"transactions", []string{"transactions", "2026-05-01", "2026-06-30"}, "transactions",
				map[string]any{"from": "2026-05-01", "to": "2026-06-30"}},
			goldenCase{"transactions in CHF, newest first", []string{"transactions", "2026-05-01", "2026-06-30", "-x", "CHF", "-r"},
				"transactions", map[string]any{"from": "2026-05-01", "to": "2026-06-30", "currency": "CHF", "newest_first": true}},
			goldenCase{"transactions filtered", []string{"transactions", "2026-05-01", "2026-06-30", "-F", "kind != dividend"},
				"transactions", map[string]any{"from": "2026-05-01", "to": "2026-06-30", "filter": "kind != dividend"}},
		)},
		{"returns", setupReturnsGold, append(returnsCases(), holdingsCases()...)},
		{"gains", setupGainsGold, gainsCases()},
		{"exposure", func(t *testing.T) string { return setupExposureGold(t) }, exposureCases()},
		{"yield", setupYieldGold, append(yieldCases(), forwardIncomeCases()...)},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			cfg := fx.setup(t)
			for _, privacy := range []bool{false, true} {
				mode := "plain"
				if privacy {
					mode = "privacy"
				}
				// The cases of one mode share a copy and run in turn: the
				// CLI and the server open it under different configurations.
				t.Run(mode, func(t *testing.T) {
					t.Parallel()
					cfg := copyFixture(t, cfg)
					s := onFixtureToday(newTestMCP(cfg, privacy))
					for _, c := range fx.cases {
						if privacy && !c.cliPrivacy() {
							continue
						}
						t.Run(c.name, func(t *testing.T) {
							checkMCPMatchesCLI(t, s, cfg, c, privacy)
						})
					}
				})
			}
		})
	}
}

// checkMCPMatchesCLI is TestMCPMatchesCLI for one case: the server s
// over cfg against the CLI, with privacy on or off.
func checkMCPMatchesCLI(t *testing.T, s *mcpServer, cfg string, c goldenCase, privacy bool) {
	t.Helper()
	cli := append([]string{"-c", cfg}, c.cli...)
	cli = append(cli, "-f", "csv")
	if !c.fixedColumns() && (c.tool != "holdings" || c.args["view"] != "global") {
		cli = append(cli, "-C", "all")
	}
	if privacy {
		cli = append(cli, "-p")
	}
	want, se, code := runToday(t, cli...)
	if code != 0 {
		t.Fatalf("CLI exit %d: %s", code, se)
	}
	args := map[string]any{"format": "csv", "columns": "all", "max_chars": maxIntegerParam}
	for k, v := range c.args {
		args[k] = v
	}
	got, err := callTool(t, s, c.tool, args)
	if err != nil {
		t.Fatalf("MCP error: %v", err)
	}
	header, body, _ := strings.Cut(got, "\n")
	// The total line of a filtered result is the server's alone.
	if i := strings.LastIndex(body, "\ntotal of "); i >= 0 && c.args["filter"] != nil {
		body = body[:i]
	}
	if strings.Count(want, "\n") < 2 {
		t.Fatalf("the fixture leaves this case empty, which proves nothing:\n%s", want)
	}
	if body != strings.TrimRight(want, "\n") {
		t.Errorf("MCP and CLI differ\nheader: %s\nMCP:\n%s\nCLI:\n%s", header, body, want)
	}
	if privacy && !strings.Contains(header, "redacted") {
		t.Errorf("privacy result's header does not say redacted: %s", header)
	}
}

// withFeeLine adds a fee and its partial refund to a fixture's gold, on its
// first account, so the fee view has rows.
func withFeeLine(t *testing.T, cfg string) string {
	t.Helper()
	db, err := gold.Open(goldPathFromCfg(cfg), gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	defer db.Close()
	at := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC).Unix()
	if _, err := db.Exec(`
		INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
		                          account_external_id, kind, currency, net_amount, description)
		SELECT silver_source_id, id, ?, account_external_id, kind, 'USD', CAST(net AS DECIMAL(28,4)), 'ANNUAL FEE'
		  FROM (SELECT silver_source_id, account_external_id FROM accounts ORDER BY 1, 2 LIMIT 1)
		 CROSS JOIN (VALUES ('fee-1', 'fee', '-95'), ('fee-2', 'refund', '20')) v(id, kind, net)`, at); err != nil {
		t.Fatalf("seed fee: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id, merchant_signature,
		                                  signature_version, spend_detailed, provenance, assigned_at)
		SELECT silver_source_id, transaction_external_id, 'sig-fee', 1, 'BANK_FEES_OTHER_BANK_FEES', 'manual', 100
		  FROM transactions WHERE transaction_external_id IN ('fee-1', 'fee-2')`); err != nil {
		t.Fatalf("seed fee enrichment: %v", err)
	}
	return cfg
}

// withResolutions adds two ticker resolutions to a fixture's gold, one
// by identifier and one by name, so the resolutions dump has rows.
func withResolutions(t *testing.T, cfg string) string {
	t.Helper()
	db, err := gold.Open(goldPathFromCfg(cfg), gold.ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO symbol_resolutions
		(silver_source_id, lookup_kind, lookup_value, symbol, resolved_at, model_name)
		VALUES ('bank', 'instrument_external_id', 'XX0000000001', 'EXDC', 1, 'test-model'),
		       ('bank', 'name', 'DIVIDEND EXAMPLE DIVIDEND CORP', 'EXDC', 1, 'test-model')`); err != nil {
		t.Fatalf("seed resolutions: %v", err)
	}
	return cfg
}

// TestMCPPrivacyRedactsResolvedNarratives: a by-name resolution's lookup
// value is a statement narrative, and the privacy endpoint masks it; an
// identifier stays legible.
func TestMCPPrivacyRedactsResolvedNarratives(t *testing.T) {
	t.Parallel()
	s := newTestMCP(withResolutions(t, setupCashflowGold(t)), true)
	text, err := callTool(t, s, "resolutions", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "DIVIDEND EXAMPLE") || !strings.Contains(text, "XX0000000001") {
		t.Errorf("privacy resolutions:\n%s", text)
	}
}

// familyCases covers each view of a windowed family at two buckets.
func familyCases(tool string, views []string, from, to string) []goldenCase {
	var out []goldenCase
	for _, v := range views {
		for _, p := range []string{"monthly", "total"} {
			out = append(out, goldenCase{tool + " " + v + " " + p,
				[]string{tool, v, from, to, "--period", p}, tool,
				map[string]any{"view": v, "from": from, "to": to, "period": p}})
		}
	}
	out = append(out, goldenCase{tool + " categories detailed in CHF",
		[]string{tool, views[1], from, to, "--period", "total", "--level", "detailed", "-x", "CHF"}, tool,
		map[string]any{"view": views[1], "from": from, "to": to, "period": "total", "level": "detailed", "currency": "CHF"}})
	return out
}

func cashflowCases() []goldenCase {
	const from, to = "2026-05-01", "2026-06-30"
	w := map[string]any{"from": from, "to": to}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range w {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	return []goldenCase{
		{"cashflow summary", []string{"cashflow", "summary", from, to, "--period", "monthly"}, "cashflow", with("view", "summary", "period", "monthly")},
		{"cashflow flows class", []string{"cashflow", "flows", from, to, "--period", "total", "--level", "class"}, "cashflow", with("view", "flows", "period", "total", "level", "class")},
		{"cashflow flows by asset class", []string{"cashflow", "flows", from, to, "--period", "total", "--investing", "class"}, "cashflow", with("view", "flows", "period", "total", "investing", "class")},
		{"cashflow sankey", []string{"cashflow", "sankey", from, to}, "cashflow", with("view", "sankey")},
		{"cashflow transactions", []string{"cashflow", "transactions", from, to, "-x", "CHF"}, "cashflow", with("view", "transactions", "currency", "CHF")},
		{"cashflow coverage", []string{"cashflow", "coverage", from, to, "--period", "total"}, "cashflow", with("view", "coverage", "period", "total")},
	}
}

func returnsCases() []goldenCase {
	var out []goldenCase
	for _, v := range returnsFamily.views {
		if v == "stated" {
			// The document fixes the period: no --method, no --period.
			out = append(out, goldenCase{"returns stated", []string{"returns", "stated"}, "returns",
				map[string]any{"view": v}})
			continue
		}
		if v == "bridge" {
			// The bridge computes no return: no --method.
			out = append(out, goldenCase{"returns bridge", []string{"returns", "bridge", "--period", "total"}, "returns",
				map[string]any{"view": v}},
				goldenCase{"returns bridge quarterly in CHF", []string{"returns", "bridge", "--period", "quarterly", "-x", "CHF"},
					"returns", map[string]any{"view": v, "period": "quarterly", "currency": "CHF"}})
			continue
		}
		out = append(out, goldenCase{"returns " + v,
			[]string{"returns", v, "--method", "both", "--period", "total"}, "returns",
			map[string]any{"view": v}})
	}
	return append(out,
		goldenCase{"status marks", []string{"status", "--marks"}, "status", map[string]any{"marks": true}},
		goldenCase{"status marks in CHF", []string{"status", "--marks", "2024", "2024", "-x", "CHF"}, "status",
			map[string]any{"marks": true, "from": "2024", "to": "2024", "currency": "CHF"}},
		goldenCase{"returns accounts quarterly twr in CHF",
			[]string{"returns", "accounts", "--method", "twr", "--period", "quarterly", "-x", "CHF"}, "returns",
			map[string]any{"view": "accounts", "method": "twr", "period": "quarterly", "currency": "CHF"}},
		goldenCase{"returns sources without netting",
			[]string{"returns", "sources", "--method", "mwr", "--period", "annual", "--netting", "off", "--inception", "strict"}, "returns",
			map[string]any{"view": "sources", "method": "mwr", "period": "annual", "netting": false, "inception": "strict"}},
	)
}

func exposureCases() []goldenCase {
	var out []goldenCase
	for _, v := range exposureFamily.views {
		out = append(out, goldenCase{"exposure " + v, []string{"exposure", v, "-d", "2026-06-30"}, "exposure",
			map[string]any{"view": v, "as_of": "2026-06-30"}})
	}
	return append(out,
		goldenCase{"exposure countries by domicile", []string{"exposure", "countries", "-d", "2026-06-30",
			"--country", "domicile"}, "exposure",
			map[string]any{"view": "countries", "as_of": "2026-06-30", "country": "domicile"}},
		goldenCase{"exposure countries quarterly", []string{"exposure", "countries", "--period", "quarterly", "2026-01-01", "2026-06-30"},
			"exposure", map[string]any{"view": "countries", "period": "quarterly", "from": "2026-01-01", "to": "2026-06-30"}},
		goldenCase{"exposure classes monthly, filtered", []string{"exposure", "classes", "--period", "monthly", "2026-04", "2026-06",
			"-F", "period >= 2026-05"}, "exposure",
			map[string]any{"view": "classes", "period": "monthly", "from": "2026-04", "to": "2026-06", "filter": "period >= 2026-05"}})
}

func holdingsCases() []goldenCase {
	var out []goldenCase
	for _, v := range holdingsFamily.views {
		out = append(out, goldenCase{"holdings " + v, []string{"holdings", v}, "holdings", map[string]any{"view": v}})
	}
	return append(out,
		goldenCase{"holdings positions with cash in CHF", []string{"holdings", "positions", "--with-cash", "-x", "CHF"}, "holdings",
			map[string]any{"view": "positions", "with_cash": true, "currency": "CHF"}})
}

// yieldCases covers each view of the yield tool, the converted rows and a
// series.
func yieldCases() []goldenCase {
	var out []goldenCase
	for _, v := range yieldViews {
		out = append(out, goldenCase{"yield " + v, []string{"yield", v, "-d", "2026-06-30"}, "yield",
			map[string]any{"view": v, "as_of": "2026-06-30"}})
	}
	return append(out,
		goldenCase{"yield accounts converted", []string{"yield", "accounts", "-d", "2026-06-30", "--converted"}, "yield",
			map[string]any{"view": "accounts", "as_of": "2026-06-30", "converted": true}},
		goldenCase{"yield global quarterly", []string{"yield", "global", "--period", "quarterly", "2026-04-01", "2026-06-30"},
			"yield", map[string]any{"view": "global", "period": "quarterly", "from": "2026-04-01", "to": "2026-06-30"}},
		goldenCase{"yield positions filtered", []string{"yield", "positions", "-d", "2026-06-30", "-F", "kind in bond, deposit"},
			"yield", map[string]any{"view": "positions", "as_of": "2026-06-30", "filter": []any{"kind in bond, deposit"}}})
}

// forwardIncomeCases covers each forward view of the income tool over a
// window from fixtureToday's month to two years on, under assumptions
// other than the defaults; then the default window and each lone bound (a
// lone to starts next month); and the cash flow projection over the default
// window and quarterly under the same assumptions.
func forwardIncomeCases() []goldenCase {
	now := fixtureToday
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	to := time.Date(now.Year()+2, now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1).Format(time.DateOnly)
	next := strconv.Itoa(now.Year() + 1)
	nextMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0).Format(time.DateOnly)
	var out []goldenCase
	for _, v := range forwardViews {
		out = append(out, goldenCase{"income " + v, []string{"income", v, from, to, "--rates", "forward",
			"--maturing", "roll", "--shift", "-50"}, "income",
			map[string]any{"view": v, "from": from, "to": to, "rates": "forward", "maturing": "roll", "shift": -50}})
	}
	return append(out,
		goldenCase{"income projection default", []string{"income", "projection"}, "income",
			map[string]any{"view": "projection"}},
		goldenCase{"income projection from", []string{"income", "projection", next}, "income",
			map[string]any{"view": "projection", "from": next}},
		goldenCase{"income projection to", []string{"income", "projection", nextMonth, next}, "income",
			map[string]any{"view": "projection", "to": next}},
		// The cash flow projection: the income projection set against
		// the look-back.
		goldenCase{"cashflow projection default", []string{"cashflow", "projection"}, "cashflow",
			map[string]any{"view": "projection"}},
		goldenCase{"cashflow projection quarterly", []string{"cashflow", "projection", nextMonth, to, "--period",
			"quarterly", "--rates", "forward", "--maturing", "roll", "--shift", "-50"}, "cashflow",
			map[string]any{"view": "projection", "from": nextMonth, "to": to, "period": "quarterly", "rates": "forward",
				"maturing": "roll", "shift": -50}})
}
