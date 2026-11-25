package wizard

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/jsonedit"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The groups of the menu, in order.
const (
	groupBasic    = "Basic"
	groupAdvanced = "Advanced"
	groupFile     = "Config file"
)

// section is one part of the config: its title, the group of the menu
// it stands in, the members it covers (a dotted path names a member
// inside one), a summary of what is saved, and its editor. alias is one
// more name the command line takes for it.
type section struct {
	title   string
	group   string
	alias   string
	keys    []string
	summary func(w *setup) string
	edit    func(w *setup) error
}

// slug is how a section is named on the command line.
func (s section) slug() string {
	return strings.ReplaceAll(strings.ToLower(s.title), " ", "-")
}

// basicSections are the parts of the config the setup asks about. The
// wizard walks the first four.
var basicSections = []section{
	{title: "Primary currency", alias: "currency", keys: []string{"default_currency"}, summary: (*setup).currencySummary, edit: (*setup).editCurrency},
	{title: "Banks and brokers", keys: []string{"silver_sources"}, summary: (*setup).sourcesSummary, edit: (*setup).editSources},
	{title: "Dashboards", keys: []string{"web"}, summary: (*setup).webSummary, edit: func(w *setup) error { return w.editWeb(true) }},
	{title: "MCP server", keys: []string{"mcp"}, summary: (*setup).mcpSummary, edit: func(w *setup) error { return w.editMCP(true) }},
	{title: "Data folder", keys: []string{"gold_db"}, summary: (*setup).dataSummary, edit: (*setup).showData},
}

// sections are the basic sections, then the advanced ones, then the
// settings edited in the file; the menu adds the check of the file.
func sections() []section {
	out := append([]section(nil), basicSections...)
	for i := range out {
		out[i].group = groupBasic
	}
	return append(out, moreSections...)
}

// sectionNamed is the section a command line names: its slug, the first
// word of its title, or its alias.
func sectionNamed(name string) (section, bool) {
	name = strings.ToLower(name)
	for _, s := range sections() {
		first, _, _ := strings.Cut(s.slug(), "-")
		if name == s.slug() || name == first || name == s.alias {
			return s, true
		}
	}
	return section{}, false
}

// ---- Currency ----

func (w *setup) currency() string {
	var c string
	_, _ = w.store.doc.Decode(&c, "default_currency")
	return c
}

func (w *setup) currencySummary() string { return cmp.Or(w.currency(), "not set") }

func (w *setup) editCurrency() error {
	w.out.help("The currency the reports show amounts in, such as CHF, EUR or USD.")
	saved := cmp.Or(w.currency(), guessCurrency(w.opts.Locale))
	cur, err := w.p.askValid("Currency", saved, func(s string) (string, error) {
		s = strings.ToUpper(s)
		if !config.IsLikelyISO4217(s) {
			return "", fmt.Errorf("A currency is three letters, such as CHF, EUR or USD.")
		}
		return s, nil
	})
	if err != nil {
		return err
	}
	if err := w.store.apply(func(d *jsonedit.Doc) error { return d.Set(cur, "default_currency") }); err != nil {
		return w.explain(err)
	}
	w.out.ok("Saved: %s.", cur)
	w.noteExchangeRates(cur)
	return nil
}

// noteExchangeRates says, once the reports' currency cur is chosen, that
// an account in another currency needs the exchange-rate source. It
// says nothing when a configured source states exchange rates.
func (w *setup) noteExchangeRates(cur string) {
	fx, ok := fxSource()
	if es, err := w.entries(); !ok || err == nil && statesFX(es) {
		return
	}
	w.out.help("A bank or broker that reports in another currency than %s needs exchange rates. "+
		"Add %s among the banks and brokers: without it, amounts in another currency are missing from every total.",
		cur, fx.Display)
}

// ---- Banks and brokers ----

func (w *setup) sourcesSummary() string {
	es, err := w.entries()
	if err != nil {
		return "edited by hand"
	}
	if len(es) == 0 {
		return "none yet"
	}
	// As many names as fit the line, then a count of the rest.
	const width = 34
	var names []string
	for i, e := range es {
		if n := len(strings.Join(append(names, e.display), ", ")); n > width && i > 0 {
			return strings.Join(names, ", ") + fmt.Sprintf(", %d more", len(es)-i)
		}
		names = append(names, e.display)
	}
	return strings.Join(names, ", ")
}

