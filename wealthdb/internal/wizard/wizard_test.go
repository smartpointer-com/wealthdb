package wizard

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/jsonedit"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/all"
)

// harness is a scratch config folder and data folder, with actions that
// record what the setup asked of the engine. Its config is the default
// one (newHarness), or a setup of its own (newOwnHarness).
type harness struct {
	t                   *testing.T
	cfg, root           string
	loads, resets       []string
	inits               int
	loaded              []string
	terminal            Terminal
	section             string
	wizard, rootFromEnv bool
	inContainer         bool
	// program is Options.Program and collectCmd Options.Collect: empty
	// for the default config.
	program, collectCmd string
	// own is Options.OwnSetup.
	own bool
	// asked are the models a test question went to; reply is the answer,
	// or the failure when replyErr is set.
	asked    []config.ModelConfig
	reply    string
	replyErr error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// The config is the default one, as config.IsDefault reads it.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	return &harness{t: t, cfg: filepath.Join(dir, "config", "wealthdb.cfg"), root: root}
}

// newOwnHarness is a harness whose config is not the default one: a
// setup of its own in a folder with a blank in its name, which is its
// data folder too. WEALTHDB_DATA_ROOT names another folder, which the
// setup must not read.
func newOwnHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "my setup")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("WEALTHDB_DATA_ROOT", filepath.Join(dir, "elsewhere"))
	cfg := filepath.Join(root, "wealthdb.cfg")
	prog := "wealthdb -c '" + cfg + "'"
	return &harness{t: t, cfg: cfg, root: root, own: true, program: prog, collectCmd: prog + " collect"}
}

func (h *harness) write(body string, mode os.FileMode) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.cfg), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.cfg, []byte(body), mode); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) read() string {
	h.t.Helper()
	b, err := os.ReadFile(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

// collect writes an empty silver file under the data folder, as a
// collector's first download does.
func (h *harness) collect(parts ...string) {
	h.t.Helper()
	p := filepath.Join(append([]string{h.root}, parts...)...)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// run runs the setup on the typed lines.
func (h *harness) run(lines ...string) (string, error) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.cfg), 0o700); err != nil {
		h.t.Fatal(err)
	}
	var out bytes.Buffer
	in := strings.Join(lines, "\n")
	if len(lines) > 0 {
		in += "\n"
	}
	err := Run(Options{
		ConfigPath:      h.cfg,
		DataRoot:        h.root,
		DataRootFromEnv: h.rootFromEnv,
		OwnSetup:        h.own,
		Locale:          "de_CH.UTF-8",
		In:              strings.NewReader(in),
		Out:             &out,
		Terminal:        h.terminal,
		Wizard:          h.wizard,
		Section:         h.section,
		InContainer:     h.inContainer,
		Program:         h.program,
		Collect:         h.collectCmd,
		Actions: Actions{
			Init: func() error {
				h.inits++
				return os.WriteFile(filepath.Join(h.root, "wealthdb.db"), nil, 0o600)
			},
			Load:   func(id string) error { h.loads = append(h.loads, id); return nil },
			Reset:  func(id string) error { h.resets = append(h.resets, id); return nil },
			Loaded: func(string) ([]string, error) { return h.loaded, nil },
			TestModel: func(m config.ModelConfig) (string, error) {
				h.asked = append(h.asked, m)
				return h.reply, h.replyErr
			},
		},
	})
	return out.String(), err
}

// cfgOf loads the config the setup wrote, and fails unless it loads.
func (h *harness) cfgOf() *config.Config {
	h.t.Helper()
	st, cfg, err := config.Inspect(h.cfg)
	if st != config.StateValid {
		h.t.Fatalf("config state %d (%v):\n%s", st, err, h.read())
	}
	return cfg
}

// num is the number the Banks and brokers list gives collector name:
// its place in the catalogue, the same for the whole run.
func num(t *testing.T, name string) string {
	t.Helper()
	for i, c := range silver.Catalogue() {
		if c.Name == name {
			return fmt.Sprint(i + 1)
		}
	}
	t.Fatalf("no collector %s", name)
	return ""
}

func TestReadKey(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  []Key
		digit byte
	}{
		{"\x1b[A", []Key{KeyUp}, 0},
		{"\x1b[B", []Key{KeyDown}, 0},
		{"\x1bOA\x1bOB", []Key{KeyUp, KeyDown}, 0},
		{"\x1b[1;5A", []Key{KeyUp}, 0},
		{"\x1b[C", []Key{KeyOther}, 0},
		{"\r", []Key{KeyEnter}, 0},
		{"\n", []Key{KeyEnter}, 0},
		{"7", []Key{KeyDigit}, 7},
		{"q", []Key{KeyQuit}, 0},
		{"Q", []Key{KeyQuit}, 0},
		{"\x03", []Key{KeyInterrupt}, 0},
		{"\x04", []Key{KeyEOF}, 0},
		{"", []Key{KeyEOF}, 0},
		{"\x1bq", []Key{KeyQuit}, 0},
		{"x", []Key{KeyOther}, 0},
	} {
		r := bufio.NewReader(strings.NewReader(tc.in))
		for _, want := range tc.want {
			k, d, err := ReadKey(r)
			if err != nil || k != want || want == KeyDigit && d != tc.digit {
				t.Errorf("ReadKey(%q) = %v, %d, %v; want %v", tc.in, k, d, err, want)
			}
		}
	}
}

func TestGuessCurrency(t *testing.T) {
	for locale, want := range map[string]string{
		"de_CH.UTF-8":         "CHF",
		"en_CH":               "CHF",
		"en_US@rg=chzzzz":     "CHF",
		"de-AT":               "EUR",
		"en_GB.UTF-8@euro":    "GBP",
		"en_US":               "USD",
		"C":                   "USD",
		"":                    "USD",
		"POSIX":               "USD",
		"en_ZZ":               "USD",
		"fr_FR.ISO8859-1":     "EUR",
		"ja_JP.UTF-8":         "JPY",
		"en_US.UTF-8@rg=gbzz": "GBP",
	} {
		if got := guessCurrency(locale); got != want {
			t.Errorf("guessCurrency(%q) = %s, want %s", locale, got, want)
		}
	}
}

// TestSectionsCoverEveryKey holds the menu to the config: every member
// the config may hold is in a section, and a section names only members
// the config has.
func TestSectionsCoverEveryKey(t *testing.T) {
	heads := map[string]bool{}
	for _, s := range sections() {
		for _, k := range s.keys {
			head, _, _ := strings.Cut(k, ".")
			heads[head] = true
		}
	}
	want := config.TopLevelKeys()
	if got, w := slices.Sorted(maps.Keys(heads)), slices.Sorted(slices.Values(want)); !slices.Equal(got, w) {
		t.Errorf("sections cover %v\nthe config holds %v", got, w)
	}
	// Every top-level member is in one section, but a member inside one
	// may be edited in a section of its own (the AI model).
	var top []string
	for _, s := range sections() {
		for _, k := range s.keys {
			if !strings.Contains(k, ".") {
				top = append(top, k)
			}
		}
	}
	if len(top) != len(slices.Compact(slices.Sorted(slices.Values(top)))) {
		t.Errorf("a member is in two sections: %v", top)
	}
	// A section answers to its name, its first word, and its short name.
	for name, title := range map[string]string{"currency": "Primary currency", "primary-currency": "Primary currency",
		"model": "AI model", "ai-model": "AI model", "banks": "Banks and brokers", "settings": "Settings edited in the file",
		"file": "Settings edited in the file", "dashboards": "Dashboards", "mcp-server": "MCP server"} {
		if sec, ok := sectionNamed(name); !ok || sec.title != title {
			t.Errorf("section %q is %q, want %q", name, sec.title, title)
		}
	}
	for _, name := range Sections() {
		if _, ok := sectionNamed(name); !ok {
			t.Errorf("section %q is not found by its own name", name)
		}
	}
	if _, ok := sectionNamed("validate"); ok {
		t.Error("validate is the subcommand, not a section")
	}
}

