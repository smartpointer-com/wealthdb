package wizard

import (
	"bufio"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/jsonedit"
)

// basics is a config whose wizard steps all hold an answer.
const basics = `{"gold_db": "/x/g.db", "default_currency": "USD",
 "silver_sources": [{"id": "viac", "kind": "viac", "path": "/x/viac.db"}],
 "web": {"enabled": false}, "mcp": {"enabled": false}`

func TestAIModelFromNothing(t *testing.T) {
	h := newHarness(t)
	h.write(basics+"}\n", 0o600)
	h.section, h.inContainer, h.reply = "model", true, " OK\n"
	// The address, the offer of the container's name for localhost, the
	// name, no key, and the test question.
	out, err := h.run("http://localhost:11434/v1/", "", "example-model", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.cfgOf()
	want := config.ModelConfig{BaseURL: "http://host.docker.internal:11434/v1", API: "openai-completions", Name: "example-model"}
	if m := cfg.SymbolResolution.Model; m == nil || *m != want {
		t.Errorf("symbol_resolution.model = %+v", m)
	}
	if m := cfg.Spending.Categorization.CategorizationModel(); m == nil || *m != want {
		t.Errorf("spending.categorization.model = %+v", m)
	}
	if cfg.Income != nil {
		t.Errorf("income has a block of its own: %+v", cfg.Income)
	}
	if len(h.asked) != 1 || h.asked[0] != want {
		t.Errorf("asked %+v", h.asked)
	}
	for _, w := range []string{
		"where localhost is the container itself",
		"Use http://host.docker.internal:11434/v1 instead? [Y/n]",
		"Saved: example-model at host.docker.internal:11434.",
		"The model answered: OK\n",
		"  wealthdb resolve-symbols\n\n      Asks the model for the tickers the banks leave out.",
		"  wealthdb categorize\n\n      Asks the model for the category of each shop and payer",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}

	// Removing it takes the blocks the editor made, and nothing else.
	if _, err := h.run("r", "y"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.read(), "spending") || strings.Contains(h.read(), "symbol_resolution") {
		t.Errorf("an empty block is left:\n%s", h.read())
	}
	h.cfgOf()

	// Outside a container, localhost is this computer: no offer. Enter
	// with nothing saved goes back and writes nothing.
	h2 := newHarness(t)
	h2.write(basics+"}\n", 0o600)
	h2.section = "model"
	before := h2.read()
	if _, err := h2.run(""); err != nil {
		t.Fatal(err)
	}
	// An address, then Enter at the name, backs out as well.
	if _, err := h2.run("http://127.0.0.1:1234/v1", ""); err != nil {
		t.Fatal(err)
	}
	if h2.read() != before {
		t.Errorf("going back changed the file:\n%s", h2.read())
	}
	out, err = h2.run("http://127.0.0.1:1234/v1", "local", "", "n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "container") || h2.cfgOf().SymbolResolution.Model.BaseURL != "http://127.0.0.1:1234/v1" {
		t.Errorf("outside a container:\n%s\n%s", out, h2.read())
	}
	if len(h2.asked) != 0 {
		t.Errorf("a test question went out on n: %+v", h2.asked)
	}
}

// A key is saved, keeps the file to its owner, and is removed with -;
// the block keeps every member the editor does not write, and the
// income block, when it has one of its own, gets the same model.
func TestAIModelChangeKeyRemove(t *testing.T) {
	h := newHarness(t)
	h.write(basics+`,
 "symbol_resolution": {"model": {"baseUrl": "http://old:1/v1", "api": "openai-completions", "name": "old", "thinkingFormat": "x"},
   "overrides": []},
 "spending": {"rules": [{"match": "example", "category": "FOOD_AND_DRINK_COFFEE"}],
   "categorization": {"context": "merchant", "model": {"baseUrl": "http://other:2/v1", "api": "openai-completions", "name": "other"}}},
 "income": {"categorization": {"context": "payer"}}}
`, 0o644)
	h.section = "model"
	out, err := h.run("c", "http://new:3/v1", "new", "sk-example", "n")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Tickers of funds         old at old:1", "Categories of spending   other at other:2",
		"The file names different models."} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	cfg := h.cfgOf()
	want := config.ModelConfig{BaseURL: "http://new:3/v1", API: "openai-completions", Name: "new", APIKey: "sk-example", ThinkingFormat: "x"}
	if m := cfg.SymbolResolution.Model; *m != want {
		t.Errorf("symbol_resolution.model = %+v", m)
	}
	want.ThinkingFormat = ""
	for key, m := range map[string]*config.ModelConfig{
		"spending": cfg.Spending.Categorization.CategorizationModel(), "income": cfg.Income.Categorization.CategorizationModel(),
	} {
		if m == nil || *m != want {
			t.Errorf("%s model = %+v", key, m)
		}
	}
	if cfg.Spending.Categorization.Context != "merchant" || len(cfg.Spending.Rules) != 1 {
		t.Errorf("spending lost a member:\n%s", h.read())
	}
	if fi, _ := os.Stat(h.cfg); fi.Mode().Perm() != 0o600 {
		t.Errorf("a file with a key is %v", fi.Mode().Perm())
	}

	// Enter keeps the saved key; - removes it.
	if _, err := h.run("c", "", "", "", "n"); err != nil {
		t.Fatal(err)
	}
	if h.cfgOf().SymbolResolution.Model.APIKey != "sk-example" {
		t.Errorf("Enter dropped the key:\n%s", h.read())
	}
	if _, err := h.run("c", "", "", "-", "n"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.read(), "apiKey") {
		t.Errorf("- kept the key:\n%s", h.read())
	}

	// Remove takes every model, and each block it leaves empty.
	if _, err := h.run("r", "y"); err != nil {
		t.Fatal(err)
	}
	d, err := jsonedit.Parse([]byte(h.read()))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][]any{{"symbol_resolution", "model"}, {"spending", "categorization", "model"}, {"income", "categorization", "model"}} {
		if d.Has(p...) {
			t.Errorf("%v is left:\n%s", p, h.read())
		}
	}
	if !d.Has("symbol_resolution", "overrides") || !d.Has("spending", "categorization", "context") ||
		!d.Has("income", "categorization", "context") {
		t.Errorf("remove took more than the models:\n%s", h.read())
	}
}