// editSources is the Banks and brokers section. The configured
// sources, with their state, and the collected data no entry names take
// letters; the catalogue takes numbers, fixed for the run, and leaves
// out a collector the config names already (all but a per-item one,
// which takes an entry per login). Enter leaves; in the wizard, not
// before a source is listed. The line that says an exchange-rate source
// is needed (fxHint) stands under the configured sources; a step that
// makes it apply is followed by it at once, with an offer to add that
// source (offerFX).
func (w *setup) editSources() error {
	cat := silver.Catalogue()
	hinted := w.fxNeeded()
	added, err := w.offerFound()
	if err != nil {
		return err
	}
	if added && !hinted {
		if err := w.offerFX(); err != nil {
			return err
		}
	}
	// The catalogue is listed on the way in and when asked for (?). It
	// is not listed again after a pick, so the command the pick names
	// stays on the screen above the prompt.
	list := true
	for {
		es, err := w.entries()
		if err != nil {
			w.out.fail("silver_sources is edited by hand: %s", err.Error())
			return nil
		}
		loaded := w.loaded()
		disc := w.discovered(es)
		named := map[string]bool{}
		for _, e := range es {
			for _, f := range e.feeds {
				named[f.Collector] = true
			}
		}
		if len(es) > 0 {
			w.out.title("Your sources:")
			for i, e := range es {
				fmt.Fprintf(w.out, "  %s  %-34s %s\n", w.out.Key(fmt.Sprintf("%3s", letter(i))), e.display, stateOf(e, loaded))
			}
		}
		hint := w.fxHint(es)
		if hint != "" && len(es) > 0 {
			w.out.help("%s", hint)
		}
		if len(disc) > 0 {
			w.out.title("Collected, but not in the list:")
			for i, f := range disc {
				fmt.Fprintf(w.out, "  %s  %s\n", w.out.Key(fmt.Sprintf("%3s", letter(len(es)+i))), foundName(f))
			}
		}
		if list {
			w.out.title("Add one:")
		}
		offered := map[int]bool{}
		group := silver.Group(-1)
		for i, c := range cat {
			if named[c.Name] && !c.PerItem {
				continue
			}
			offered[i+1] = true
			if !list {
				continue
			}
			if c.Group != group {
				group = c.Group
				fmt.Fprintf(w.out, "  %s\n", w.out.Title(group.String()))
			}
			// What a collector covers wraps within an 80-column window,
			// under its own column.
			covers := wrap(c.Covers, 80-catalogueIndent)
			fmt.Fprintf(w.out, "  %s  %-34s %s\n", w.out.Key(fmt.Sprintf("%3d", i+1)), c.Display, w.out.Help(covers[0]))
			for _, more := range covers[1:] {
				fmt.Fprintf(w.out, "%*s%s\n", catalogueIndent, "", w.out.Help(more))
			}
		}
		// One clause per way to answer, joined into one question.
		clauses := []string{"Type a number to add a source"}
		if !list && len(es) > 0 {
			clauses[0] = "Type a number to add another source"
		}
		if !list {
			clauses = append(clauses, "? to see the list again")
		}
		switch n := len(es) + len(disc); {
		case n == 1:
			clauses = append(clauses, "a for the one listed")
		case n > 1:
			clauses = append(clauses, fmt.Sprintf("a letter (a to %s) for one listed", letter(n-1)))
		}
		clauses = append(clauses, "or press Enter when done")
		list = false
		ans, err := w.p.line(joinClauses(clauses) + ": ")
		if err != nil {
			return err
		}
		ans = strings.ToLower(ans)
		if ans == "?" {
			list = true
			continue
		}
		if ans == "" {
			if w.walking && len(es) == 0 {
				w.out.fail("  Pick at least one source, or type q to stop.")
				continue
			}
			return nil
		}
		if k, ok := letterIndex(ans); ok && k < len(es)+len(disc) {
			if k < len(es) {
				err = w.editEntry(es[k], loaded)
			} else {
				err = w.register(disc[k-len(es)])
			}
		} else if k, aerr := strconv.Atoi(ans); aerr == nil && offered[k] {
			err = w.pick(cat[k-1])
		} else {
			w.out.fail("  Type one of the numbers or letters on the list.")
		}
		if err != nil {
			return err
		}
		if hint == "" {
			if err := w.offerFX(); err != nil {
				return err
			}
		}
	}
}

// fxNeeded says the line of fxHint applies to the config as it stands.
func (w *setup) fxNeeded() bool {
	es, err := w.entries()
	return err == nil && w.fxHint(es) != ""
}

// offerFX follows a step that made the line of fxHint apply: it shows
// the line and offers to add the exchange-rate source at once, as a
// pick of it from the list does. It does nothing while the line does
// not apply. A no leaves the line under the configured sources.
func (w *setup) offerFX() error {
	fx, ok := fxSource()
	es, err := w.entries()
	if !ok || err != nil {
		return nil
	}
	hint := w.fxHint(es)
	if hint == "" {
		return nil
	}
	w.out.gap()
	w.out.help("%s", hint)
	yes, err := w.p.yesNo("Add "+fx.Display+" now?", true)
	if err != nil || !yes {
		return err
	}
	return w.pick(fx)
}

