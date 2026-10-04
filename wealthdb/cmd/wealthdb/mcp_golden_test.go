package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
		rows:       100,
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
			out, err = s.runFamily(context.Background(), tool, spec.family, a)
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
	args map[string]any // the call's arguments, before format/columns/limit
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
// every view of the six report tools and for the two dumps, the
// server's csv result equals `wealthdb … -f csv -C all` cell for cell,
// over seeded gold, with and without privacy. The two front-ends share the report runner and rowsToTable,
// so this holds by construction; the test keeps it holding.
func TestMCPMatchesCLI(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		cfg   string
		cases []goldenCase
	}{
		{setupSpendingGold(t), familyCases("spending", spendingFamily.views, "2026-05-01", "2026-06-30")},
		{setupIncomeGold(t), familyCases("income", incomeFamily.views, "2026-05-01", "2026-06-30")},
		{withResolutions(t, setupCashflowGold(t)), append(cashflowCases(),
			goldenCase{"categorizations", []string{"categorizations"}, "categorizations", map[string]any{}},
			goldenCase{"resolutions", []string{"resolutions"}, "resolutions", map[string]any{}},
			goldenCase{"transactions", []string{"transactions", "2026-05-01", "2026-06-30"}, "transactions",
				map[string]any{"from": "2026-05-01", "to": "2026-06-30"}},
			goldenCase{"transactions in CHF, newest first", []string{"transactions", "2026-05-01", "2026-06-30", "-x", "CHF", "-r"},
				"transactions", map[string]any{"from": "2026-05-01", "to": "2026-06-30", "currency": "CHF", "newest_first": true}},
		)},
		{setupReturnsGold(t), append(returnsCases(), holdingsCases()...)},
	}
	for _, fx := range fixtures {
		for _, privacy := range []bool{false, true} {
			s := newTestMCP(fx.cfg, privacy)
			for _, c := range fx.cases {
				if privacy && !c.cliPrivacy() {
					continue
				}
				cli := append([]string{"-c", fx.cfg}, c.cli...)
				cli = append(cli, "-f", "csv")
				if !c.fixedColumns() && (c.tool != "holdings" || c.args["view"] != "global") {
					cli = append(cli, "-C", "all")
				}
				if privacy {
					cli = append(cli, "-p")
				}
				want, se, code := run(t, cli...)
				if code != 0 {
					t.Fatalf("%s: CLI exit %d: %s", c.name, code, se)
				}
				args := map[string]any{"format": "csv", "columns": "all", "limit": 0}
				for k, v := range c.args {
					args[k] = v
				}
				got, err := callTool(t, s, c.tool, args)
				if err != nil {
					t.Fatalf("%s (privacy %v): MCP error: %v", c.name, privacy, err)
				}
				header, body, _ := strings.Cut(got, "\n")
				if strings.Count(want, "\n") < 2 {
					t.Fatalf("%s: the fixture leaves this case empty, which proves nothing:\n%s", c.name, want)
				}
				if body != strings.TrimRight(want, "\n") {
					t.Errorf("%s (privacy %v): MCP and CLI differ\nheader: %s\nMCP:\n%s\nCLI:\n%s", c.name, privacy, header, body, want)
				}
				if privacy && !strings.Contains(header, "redacted") {
					t.Errorf("%s: privacy result's header does not say redacted: %s", c.name, header)
				}
			}
		}
	}
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
		out = append(out, goldenCase{"returns " + v,
			[]string{"returns", v, "--method", "both", "--period", "total"}, "returns",
			map[string]any{"view": v}})
	}
	return append(out,
		goldenCase{"returns accounts quarterly twr in CHF",
			[]string{"returns", "accounts", "--method", "twr", "--period", "quarterly", "-x", "CHF"}, "returns",
			map[string]any{"view": "accounts", "method": "twr", "period": "quarterly", "currency": "CHF"}},
		goldenCase{"returns sources without netting",
			[]string{"returns", "sources", "--method", "mwr", "--period", "annual", "--netting", "off", "--inception", "strict"}, "returns",
			map[string]any{"view": "sources", "method": "mwr", "period": "annual", "netting": false, "inception": "strict"}},
	)
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
