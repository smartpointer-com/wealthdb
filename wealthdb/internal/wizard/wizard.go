// Package wizard is `wealthdb config`: the setup of wealthdb.cfg, as a
// step-by-step wizard for a first setup and as a menu over its sections
// once the basics are set. Every confirmed answer is saved at once, as
// an edit of the file's own text (package jsonedit), so a run stopped
// part-way leaves what was answered, and a run again starts from it.
//
// The input is read a line at a time, so tests and a terminal that
// cannot do more drive it with typed answers. Given a Terminal, the
// menu reads single keys in raw mode instead.
package wizard

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Options is what a run of the setup works with.
type Options struct {
	// ConfigPath is the config file the setup edits.
	ConfigPath string
	// DataRoot is the data folder the wrapper mounts: the database and
	// every collector's data. DataRootFromEnv says WEALTHDB_DATA_ROOT
	// names it, so the paths the setup writes go through the variable.
	DataRoot        string
	DataRootFromEnv bool
	// Locale is the host's locale, whose region guesses the currency.
	Locale string
	In     io.Reader
	Out    io.Writer
	// Terminal, when set, puts the input in raw mode for the menu.
	Terminal Terminal
	Actions  Actions
	// Wizard walks the wizard whatever the config holds.
	Wizard bool
	// Section, when set, opens that section alone.
	Section string
	// Command is the command that runs the setup again, named to a
	// person who stopped part-way. Empty: `wealthdb config`.
	Command string
	// InContainer says the engine runs in a container, where localhost
	// is the container itself and not this computer.
	InContainer bool
	// Colors colours the output: for a terminal that takes colours.
	Colors Palette
}

// Terminal switches the input to raw mode and back. Rows is the height
// of its window in lines, 0 when unknown: a menu taller than the window
// shows the part around the marked line.
type Terminal interface {
	Raw() (restore func(), err error)
	Rows() int
}

// Actions are what the setup runs in the engine: each prints as its
// command does. A nil action is not offered.
type Actions struct {
	// Init creates the database, as `wealthdb init` does.
	Init func() error
	// Load loads one source, as `wealthdb load <id>` does.
	Load func(id string) error
	// Reset removes one source's data, as `wealthdb reset <id>` does.
	Reset func(id string) error
	// Loaded are the sources the database at gold holds.
	Loaded func(gold string) ([]string, error)
	// TestModel sends the model a short question and returns its reply.
	// A failure the server answered is a *ModelRefused.
	TestModel func(m config.ModelConfig) (string, error)
}

// setup is one run.
type setup struct {
	opts  Options
	store *store
	in    *bufio.Reader
	p     *prompter
	out   *screen
	// walking: the run is in the wizard, whose Banks and brokers step
	// wants a source before it goes on.
	walking bool
	// then are the commands a change of this run calls for: the start
	// of a server turned on, the first run of the AI model. They join the
	// to-do block the run ends with.
	then []Step
}

// Sections are the command-line names of the sections, in menu order.
func Sections() []string {
	var out []string
	for _, s := range sections() {
		out = append(out, s.slug())
	}
	return out
}

// Run is one run of `wealthdb config`: the section named, the wizard,
// or the menu. A config the engine refuses is reported and left alone.
// A q or the end of the input stops the run, and every answer confirmed
// before it is on disk.
func Run(o Options) error {
	abs, target, data, err := configFile(o.ConfigPath)
	if err != nil {
		return err
	}
	st, _, err := config.InspectBytes(data, abs)
	if st == config.StateRefused {
		return fmt.Errorf("%w%s\nFix the file, then run %s again", err, position(data, err), cmp.Or(o.Command, "wealthdb config"))
	}
	w := &setup{opts: o}
	s, err := openStore(abs, target, data, w.dataPath("wealthdb.db"))
	if err != nil {
		return err
	}
	in := bufio.NewReader(o.In)
	out := newScreen(o.Out, o.Colors)
	w.store, w.in, w.p, w.out = s, in, &prompter{in: in, out: out}, out

	switch {
	case o.Section != "":
		sec, ok := sectionNamed(o.Section)
		if !ok {
			return fmt.Errorf("no section %q (want one of %s)", o.Section, strings.Join(Sections(), ", "))
		}
		w.out.title(sec.title)
		err = sec.edit(w)
	case o.Wizard || st != config.StateValid || !w.answered():
		if !o.Wizard && st != config.StateAbsent {
			w.out.help("Going on with the setup. Each step shows the saved answer; Enter keeps it.")
		}
		err = w.wizard()
	default:
		err = w.menu()
	}
	quit := errors.Is(err, errQuit)
	if err != nil && !quit {
		return err
	}
	// The run ends with what to do next, set apart so that it reads as
	// an instruction.
	if quit {
		if !w.answered() {
			fmt.Fprint(w.out, Stopped(o.Colors, o.Command))
			return nil
		}
		w.out.gap()
		fmt.Fprintln(w.out, "Stopped. Every answer so far is saved.")
	}
	fmt.Fprint(w.out, ToDo(o.Colors, w.todo()))
	return nil
}

