package main

import (
	"context"
	"encoding/json"
	"flag"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/version"
)

var updateContract = flag.Bool("update", false, "rewrite testdata/mcp_contract from the server")

const contractDir = "testdata/mcp_contract"

// TestMCPContract holds what a model reads of the server to the files
// under testdata/mcp_contract, one per item and endpoint: the
// instructions, every tool as tools/list carries it (name, title,
// description, input schema, annotations), the resource list, every
// resource and every describe topic. These texts were tuned; a change
// to one shows here as a diff, and -update rewrites the files.
//
// The config names a gold that does not exist, so the overview and
// the glossary state that they cannot read it, and nothing is opened.
func TestMCPContract(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "wealthdb.cfg")
	body := `{"gold_db": "` + filepath.Join(dir, "none.db") + `", "default_currency": "USD", "silver_sources": []}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, privacy := range []bool{false, true} {
		for name, text := range contractItems(t, cfg, privacy) {
			got[name] = strings.ReplaceAll(text, version.String(), "<version>")
		}
	}
	if *updateContract {
		if err := os.RemoveAll(contractDir); err != nil {
			t.Fatal(err)
		}
		for name, text := range got {
			path := filepath.Join(contractDir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	want := map[string]string{}
	err := filepath.WalkDir(contractDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(contractDir, path)
		want[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(got)) {
		w, ok := want[name]
		switch {
		case !ok:
			t.Errorf("%s is new; run go test -run TestMCPContract -update and review the diff", name)
		case w != got[name]:
			t.Errorf("%s changed; run go test -run TestMCPContract -update and review the diff", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if _, ok := got[name]; !ok {
			t.Errorf("%s is gone; run go test -run TestMCPContract -update and review the diff", name)
		}
	}
}

// contractItems are one endpoint's items by file name.
func contractItems(t *testing.T, cfg string, privacy bool) map[string]string {
	t.Helper()
	ep := "plain/"
	if privacy {
		ep = "privacy/"
	}
	day := time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)
	s := newTestMCP(cfg, privacy)
	s.chars = defaultMCPChars
	s.now = func() time.Time { return day }
	cs := connect(t, s, day)
	ctx := context.Background()
	items := map[string]string{ep + "instructions.txt": cs.InitializeResult().Instructions + "\n"}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		items[ep+"tools/"+tool.Name+".json"] = contractJSON(t, struct {
			Name        string               `json:"name"`
			Title       string               `json:"title"`
			Description string               `json:"description"`
			InputSchema any                  `json:"inputSchema"`
			Annotations *mcp.ToolAnnotations `json:"annotations"`
		}{tool.Name, tool.Title, tool.Description, tool.InputSchema, tool.Annotations})
	}
	resources, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	items[ep+"resources.json"] = contractJSON(t, resources.Resources)
	for _, r := range resources.Resources {
		res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: r.URI})
		if err != nil {
			t.Fatalf("read %s: %v", r.URI, err)
		}
		var text strings.Builder
		for _, c := range res.Contents {
			text.WriteString(c.Text)
		}
		name := strings.TrimPrefix(strings.TrimPrefix(r.URI, "wealthdb://guide"), "/")
		if name == "" {
			name = "overview"
		}
		items[ep+"resources/"+name+".txt"] = text.String() + "\n"
	}
	topics := append(append([]string{""}, s.reportToolNames()...), describeTopics...)
	for _, topic := range topics {
		text, err := s.describeText(ctx, topic)
		if err != nil {
			t.Fatalf("describe %q: %v", topic, err)
		}
		name := topic
		if name == "" {
			name = "overview"
		}
		items[ep+"describe/"+name+".txt"] = text + "\n"
	}
	return items
}

func contractJSON(t *testing.T, v any) string {
	t.Helper()
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