func TestWizardFromNothing(t *testing.T) {
	h := newHarness(t)
	out, err := h.run(
		"chf", // currency, any case
		num(t, "viac"),
		"", // done with the banks
		"", // dashboards: keep off
		"", // MCP server: keep off
		"", // run init
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.cfgOf()
	if cfg.DefaultCurrency != "CHF" {
		t.Errorf("currency = %s", cfg.DefaultCurrency)
	}
	if cfg.GoldDB != filepath.Join(h.root, "wealthdb.db") {
		t.Errorf("gold_db = %s", cfg.GoldDB)
	}
	if len(cfg.SilverSources) != 1 || cfg.SilverSources[0].ID != "viac" ||
		cfg.SilverSources[0].Path != filepath.Join(h.root, "viac", "viac.db") {
		t.Errorf("sources = %+v", cfg.SilverSources)
	}
	// Off is an answer, written so a re-run knows the step is done.
	if cfg.Web == nil || cfg.Web.Enabled || cfg.MCP == nil || cfg.MCP.Enabled {
		t.Errorf("off is not written as off: web %+v, mcp %+v", cfg.Web, cfg.MCP)
	}
	if h.inits != 1 {
		t.Errorf("init ran %d times", h.inits)
	}
	for _, want := range []string{
		"Data folder:  " + h.root,
		"Currency [CHF]: ", // guessed from the locale
		"Added VIAC.",
		// The command of a pick stands on a line of its own, set apart.
		"\n\nTo collect the data of VIAC, run this command when wealthdb config is done:\n\n      wealthdb-collect viac login\n\n" +
			"wealthdb config lists it again at the end.\n",
		// A blank line stands before every question, and a title begins a
		// step, with what it asks below it.
		"\n\nStep 1 of 4: Primary currency\nThe currency the reports show amounts in, such as CHF, EUR or USD.\n\nCurrency [CHF]: ",
		"The database does not exist yet.\n\nCreate it now? [Y/n]: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The run ends with the to-do block, and nothing after it.
	rule := strings.Repeat("-", 72)
	block := "\n" + rule + "\nWhat to do now\n\nRun these commands in the terminal, in this order:\n" +
		"\n  1.  wealthdb-collect viac login\n\n" +
		"      Starts collecting the data of VIAC.\n" +
		"      It lists the sign-in details to save first, and the commands\n" +
		"      that follow it.\n" +
		"\n  2.  wealthdb load -a\n\n" +
		"      Brings the database of every source into the one database that\n" +
		"      the reports read. Run it when the steps above are done.\n" + rule + "\n"
	if !strings.HasSuffix(out, block) {
		t.Errorf("the run does not end with the to-do block:\n%s", out)
	}
	// After a pick the catalogue is not listed again, so the command
	// stays on the screen above the prompt.
	if n := strings.Count(out, "Add one:"); n != 1 {
		t.Errorf("the catalogue is listed %d times, want once:\n%s", n, out)
	}
	after := out[strings.Index(out, "Added VIAC."):]
	if !strings.Contains(after[:strings.Index(after, "Step 3 of 4")],
		"Type a number to add another source, ? to see the list again,\na for the one listed, or press Enter when done: ") {
		t.Errorf("the prompt after a pick:\n%s", after)
	}
	if fi, err := os.Stat(h.cfg); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("a new config is mode %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	if _, err := os.Stat(h.cfg + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a new config got a backup: %v", err)
	}
}

// TestWizardStopsAndGoesOn stops the wizard after each step, by the end
// of the input as Ctrl+D gives it, and checks the file holds the answers
// so far, loads or is merely unfinished, and that the next run goes on
// with the wizard, every step pre-filled, until all four steps hold an
// answer.
func TestWizardStopsAndGoesOn(t *testing.T) {
	answers := []string{"eur", num(t, "swissquote"), "", "on", "off", "q"}
	for stop := 0; stop <= len(answers); stop++ {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			h := newHarness(t)
			out, err := h.run(answers[:stop]...)
			if err != nil {
				t.Fatal(err)
			}
			if stop < len(answers)-1 && !strings.Contains(out, "Stopped. Every answer so far is saved.") {
				t.Errorf("no stop line:\n%s", out)
			}
			st, _, err := config.Inspect(h.cfg)
			if st == config.StateRefused {
				t.Fatalf("the stopped config does not load: %v", err)
			}
			if stop == 0 {
				if st != config.StateAbsent {
					t.Errorf("nothing answered, yet the file holds %q", h.read())
				}
				return
			}
			check := func(cfg *config.Config) {
				t.Helper()
				if cfg.DefaultCurrency != "EUR" {
					t.Errorf("currency = %s", cfg.DefaultCurrency)
				}
				if stop >= 2 && (len(cfg.SilverSources) != 1 || cfg.SilverSources[0].Kind != "swissquote") {
					t.Errorf("sources = %+v", cfg.SilverSources)
				}
				if stop >= 4 && (cfg.Web == nil || !cfg.Web.Enabled) {
					t.Errorf("web = %+v", cfg.Web)
				}
				if stop >= 5 && (cfg.MCP == nil || cfg.MCP.Enabled) {
					t.Errorf("mcp = %+v", cfg.MCP)
				}
			}
			check(h.cfgOf())
			// The next run: Enter alone at every prompt keeps every
			// saved answer.
			out, err = h.run("", "", "", "", "", "q")
			if err != nil {
				t.Fatal(err)
			}
			check(h.cfgOf())
			if stop >= 5 {
				if !strings.Contains(out, "Walk through the basic setup again") || strings.Contains(out, "Going on with") {
					t.Errorf("an answered setup does not open the menu:\n%s", out)
				}
				return
			}
			for _, want := range []string{"Going on with the setup. Each step shows the saved answer; Enter keeps it.",
				"Currency [EUR]: "} {
				if !strings.Contains(out, want) {
					t.Errorf("the re-run lacks %q:\n%s", want, out)
				}
			}
			if stop < 2 && !strings.Contains(out, "Pick at least one source, or type q to stop.") {
				t.Errorf("Enter with no source went on:\n%s", out)
			}
			if stop >= 2 && !strings.Contains(out, "a  Swissquote") {
				t.Errorf("the source is not listed:\n%s", out)
			}
			if stop >= 4 && !strings.Contains(out, "Dashboards (on or off) [on]: ") {
				t.Errorf("dashboards are not pre-filled:\n%s", out)
			}
		})
	}
}

func TestCurrencySection(t *testing.T) {
	h := newHarness(t)
	h.section = "currency"
	out, err := h.run("eu", "usd")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "A currency is three letters") {
		t.Errorf("a bad answer is not refused:\n%s", out)
	}
	if !strings.Contains(h.read(), `"default_currency": "USD"`) {
		t.Errorf("file = %s", h.read())
	}
	// gold_db comes first, through the variable when it names the folder.
	h2 := newHarness(t)
	h2.section, h2.rootFromEnv = "currency", true
	t.Setenv("WEALTHDB_DATA_ROOT", h2.root)
	if _, err := h2.run("gbp"); err != nil {
		t.Fatal(err)
	}
	want := "{\n    \"gold_db\": \"${WEALTHDB_DATA_ROOT}/wealthdb.db\",\n    \"default_currency\": \"GBP\"\n}\n"
	if got := h2.read(); got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