// Stopped is what a setup that stops part-way prints: that the answers
// are saved, and the command that goes on, on a line of its own.
func Stopped(p Palette, command string) string {
	return fmt.Sprintf("\nStopped. Every answer so far is saved.\n\n"+
		"To go on with the setup, run this command:\n\n    %s\n\n", p.Cmd(cmp.Or(command, "wealthdb config")))
}

// Step is one command of a to-do block, with what it does. Does may
// hold several lines.
type Step struct {
	Command, Does string
}

// ToDo is the block a setup command ends with: the commands to run
// next, numbered, each on a line of its own with space around it,
// between two rules. It stands apart from the text before it, so it
// reads as an instruction. No step gives no block. p colours the title,
// the commands and what each does.
func ToDo(p Palette, steps []Step) string {
	if len(steps) == 0 {
		return ""
	}
	rule := p.Help(strings.Repeat("-", 72))
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n%s\n\n", rule, p.Title("What to do now"))
	if len(steps) == 1 {
		b.WriteString("Run this command in the terminal:\n")
	} else {
		b.WriteString("Run these commands in the terminal, in this order:\n")
	}
	for i, s := range steps {
		fmt.Fprintf(&b, "\n  %d.  %s\n\n", i+1, p.Cmd(s.Command))
		for _, line := range strings.Split(s.Does, "\n") {
			fmt.Fprintf(&b, "      %s\n", p.Help(line))
		}
	}
	fmt.Fprintf(&b, "%s\n", rule)
	return b.String()
}

// todo are the commands this setup leaves to run: the database's
// creation, the first sign-in of each collector without data, the load
// while a source is not loaded, and what this run's changes call for
// (setup.then). While a wizard step has no answer, the setup itself comes
// first, in place of the database and the data.
func (w *setup) todo() []Step {
	var steps []Step
	if !w.answered() {
		steps = append(steps, Step{cmp.Or(w.opts.Command, "wealthdb config"),
			"Goes on with the setup: a step has no answer yet."})
		return append(steps, w.then...)
	}
	if !w.goldExists() {
		steps = append(steps, Step{"wealthdb init", "Creates the database."})
	}
	if es, err := w.entries(); err == nil {
		loaded := w.loaded()
		unloaded := false
		for _, e := range es {
			unloaded = unloaded || !loaded[e.src.ID]
		}
		steps = append(steps, collectSteps(feedsOf(es), unloaded)...)
	}
	return append(steps, w.then...)
}

// thenRun adds a command to setup.then, once.
func (w *setup) thenRun(s Step) {
	if !slices.ContainsFunc(w.then, func(t Step) bool { return t.Command == s.Command }) {
		w.then = append(w.then, s)
	}
}

// answered says each wizard step holds an answer: a currency, a source,
// and a web and an mcp member, on or off.
func (w *setup) answered() bool {
	d := w.store.doc
	return d.Has("default_currency") && d.Len("silver_sources") > 0 && d.Has("web") && d.Has("mcp")
}

// header names the file and the data folder.
func (w *setup) header() {
	w.out.title("wealthdb config")
	fmt.Fprintf(w.out, "Config file:  %s\n", w.store.path)
	fmt.Fprintf(w.out, "Data folder:  %s\n", w.opts.DataRoot)
}

// pal is the palette of the run's screen; none before there is one.
func (w *setup) pal() Palette {
	if w.out == nil {
		return Palette{}
	}
	return w.out.Palette
}