// joinClauses joins the parts of a question with commas, breaking the
// line between two parts, never inside one, where it would pass the
// window's width.
func joinClauses(clauses []string) string {
	var b strings.Builder
	line := 0
	for i, c := range clauses {
		sep := ""
		if i > 0 {
			sep = ", "
		}
		if line > 0 && line+len(sep)+len(c)+2 > width {
			b.WriteString(",\n")
			line, sep = 0, ""
		}
		b.WriteString(sep + c)
		line += len(sep) + len(c)
	}
	return b.String()
}

// catalogueIndent is the column where a catalogue line's description
// starts: two spaces, the number, two spaces, the name and a space.
const catalogueIndent = 2 + 3 + 2 + 34 + 1

// offerFound offers, on the way into the section, to add all at once
// the collected data that no entry names: a bank linked and downloaded
// since the last run of the setup takes one Enter. It reports whether
// it added any.
func (w *setup) offerFound() (added bool, err error) {
	es, err := w.entries()
	if err != nil {
		return false, nil
	}
	disc := w.discovered(es)
	if len(disc) == 0 {
		return false, nil
	}
	w.out.title("Collected, but not in the list yet:")
	for _, f := range disc {
		fmt.Fprintf(w.out, "  %s\n", foundName(f))
	}
	add, err := w.p.yesNo("Add them now?", true)
	if err != nil || !add {
		return false, err
	}
	for _, f := range disc {
		res, err := w.add(f.c, filepath.Base(f.file), f.item)
		if err != nil {
			return added, w.explain(err)
		}
		if res == handEdited {
			w.out.fail("The %s entry is edited by hand. Add %s to it in %s.", f.c.Kind, foundName(f), w.store.path)
			continue
		}
		w.out.ok("Added %s.", foundName(f))
		added = true
	}
	return added, nil
}

// labels are the letters a line of a list is labelled with. q is not
// one: it is the answer that stops, at every prompt.
const labels = "abcdefghijklmnoprstuvwxyz"

// letter is the label of the i-th line of the list: a to z without q,
// then aa, ab, and so on.
func letter(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / len(labels) {
		s = string(labels[(i-1)%len(labels)]) + s
	}
	return s
}

// letterIndex is the line a label names; false when s is no label.
func letterIndex(s string) (int, bool) {
	if s == "" || len(s) > 3 {
		return 0, false
	}
	n := 0
	for i := range len(s) {
		k := strings.IndexByte(labels, s[i])
		if k < 0 {
			return 0, false
		}
		n = n*len(labels) + k + 1
	}
	return n - 1, true
}

// foundName is how collected data the config does not name is listed.
func foundName(f found) string {
	if f.item != "" {
		return fmt.Sprintf("%s: %s", f.c.Display, f.item)
	}
	if base := filepath.Base(f.file); base != f.c.Files[0] {
		return fmt.Sprintf("%s: %s", f.c.Display, base)
	}
	return f.c.Display
}

// register adds collected data the config does not name.
func (w *setup) register(f found) error {
	res, err := w.add(f.c, filepath.Base(f.file), f.item)
	if err != nil {
		return w.explain(err)
	}
	if res == handEdited {
		w.out.fail("The %s entry is edited by hand. Add %s to it in %s.", f.c.Kind, f.c.Display, w.store.path)
		return nil
	}
	w.out.ok("Added %s.", foundName(f))
	return w.afterAdd(filepath.Clean(f.file))
}

// editEntry shows one configured source and what can be done with it.
func (w *setup) editEntry(e entry, loaded map[string]bool) error {
	st := stateOf(e, loaded)
	w.out.title(fmt.Sprintf("%s (%s)", e.display, e.src.ID))
	fmt.Fprintf(w.out, "%s.\n", strings.ToUpper(st[:1])+st[1:])
	collected := false
	for _, f := range e.feeds {
		if exists(f.Path) {
			collected = true
		} else if f.Collector != "" {
			w.printNext(f)
		}
	}
	canLoad := collected && w.goldExists() && w.opts.Actions.Load != nil
	var opts [][2]string
	if canLoad {
		opts = append(opts, [2]string{"l", "load its data now"})
	}
	if !e.hand {
		opts = append(opts, [2]string{"r", "remove it"})
	}
	if len(opts) == 0 {
		return nil
	}
	w.out.options(opts...)
	ans, err := w.p.line("Type a letter, or press Enter to go back: ")
	if err != nil {
		return err
	}
	switch strings.ToLower(ans) {
	case "l", "load":
		if canLoad {
			return w.load(e.src.ID)
		}
	case "r", "remove":
		if !e.hand {
			return w.remove(e, loaded)
		}
	}
	return nil
}