func TestBanksPairsUBS(t *testing.T) {
	h := newHarness(t)
	h.write(`{"gold_db": "/x/g.db", "default_currency": "CHF", "silver_sources": []}`, 0o600)
	h.section = "banks"
	out, err := h.run(num(t, "ubs-web"), num(t, "ubs-psn"), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.cfgOf()
	if len(cfg.SilverSources) != 1 {
		t.Fatalf("sources = %+v", cfg.SilverSources)
	}
	s := cfg.SilverSources[0]
	if s.ID != "ubs" || s.Kind != "ubs" || len(s.Subsources) != 2 ||
		s.Subsources[0].Kind != "ubs-web" || s.Subsources[1].Kind != "ubs-psn" ||
		s.Subsources[1].Path != filepath.Join(h.root, "ubs-psn", "ubs-psn.db") {
		t.Errorf("entry = %+v", s)
	}
	if strings.Count(out, `add "relationships"`) != 1 {
		t.Errorf("the pairing line is not said once, after the pair:\n%s", out)
	}
	if !strings.Contains(out, "UBS e-banking + UBS PSN (SFTP files)") {
		t.Errorf("the pair is not listed as one:\n%s", out)
	}
}

func TestBanksIDsAndItems(t *testing.T) {
	h := newHarness(t)
	h.write(`{"gold_db": "/x/g.db", "default_currency": "USD",
 "silver_sources": [{"id": "plaid", "kind": "chase", "path": "/x/chase.db"}]}`, 0o600)
	h.section = "banks"
	h.collect("svb", "svb-mortgage.db")
	out, err := h.run(
		"n",                 // the collected mortgage file is not added on the way in
		num(t, "plaid"), "", // Enter backs out, with nothing added
		num(t, "plaid"), "plaid", // a login named plaid: the id clashes
		num(t, "plaid"), "my bank", "broker", // a name with a space is refused
		num(t, "plaid"), "broker", // the same login twice
		num(t, "svb"),                  // the brokerage file, and the mortgage file that is there
		num(t, "viac"), num(t, "viac"), // twice: the second is not offered
		num(t, "svb"),
		"99", "?", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.cfgOf()
	var got []string
	for _, s := range cfg.SilverSources {
		got = append(got, s.ID+"="+strings.TrimPrefix(s.Path, h.root))
	}
	want := []string{"plaid=/x/chase.db", "plaid-2=/plaid/plaid/plaid.db", "broker=/plaid/broker/broker.db",
		"svb=/svb/svb.db", "svb-mortgage=/svb/svb-mortgage.db", "viac=/viac/viac.db"}
	if !slices.Equal(got, want) {
		t.Errorf("sources = %v\nwant      %v", got, want)
	}
	// A per-item collector's command names the login it starts with.
	if !strings.Contains(out, "\n      wealthdb-collect plaid link --item broker\n") {
		t.Errorf("the plaid command does not name the login:\n%s", out)
	}
	for _, w := range []string{"Use letters, digits, - and _ only.", "Plaid (linked banks) is in the list already.",
		"\nPlaid (linked banks)\nIt links one bank at a time. Choose a short name for the bank you want\nto add",
		"After this setup, you link the bank\nwith that name:\n\n      wealthdb-collect plaid link --item <name>\n\n",
		"\n\nName for the bank (Enter to go back): ",
		"Type one of the numbers or letters on the list.", "Chase (plaid)", "Plaid (linked banks) (broker)",
		"SVB statement archive (svb-mortgage)"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	// A number stays the bank's for the whole run, and a configured bank
	// leaves "Add one"; a per-item one stays.
	lists := strings.Split(out, "Add one:")
	last := lists[len(lists)-1]
	for _, name := range []string{"viac", "svb"} {
		c, _ := silver.CollectorNamed(name)
		if strings.Contains(last, c.Display) {
			t.Errorf("%s is offered again:\n%s", name, last)
		}
	}
	for _, name := range []string{"plaid", "swissquote"} {
		c, _ := silver.CollectorNamed(name)
		if !strings.Contains(last, fmt.Sprintf("%3s  %-34s", num(t, name), c.Display)) {
			t.Errorf("%s does not keep its number %s:\n%s", name, num(t, name), last)
		}
	}
}

// A pick of a per-item collector adds the logins whose data is collected
// already, all at once, and offers to drop an entry that no login took.
func TestPlaidPickAddsCollectedLogins(t *testing.T) {
	h := newHarness(t)
	h.write(fmt.Sprintf(`{"gold_db": "/x/g.db", "default_currency": "USD",
 "silver_sources": [{"id": "plaid", "kind": "plaid", "path": %q}]}`, filepath.Join(h.root, "plaid", "plaid", "plaid.db")), 0o600)
	h.section = "banks"
	h.collect("plaid", "chase", "chase.db")
	h.collect("plaid", "schwab", "schwab.db")
	out, err := h.run("n", num(t, "plaid"), "", "", "", "") // not on the way in; add them, remove the empty entry, no more, done
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range h.cfgOf().SilverSources {
		got = append(got, s.ID+"="+strings.TrimPrefix(s.Path, h.root))
	}
	if want := []string{"chase=/plaid/chase/chase.db", "schwab=/plaid/schwab/schwab.db"}; !slices.Equal(got, want) {
		t.Errorf("sources = %v, want %v\n%s", got, want, out)
	}
	for _, w := range []string{"Data is collected already for: chase, schwab.", "Added Plaid (linked banks): chase, schwab.",
		"The entry plaid has no data: no bank was linked under the name plaid.", "Removed plaid."} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "Name for the bank") {
		t.Errorf("a name is asked although no further bank is wanted:\n%s", out)
	}
	// Unlisted names what a load would pass over: nothing now, both
	// before the pick, and nothing for a collector the config never names.
	feeds := func(c *config.Config) [][]config.Feed {
		var out [][]config.Feed
		for i := range c.SilverSources {
			out = append(out, c.SilverSources[i].Feeds())
		}
		return out
	}
	if got := Unlisted(h.root, feeds(h.cfgOf())); len(got) != 0 {
		t.Errorf("unlisted after the pick: %v", got)
	}
	before := [][]config.Feed{{{Path: filepath.Join(h.root, "plaid", "plaid", "plaid.db"), Collector: "plaid", Item: "plaid"}}}
	if got, want := Unlisted(h.root, before), []string{"Plaid (linked banks): chase", "Plaid (linked banks): schwab"}; !slices.Equal(got, want) {
		t.Errorf("unlisted before the pick = %v, want %v", got, want)
	}
	if got := Unlisted(h.root, [][]config.Feed{{{Path: "/x/viac.db", Collector: "viac"}}}); len(got) != 0 {
		t.Errorf("a collector the config does not name is listed: %v", got)
	}
}

func TestBanksStatesFoundLoadRemove(t *testing.T) {
	h := newHarness(t)
	gold := filepath.Join(h.root, "wealthdb.db")
	h.write(fmt.Sprintf(`{"gold_db": %q, "default_currency": "CHF",
 "silver_sources": [
  {"id": "viac", "kind": "viac", "path": %q},
  {"id": "sq", "kind": "swissquote", "path": %q},
  {"id": "rel", "kind": "relevate", "path": %q}
 ],
 "account_overrides": {"rel": {"A1": {"nickname": "Vested"}}}
}`, gold, filepath.Join(h.root, "viac", "viac.db"), filepath.Join(h.root, "swissquote", "swissquote.db"),
		filepath.Join(h.root, "relevate", "relevate.db")), 0o640)
	h.collect("wealthdb.db")
	h.collect("swissquote", "swissquote.db")
	h.collect("relevate", "relevate.db")
	h.collect("chase", "chase.db")
	h.loaded = []string{"rel"}
	h.section = "banks"
	prior := h.read()
	out, err := h.run(
		"n",      // chase is not added on the way in
		"b", "l", // load sq
		"d", "", // register chase, found in the data folder; load it at once
		"a", "r", "y", // remove viac
		"b", "r", "y", // remove rel, now b: the file names it elsewhere
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"VIAC                               waiting for its first download",
		"Swissquote (sq)                    collected, not loaded",
		"Relevate (Pensexpert) (rel)        loaded",
		"Collected, but not in the list:\n    d  Chase",
		"Added Chase.\nIts data is collected already.",
		"Removed viac.",
		"Cannot remove rel: the file names it elsewhere.",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if !slices.Equal(h.loads, []string{"sq", "chase"}) || len(h.resets) != 0 {
		t.Errorf("loads %v, resets %v", h.loads, h.resets)
	}
	var ids []string
	for _, s := range h.cfgOf().SilverSources {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []string{"sq", "rel", "chase"}) {
		t.Errorf("ids = %v", ids)
	}
	if fi, _ := os.Stat(h.cfg); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want the file's own 0640", fi.Mode().Perm())
	}
	// Three saves in the run; the backup is the file before the first.
	if bak, err := os.ReadFile(h.cfg + ".bak"); err != nil || string(bak) != prior {
		t.Errorf("the backup does not hold the file as the run found it: %v\n%s", err, bak)
	}
}

func TestRemoveResetsLoadedSource(t *testing.T) {
	h := newHarness(t)
	h.write(fmt.Sprintf(`{"gold_db": %q, "default_currency": "CHF",
 "silver_sources": [{"id": "viac", "kind": "viac", "path": "/x/viac.db"}]}`, filepath.Join(h.root, "wealthdb.db")), 0o600)
	h.collect("wealthdb.db")
	h.loaded = []string{"viac"}
	h.section = "banks"
	if _, err := h.run("a", "r", "n", "a", "r", "y", ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.resets, []string{"viac"}) {
		t.Errorf("resets = %v", h.resets)
	}
	if n := len(h.cfgOf().SilverSources); n != 0 {
		t.Errorf("%d sources left", n)
	}
}

func TestFXHint(t *testing.T) {
	h := newHarness(t)
	h.write(`{"gold_db": "/x/g.db", "default_currency": "USD",
 "silver_sources": [{"id": "viac", "kind": "viac", "path": "/x/viac.db"}]}`, 0o600)
	h.section = "banks"
	out, err := h.run(num(t, "fred"), "")
	if err != nil {
		t.Fatal(err)
	}
	hint := "The reports are in USD and VIAC reports in CHF. Add FRED exchange and interest\nrates so the reports can convert."
	if strings.Count(out, hint) != 1 {
		t.Errorf("the hint is not shown once, before fred is added:\n%s", out)
	}
}

func TestServices(t *testing.T) {
	h := newHarness(t)
	h.write(`{"gold_db": "/x/g.db", "default_currency": "USD",
  "mcp": {"auth": "none", "insecure": true}}`, 0o600)
	h.section = "mcp-server"
	out, err := h.run("on", "3301")
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.cfgOf()
	if !cfg.MCP.Enabled || cfg.MCP.Port != 3301 || cfg.MCP.Auth != "none" || !cfg.MCP.Insecure {
		t.Errorf("mcp = %+v", cfg.MCP)
	}
	if !strings.Contains(out, "\n\nTo start the MCP server, run this command when wealthdb config is done:\n\n      wealthdb mcp start\n\n") {
		t.Errorf("no next step:\n%s", out)
	}
	// A server turned on joins the to-do block the run ends with.
	// The setup is not finished here, so it comes first; then the
	// server this run turned on.
	if !strings.Contains(out, "\n  1.  wealthdb config\n\n      Goes on with the setup: a step has no answer yet.\n") ||
		!strings.HasSuffix(out, "\n  2.  wealthdb mcp start\n\n      Starts the MCP server.\n"+strings.Repeat("-", 72)+"\n") {
		t.Errorf("the to-do block lacks the start:\n%s", out)
	}
	if strings.Contains(out, "Stopped.") {
		t.Errorf("a section that ends is no stop:\n%s", out)
	}
	// The default port is not written; a port the engine refuses is not
	// saved.
	h.section = "dashboards"
	out, err = h.run("on", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg := h.cfgOf(); cfg.Web == nil || !cfg.Web.Enabled || cfg.Web.Port != 0 {
		t.Errorf("web = %+v", cfg.Web)
	}
	before := h.read()
	out, err = h.run("on", "3301")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Not saved: config: mcp.port 3301 is also the web server's port") || h.read() != before {
		t.Errorf("a clash was saved:\n%s\n%s", out, h.read())
	}
	h.section = "dashboards"
	if _, err := h.run("off"); err != nil {
		t.Fatal(err)
	}
	if cfg := h.cfgOf(); cfg.Web.Enabled {
		t.Errorf("web = %+v", cfg.Web)
	}
}

func TestFileSettingsAndDataSections(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "config", "testdata", "every_key.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.write(string(src), 0o600)
	h.section = "settings"
	out, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"\n Features\n   Benchmark portfolios       1 entry\n" +
			"       Fund mixes to compare the returns with, beside the built-in ones.\n" +
			"       benchmarks · wealthdb help benchmarks\n",
		// The lot method is a report option: the block holds nothing else.
		"   Lots per account           not set\n",
		"\n Overrides\n   Accounts and portfolios    ",
		"       inception_overrides, returns_exclude, returns_policy_overrides,\n" +
			"       returns_hide, returns_transfer_matching · docs/DESIGN.md §5.4 to §5.8\n",
		"These settings are edited in the file:\n    " + h.cfg + "\n",
		"The docs: " + docsURL,
		"After an edit, check the file with this command:\n\n      wealthdb config validate\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	h.section = "data-folder"
	out, err = h.run()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "The file names another place for the database: /tmp/wealthdb-example/wealthdb.db") {
		t.Errorf("output:\n%s", out)
	}
	if h.read() != string(src) {
		t.Error("showing a section changed the file")
	}
}

// TestMenuChangesNothingUnasked runs the menu over the demo household's
// config and the config that sets every key: every section opened with
// Enter alone, and the file stays byte for byte, its time too.
func TestMenuChangesNothingUnasked(t *testing.T) {
	for _, src := range []string{
		filepath.Join("..", "jsonedit", "testdata", "demo.cfg"),
		filepath.Join("..", "config", "testdata", "every_key.cfg"),
	} {
		t.Run(filepath.Base(src), func(t *testing.T) {
			text, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			h := newHarness(t)
			h.write(string(text), 0o600)
			// The demo's relative paths resolve beside the config.
			h.collect("..", "config", "silver", "fx.db")
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(h.cfg, old, old); err != nil {
				t.Fatal(err)
			}
			// Every item, by its number: the walk stands sixth, at the end
			// of the basic group, and is left out here; the check of the
			// file closes the menu.
			n := len(sections()) + 2
			var lines []string
			for i := 1; i <= n; i++ {
				if i == 6 {
					continue
				}
				lines = append(lines, fmt.Sprint(i))
				switch i {
				case 1:
					lines = append(lines, "") // currency: keep
				case 2:
					lines = append(lines, "") // banks: done
				case 3, 4:
					lines = append(lines, "", "") // on or off, the port: keep
				case 7, 8, 9:
					lines = append(lines, "") // the AI model, the options, the ledgers: back
				}
			}
			out, err := h.run(append(lines, "q")...)
			if err != nil {
				t.Fatal(err)
			}
			if h.read() != string(text) {
				t.Errorf("the file changed:\n%s", h.read())
			}
			if fi, _ := os.Stat(h.cfg); !fi.ModTime().Equal(old) {
				t.Error("the file was rewritten")
			}
			if strings.Contains(out, "Stopped") {
				t.Errorf("q at the menu is no stop:\n%s", out)
			}
			if !strings.Contains(out, "Walk through the basic setup again") {
				t.Errorf("no menu:\n%s", out)
			}
		})
	}
}

func TestMenuSummaries(t *testing.T) {
	src, _ := os.ReadFile(filepath.Join("..", "config", "testdata", "every_key.cfg"))
	h := newHarness(t)
	h.write(string(src), 0o600)
	out, err := h.run("q")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"Primary currency             CHF",
		"Banks and brokers            UBS e-banking + UBS PSN (SFTP files), 3 more",
		"Dashboards                   on, http://127.0.0.1:3000",
		"MCP server                   on, port 3300",
		"AI model                     example-model at 127.0.0.1:1",
		"Report options               7 of 8 set",
		"Ledger files                 1 of 4 set",
		"Settings edited in the file  9 of 11 set",
		// The groups, each a heading above its items; the walk closes the
		// basic one, and the check of the file the last.
		"\n Basic\n     1  Primary currency ",
		"     6  Walk through the basic setup again\n\n Advanced\n     7  AI model ",
		"     9  Ledger files ",
		"\n\n Config file\n    10  Settings edited in the file ",
		"    11  Validate the config file\n\n     q  Quit",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("menu lacks %q:\n%s", w, out)
		}
	}
}