// wizard walks the four basic steps, then names what to run next.
func (w *setup) wizard() error {
	w.walking = true
	defer func() { w.walking = false }()
	w.header()
	w.out.gap()
	w.out.help("Every answer is saved at once. Press Enter to keep the value in brackets.\n" +
		"Type q to stop; wealthdb config goes on from there.")
	steps := basicSections[:4]
	for i, sec := range steps {
		w.out.title(fmt.Sprintf("Step %d of %d: %s", i+1, len(steps), sec.title))
		var err error
		switch sec.title {
		case "Dashboards":
			err = w.editWeb(false)
		case "MCP server":
			err = w.editMCP(false)
		default:
			err = sec.edit(w)
		}
		if err != nil {
			return err
		}
	}
	return w.finish()
}

// finish shows the saved setup, offers to create the database, and
// names the collect commands and the load.
func (w *setup) finish() error {
	w.out.title("Your setup:")
	for _, sec := range basicSections {
		fmt.Fprintf(w.out, "  %-18s %s\n", sec.title, sec.summary(w))
	}
	if !w.goldExists() && w.opts.Actions.Init != nil {
		w.out.gap()
		fmt.Fprintln(w.out, "The database does not exist yet.")
		if _, err := w.p.line("Press Enter to create it now, or q to stop: "); err != nil {
			return err
		}
		w.out.gap()
		err := w.opts.Actions.Init()
		w.out.outside()
		if err != nil {
			return err
		}
	}
	return nil
}

func feedsOf(es []entry) [][]config.Feed {
	out := make([][]config.Feed, len(es))
	for i, e := range es {
		out[i] = e.feeds
	}
	return out
}

// NextSteps is the to-do block that follows a new database: the first
// sign-in of each collector whose data is not there yet, then the load.
// sources are the feeds of each configured source.
func NextSteps(p Palette, sources [][]config.Feed) string {
	return ToDo(p, collectSteps(sources, true))
}

// collectSteps are the first sign-in of each collector whose data is
// not there yet, in config order, then the load when load says a source
// is not loaded or a collector waits.
func collectSteps(sources [][]config.Feed, load bool) []Step {
	var steps []Step
	var waiting []string
	for _, feeds := range sources {
		for _, f := range feeds {
			command := config.CollectCommand(f)
			if f.Collector == "" || exists(f.Path) || slices.Contains(waiting, command) {
				continue
			}
			waiting = append(waiting, command)
			c, _ := silver.CollectorNamed(f.Collector)
			steps = append(steps, Step{command,
				"Starts collecting the data of " + cmp.Or(c.Display, f.Collector) + ".\n" +
					"It lists the sign-in details to save first, and the commands\n" +
					"that follow it."})
		}
	}
	switch {
	case len(waiting) > 0:
		steps = append(steps, Step{"wealthdb load -a",
			"Brings the database of every source into the one database that\nthe reports read. Run it when the steps above are done."})
	case load:
		steps = append(steps, Step{"wealthdb load -a", "Brings the database of every source into the one database that\nthe reports read."})
	}
	return steps
}

// menuItem is one line of the menu: a section, walking the wizard, or
// the check of the file. group is the heading it stands under.
type menuItem struct {
	title, value, group string
	open                func() error
}

func (w *setup) menuItems() []menuItem {
	var out []menuItem
	walk := menuItem{title: "Walk through the basic setup again", group: groupBasic, open: w.wizard}
	for _, sec := range sections() {
		// The walk closes the basic group.
		if sec.group != groupBasic && walk.open != nil {
			out = append(out, walk)
			walk.open = nil
		}
		out = append(out, menuItem{title: sec.title, value: sec.summary(w), group: sec.group, open: func() error {
			w.out.title(sec.title)
			return sec.edit(w)
		}})
	}
	// The check of the file closes the last group.
	return append(out, menuItem{title: "Validate the config file", group: groupFile, open: func() error {
		w.out.title("Validate the config file")
		return w.validate()
	}})
}