// ---- Dashboards and MCP server ----

// service is the web or the mcp block as the setup reads it.
type service struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
}

func (w *setup) service(key string) service {
	var s service
	_, _ = w.store.doc.Decode(&s, key)
	return s
}

func (w *setup) webSummary() string {
	s := w.service("web")
	if !s.Enabled {
		return "off"
	}
	return fmt.Sprintf("on, http://127.0.0.1:%d", (&config.WebConfig{Port: s.Port}).EffectivePort())
}

func (w *setup) mcpSummary() string {
	s := w.service("mcp")
	if !s.Enabled {
		return "off"
	}
	return fmt.Sprintf("on, port %d", (&config.MCPConfig{Port: s.Port}).EffectivePort())
}

func (w *setup) editWeb(withPort bool) error {
	w.out.help("Dashboards show the reports in a web browser, on this computer only.")
	return w.editService("web", "Dashboards", w.cmd("web start"), config.DefaultWebPort, withPort)
}

func (w *setup) editMCP(withPort bool) error {
	w.out.help("The MCP server lets an AI agent on this computer read the reports.")
	return w.editService("mcp", "MCP server", w.cmd("mcp start"), config.DefaultMCPPort, withPort)
}

// editService asks on or off, and the port when withPort, and keeps
// every other member of the block. An answer is written even when it
// is off, as {"enabled": false}: a block present is how a re-run knows
// the step was answered.
func (w *setup) editService(key, title, start string, defaultPort int, withPort bool) error {
	saved := w.service(key)
	on, err := w.p.onOff(title, saved.Enabled)
	if err != nil {
		return err
	}
	port := saved.Port
	if on && withPort {
		v, err := w.p.askValid("Port", strconv.Itoa(cmp.Or(saved.Port, defaultPort)), func(s string) (string, error) {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 65535 {
				return "", fmt.Errorf("A port is a number from 1 to 65535.")
			}
			return s, nil
		})
		if err != nil {
			return err
		}
		port, _ = strconv.Atoi(v)
	}
	err = w.store.apply(func(d *jsonedit.Doc) error {
		if on != saved.Enabled || !d.Has(key) {
			if err := d.Set(on, key, "enabled"); err != nil {
				return err
			}
		}
		if port != saved.Port && !(saved.Port == 0 && port == defaultPort) {
			return d.Set(port, key, "port")
		}
		return nil
	})
	if err != nil {
		return w.explain(err)
	}
	w.out.ok("Saved: %s.", onOffWord(on))
	if on && !saved.Enabled {
		what := strings.ToLower(title[:1]) + title[1:]
		if key == "mcp" {
			what = title
		}
		w.runLater("To start the "+what, start)
		w.thenRun(Step{start, "Starts the " + what + "."})
	}
	return nil
}

// runLater names a command to run once the setup is done: what it is
// for, the command on a line of its own between blank lines, and that
// the end of the setup lists it again.
func (w *setup) runLater(purpose, command string) {
	w.out.gap()
	w.out.say("%s, run this command when %s is done:", purpose, w.cmd("config"))
	w.out.command(command)
	w.out.help("%s lists it again at the end.", w.cmd("config"))
}

// ---- Data folder ----

func (w *setup) dataSummary() string {
	if g := w.goldPath(); filepath.Dir(g) != filepath.Clean(w.opts.DataRoot) {
		return tilde(g)
	}
	return tilde(w.opts.DataRoot)
}

// showData says where the data lives. The environment names the
// folder, or the config's own place does, so it is shown, not asked.
func (w *setup) showData() error {
	fmt.Fprintf(w.out, "Data folder: %s\n", w.opts.DataRoot)
	w.out.help("The database and the data of every bank live here.")
	g := w.goldPath()
	if filepath.Dir(g) != filepath.Clean(w.opts.DataRoot) {
		fmt.Fprintf(w.out, "The file names another place for the database: %s\n", g)
	}
	if !w.goldExists() {
		w.out.say("The database is not created yet. %s creates it at %s.", w.cmd("init"), g)
		return nil
	}
	fmt.Fprintf(w.out, "The database: %s\n", g)
	switch {
	case w.opts.OwnSetup:
		w.out.help("This config is a setup of its own, and its folder is the data folder.\n" +
			"To move the setup, move the folder.")
	case w.opts.DataRootFromEnv:
		w.out.help("To move the folder, move it and set WEALTHDB_DATA_ROOT to its new place.")
	}
	return nil
}

// tilde writes a path under the home folder with ~.
func tilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return p
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}