func TestAIModelTestFails(t *testing.T) {
	h := newHarness(t)
	h.write(basics+`, "symbol_resolution": {"model": {"baseUrl": "http://m:1/v1", "name": "m"}}}`, 0o600)
	h.section, h.replyErr = "model", errors.New("POST http://m:1/v1/chat/completions: connection refused")
	out, err := h.run("t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No answer:\n  POST http://m:1/v1/chat/completions: connection refused\n"+
		"Check that the server runs, and the address.\n") {
		t.Errorf("output:\n%s", out)
	}
	h.replyErr = &ModelRefused{errors.New("HTTP 404 from http://m:1/v1/chat/completions: model not found")}
	if out, _ = h.run("t"); !strings.Contains(out, "The server answered, but not with a reply:\n  HTTP 404") ||
		!strings.Contains(out, "The address is right. Check the model's name") {
		t.Errorf("output:\n%s", out)
	}
}

func TestWrap(t *testing.T) {
	got := wrap("instrument_overrides, transaction_instruments, holding_aliases, symbol_resolution.overrides · docs/DESIGN.md", 40)
	want := []string{"instrument_overrides,", "transaction_instruments,", "holding_aliases,", "symbol_resolution.overrides ·", "docs/DESIGN.md"}
	if !slices.Equal(got, want) {
		t.Errorf("wrap = %q", got)
	}
	for _, l := range got {
		if len(l) > 40 {
			t.Errorf("line %q is too long", l)
		}
	}
}