// renderMenu is the menu's text: the items under the heading of their
// group, the line at cursor marked when raw. In a window of rows lines
// (0: any height) that the menu does not fit, the items around the
// marked line show, between notes that more stand above or below.
func (w *setup) renderMenu(items []menuItem, cursor int, raw bool, rows int) string {
	p := w.pal()
	var body []string
	at, group := 0, ""
	for i, it := range items {
		if it.group != group {
			if group != "" {
				body = append(body, "")
			}
			group = it.group
			body = append(body, " "+p.Title(group))
		}
		num, rest := fmt.Sprintf("%3d", i+1), it.title
		if it.value != "" {
			rest = fmt.Sprintf("%-28s %s", it.title, it.value)
		}
		if raw && i == cursor {
			// The marked line: a > before it, and in colours a bar of
			// reverse video across the menu's width.
			at = len(body)
			line := fmt.Sprintf(" > %s  %s", num, rest)
			if p.on {
				line = p.Mark(fmt.Sprintf("%-78s", line))
			}
			body = append(body, line)
			continue
		}
		body = append(body, fmt.Sprintf("   %s  %s", p.Key(num), rest))
	}
	body = append(body, "", "     "+p.Key("q")+"  Quit")
	// The config's path ends the first line, at column 65 when it is
	// short. A long one keeps its end, the part that tells two configs
	// apart, and stays within the screen's width, two spaces after the
	// title.
	const title = "wealthdb config"
	path := clipLeft(tilde(w.store.path), width-len(title)-2)
	gap := strings.Repeat(" ", max(50-utf8.RuneCountInString(path), 2))
	head := []string{p.Title(title) + p.Help(gap+path), ""}
	foot := []string{"", p.Help(" Every change is saved at once.")}
	if raw {
		foot[1] = p.Help(" Up/Down or a number move   Enter open   q quit   Every change is saved at once.")
	}
	// One line stays free for the cursor below the menu.
	if fit := rows - 1 - len(head) - len(foot); rows > 0 && fit >= 5 && len(body) > fit {
		start := min(max(at-fit/2, 0), len(body)-fit)
		clipped := append([]string(nil), body[start:start+fit]...)
		if start > 0 {
			clipped[0] = p.Help("        ... more above")
		}
		if start+fit < len(body) {
			clipped[fit-1] = p.Help("        ... more below")
		}
		body = clipped
	}
	return strings.Join(append(append(head, body...), foot...), "\n") + "\n"
}

// menu lists every section with its saved value and opens the one
// picked: by a typed number, or in raw mode by the arrows and Enter.
func (w *setup) menu() error {
	if w.opts.Terminal != nil {
		return w.rawMenu()
	}
	for {
		// The file may have been edited by hand since the menu was shown.
		_ = w.store.refresh()
		items := w.menuItems()
		w.out.gap()
		fmt.Fprint(w.out, w.renderMenu(items, 0, false, 0))
		ans, err := w.p.line("Type a number and press Enter, or q to quit: ")
		if err != nil {
			return quietQuit(err)
		}
		k, err := strconv.Atoi(ans)
		if err != nil || k < 1 || k > len(items) {
			w.out.fail("  Type a number from 1 to %d.", len(items))
			continue
		}
		if err := items[k-1].open(); err != nil {
			return err
		}
	}
}

// quietQuit is a quit from the menu itself: the normal way out, which
// needs no word on what is saved.
func quietQuit(err error) error {
	if errors.Is(err, errQuit) {
		return nil
	}
	return err
}

// rawMenu is the menu in raw mode. A section's editor runs with the
// terminal restored, and the menu comes back once it is read.
func (w *setup) rawMenu() error {
	cursor, typed := 0, 0
	for {
		_ = w.store.refresh()
		items := w.menuItems()
		restore, err := w.opts.Terminal.Raw()
		if err != nil {
			w.opts.Terminal = nil
			return w.menu()
		}
		open := -1
		for open < 0 {
			w.out.clear()
			text := w.renderMenu(items, cursor, true, w.opts.Terminal.Rows())
			fmt.Fprint(w.out, strings.ReplaceAll(text, "\n", "\r\n"))
			key, digit, err := ReadKey(w.in)
			if err != nil {
				restore()
				return err
			}
			switch key {
			case KeyUp:
				cursor = (cursor + len(items) - 1) % len(items)
				typed = 0
			case KeyDown:
				cursor = (cursor + 1) % len(items)
				typed = 0
			case KeyDigit:
				typed = typed*10 + int(digit)
				if typed < 1 || typed > len(items) {
					typed = int(digit)
				}
				if typed >= 1 && typed <= len(items) {
					cursor = typed - 1
				}
			case KeyEnter:
				open = cursor
				typed = 0
			case KeyQuit, KeyInterrupt, KeyEOF:
				restore()
				fmt.Fprintln(w.out)
				return nil
			}
		}
		restore()
		w.out.clear()
		if err := items[open].open(); err != nil {
			return err
		}
		// The menu clears the screen. A section that ended at a prompt
		// was read; one that printed after the last answer waits for
		// Enter, unless all it printed is the confirmation of a change,
		// which the menu's summary shows too.
		if w.out.unread() {
			if _, err := w.p.line("Press Enter to go back to the menu: "); err != nil {
				return err
			}
		}
	}
}
