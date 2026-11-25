package wizard

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/jsonedit"
)

// moreSections follow the basics: the advanced settings that take one
// value each, then one entry for every setting that is edited in the
// file. A setting that gets an editor leaves the file's list.
var moreSections = []section{
	{title: "AI model", group: groupAdvanced, alias: "model", keys: append(slices.Clip(modelKeys), fieldKeys(modelSent)...),
		summary: (*setup).modelSummary, edit: (*setup).editModel},
	{title: "Report options", group: groupAdvanced, alias: "options", keys: fieldKeys(reportOptions),
		summary: func(w *setup) string { return w.fieldsSummary(reportOptions, "the defaults") },
		edit:    (*setup).editReportOptions},
	{title: "Ledger files", group: groupAdvanced, alias: "ledgers", keys: fieldKeys(ledgerFiles),
		summary: func(w *setup) string { return w.fieldsSummary(ledgerFiles, "none") },
		edit:    (*setup).editLedgerFiles},
	{title: "Settings edited in the file", group: groupFile, alias: "file", keys: fileSettingKeys(),
		summary: (*setup).fileSummary, edit: (*setup).showFileSettings},
}

// docsURL is where the docs a setting names are read.
const docsURL = "https://github.com/smartpointer-com/wealthdb/tree/main/wealthdb/docs"

// ---- AI model ----

// modelUse is a member that names a model, and what the model does there.
type modelUse struct {
	path []any
	does string
}

// modelUses are the members that name a model. income.categorization
// takes spending's whole block while it has none of its own, so the
// editor writes its model only where the block is there.
var modelUses = []modelUse{
	{[]any{"symbol_resolution", "model"}, "Tickers of funds"},
	{[]any{"spending", "categorization", "model"}, "Categories of spending"},
	{[]any{"income", "categorization", "model"}, "Categories of income"},
}

// modelKeys are the dotted paths of modelUses.
var modelKeys = func() []string {
	var out []string
	for _, u := range modelUses {
		out = append(out, dotted(u.path))
	}
	return out
}()

func dotted(path []any) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = fmt.Sprint(p)
	}
	return strings.Join(parts, ".")
}

// placedModel is a model the file names, and where.
type placedModel struct {
	use modelUse
	m   config.ModelConfig
}

// models are the models the file names, in the order of modelUses. A
// block the engine would not read is left out.
func (w *setup) models() []placedModel {
	var out []placedModel
	for _, u := range modelUses {
		var m config.ModelConfig
		if ok, err := w.store.doc.Decode(&m, u.path...); ok && err == nil {
			out = append(out, placedModel{u, m})
		}
	}
	return out
}

// sameModel says two blocks ask the same server for the same model.
func sameModel(a, b config.ModelConfig) bool {
	return a.BaseURL == b.BaseURL && a.Name == b.Name && a.APIKey == b.APIKey
}

func (w *setup) modelSummary() string {
	ms := w.models()
	if len(ms) == 0 {
		return "not set"
	}
	for _, m := range ms[1:] {
		if !sameModel(m.m, ms[0].m) {
			return fmt.Sprintf("%d different models", len(ms))
		}
	}
	return modelLine(ms[0].m)
}

