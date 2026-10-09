package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect runs s's MCP server over an in-memory transport and returns
// a connected client session.
func connect(t *testing.T, s *mcpServer, today time.Time) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.newServer(today).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func resultText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestMCPInitialize: the instructions open with the date and, on the
// privacy server, say what is redacted; the server offers tools and
// resources and nothing else.
func TestMCPInitialize(t *testing.T) {
	t.Parallel()
	cfg := setupCashflowGold(t)
	day := time.Date(2026, 10, 4, 23, 0, 0, 0, time.UTC)
	for _, privacy := range []bool{false, true} {
		init := connect(t, newTestMCP(cfg, privacy), day).InitializeResult()
		if !strings.HasPrefix(init.Instructions, "Today is 2026-10-04 (UTC).") {
			t.Errorf("instructions open with %q", strings.SplitN(init.Instructions, "\n", 2)[0])
		}
		if strings.Contains(init.Instructions, privacyNotice) != privacy {
			t.Errorf("privacy %v: the privacy notice present = %v", privacy, !privacy)
		}
		caps := init.Capabilities
		if caps.Tools == nil || caps.Resources == nil || caps.Logging != nil || caps.Prompts != nil {
			t.Errorf("capabilities = %+v, want tools and resources only", caps)
		}
		if init.ServerInfo.Name != "wealthdb" {
			t.Errorf("server name = %q", init.ServerInfo.Name)
		}
	}
}

// TestMCPToolList: twelve tools, every one read-only, idempotent and
// closed-world, none with a required parameter, each view enum the
// family's own.
func TestMCPToolList(t *testing.T) {
	t.Parallel()
	cs := connect(t, newTestMCP(setupCashflowGold(t), false), time.Now())
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || *a.OpenWorldHint ||
			a.DestructiveHint == nil || *a.DestructiveHint {
			t.Errorf("%s: annotations = %+v, want read-only, idempotent, closed-world, not destructive", tool.Name, a)
		}
		schema, _ := json.Marshal(tool.InputSchema)
		var sm struct {
			Type       string                    `json:"type"`
			Required   []string                  `json:"required"`
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(schema, &sm); err != nil || sm.Type != "object" {
			t.Errorf("%s: schema %s is not an object schema", tool.Name, schema)
		}
		if len(sm.Required) > 0 {
			t.Errorf("%s: required parameters %v; every parameter is optional", tool.Name, sm.Required)
		}
		for name, prop := range sm.Properties {
			if prop["description"] == "" || prop["type"] == "" {
				t.Errorf("%s.%s: no type or description", tool.Name, name)
			}
		}
	}
	sort.Strings(names)
	want := "cashflow,categorizations,describe,gains,holdings,income,resolutions,returns,snapshots,spending,status,transactions"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("tools = %s, want %s", got, want)
	}
}