// fakeTerminal counts the switches to raw mode and back.
type fakeTerminal struct{ raw, restored, rows int }

func (f *fakeTerminal) Rows() int { return f.rows }

func (f *fakeTerminal) Raw() (func(), error) {
	f.raw++
	return func() { f.restored++ }, nil
}

func TestRawMenu(t *testing.T) {
	h := newHarness(t)
	h.write(`{"gold_db": "/x/g.db", "default_currency": "USD",
 "silver_sources": [{"id": "viac", "kind": "viac", "path": "/x/viac.db"}],
 "web": {"enabled": false}, "mcp": {"enabled": false}}`, 0o600)
	term := &fakeTerminal{}
	h.terminal = term
	var out bytes.Buffer
	// Down, Up, Down twice to Dashboards... then 1 and Enter: Currency.
	// A change the menu shows needs no Enter to go back.
	keys := "\x1b[B\x1b[A\x1b[B\x1b[B\x1b[A\x1b[A1\r" + "eur\n" + "\x03"
	err := Run(Options{ConfigPath: h.cfg, DataRoot: h.root, In: strings.NewReader(keys), Out: &out, Terminal: term})
	if err != nil {
		t.Fatal(err)
	}
	if h.cfgOf().DefaultCurrency != "EUR" {
		t.Errorf("currency not saved:\n%s", h.read())
	}
	if term.raw != 2 || term.restored != 2 {
		t.Errorf("raw %d times, restored %d", term.raw, term.restored)
	}
	if !strings.Contains(out.String(), "\r\n Basic\r\n >   1  Primary currency") || !strings.Contains(out.String(), "Up/Down") {
		t.Errorf("no raw menu with the cursor:\n%q", out.String())
	}
	if strings.Contains(out.String(), "Press Enter to go back to the menu") {
		t.Errorf("a saved change waits for Enter:\n%q", out.String())
	}
}