// modelLine names a model and its server in a few words.
func modelLine(m config.ModelConfig) string {
	host := m.BaseURL
	if u, err := url.Parse(m.BaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	return cmp.Or(m.Name, "no name") + " at " + host
}

// editModel is the AI model section: one model, written to every member
// that names one.
func (w *setup) editModel() error {
	w.out.help(`A language model fills in what the banks leave out: the ticker of a
fund, and the category of a shop or a payer that no rule places.
It is sent the names of your securities, and of the shops and payers in
your transactions. A model on this computer keeps them here; a service
on the internet receives them (docs/SPENDING.md §5).
Any server with the OpenAI Chat Completions API works: Ollama or
LM Studio on this computer, or a hosted service.`)
	ms := w.models()
	saved := config.ModelConfig{}
	if len(ms) > 0 {
		saved = ms[0].m
		w.out.title("Saved:")
		differ := false
		for _, m := range ms {
			fmt.Fprintf(w.out, "  %-24s %s\n", m.use.does, modelLine(m.m))
			differ = differ || !sameModel(m.m, saved)
		}
		if differ {
			w.out.gap()
			fmt.Fprintln(w.out, "The file names different models. A change here sets one model for all of them.")
		}
		opts := [][2]string{{"c", "change it"}, {"s", "what it is sent"}, {"r", "remove it"}}
		if w.opts.Actions.TestModel != nil {
			opts = append(opts, [2]string{"t", "send it a test question"})
		}
		w.out.options(opts...)
		for change := false; !change; {
			ans, err := w.p.line("Type a letter, or press Enter to go back: ")
			if err != nil {
				return err
			}
			switch strings.ToLower(ans) {
			case "":
				return nil
			case "c", "change":
				change = true
			case "s", "sent":
				return w.editModelSent()
			case "r", "remove":
				return w.removeModel()
			case "t", "test":
				if w.opts.Actions.TestModel != nil {
					w.testModel(saved)
					return nil
				}
				fallthrough
			default:
				w.out.fail("  Type one of the letters on the list.")
			}
		}
	}

	w.out.gap()
	w.out.help("The address of the server ends in /v1, such as http://localhost:11434/v1 for Ollama.")
	question := "Address"
	if saved.BaseURL == "" {
		question = "Address (Enter to go back)"
	}
	base, err := w.p.askValid(question, saved.BaseURL, func(s string) (string, error) {
		if s == "" {
			return "", nil
		}
		return w.checkAddress(s)
	})
	if err != nil || base == "" {
		return err
	}
	if base, err = w.reachableAddress(base); err != nil {
		return err
	}
	w.out.gap()
	w.out.help("The model's name, as the server lists it.")
	// Nothing is saved before the key: Enter with no name backs out.
	question = "Model name"
	if saved.Name == "" {
		question = "Model name (Enter to go back)"
	}
	name, err := w.p.askValid(question, saved.Name, func(s string) (string, error) { return s, nil })
	if err != nil || name == "" {
		return err
	}
	key, err := w.askKey(saved.APIKey)
	if err != nil {
		return err
	}
	m := config.ModelConfig{BaseURL: base, API: "openai-completions", Name: name, APIKey: key}
	if key != "" {
		// The file holds a key from now on: no one else may read it.
		w.store.mode &^= 0o077
	}
	err = w.store.apply(func(d *jsonedit.Doc) error {
		for _, u := range modelUses {
			if u.path[0] == "income" && !d.Has("income", "categorization") {
				continue
			}
			if err := setModel(d, u.path, m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return w.explain(err)
	}
	w.out.ok("Saved: %s.", modelLine(m))
	if w.opts.Actions.TestModel != nil {
		if ok, err := w.p.yesNo("Send it a test question now?", true); err != nil {
			return err
		} else if ok {
			w.testModel(m)
		}
	}
	w.thenRun(Step{w.cmd("resolve-symbols"), "Asks the model for the tickers the banks leave out."})
	w.thenRun(Step{w.cmd("categorize"), "Asks the model for the category of each shop and payer that\nno rule places."})
	return nil
}

// setModel writes m at path member by member, so the block keeps every
// other member it holds, and an unchanged one is no change.
func setModel(d *jsonedit.Doc, path []any, m config.ModelConfig) error {
	at := func(k string) []any { return append(slices.Clip(path), k) }
	for _, f := range []struct{ key, v string }{{"baseUrl", m.BaseURL}, {"api", m.API}, {"name", m.Name}} {
		var old string
		if _, err := d.Decode(&old, at(f.key)...); err == nil && old == f.v && d.Has(at(f.key)...) {
			continue
		}
		if err := d.Set(f.v, at(f.key)...); err != nil {
			return err
		}
	}
	if m.APIKey == "" {
		return d.Remove(at("apiKey")...)
	}
	var old string
	if _, _ = d.Decode(&old, at("apiKey")...); old == m.APIKey {
		return nil
	}
	return d.Set(m.APIKey, at("apiKey")...)
}

// removeModel removes every model the file names, and each block the
// removal leaves empty.
func (w *setup) removeModel() error {
	ok, err := w.p.yesNo("Remove the model? Tickers and categories found so far stay.", false)
	if err != nil || !ok {
		return err
	}
	err = w.store.apply(func(d *jsonedit.Doc) error {
		for _, u := range modelUses {
			if err := removeEmptied(d, u.path); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return w.explain(err)
	}
	w.out.ok("Removed.")
	return nil
}

// checkAddress takes the address of an OpenAI-compatible server.
func (w *setup) checkAddress(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("An address starts with http:// or https://, such as http://localhost:11434/v1.")
	}
	return strings.TrimRight(s, "/"), nil
}

// reachableAddress is base, or, when the engine runs in a container,
// the address that reaches the same server: there, localhost is the
// container itself, and host.docker.internal is this computer.
func (w *setup) reachableAddress(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || !w.opts.InContainer {
		return base, nil
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
	default:
		return base, nil
	}
	alt := *u
	alt.Host = "host.docker.internal"
	if port := u.Port(); port != "" {
		alt.Host = net.JoinHostPort("host.docker.internal", port)
	}
	w.out.gap()
	w.out.help("wealthdb runs in a container, where %s is the container itself.\n"+
		"From there, this computer is host.docker.internal.", u.Hostname())
	ok, err := w.p.yesNo("Use "+alt.String()+" instead?", true)
	if err != nil || !ok {
		return base, err
	}
	return alt.String(), nil
}

// askKey asks for the API key without showing it. Enter keeps the saved
// key, and - removes it.
func (w *setup) askKey(saved string) (string, error) {
	q := "API key (Enter for none; a local server needs none): "
	if saved != "" {
		q = "API key (Enter keeps the saved one, - removes it): "
	}
	s, err := w.secret(q)
	switch {
	case err != nil:
		return "", err
	case s == "":
		return saved, nil
	case s == "-":
		return "", nil
	}
	return s, nil
}

// secret reads one line that is not shown: in raw mode on a terminal,
// else as any other line (a test, or a terminal that cannot do raw
// mode). In raw mode, Ctrl+C and Ctrl+D quit like q.
func (w *setup) secret(question string) (string, error) {
	if w.opts.Terminal == nil {
		return w.p.line(question)
	}
	// The blank line goes out before raw mode, where a line break does
	// not return the cursor to the first column.
	w.out.gap()
	restore, err := w.opts.Terminal.Raw()
	if err != nil {
		return w.p.line(question)
	}
	fmt.Fprint(w.out, w.out.Ask(question))
	s, err := readHidden(w.in)
	restore()
	fmt.Fprintln(w.out)
	w.out.answered()
	if err != nil {
		return "", err
	}
	if strings.EqualFold(s, "q") {
		return "", errQuit
	}
	return s, nil
}

// readHidden reads raw-mode bytes up to Enter: Backspace takes the last
// one back, Ctrl+U the whole line, and an escape sequence (an arrow key)
// is dropped.
func readHidden(r interface{ ReadByte() (byte, error) }) (string, error) {
	var b []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			if len(b) == 0 {
				return "", errQuit
			}
			return strings.TrimSpace(string(b)), nil
		}
		switch {
		case c == '\r' || c == '\n':
			return strings.TrimSpace(string(b)), nil
		case c == 0x03 || (c == 0x04 && len(b) == 0):
			return "", errQuit
		case c == 0x7f || c == 0x08:
			if len(b) > 0 {
				b = b[:len(b)-1]
			}
		case c == 0x15:
			b = b[:0]
		case c == 0x1b:
			if n, err := r.ReadByte(); err == nil && (n == '[' || n == 'O') {
				for {
					f, err := r.ReadByte()
					if err != nil || (f >= 0x40 && f <= 0x7e) {
						break
					}
				}
			}
		case c >= 0x20:
			b = append(b, c)
		}
	}
}

// ModelRefused is the failure of a test question that the server
// answered: an error status, or a body that is no reply.
type ModelRefused struct{ Err error }

func (e *ModelRefused) Error() string { return e.Err.Error() }
func (e *ModelRefused) Unwrap() error { return e.Err }

// testModel sends the model a short question and says how it answered.
func (w *setup) testModel(m config.ModelConfig) {
	w.out.gap()
	w.out.say("Asking %s. A model that is not loaded yet takes a minute ...", modelLine(m))
	reply, err := w.opts.Actions.TestModel(m)
	var refused *ModelRefused
	switch {
	case errors.As(err, &refused):
		w.out.fail("The server answered, but not with a reply:\n  %s", err.Error())
		fmt.Fprintln(w.out, "The address is right. Check the model's name, as the server lists it, and the key.")
		return
	case err != nil:
		w.out.fail("No answer:\n  %s", err.Error())
		fmt.Fprintln(w.out, "Check that the server runs, and the address.")
		return
	}
	reply = strings.Join(strings.Fields(reply), " ")
	if r := []rune(reply); len(r) > 60 {
		reply = string(r[:60]) + " ..."
	}
	// Not a change the menu shows: it stays to be read.
	fmt.Fprintln(w.out, w.out.OK("The model answered: "+reply))
}

// ---- Settings edited in the file ----

// fileSetting is a part of the config with no editor: what it does, the
// members it holds and the doc that describes them. entries says its one
// member is a list, counted in the summary.
type fileSetting struct {
	title, does string
	// group is Features or Overrides.
	group   string
	keys    []string
	doc     string
	entries bool
}

var fileSettings = []fileSetting{
	{"Benchmark portfolios", "Fund mixes to compare the returns with, beside the built-in ones.",
		"Features", []string{"benchmarks"}, "wealthdb help benchmarks", true},
	{"Yield curves and withholding", "Yield curves and the tax withheld at source, for the yield report.",
		"Features", []string{"yield"}, "docs/YIELD.md §8", false},
	{"Exposure funds", "Stand-in funds, reference funds and custody, for the exposure report.",
		"Features", []string{"exposure"}, "docs/EXPOSURE.md §9", false},
	{"Spending rules", "Rules, the accounts that count, and the names of transfers, for spending.",
		"Features", []string{"spending"}, "docs/SPENDING.md", false},
	{"Income rules", "Rules and the accounts that count, for the income report.",
		"Features", []string{"income"}, "docs/INCOME.md", false},
	{"Cash flow accounts", "The accounts the cash flow statement leaves out, and its wrappers.",
		"Features", []string{"cashflow"}, "docs/CASHFLOW.md §7", false},
	{"Lots per account", "A lot method per source, portfolio or account, and costs to seed.",
		"Features", []string{"lots"}, "docs/LOTS.md §7", false},
	{"Declared accounts", "Accounts at a bank no collector reads, so the reports know them.",
		"Features", []string{"declared_accounts"}, "docs/DESIGN.md §5.1", true},
	{"Accounts and portfolios", "Names, kinds and tax wrappers of accounts, and accounts left out.",
		"Overrides", []string{"account_overrides", "portfolio_overrides", "supersession"}, "docs/DESIGN.md §13.8", false},
	{"Instruments", "Asset classes, tickers and other names of securities.",
		"Overrides", []string{"instrument_overrides", "transaction_instruments", "holding_aliases",
			"symbol_resolution.overrides"}, "docs/DESIGN.md §5.1 and §13.8", false},
	{"Returns", "Start dates of track records, and accounts left out of the returns.",
		"Overrides", []string{"inception_overrides", "returns_exclude", "returns_policy_overrides",
			"returns_hide", "returns_transfer_matching"}, "docs/DESIGN.md §5.4 to §5.8", false},
}

// fileSettingKeys are the members of every fileSetting.
func fileSettingKeys() []string {
	var out []string
	for _, f := range fileSettings {
		out = append(out, f.keys...)
	}
	return out
}

// pathOf is a dotted key as a path.
func pathOf(key string) []any {
	var out []any
	for _, p := range strings.Split(key, ".") {
		out = append(out, p)
	}
	return out
}

// holds says the value at path holds something beyond the members the
// setup's editors write (editedPaths).
func holds(d *jsonedit.Doc, path []any) bool {
	if !d.Has(path...) {
		return false
	}
	edited := editedPaths()
	if slices.ContainsFunc(edited, func(p []any) bool { return slices.Equal(p, path) }) {
		return false
	}
	if !slices.ContainsFunc(edited, func(p []any) bool { return len(p) > len(path) && slices.Equal(p[:len(path)], path) }) {
		return true
	}
	for _, k := range d.Keys(path...) {
		if holds(d, append(slices.Clip(path), k)) {
			return true
		}
	}
	return false
}

// state is what a setting holds, in a few words.
func (w *setup) state(f fileSetting) string {
	d := w.store.doc
	var set []string
	for _, k := range f.keys {
		if holds(d, pathOf(k)) {
			set = append(set, k)
		}
	}
	switch {
	case len(set) == 0:
		return "not set"
	case len(f.keys) > 1:
		return fmt.Sprintf("%d of %d keys set", len(set), len(f.keys))
	case !f.entries:
		return "set"
	}
	if n := d.Len(pathOf(set[0])...); n != 1 {
		return fmt.Sprintf("%d entries", n)
	}
	return "1 entry"
}

func (w *setup) fileSummary() string {
	n := 0
	for _, f := range fileSettings {
		if w.state(f) != "not set" {
			n++
		}
	}
	return fmt.Sprintf("%d of %d set", n, len(fileSettings))
}

// showFileSettings lists every setting edited in the file: what it does,
// what is set, its keys and its doc, then how to edit and check them.
func (w *setup) showFileSettings() error {
	group := ""
	for _, f := range fileSettings {
		if f.group != group {
			group = f.group
			w.out.title(" " + group)
		}
		fmt.Fprintf(w.out, "   %-26s %s\n", f.title, w.state(f))
		w.out.help("       %s", f.does)
		for _, line := range wrap(strings.Join(f.keys, ", ")+" · "+f.doc, width-7) {
			w.out.help("       %s", line)
		}
	}
	w.out.gap()
	fmt.Fprintf(w.out, "These settings are edited in the file:\n    %s\n", w.store.path)
	fmt.Fprintf(w.out, "The docs: %s\n", docsURL)
	w.out.gap()
	fmt.Fprintln(w.out, "After an edit, check the file with this command:")
	w.out.command(w.validateCommand())
	return nil
}

// wrap breaks text at spaces into lines of at most width characters; a word
// longer than width stands on a line of its own.
func wrap(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		if line != "" && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > width {
			lines, line = append(lines, line), ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	return append(lines, line)
}

// validateCommand is the command that checks this config.
func (w *setup) validateCommand() string {
	return w.cmd("config validate")
}

// ---- Validate the config file ----

// Check is `wealthdb config validate`: the verdict on the config at
// path, by the engine's own rules and the checks of the commands that
// ask a model. ok is a one-line summary of a valid config; err says
// what is wrong, with the state that tells a config to finish from one
// to fix.
func Check(path string) (state config.State, ok string, err error) {
	abs, _, data, err := configFile(path)
	if err != nil {
		return config.StateRefused, "", err
	}
	st, cfg, err := config.InspectBytes(data, abs)
	switch st {
	case config.StateAbsent:
		return st, "", fmt.Errorf("%s holds no setup yet", abs)
	case config.StateUnfinished:
		// A member with an empty value is missing as one not written is
		// (config.InspectBytes).
		var top struct {
			Currency string `json:"default_currency"`
			Gold     string `json:"gold_db"`
		}
		_ = json.Unmarshal(data, &top)
		var missing []string
		if top.Currency == "" {
			missing = append(missing, "default_currency")
		}
		if top.Gold == "" {
			missing = append(missing, "gold_db")
		}
		return st, "", fmt.Errorf("the setup in %s is not finished: it has no %s", abs, strings.Join(missing, " and no "))
	case config.StateRefused:
		return st, "", fmt.Errorf("%w%s", err, position(data, err))
	}
	for _, u := range []struct {
		key string
		m   *config.ModelConfig
	}{
		{"symbol_resolution.model", modelOf(cfg.SymbolResolution)},
		{"spending.categorization.model", spendingModel(cfg)},
		{"income.categorization.model", incomeModel(cfg)},
	} {
		if u.m == nil {
			continue
		}
		if err := u.m.Check(u.key); err != nil {
			return config.StateRefused, "", fmt.Errorf("config: %s", err.Error())
		}
	}
	return st, fmt.Sprintf("%s is valid: %s, reports in %s.", abs, sourceCount(len(cfg.SilverSources)), cfg.DefaultCurrency), nil
}

func modelOf(s *config.SymbolResolutionConfig) *config.ModelConfig {
	if s == nil {
		return nil
	}
	return s.Model
}

func spendingModel(c *config.Config) *config.ModelConfig {
	if c.Spending == nil {
		return nil
	}
	return c.Spending.Categorization.CategorizationModel()
}

func incomeModel(c *config.Config) *config.ModelConfig {
	if c.Income == nil {
		return nil
	}
	return c.Income.Categorization.CategorizationModel()
}

func sourceCount(n int) string {
	if n == 1 {
		return "1 source"
	}
	return fmt.Sprintf("%d sources", n)
}

// position is where in data a JSON error that names only a byte offset
// sits, as " (line N, column M)"; empty for any other error.
func position(data []byte, err error) string {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	off := int64(-1)
	switch {
	case errors.As(err, &syn):
		off = syn.Offset
	case errors.As(err, &typ):
		off = typ.Offset
	}
	if off < 0 || off > int64(len(data)) {
		return ""
	}
	line, col := 1, 1
	for _, c := range data[:off] {
		if c == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return fmt.Sprintf(" (line %d, column %d)", line, col)
}

// validate is the menu's check of the file.
func (w *setup) validate() error {
	_, ok, err := Check(w.store.path)
	w.out.gap()
	if err != nil {
		w.out.fail("The config is not valid:\n  %s", err.Error())
		return nil
	}
	// Not a change the menu shows: it stays to be read.
	fmt.Fprintln(w.out, w.out.OK(ok))
	return nil
}