// TestMCPCoversEveryCLIView holds the tools against the CLI's view
// tables, the way TestUsageListsEverySubcommand holds the listing
// against dispatch: a view the CLI gains cannot ship without its tool
// counterpart.
func TestMCPCoversEveryCLIView(t *testing.T) {
	t.Parallel()
	cli := map[string][]string{
		"holdings": keys(holdingsViews), "returns": keys(returnsViews), "spending": keys(spendingViews),
		"income": keys(incomeViews), "cashflow": keys(cashflowViews),
	}
	s := newTestMCP("", false)
	seen := map[string]bool{}
	for _, tool := range s.tools() {
		want, ok := cli[tool.name]
		if !ok {
			continue
		}
		seen[tool.name] = true
		got := append([]string{}, tool.family.views...)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s views = %v, the CLI has %v", tool.name, got, want)
		}
		for _, p := range tool.params {
			if p.name == "view" && strings.Join(p.enum, ",") != strings.Join(tool.family.views, ",") {
				t.Errorf("%s: the view parameter offers %v, the family runs %v", tool.name, p.enum, tool.family.views)
			}
		}
	}
	for name := range cli {
		if !seen[name] {
			t.Errorf("no tool serves the CLI's %s", name)
		}
	}
	// The diagnostics the CLI has beside the reports.
	for _, name := range []string{"transactions", "status", "snapshots", "resolutions", "categorizations"} {
		if _, ok := subcommands[name]; !ok {
			t.Errorf("the CLI has no %s, yet the server serves it", name)
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestMCPCallShapes: a call answers with text and the header line; a
// bad call is a tool error the model can read, not a protocol error.
func TestMCPCallShapes(t *testing.T) {
	t.Parallel()
	cs := connect(t, newTestMCP(setupCashflowGold(t), false), time.Now())
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "cashflow",
		Arguments: map[string]any{"view": "flows", "from": "2026-05-01", "to": "2026-06-30", "level": "class"}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %s", err, resultText(res))
	}
	if text := resultText(res); !strings.HasPrefix(text, "cashflow flows · window 2026-05-01 → 2026-06-30 · USD · period=total · level=class") {
		t.Errorf("header = %s", strings.SplitN(text, "\n", 2)[0])
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "cashflow", Arguments: map[string]any{"view": "sankey", "period": "annual"}})
	if err != nil {
		t.Fatalf("a refused call is a protocol error: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "sankey takes no period") {
		t.Errorf("sankey with a period: isError %v, %s", res.IsError, resultText(res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "spending", Arguments: map[string]any{"format": "json", "from": "2026"}})
	if err != nil || res.IsError {
		t.Fatalf("json call: %v %s", err, resultText(res))
	}
	if res.StructuredContent == nil {
		t.Error("a json result carries no structured content")
	}
}

// TestMCPMissingGold: before the first load every call says what to run.
func TestMCPMissingGold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "wealthdb.cfg")
	body := `{"gold_db": "` + filepath.Join(dir, "none.db") + `", "default_currency": "USD", "silver_sources": []}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestMCP(cfg, false)
	_, err := callTool(t, s, "holdings", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "wealthdb init") {
		t.Errorf("err = %v, want the init hint", err)
	}
	if msg := s.scrubPaths("open " + filepath.Join(dir, "none.db") + " in " + cfg); strings.Contains(msg, dir) {
		t.Errorf("a path survived the scrub: %s", msg)
	}
}

// TestMCPDescribe: every topic answers, an unknown one lists them all.
func TestMCPDescribe(t *testing.T) {
	t.Parallel()
	s := newTestMCP(setupCashflowGold(t), false)
	topics := append(append([]string{""}, s.reportToolNames()...), describeTopics...)
	for _, topic := range topics {
		text, err := callTool(t, s, "describe", map[string]any{"topic": topic})
		if err != nil || text == "" {
			t.Errorf("describe %q: %v", topic, err)
		}
	}
	text, _ := callTool(t, s, "describe", map[string]any{"topic": "spending"})
	for _, want := range []string{"categories: period*", "merchant*", "Filters each view takes", "transactions: source, account, category, merchant"} {
		if !strings.Contains(text, want) {
			t.Errorf("describe spending lacks %q", want)
		}
	}
	if _, err := callTool(t, s, "describe", map[string]any{"topic": "weather"}); err == nil ||
		!strings.Contains(err.Error(), "holdings") || !strings.Contains(err.Error(), "glossary") {
		t.Errorf("unknown topic: err = %v", err)
	}
}

// TestMCPResources: the guide is published for clients that preload.
func TestMCPResources(t *testing.T) {
	t.Parallel()
	cs := connect(t, newTestMCP(setupCashflowGold(t), false), time.Now())
	ctx := context.Background()
	list, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	uris := map[string]bool{}
	for _, r := range list.Resources {
		uris[r.URI] = true
	}
	for _, want := range []string{"wealthdb://guide", "wealthdb://guide/cashflow", "wealthdb://guide/quality"} {
		if !uris[want] {
			t.Errorf("no resource %s", want)
		}
	}
	got, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "wealthdb://guide/cashflow"})
	if err != nil || len(got.Contents) != 1 || !strings.HasPrefix(got.Contents[0].Text, "cashflow — ") {
		t.Errorf("read guide/cashflow: %v %+v", err, got)
	}
}

// TestMCPEmptyResultListsValues: a filter that matches nothing says
// which values its column takes, so the next call can correct it.
func TestMCPEmptyResultListsValues(t *testing.T) {
	t.Parallel()
	s := newTestMCP(setupCashflowGold(t), false)
	text, err := callTool(t, s, "cashflow", map[string]any{"from": "2026", "to": "2026", "section": "operating_out", "class": "Mortgage"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"view flows chosen for the section, class filter", "(no rows: nothing matched the filter; a finer view (transactions) may carry",
		"class values here:", "Mortgage"} {
		if !strings.Contains(text, want) {
			t.Errorf("empty result lacks %q:\n%s", want, text)
		}
	}
}

// TestMCPRowCeiling: the ceiling lowers a larger limit, and limit 0, and
// the paging line says so.
func TestMCPRowCeiling(t *testing.T) {
	t.Parallel()
	s := newTestMCP(setupCashflowGold(t), false)
	s.maxRows = 2
	for _, limit := range []int{0, 5} {
		text, err := callTool(t, s, "transactions", map[string]any{"from": "2026", "to": "2026", "limit": limit})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, "showing rows 1–2 of") || !strings.Contains(text, "at most 2 rows per call") {
			t.Errorf("limit %d under a ceiling of 2:\n%s", limit, text)
		}
	}
}

// TestMCPPagingPastTheEnd: an offset past the last row returns no rows,
// never the whole unfiltered result, and a huge limit is refused rather
// than overflowing.
func TestMCPPagingPastTheEnd(t *testing.T) {
	t.Parallel()
	s := newTestMCP(setupCashflowGold(t), false)
	text, err := callTool(t, s, "transactions", map[string]any{"from": "2026", "to": "2026", "kind": "dividend", "offset": 5})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(text, "\n| bank |") != 0 || !strings.Contains(text, "no rows at offset=5") {
		t.Errorf("offset past the end:\n%s", text)
	}
	if _, err := callTool(t, s, "transactions", map[string]any{"limit": "9223372036854775807", "offset": 1}); err == nil ||
		!strings.Contains(err.Error(), "at most") {
		t.Errorf("a huge limit: err = %v", err)
	}
}

// TestMCPEmptyJSON: an empty result in json is still the json object,
// with the explanation in its note.
func TestMCPEmptyJSON(t *testing.T) {
	t.Parallel()
	s := newTestMCP(setupCashflowGold(t), false)
	text, err := callTool(t, s, "spending", map[string]any{"view": "transactions", "merchant": "zzzz", "format": "json"})
	if err != nil {
		t.Fatal(err)
	}
	var r jsonResult
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatalf("not json: %v\n%s", err, text)
	}
	if r.TotalRows != 0 || len(r.Rows) != 0 || !strings.Contains(r.Note, "nothing matched the filter") {
		t.Errorf("empty json = %+v", r)
	}
}

// TestMCPFilterOnlyACoarserViewHas names the view to call instead.
func TestMCPFilterOnlyACoarserViewHas(t *testing.T) {
	t.Parallel()
	s := newTestMCP(setupCashflowGold(t), false)
	_, err := callTool(t, s, "holdings", map[string]any{"view": "positions", "tax_wrapper": "roth"})
	if err == nil || !strings.Contains(err.Error(), "call view=accounts") {
		t.Errorf("err = %v", err)
	}
}

// TestMCPPrivacyHidesConfigErrors: a config that fails validation quotes
// itself, and the config names accounts; the privacy endpoint says only
// that it failed.
func TestMCPPrivacyHidesConfigErrors(t *testing.T) {
	t.Parallel()
	cfg := webTestCfg(t, `{"gold_db": "/tmp/g.db", "default_currency": "USD",
		"silver_sources": [{"id": "bank", "kind": "chase", "path": "/tmp/bank.db"}],
		"portfolio_overrides": {"bank": {"PF0012345678": {}}}}`)
	_, full := callTool(t, newTestMCP(cfg, false), "holdings", map[string]any{})
	_, priv := callTool(t, newTestMCP(cfg, true), "holdings", map[string]any{})
	if full == nil || priv == nil || !strings.Contains(full.Error(), "PF0012345678") ||
		strings.Contains(priv.Error(), "PF0012345678") {
		t.Errorf("full: %v\nprivacy: %v", full, priv)
	}
}

// TestFilterOrderNamesEveryFilter: a filter missing from filterOrder is
// parsed and then never applied.
func TestFilterOrderNamesEveryFilter(t *testing.T) {
	t.Parallel()
	if len(filterOrder) != len(filterColumns) {
		t.Fatalf("filterOrder has %d filters, filterColumns %d", len(filterOrder), len(filterColumns))
	}
	for _, f := range filterOrder {
		if _, ok := filterColumns[f]; !ok {
			t.Errorf("%s is in filterOrder but not filterColumns", f)
		}
	}
}

func TestLoopbackAddr(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{
		"127.0.0.1:3300": true, "[::1]:3300": true, "localhost:3300": true,
		":3300": false, "0.0.0.0:3300": false, "192.168.1.5:3300": false, "3300": false,
	} {
		if got := loopbackAddr(addr); got != want {
			t.Errorf("loopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