// The menu clears the screen when it comes back. A section that ended
// at its own question goes back at once; one that printed more than a
// confirmation after the last answer waits for Enter, so it can be read.
func TestRawMenuWaitsOnlyForWhatWouldBeLost(t *testing.T) {
	h := newHarness(t)
	h.write(`{"gold_db": "/x/g.db", "default_currency": "USD",
 "silver_sources": [{"id": "viac", "kind": "viac", "path": "/x/viac.db"}],
 "web": {"enabled": false}, "mcp": {"enabled": false}}`, 0o600)
	term := &fakeTerminal{}
	h.terminal = term
	n := func(title string) string {
		w := &setup{opts: Options{ConfigPath: h.cfg, DataRoot: h.root}, out: newScreen(io.Discard, Palette{})}
		w.store = openTestStore(t, h.cfg, "")
		for i, it := range w.menuItems() {
			if it.title == title {
				return fmt.Sprint(i + 1)
			}
		}
		t.Fatalf("no item %q", title)
		return ""
	}
	pause := "Press Enter to go back to the menu"
	for _, c := range []struct {
		name, keys string
		waits      bool
	}{
		{"a section that ends at its question", n("Report options") + "\r" + "\n", false},
		{"a change the menu shows", n("Primary currency") + "\r" + "chf\n", false},
		{"what a check prints", n("Validate the config file") + "\r", true},
		{"a screen to read", n("Settings edited in the file") + "\r", true},
		{"a change with more to say", n("Dashboards") + "\r" + "on\n\n", true},
		// The walk ends with the creation of the database, whose command
		// prints past the screen: its line stays to be read.
		{"what a command the setup ran prints", n("Walk through the basic setup again") + "\r" + "\n\n\n\n\n", true},
	} {
		var out bytes.Buffer
		inits := 0
		if err := Run(Options{ConfigPath: h.cfg, DataRoot: h.root, In: strings.NewReader(c.keys + "\x03"), Out: &out,
			Terminal: term, Actions: Actions{Init: func() error { inits++; return nil }}}); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out.String(), pause); got != c.waits {
			t.Errorf("%s: waits %v, want %v:\n%s", c.name, got, c.waits, out.String())
		}
		if walk := strings.HasPrefix(c.name, "what a command"); walk != (inits == 1) {
			t.Errorf("%s: the database was created %d times", c.name, inits)
		}
	}
}

// In a window the menu does not fit, the lines around the marked one
// show, with a note for what stands above or below, and the menu is
// never taller than the window.
func TestRawMenuFitsTheWindow(t *testing.T) {
	h := newHarness(t)
	h.write(`{"gold_db": "/x/g.db", "default_currency": "USD",
 "silver_sources": [{"id": "viac", "kind": "viac", "path": "/x/viac.db"}],
 "web": {"enabled": false}, "mcp": {"enabled": false}}`, 0o600)
	w := &setup{opts: Options{ConfigPath: h.cfg, DataRoot: h.root}, out: newScreen(io.Discard, Palette{})}
	w.store = openTestStore(t, h.cfg, "")
	items := w.menuItems()
	full := w.renderMenu(items, 0, true, 0)
	tall := strings.Count(full, "\n")
	if strings.Contains(full, "... more") {
		t.Errorf("an unbounded window clips:\n%s", full)
	}
	if got := w.renderMenu(items, 0, true, tall+1); got != full {
		t.Errorf("a window that fits clips:\n%s", got)
	}
	const rows = 16
	for cursor := range items {
		got := w.renderMenu(items, cursor, true, rows)
		if n := strings.Count(got, "\n"); n > rows-1 {
			t.Errorf("cursor %d: %d lines in a window of %d:\n%s", cursor, n, rows, got)
		}
		if !strings.Contains(got, fmt.Sprintf(" > %3d  %s", cursor+1, items[cursor].title)) {
			t.Errorf("cursor %d: the marked line is not shown:\n%s", cursor, got)
		}
		for _, line := range strings.Split(got, "\n") {
			if len(line) > 80 {
				t.Errorf("cursor %d: a line of %d columns: %q", cursor, len(line), line)
			}
		}
		if !strings.Contains(got, "wealthdb config") || !strings.Contains(got, "Up/Down") {
			t.Errorf("cursor %d: the head or the foot is gone:\n%s", cursor, got)
		}
	}
	top, bottom := w.renderMenu(items, 0, true, rows), w.renderMenu(items, len(items)-1, true, rows)
	if strings.Contains(top, "more above") || !strings.Contains(top, "... more below") {
		t.Errorf("at the top:\n%s", top)
	}
	if !strings.Contains(bottom, "... more above") || strings.Contains(bottom, "more below") || !strings.Contains(bottom, "q  Quit") {
		t.Errorf("at the bottom:\n%s", bottom)
	}
}

// A config path longer than the first line has room for keeps its end
// and stays within the screen: a temporary folder on a Mac is that long.
func TestTheMenusFirstLineFitsALongConfigPath(t *testing.T) {
	h := newHarness(t)
	h.write(`{"gold_db": "/x/g.db", "default_currency": "USD"}`, 0o600)
	w := &setup{opts: Options{ConfigPath: h.cfg, DataRoot: h.root}, out: newScreen(io.Discard, Palette{})}
	w.store = openTestStore(t, h.cfg, "")
	w.store.path = "/" + strings.Repeat("a-long-folder-name/", 8) + "wealthdb.cfg"
	first, _, _ := strings.Cut(w.renderMenu(w.menuItems(), 0, true, 0), "\n")
	if n := utf8.RuneCountInString(first); n > width {
		t.Errorf("the first line has %d columns, want %d at most: %q", n, width, first)
	}
	if !strings.HasPrefix(first, "wealthdb config  …") || !strings.HasSuffix(first, w.store.path[len(w.store.path)-40:]) {
		t.Errorf("the first line does not end with the path's end after an ellipsis: %q", first)
	}
	w.store.path = "/x/wealthdb.cfg"
	first, _, _ = strings.Cut(w.renderMenu(w.menuItems(), 0, true, 0), "\n")
	if want := "wealthdb config" + strings.Repeat(" ", 50-len(w.store.path)) + w.store.path; first != want {
		t.Errorf("a short path: %q, want %q", first, want)
	}
}

func TestRefusedConfigIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	body := `{"gold_db": "/x/g.db", "default_currency": "USD", "colour": "blue"}`
	h.write(body, 0o600)
	_, err := h.run("eur")
	if err == nil || !strings.Contains(err.Error(), `unknown field "colour"`) {
		t.Errorf("err = %v", err)
	}
	if h.read() != body {
		t.Error("a refused config was written")
	}
}

func TestNextSteps(t *testing.T) {
	dir := t.TempDir()
	have := filepath.Join(dir, "have.db")
	if err := os.WriteFile(have, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := NextSteps(Palette{}, "", "wealthdb load -a", [][]config.Feed{
		{{Path: have, Collector: "viac"}},
		{{Path: filepath.Join(dir, "a.db"), Collector: "ubs-web"}, {Path: filepath.Join(dir, "b.db"), Collector: "ubs-psn"}},
		{{Path: filepath.Join(dir, "c.db"), Collector: "ubs-web"}},
	})
	rule := strings.Repeat("-", 72)
	want := "\n" + rule + "\nWhat to do now\n\nRun these commands in the terminal, in this order:\n" +
		"\n  1.  wealthdb-collect ubs-web login\n\n" +
		"      Starts collecting the data of UBS e-banking.\n" +
		"      It lists the sign-in details to save first, and the commands\n      that follow it.\n" +
		"\n  2.  wealthdb-collect ubs-psn login\n\n" +
		"      Starts collecting the data of UBS PSN (SFTP files).\n" +
		"      It lists the sign-in details to save first, and the commands\n      that follow it.\n" +
		"\n  3.  wealthdb load -a\n\n" +
		"      Brings the database of every source into the one database that\n      the reports read. Run it when the steps above are done.\n" + rule + "\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	one := "\n" + rule + "\nWhat to do now\n\nRun this command in the terminal:\n" +
		"\n  1.  wealthdb load -a\n\n      Brings the database of every source into the one database that\n      the reports read.\n" + rule + "\n"
	if got := NextSteps(Palette{}, "", "wealthdb load -a", [][]config.Feed{{{Path: have, Collector: "viac"}}}); got != one {
		t.Errorf("got\n%s\nwant\n%s", got, one)
	}
	// For a config that is not the default one, each collect command
	// starts as the caller states it, a per-item one too.
	const prog = "wealthdb -c '/x/my setup/wealthdb.cfg'"
	got = NextSteps(Palette{}, prog+" collect", prog+" load -a", [][]config.Feed{
		{{Path: filepath.Join(dir, "a.db"), Collector: "ubs-web"}},
		{{Path: filepath.Join(dir, "first", "first.db"), Collector: "plaid", Item: "first"}},
	})
	for _, w := range []string{"\n  1.  " + prog + " collect ubs-web login\n", "\n  2.  " + prog + " collect plaid link --item first\n",
		"\n  3.  " + prog + " load -a\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("the block lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "wealthdb-collect") {
		t.Errorf("a collect command names no config:\n%s", got)
	}
	if got := ToDo(Palette{}, nil); got != "" {
		t.Errorf("no step gives a block: %q", got)
	}
}

// A run that stops before every step holds an answer names the setup
// command, on a line of its own; one that stops later ends with the
// to-do block. Leaving the menu ends with the block too.
func TestTheRunEndsWithWhatToDo(t *testing.T) {
	h := newHarness(t)
	out, err := h.run("chf", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "\nStopped. Every answer so far is saved.\n\n"+
		"To go on with the setup, run this command:\n\n    wealthdb config\n\n") {
		t.Errorf("a stop part-way:\n%s", out)
	}

	h = newHarness(t)
	h.write(`{"gold_db": "/x/g.db", "default_currency": "USD", "web": {"enabled": false}, "mcp": {"enabled": false},
 "silver_sources": [{"id": "viac", "kind": "viac", "path": "/x/viac/viac.db"}]}`, 0o600)
	out, err = h.run("q") // the menu
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Stopped.") {
		t.Errorf("leaving the menu is no stop:\n%s", out)
	}
	if !strings.Contains(out, "What to do now") || !strings.Contains(out, "\n  1.  wealthdb init\n\n      Creates the database.\n") ||
		!strings.Contains(out, "\n  2.  wealthdb-collect viac login\n") || !strings.Contains(out, "\n  3.  wealthdb load -a\n") ||
		!strings.HasSuffix(out, strings.Repeat("-", 72)+"\n") {
		t.Errorf("leaving the menu does not end with the to-do block:\n%s", out)
	}
}