func TestReadHidden(t *testing.T) {
	for in, want := range map[string]string{
		"key\r":             "key",
		"ab\x7fc\n":         "ac",
		"\x1b[Akey\x1bOB\r": "key",
		"junk\x15key\r":     "key",
		" spaced \r":        "spaced",
		"partial":           "partial",
	} {
		got, err := readHidden(bufio.NewReader(strings.NewReader(in)))
		if err != nil || got != want {
			t.Errorf("readHidden(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"\x03", "ab\x03", "\x04", ""} {
		if _, err := readHidden(bufio.NewReader(strings.NewReader(in))); !errors.Is(err, errQuit) {
			t.Errorf("readHidden(%q): %v, want a quit", in, err)
		}
	}
}

// On a terminal, the key is read in raw mode, where it is not shown.
func TestKeyIsReadInRawMode(t *testing.T) {
	h := newHarness(t)
	h.write(basics+"}\n", 0o600)
	term := &fakeTerminal{}
	h.terminal, h.section = term, "model"
	if _, err := h.run("http://m:1/v1", "m", "sk-example\r", "n"); err != nil {
		t.Fatal(err)
	}
	if term.raw != 1 || term.restored != 1 {
		t.Errorf("raw %d times, restored %d", term.raw, term.restored)
	}
	if h.cfgOf().SymbolResolution.Model.APIKey != "sk-example" {
		t.Errorf("key not saved:\n%s", h.read())
	}
}

// A setting the AI model shares a block with counts as set only for
// what the model section does not write.
func TestFileSettingState(t *testing.T) {
	for body, want := range map[string]string{
		`{"spending": {"categorization": {"model": {"baseUrl": "http://m/v1", "name": "m"}}}}`:                        "not set",
		`{"spending": {"categorization": {"context": "merchant", "model": {"baseUrl": "http://m/v1", "name": "m"}}}}`: "not set",
		`{"spending": {"pins": "/x/p.csv", "internal_transfer_matching": {"window_days": 3}}}`:                        "not set",
		`{"spending": {"internal_transfer_matching": {"names": []}}}`:                                                 "set",
		`{"spending": {"rules": [], "categorization": {}}}`:                                                           "set",
		`{"spending": {}}`: "not set",
		`{}`:               "not set",
	} {
		d, err := jsonedit.Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		w := &setup{store: &store{doc: d}}
		if got := w.state(fileSettings[3]); got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
}

// An edit by hand while the setup runs is kept: the next change applies
// on top of it. A file that does not parse at that moment is left alone.
func TestStoreKeepsAnEditByHand(t *testing.T) {
	h := newHarness(t)
	h.write(basics+"}\n", 0o600)
	s := openTestStore(t, h.cfg, "")
	h.write(basics+`, "lots": {"method": "hifo"}}`+"\n", 0o600)
	if err := s.apply(func(d *jsonedit.Doc) error { return d.Set("EUR", "default_currency") }); err != nil {
		t.Fatal(err)
	}
	if got := h.read(); !strings.Contains(got, `"hifo"`) || !strings.Contains(got, `"EUR"`) {
		t.Errorf("the edit by hand is lost:\n%s", got)
	}
	broken := basics + `, "lots": {"method": }` + "\n"
	h.write(broken, 0o600)
	err := s.apply(func(d *jsonedit.Doc) error { return d.Set("CHF", "default_currency") })
	var r *refusal
	if !errors.As(err, &r) || !strings.Contains(err.Error(), "changed and does not parse now") {
		t.Errorf("err = %v", err)
	}
	if h.read() != broken {
		t.Errorf("a broken file was written over:\n%s", h.read())
	}
}

func TestCheck(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		body  string
		state config.State
		want  string
	}{
		{"", config.StateAbsent, "holds no setup yet"},
		{`{"gold_db": "/x/g.db"}`, config.StateUnfinished, "is not finished: it has no default_currency"},
		{`{"web": {}}`, config.StateUnfinished, "it has no default_currency and no gold_db"},
		{"{\n  \"gold_db\": \"/x/g.db\",\n  \"default_currency\": \"USD\",,\n}", config.StateRefused, "(line 3, column 30)"},
		{`{"gold_db": "/x/g.db", "default_currency": "USD", "colour": 1}`, config.StateRefused, `unknown field "colour"`},
		{basics + `, "symbol_resolution": {"model": {"baseUrl": "http://m/v1"}}}`, config.StateRefused,
			"symbol_resolution.model.name is required"},
		{basics + `, "income": {"categorization": {"model": {"name": "m"}}}}`, config.StateRefused,
			"income.categorization.model.baseUrl is required"},
		{basics + "}", config.StateValid, "is valid: 1 source, reports in USD."},
	} {
		h.write(c.body, 0o600)
		st, ok, err := Check(h.cfg)
		got := ok
		if err != nil {
			got = err.Error()
		}
		if st != c.state || !strings.Contains(got, c.want) || (err == nil) != (c.state == config.StateValid) {
			t.Errorf("%s:\nstate %d, %q; want %d, %q", c.body, st, got, c.state, c.want)
		}
	}
	if _, _, err := Check(h.cfg + ".none"); err == nil || !strings.Contains(err.Error(), "holds no setup yet") {
		t.Errorf("no file: %v", err)
	}
}

// The menu's check of the file says what is wrong, and the menu goes on.
func TestMenuValidates(t *testing.T) {
	h := newHarness(t)
	h.write(basics+`, "symbol_resolution": {"model": {"baseUrl": "http://m/v1"}}}`, 0o600)
	out, err := h.run("11", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Validate the config file\n\nThe config is not valid:\n  config: symbol_resolution.model.name is required\n") {
		t.Errorf("output:\n%s", out)
	}
	h.write(basics+"}", 0o600)
	if out, _ = h.run("11", "q"); !strings.Contains(out, h.cfg+" is valid: 1 source, reports in USD.\n") {
		t.Errorf("output:\n%s", out)
	}
}

// A command the run's changes call for is listed once, however often
// the change is made.
func TestThenRunOnce(t *testing.T) {
	w := &setup{}
	for range 2 {
		w.thenRun(Step{"wealthdb categorize", "a"})
		w.thenRun(Step{"wealthdb web start", "b"})
	}
	if len(w.then) != 2 {
		t.Errorf("then = %+v", w.then)
	}
}

// A bank linked and downloaded since the last run of the setup is added
// with one Enter, on the way into Banks and brokers.
func TestBanksOffersCollectedData(t *testing.T) {
	h := newHarness(t)
	h.write(basics+"}\n", 0o600)
	h.collect("plaid", "chase", "chase.db")
	h.collect("plaid", "schwab", "schwab.db")
	h.section = "banks"
	out, err := h.run("", "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range h.cfgOf().SilverSources {
		got = append(got, s.ID)
	}
	if want := []string{"viac", "chase", "schwab"}; !slices.Equal(got, want) {
		t.Errorf("sources = %v, want %v", got, want)
	}
	for _, w := range []string{
		"Collected, but not in the list yet:\n  Plaid (linked banks): chase\n  Plaid (linked banks): schwab\n\nAdd them now [Y/n]: ",
		"Added Plaid (linked banks): chase.\nAdded Plaid (linked banks): schwab.\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "Collected, but not in the list:\n") {
		t.Errorf("the added banks are still offered:\n%s", out)
	}
	// The Plaid pick names the command that lists the banks linked so far.
	h2 := newHarness(t)
	h2.write(basics+"}\n", 0o600)
	h2.section = "banks"
	if out, _ = h2.run(num(t, "plaid"), "chase", ""); !strings.Contains(out, "A bank you linked already keeps its name. To see those names, run:\n\n      wealthdb-collect plaid banks\n\n") ||
		!strings.Contains(out, "Added Plaid (linked banks): chase.\n") {
		t.Errorf("output:\n%s", out)
	}
}