// openTestStore opens the store of the config at path.
func openTestStore(t *testing.T, path, gold string) *store {
	t.Helper()
	abs, target, data, err := configFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openStore(abs, target, data, gold)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreWritesAtomically(t *testing.T) {
	h := newHarness(t)
	prior := "{\n  \"default_currency\": \"USD\"\n}\n"
	h.write(prior, 0o644)
	s := openTestStore(t, h.cfg, "/x/g.db")
	var bakTime time.Time
	for i, cur := range []string{"EUR", "CHF", "GBP", "JPY"} {
		if err := s.apply(func(d *jsonedit.Doc) error { return d.Set(cur, "default_currency") }); err != nil {
			t.Fatal(err)
		}
		// After every save, the backup is the file before the run's
		// first, written once.
		bak, err := os.ReadFile(h.cfg + ".bak")
		if err != nil || string(bak) != prior {
			t.Fatalf("after save %d the backup is %q (%v), want the text the run began with", i+1, bak, err)
		}
		fi, _ := os.Stat(h.cfg + ".bak")
		if i == 0 {
			bakTime = fi.ModTime()
			old := bakTime.Add(-time.Hour)
			_ = os.Chtimes(h.cfg+".bak", old, old)
			bakTime = old
		} else if !fi.ModTime().Equal(bakTime) {
			t.Errorf("save %d rewrote the backup", i+1)
		}
	}
	if got := h.read(); got != "{\n  \"default_currency\": \"JPY\",\n  \"gold_db\": \"/x/g.db\"\n}\n" {
		t.Errorf("file = %q", got)
	}
	if fi, _ := os.Stat(h.cfg); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(h.cfg))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary file is left: %s", e.Name())
		}
	}
	// A new run backs up the file as that run finds it.
	s = openTestStore(t, h.cfg, "/x/g.db")
	if err := s.apply(func(d *jsonedit.Doc) error { return d.Set("CHF", "default_currency") }); err != nil {
		t.Fatal(err)
	}
	if bak, _ := os.ReadFile(h.cfg + ".bak"); !strings.Contains(string(bak), "JPY") {
		t.Errorf("the next run's backup = %q", bak)
	}
}

// TestWizardBacksUpOnce: a whole wizard over a file with content saves
// several times, and the backup is the file as the run found it.
func TestWizardBacksUpOnce(t *testing.T) {
	h := newHarness(t)
	prior := `{"default_currency": "USD"}`
	h.write(prior, 0o600)
	if _, err := h.run("eur", num(t, "viac"), num(t, "swissquote"), "", "on", "off", "q"); err != nil {
		t.Fatal(err)
	}
	if bak, err := os.ReadFile(h.cfg + ".bak"); err != nil || string(bak) != prior {
		t.Errorf("backup = %q (%v), want %q", bak, err, prior)
	}
	if cfg := h.cfgOf(); len(cfg.SilverSources) != 2 || !cfg.Web.Enabled {
		t.Errorf("config = %+v", cfg)
	}
}

// TestConfigThatIsALink: a config that is a symbolic link, as a dotfile
// manager makes, is written through: the link stays a link, its file
// changes, and the backup sits beside the file. A link into a folder
// that does not exist is refused and nothing is written.
func TestConfigThatIsALink(t *testing.T) {
	h := newHarness(t)
	dots := filepath.Join(filepath.Dir(filepath.Dir(h.cfg)), "dotfiles")
	if err := os.MkdirAll(dots, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dots, "wealthdb.cfg")
	prior := `{"gold_db": "/x/g.db", "default_currency": "USD"}`
	if err := os.WriteFile(target, []byte(prior), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(h.cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "dotfiles", "wealthdb.cfg"), h.cfg); err != nil {
		t.Fatal(err)
	}
	h.section = "currency"
	if _, err := h.run("eur"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(h.cfg); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is gone: %v", err)
	}
	got, _ := os.ReadFile(target)
	if !strings.Contains(string(got), `"default_currency": "EUR"`) {
		t.Errorf("the link's file = %s", got)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o640 {
		t.Errorf("the file's mode = %v", fi.Mode().Perm())
	}
	if bak, err := os.ReadFile(target + ".bak"); err != nil || string(bak) != prior {
		t.Errorf("backup beside the file = %q (%v)", bak, err)
	}
	if _, err := os.Lstat(h.cfg + ".bak"); err == nil {
		t.Error("a backup was written beside the link")
	}

	h2 := newHarness(t)
	if err := os.MkdirAll(filepath.Dir(h2.cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent-folder/wealthdb.cfg", h2.cfg); err != nil {
		t.Fatal(err)
	}
	h2.section = "currency"
	_, err := h2.run("eur")
	if err == nil || !strings.Contains(err.Error(), "whose folder does not exist; nothing is changed") {
		t.Errorf("err = %v", err)
	}
	if l, _ := os.Readlink(h2.cfg); l != "/nonexistent-folder/wealthdb.cfg" {
		t.Errorf("the link changed: %s", l)
	}
}

// TestEveryPromptTakesAnyInput feeds each prompt escape sequences, as
// arrow keys give them in line mode, letters, a huge number, a negative
// number and an empty line. None ends the run; the valid answer after
// them is taken.
func TestEveryPromptTakesAnyInput(t *testing.T) {
	junk := []string{"\x1b[B", "\x1b[A", "abcd", "99999999999999999999999", "-3", "0"}
	withJunk := func(answers ...string) []string { return append(slices.Clone(junk), answers...) }
	base := `{"gold_db": %q, "default_currency": "USD", "silver_sources": [
  {"id": "viac", "kind": "viac", "path": %q}], "web": {"enabled": false}, "mcp": {"enabled": false}}`
	for _, tc := range []struct {
		name, section string
		lines         []string
		check         func(t *testing.T, h *harness, out string)
	}{
		{"currency", "currency", withJunk("usd"), func(t *testing.T, h *harness, _ string) {
			if h.cfgOf().DefaultCurrency != "USD" {
				t.Error(h.read())
			}
		}},
		{"banks", "banks", withJunk("zz", "a!", num(t, "swissquote"), ""), func(t *testing.T, h *harness, _ string) {
			if len(h.cfgOf().SilverSources) != 2 {
				t.Error(h.read())
			}
		}},
		{"entry action", "banks", append([]string{"a"}, withJunk("")...), nil},
		{"remove confirm", "banks", append([]string{"a", "r"}, withJunk("y", "")...), func(t *testing.T, h *harness, _ string) {
			if len(h.cfgOf().SilverSources) != 0 {
				t.Error(h.read())
			}
		}},
		{"plaid name", "banks", []string{num(t, "plaid"), "\x1b[B", "a b", "-x", ""}, func(t *testing.T, h *harness, _ string) {
			if len(h.cfgOf().SilverSources) != 2 {
				t.Error(h.read())
			}
		}},
		{"on or off", "dashboards", withJunk("on", ""),
			func(t *testing.T, h *harness, _ string) {
				if w := h.cfgOf().Web; !w.Enabled {
					t.Error(h.read())
				}
			}},
		{"port", "dashboards", append([]string{"on"}, withJunk("65536", "3100")...), func(t *testing.T, h *harness, _ string) {
			if w := h.cfgOf().Web; !w.Enabled || w.Port != 3100 {
				t.Error(h.read())
			}
		}},
		{"menu", "", withJunk("", "q"), func(t *testing.T, h *harness, out string) {
			if !strings.Contains(out, "Type a number from 1 to") {
				t.Error(out)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.write(fmt.Sprintf(base, filepath.Join(h.root, "wealthdb.db"), filepath.Join(h.root, "viac", "viac.db")), 0o600)
			h.collect("wealthdb.db")
			h.section = tc.section
			out, err := h.run(tc.lines...)
			if err != nil {
				t.Fatalf("the run ended on %v: %v\n%s", tc.lines, err, out)
			}
			if tc.check != nil {
				tc.check(t, h, out)
			}
		})
	}
	// The wizard's Banks and brokers step takes no Enter before a source.
	h := newHarness(t)
	out, err := h.run(append(append([]string{"chf"}, withJunk("", "")...), num(t, "viac"), "", "", "", "q")...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "Pick at least one source, or type q to stop.") != 2 || len(h.cfgOf().SilverSources) != 1 {
		t.Errorf("output:\n%s", out)
	}
}

// TestLoadedSourceTakesNoFeedWithoutData: a load reads every feed of a
// source gold has loaded, so a second feed with no data yet is not added
// to it; once collected, the feed is offered as found data and added.
func TestLoadedSourceTakesNoFeedWithoutData(t *testing.T) {
	h := newHarness(t)
	web := filepath.Join(h.root, "ubs-web", "ubs-web.db")
	h.write(fmt.Sprintf(`{"gold_db": %q, "default_currency": "CHF", "silver_sources": [
  {"id": "ubs", "kind": "ubs", "subsources": [{"kind": "ubs-web", "path": %q}]},
  {"id": "viac", "kind": "viac", "path": %q}]}`,
		filepath.Join(h.root, "wealthdb.db"), web, filepath.Join(h.root, "viac", "viac.db")), 0o600)
	h.collect("wealthdb.db")
	h.collect("ubs-web", "ubs-web.db")
	h.loaded = []string{"ubs", "viac"}
	h.section = "banks"
	before := h.read()
	out, err := h.run(num(t, "ubs-psn"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"The ubs source is loaded already, and UBS PSN (SFTP files) has no data yet.",
		"  wealthdb-collect ubs-psn login", "VIAC                               loaded; the data file of VIAC is missing"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if h.read() != before {
		t.Errorf("the feed was added:\n%s", h.read())
	}
	h.collect("ubs-psn", "ubs-psn.db")
	out, err = h.run("n", "c", "n", "")
	if err != nil {
		t.Fatal(err)
	}
	if s := h.cfgOf().SilverSources[0]; len(s.Subsources) != 2 || !strings.Contains(out, "Added UBS PSN (SFTP files).") {
		t.Errorf("the collected feed was not added: %+v\n%s", s, out)
	}
}

// q is the answer that stops, so no line of a list is labelled q: the
// 17th is r, and every label names its own line.
func TestNoLineIsLabelledQ(t *testing.T) {
	for i := range 2000 {
		l := letter(i)
		if strings.Contains(l, "q") {
			t.Fatalf("line %d is labelled %q", i+1, l)
		}
		if k, ok := letterIndex(l); !ok || k != i {
			t.Fatalf("letterIndex(%q) = %d, %v; want %d", l, k, ok, i)
		}
	}
	if letter(15) != "p" || letter(16) != "r" || letter(len(labels)) != "aa" {
		t.Errorf("labels 16, 17 and %d are %q, %q and %q", len(labels)+1, letter(15), letter(16), letter(len(labels)))
	}
	for _, s := range []string{"q", "aq", "", "A", "1", "abcd"} {
		if _, ok := letterIndex(s); ok {
			t.Errorf("%q is taken as a label", s)
		}
	}

	// In the list of sources, the 17th line opens with r.
	h := newHarness(t)
	var entries []string
	for i := 1; i <= 17; i++ {
		entries = append(entries, fmt.Sprintf(`{"id": "s%d", "kind": "viac", "path": "/x/s%d.db"}`, i, i))
	}
	h.write(`{"gold_db": "/x/g.db", "default_currency": "CHF", "silver_sources": [`+strings.Join(entries, ",\n")+`]}`, 0o600)
	h.section = "banks"
	out, err := h.run("r", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\n    r  VIAC (s17) ") || strings.Contains(out, "\n    q  ") {
		t.Errorf("the 17th source is not labelled r:\n%s", out)
	}
	if !strings.Contains(out, "\nVIAC (s17) (s17)\n") || strings.Contains(out, "Stopped") {
		t.Errorf("r does not open the 17th source:\n%s", out)
	}
	if !strings.Contains(out, "a letter (a to r) for one listed") {
		t.Errorf("the question does not name the last label:\n%s", out)
	}
}

// With a config that is not the default one, every engine command the
// setup names carries the flag that names it; a collector's command
// takes none.
func TestCommandsNameTheConfig(t *testing.T) {
	const prog = "wealthdb -c /x/try.cfg"
	bare := regexp.MustCompile(`wealthdb-collect|wealthdb (init|load|web|mcp|resolve-symbols|categorize|config (is|goes|lists|validate|again))`)
	// A sentence wraps where the window ends, so the check reads the
	// output with every run of white space as one space.
	check := func(t *testing.T, out string, wants ...string) {
		t.Helper()
		out = strings.Join(strings.Fields(out), " ")
		for _, w := range wants {
			if !strings.Contains(out, w) {
				t.Errorf("output lacks %q:\n%s", w, out)
			}
		}
		if m := bare.FindString(out); m != "" {
			t.Errorf("%q names the default config:\n%s", m, out)
		}
	}
	h := newHarness(t)
	h.program, h.collectCmd = prog, prog+" collect"
	out, err := h.run("chf", num(t, "viac"), "", "on", "on", "n")
	if err != nil {
		t.Fatal(err)
	}
	check(t, out,
		"Type q to stop; "+prog+" config goes on from there.",
		"To collect the data of VIAC, run this command when "+prog+" config is done: "+prog+" collect viac login "+
			prog+" config lists it again at the end.",
		"To start the dashboards, run this command when "+prog+" config is done: "+prog+" web start ",
		"To start the MCP server, run this command when "+prog+" config is done: "+prog+" mcp start ",
		" 1. "+prog+" init Creates", " 2. "+prog+" collect viac login Starts", " 3. "+prog+" load -a Brings",
		" 4. "+prog+" web start Starts", " 5. "+prog+" mcp start Starts")

	// A stop part-way names the setup command, and so does a section.
	h = newHarness(t)
	h.program, h.collectCmd = prog, prog+" collect"
	if out, err = h.run("chf", "q"); err != nil {
		t.Fatal(err)
	}
	check(t, out, "To go on with the setup, run this command: "+prog+" config")
	h.write(basics+"}\n", 0o600)
	h.section = "model"
	if out, err = h.run("http://m:1/v1", "m", "", "n"); err != nil {
		t.Fatal(err)
	}
	check(t, out, " 4. "+prog+" resolve-symbols Asks", " 5. "+prog+" categorize Asks")
	h.section = "settings"
	if out, err = h.run(); err != nil {
		t.Fatal(err)
	}
	check(t, out, "check the file with this command: "+prog+" config validate")
	h.section = "data-folder"
	if out, err = h.run(); err != nil {
		t.Fatal(err)
	}
	check(t, out, "The database is not created yet. "+prog+" init creates it at /x/g.db.")

	// A config the engine refuses names the command that runs the setup
	// again.
	h.write(`{"gold_db": "/x/g.db", "default_currency": "USD", "colour": "blue"}`, 0o600)
	if _, err = h.run(); err == nil || !strings.HasSuffix(err.Error(), "\nFix the file, then run "+prog+" config again") {
		t.Errorf("err = %v", err)
	}
}

// A file registered from "Collected, but not in the list" is the one
// offered to load, not the collector's first entry.
func TestRegisteredFileIsTheOneLoaded(t *testing.T) {
	h := newHarness(t)
	h.write(fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD",
 "silver_sources": [{"id": "svb", "kind": "fidelity", "path": %q}]}`,
		filepath.Join(h.root, "wealthdb.db"), filepath.Join(h.root, "svb", "svb.db")), 0o600)
	h.collect("wealthdb.db")
	h.collect("svb", "svb.db")
	h.collect("svb", "svb-mortgage.db")
	h.section = "banks"
	out, err := h.run(
		"n",     // not on the way in
		"b", "", // register the mortgage file, and load it
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.loads, []string{"svb-mortgage"}) {
		t.Errorf("loads = %v, want the entry just added\n%s", h.loads, out)
	}
	if !strings.Contains(out, "Its data is collected already.\n\nLoad svb-mortgage now? [Y/n]: ") {
		t.Errorf("the load is offered for another entry:\n%s", out)
	}

	// A second file that is not collected yet names its collect command,
	// though the first entry's file is there.
	h = newHarness(t)
	h.write(basics+"}\n", 0o600)
	h.collect("plaid", "chase", "chase.db")
	h.section = "banks"
	if out, err = h.run("n", num(t, "plaid"), "n", "schwab", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\n      wealthdb-collect plaid link --item schwab\n") || strings.Contains(out, "Its data is collected already") {
		t.Errorf("the next step of the login just named:\n%s", out)
	}
}

// A stop before the last step of the wizard still lists the command a
// change of the run called for, after the setup command.
func TestAStopListsTheCommandsOfTheRun(t *testing.T) {
	h := newHarness(t)
	out, err := h.run("chf", num(t, "viac"), "", "on", "q")
	if err != nil {
		t.Fatal(err)
	}
	rule := strings.Repeat("-", 72)
	want := "\nStopped. Every answer so far is saved.\n\n" + rule + "\nWhat to do now\n\n" +
		"Run these commands in the terminal, in this order:\n" +
		"\n  1.  wealthdb config\n\n      Goes on with the setup: a step has no answer yet.\n" +
		"\n  2.  wealthdb web start\n\n      Starts the dashboards.\n" + rule + "\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("the stop does not end with the setup command and the start:\n%s", out)
	}
}

// At the offer to create the database, n leaves it to the to-do block,
// and an answer that is neither is asked again.
func TestWizardDoesNotCreateTheDatabaseOnNo(t *testing.T) {
	for _, answers := range [][]string{{"n"}, {"no"}, {"later", "n"}} {
		h := newHarness(t)
		out, err := h.run(append([]string{"chf", num(t, "viac"), "", "", ""}, answers...)...)
		if err != nil {
			t.Fatal(err)
		}
		if h.inits != 0 {
			t.Errorf("%v created the database", answers)
		}
		if !strings.Contains(out, "\n  1.  wealthdb init\n\n      Creates the database.\n") || strings.Contains(out, "Stopped") {
			t.Errorf("%v: the to-do block does not name the creation:\n%s", answers, out)
		}
		if asked := strings.Count(out, "Create it now? [Y/n]: "); asked != len(answers) {
			t.Errorf("%v: asked %d times", answers, asked)
		}
	}
}

// A setup with no Load action offers no load: not for a source of the
// list, and not after a file is registered.
func TestNoLoadActionNoOffer(t *testing.T) {
	h := newHarness(t)
	h.write(fmt.Sprintf(`{"gold_db": %q, "default_currency": "CHF",
 "silver_sources": [{"id": "viac", "kind": "viac", "path": %q}]}`,
		filepath.Join(h.root, "wealthdb.db"), filepath.Join(h.root, "viac", "viac.db")), 0o600)
	h.collect("wealthdb.db")
	h.collect("viac", "viac.db")
	h.collect("chase", "chase.db")
	run := func(actions Actions, lines ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := Run(Options{ConfigPath: h.cfg, DataRoot: h.root, Section: "banks", Out: &out,
			In: strings.NewReader(strings.Join(lines, "\n") + "\n"), Actions: actions}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	// n: not on the way in. a: the source, l is no answer there. b: the
	// found file, registered with no question after it.
	out := run(Actions{}, "n", "a", "l", "b", "")
	if strings.Contains(out, "load its data now") || strings.Contains(out, "Load ") {
		t.Errorf("a load is offered with no Load action:\n%s", out)
	}
	if !strings.Contains(out, "Added Chase.\nIts data is collected already.\n") {
		t.Errorf("the file is not registered:\n%s", out)
	}
	// With the action, both are offered.
	var loads []string
	out = run(Actions{Load: func(id string) error { loads = append(loads, id); return nil }}, "a", "l", "")
	if !strings.Contains(out, "  l  load its data now") || !slices.Equal(loads, []string{"viac"}) {
		t.Errorf("loads %v:\n%s", loads, out)
	}
}

// A config that is not the default one is a setup of its own: the data
// folder is the config's folder whatever WEALTHDB_DATA_ROOT says, the
// paths the setup writes go through the variable and load into that
// folder, and every collect command names the config, quoted for a
// shell.
func TestSetupOfItsOwn(t *testing.T) {
	h := newOwnHarness(t)
	prog, collect := h.program, h.collectCmd
	out, err := h.run("chf", num(t, "viac"), "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	text := h.read()
	for _, w := range []string{`"gold_db": "${WEALTHDB_DATA_ROOT}/wealthdb.db"`, `"path": "${WEALTHDB_DATA_ROOT}/viac/viac.db"`} {
		if !strings.Contains(text, w) {
			t.Errorf("config lacks %s:\n%s", w, text)
		}
	}
	cfg := h.cfgOf()
	if cfg.GoldDB != filepath.Join(h.root, "wealthdb.db") {
		t.Errorf("gold_db = %s, want it in the config's folder %s", cfg.GoldDB, h.root)
	}
	if len(cfg.SilverSources) != 1 || cfg.SilverSources[0].Path != filepath.Join(h.root, "viac", "viac.db") {
		t.Errorf("sources = %+v, want them in the config's folder %s", cfg.SilverSources, h.root)
	}
	for _, w := range []string{
		"Data folder:  " + h.root + "\n",
		// The command of the pick, on a line of its own, and the to-do
		// block.
		"\n\n      " + collect + " viac login\n\n",
		"\n  1.  " + collect + " viac login\n\n",
		"\n  2.  " + prog + " load -a\n\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "wealthdb-collect") || strings.Contains(out, "elsewhere") {
		t.Errorf("the output names the default setup:\n%s", out)
	}

	// The data folder section says where the setup lives, and names no
	// variable to move it by.
	h.section = "data-folder"
	if out, err = h.run(); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Data folder: " + h.root + "\n", "The database: " + filepath.Join(h.root, "wealthdb.db") + "\n",
		"This config is a setup of its own, and its folder is the data folder.\nTo move the setup, move the folder.\n"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "WEALTHDB_DATA_ROOT") {
		t.Errorf("the section names the variable, which this setup does not read:\n%s", out)
	}

	// A ledger is checked against the config's folder, and written
	// through the variable.
	h.collect("ledgers", "pins.csv")
	h.section = "ledgers"
	pins := fieldLetter(t, ledgerFiles, "Spending pins")
	outside := filepath.Join(filepath.Dir(h.root), "elsewhere", "ledgers", "pins.csv")
	if out, err = h.run(pins, outside, "${WEALTHDB_DATA_ROOT}/ledgers/pins.csv", ""); err != nil {
		t.Fatal(err)
	}
	// The refusal wraps where the window ends.
	if !strings.Contains(strings.Join(strings.Fields(out), " "), outside+" is not in the data folder, "+h.root+".") ||
		!strings.Contains(h.read(), `"${WEALTHDB_DATA_ROOT}/ledgers/pins.csv"`) {
		t.Errorf("the ledger is not held to the config's folder:\n%s\n%s", out, h.read())
	}

	// The collect commands of a per-item collector name the config too.
	h.section = "banks"
	if out, err = h.run("n", num(t, "plaid"), "chase", ""); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"with that name:\n\n      " + collect + " plaid link --item <name>\n\n",
		"To see those names, run:\n\n      " + collect + " plaid banks\n\n",
		"\n      " + collect + " plaid link --item chase\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "wealthdb-collect") {
		t.Errorf("a collect command names no config:\n%s", out)
	}
}

// The default config's data folder section names the variable that
// moves the folder, when the variable names it.
func TestDataFolderOfTheDefaultConfig(t *testing.T) {
	h := newHarness(t)
	h.rootFromEnv = true
	t.Setenv("WEALTHDB_DATA_ROOT", h.root)
	h.write(`{"gold_db": "${WEALTHDB_DATA_ROOT}/wealthdb.db", "default_currency": "USD"}`+"\n", 0o600)
	h.collect("wealthdb.db")
	h.section = "data-folder"
	out, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Data folder: " + h.root + "\n", "The database: " + filepath.Join(h.root, "wealthdb.db") + "\n",
		"To move the folder, move it and set WEALTHDB_DATA_ROOT to its new place.\n"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "setup of its own") {
		t.Errorf("the default config reads as a setup of its own:\n%s", out)
	}
}
