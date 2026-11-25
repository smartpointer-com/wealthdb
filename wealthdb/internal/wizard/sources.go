package wizard

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/jsonedit"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// entry is one source of the config's silver_sources, as the Banks and
// brokers section shows it.
type entry struct {
	src   config.SilverSource // as written
	feeds []config.Feed       // the paths expanded
	// hand: the entry is not in a shape the section writes, so the
	// section leaves it alone. Either the path form of a kind that takes
	// subsources, or a file no collector of the catalogue writes.
	hand    bool
	display string
}

// entries are the config's sources, in config order.
func (w *setup) entries() ([]entry, error) {
	var raw []config.SilverSource
	if _, err := w.store.doc.Decode(&raw, "silver_sources"); err != nil {
		return nil, fmt.Errorf("silver_sources: %w", err)
	}
	out := make([]entry, len(raw))
	for i, s := range raw {
		exp := s
		exp.Path = w.expand(s.Path)
		exp.Subsources = slices.Clone(s.Subsources)
		for j := range exp.Subsources {
			exp.Subsources[j].Path = w.expand(exp.Subsources[j].Path)
		}
		e := entry{src: s, feeds: exp.Feeds()}
		if src, err := silver.Get(s.Kind); err == nil && s.Path != "" && len(src.Subsources) > 0 {
			e.hand = true
		}
		var names []string
		for _, f := range e.feeds {
			c, ok := silver.CollectorNamed(f.Collector)
			if !ok {
				e.hand = true
				continue
			}
			names = append(names, c.Display)
		}
		e.display = strings.Join(names, " + ")
		if len(e.feeds) > 0 && s.ID != e.feeds[0].Collector && s.ID != s.Kind {
			e.display += " (" + s.ID + ")"
		}
		if e.hand {
			e.display = s.ID + " (edited by hand)"
		}
		out[i] = e
	}
	return out, nil
}

// expand is a config path made absolute; a path that cannot be expanded
// stays as written, and names no file.
func (w *setup) expand(p string) string {
	if p == "" {
		return ""
	}
	if exp, err := config.ExpandPath(p, w.store.path); err == nil {
		return exp
	}
	return p
}

// exists says a regular file is at p.
func exists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// stateOf is where an entry stands: waiting for its first download (no
// silver file yet), collected but not loaded, or loaded. A feed of
// several still waiting is named after the state. A loaded source whose
// file is gone says so: its load fails until the file is back.
func stateOf(e entry, loaded map[string]bool) string {
	var waiting []string
	for _, f := range e.feeds {
		if !exists(f.Path) {
			c, _ := silver.CollectorNamed(f.Collector)
			waiting = append(waiting, cmp.Or(c.Display, f.Path))
		}
	}
	var st string
	switch {
	case loaded[e.src.ID]:
		st = "loaded"
	case len(waiting) == len(e.feeds):
		return "waiting for its first download"
	default:
		st = "collected, not loaded"
	}
	switch {
	case len(waiting) > 0 && loaded[e.src.ID]:
		st += "; the data file of " + strings.Join(waiting, ", ") + " is missing"
	case len(waiting) > 0 && len(e.feeds) > 1:
		st += "; " + strings.Join(waiting, ", ") + " waiting for its first download"
	}
	return st
}

// found is collected data the config does not name: the collector, the
// file, and for a collector that keeps one per login, the login's name.
type found struct {
	c    silver.Collector
	file string // absolute
	item string
}

// discovered is the collected data under the data folder that no entry
// names, in catalogue order.
func (w *setup) discovered(es []entry) []found {
	return discover(w.opts.DataRoot, feedsOf(es))
}

// Unlisted names the collected data under dataRoot that the config does
// not name, for the collectors it does name: a bank linked and loaded
// after the setup, which a load would pass over in silence. sources are
// the feeds of each configured source.
func Unlisted(dataRoot string, sources [][]config.Feed) []string {
	listed := map[string]bool{}
	for _, feeds := range sources {
		for _, f := range feeds {
			listed[f.Collector] = true
		}
	}
	var out []string
	for _, f := range discover(dataRoot, sources) {
		if listed[f.c.Name] {
			out = append(out, foundName(f))
		}
	}
	return out
}

// discover is the collected data under dataRoot that no feed of sources
// names, in catalogue order.
func discover(dataRoot string, sources [][]config.Feed) []found {
	named := map[string]bool{}
	for _, feeds := range sources {
		for _, f := range feeds {
			named[filepath.Clean(f.Path)] = true
		}
	}
	var out []found
	for _, c := range silver.Catalogue() {
		dir := filepath.Join(dataRoot, c.Name)
		if c.PerItem {
			items, _ := os.ReadDir(dir)
			for _, it := range items {
				p := filepath.Join(dir, it.Name(), it.Name()+".db")
				if it.IsDir() && exists(p) && !named[p] {
					out = append(out, found{c: c, file: p, item: it.Name()})
				}
			}
			continue
		}
		for _, f := range c.Files {
			if p := filepath.Join(dir, f); exists(p) && !named[p] {
				out = append(out, found{c: c, file: p})
			}
		}
	}
	return out
}

// dataPath is a path under the data folder as the config writes it:
// through ${WEALTHDB_DATA_ROOT} when the variable stands for the folder
// in this config (the environment names it, or the config is a setup of
// its own), else absolute.
func (w *setup) dataPath(parts ...string) string {
	if w.opts.DataRootFromEnv || w.opts.OwnSetup {
		return "${WEALTHDB_DATA_ROOT}/" + strings.Join(parts, "/")
	}
	return filepath.Join(append([]string{w.opts.DataRoot}, parts...)...)
}

// freeID is want, or the first of want-2, want-3, … no entry uses.
func freeID(es []entry, want string) string {
	used := map[string]bool{}
	for _, e := range es {
		used[e.src.ID] = true
	}
	if !used[want] {
		return want
	}
	for n := 2; ; n++ {
		if id := fmt.Sprintf("%s-%d", want, n); !used[id] {
			return id
		}
	}
}

// pathEntry is a source of the path form.
func pathEntry(id, kind, path string) jsonedit.Object {
	return jsonedit.Object{{Key: "id", Value: id}, {Key: "kind", Value: kind}, {Key: "path", Value: path}}
}

// subsource is one subsource of a source.
func subsource(kind, path string) jsonedit.Object {
	return jsonedit.Object{{Key: "kind", Value: kind}, {Key: "path", Value: path}}
}

// addResult is what a pick did.
type addResult int

const (
	addedNew      addResult = iota // a new entry, or a subsource of one
	alreadyListed                  // the config names the collector's data already
	handEdited                     // the entry it belongs to is edited by hand
	collectFirst                   // the entry is loaded, and the new feed has no data yet
)

// silverParts is where a silver file of collector c lies under the data
// folder, as the parts of its path: file in the collector's folder, or
// <item>/<item>.db there for a per-item collector.
func silverParts(c silver.Collector, file, item string) []string {
	if c.PerItem {
		return []string{c.Name, item, item + ".db"}
	}
	return []string{c.Name, file}
}

// silverFile is the absolute path of silverParts.
func (w *setup) silverFile(c silver.Collector, file, item string) string {
	return filepath.Join(append([]string{w.opts.DataRoot}, silverParts(c, file, item)...)...)
}

// entryIndex is the place in d's silver_sources of the source with id,
// or -1 when d lists none.
func entryIndex(d *jsonedit.Doc, id string) int {
	type named struct {
		ID string `json:"id"`
	}
	var raw []named
	if _, err := d.Decode(&raw, "silver_sources"); err != nil {
		return -1
	}
	return slices.IndexFunc(raw, func(s named) bool { return s.ID == id })
}

// gone is the refusal of a change to a source the file lists no longer:
// an edit by hand took it out since the list was shown.
func gone(id string) error {
	return &refusal{err: fmt.Errorf("%s is no longer in the file", id)}
}

// removeEntry removes the source with id from d. The entry is found by
// its id in d itself, so an edit by hand that moved it removes no other.
func removeEntry(d *jsonedit.Doc, id string) error {
	i := entryIndex(d, id)
	if i < 0 {
		return gone(id)
	}
	return d.Remove("silver_sources", i)
}

// add registers one silver file of collector c: file is its name under
// the collector's data folder (a login's folder for a per-item
// collector, whose item names the entry). The entry id is the file's
// name, the kind's for an entry of subsources, or the item, made free.
func (w *setup) add(c silver.Collector, file, item string) (addResult, error) {
	es, err := w.entries()
	if err != nil {
		return 0, err
	}
	rel := silverParts(c, file, item)
	abs := w.silverFile(c, file, item)
	for _, e := range es {
		for _, f := range e.feeds {
			if filepath.Clean(f.Path) == abs {
				return alreadyListed, nil
			}
		}
	}
	if c.Subsource != "" {
		for _, e := range es {
			if e.src.Kind != c.Kind {
				continue
			}
			if e.src.Path != "" || e.hand {
				return handEdited, nil
			}
			for _, sub := range e.src.Subsources {
				if sub.Kind == c.Subsource {
					// The feed is listed already, at another path.
					return handEdited, nil
				}
			}
			// A loaded source loads every feed it lists or none
			// (wealthdb load), so a feed with no data yet would stop it.
			if !exists(abs) && w.loaded()[e.src.ID] {
				return collectFirst, nil
			}
			return addedNew, w.store.apply(func(d *jsonedit.Doc) error {
				i := entryIndex(d, e.src.ID)
				if i < 0 {
					return gone(e.src.ID)
				}
				return d.Append(subsource(c.Subsource, w.dataPath(rel...)), "silver_sources", i, "subsources")
			})
		}
		id := freeID(es, c.Kind)
		return addedNew, w.store.apply(func(d *jsonedit.Doc) error {
			return d.Append(jsonedit.Object{{Key: "id", Value: id}, {Key: "kind", Value: c.Kind},
				{Key: "subsources", Value: []any{subsource(c.Subsource, w.dataPath(rel...))}}}, "silver_sources")
		})
	}
	want := strings.TrimSuffix(file, filepath.Ext(file))
	if c.PerItem {
		want = item
	}
	id := freeID(es, want)
	return addedNew, w.store.apply(func(d *jsonedit.Doc) error {
		return d.Append(pathEntry(id, c.Kind, w.dataPath(rel...)), "silver_sources")
	})
}

// pick adds collector c: its first file always, and its other files
// once they hold data. It prints what happened and the next step.
func (w *setup) pick(c silver.Collector) error {
	item := ""
	if c.PerItem {
		// Banks linked and loaded already are added at once, by their
		// names; a name is asked only for one more.
		done, err := w.addCollected(c)
		if err != nil || done {
			return err
		}
		// The bank is linked after the setup, under the name given here:
		// the name is a new one of the person's choice, and Enter backs
		// out with nothing added.
		w.out.title(c.Display)
		w.out.help("It links one bank at a time. Choose a short name for the bank you want\n" +
			"to add, such as chase or schwab. After this setup, you link the bank\n" +
			"with that name:")
		w.out.command(w.opts.collect(config.CollectArgs(config.Feed{Collector: c.Name, Item: "<name>"})))
		if c.Logins != "" {
			w.out.help("A bank you linked already keeps its name. To see those names, run:")
			w.out.command(w.opts.collect(c.Name + " " + c.Logins))
		}
		item, err = w.p.askValid("Name for the bank (Enter to go back)", "", func(s string) (string, error) {
			if s == "" {
				return "", nil
			}
			return checkID(s)
		})
		if err != nil || item == "" {
			return err
		}
	}
	files := c.Files
	if c.PerItem {
		files = []string{item + ".db"}
	}
	res, err := w.add(c, files[0], item)
	if err != nil {
		return w.explain(err)
	}
	switch res {
	case alreadyListed:
		fmt.Fprintf(w.out, "%s is in the list already.\n", c.Display)
	case handEdited:
		w.out.fail("The %s entry is edited by hand. Add %s to it in %s.", c.Kind, c.Display, w.store.path)
		return nil
	case collectFirst:
		w.out.say("The %s source is loaded already, and %s has no data yet. Collect it first:", c.Kind, c.Display)
		w.out.command(w.opts.collect(config.CollectArgs(config.Feed{Collector: c.Name, Item: item})))
		w.out.say("Then run %s again: the data shows under \"Collected, but not in the list\".", w.cmd("config"))
		return nil
	default:
		if item != "" {
			w.out.ok("Added %s: %s.", c.Display, item)
		} else {
			w.out.ok("Added %s.", c.Display)
		}
		if st, err := silver.Get(c.Kind); err == nil && st.Settings != nil && st.Settings.Key == "relationships" &&
			w.pairComplete(c.Kind) {
			w.out.help("To pair the two %s feeds, add \"relationships\" to the entry (docs/DESIGN.md §5.1).",
				strings.ToUpper(c.Kind))
		}
	}
	for _, f := range files[1:] {
		if exists(filepath.Join(w.opts.DataRoot, c.Name, f)) {
			if _, err := w.add(c, f, ""); err != nil {
				return w.explain(err)
			}
		}
	}
	return w.afterAdd(w.silverFile(c, files[0], item))
}

// addCollected offers the logins of per-item collector c whose data is
// collected and that the config does not name, all at once. It reports
// done when it added them and no further login is wanted.
func (w *setup) addCollected(c silver.Collector) (bool, error) {
	es, err := w.entries()
	if err != nil {
		return false, err
	}
	var have []found
	for _, f := range w.discovered(es) {
		if f.c.Name == c.Name {
			have = append(have, f)
		}
	}
	if len(have) == 0 {
		return false, nil
	}
	names := make([]string, len(have))
	for i, f := range have {
		names[i] = f.item
	}
	w.out.gap()
	w.out.say("Data is collected already for: %s.", strings.Join(names, ", "))
	yes, err := w.p.yesNo("Add them?", true)
	if err != nil || !yes {
		return false, err
	}
	for _, f := range have {
		if _, err := w.add(f.c, filepath.Base(f.file), f.item); err != nil {
			return false, w.explain(err)
		}
	}
	w.out.ok("Added %s: %s.", c.Display, strings.Join(names, ", "))
	// An entry of the collector that never got data is most often a
	// name that no login took: offer to drop it, the last entry first.
	es, err = w.entries()
	if err != nil {
		return false, err
	}
	loaded := w.loaded()
	for i := len(es) - 1; i >= 0; i-- {
		e := es[i]
		if len(e.feeds) != 1 || e.feeds[0].Collector != c.Name || exists(e.feeds[0].Path) || loaded[e.src.ID] {
			continue
		}
		w.out.gap()
		w.out.say("The entry %s has no data: no bank was linked under the name %s.", e.src.ID, e.feeds[0].Item)
		drop, err := w.p.yesNo("Remove it?", true)
		if err != nil {
			return false, err
		}
		if drop {
			if err := w.store.apply(func(d *jsonedit.Doc) error { return removeEntry(d, e.src.ID) }); err != nil {
				return false, w.explain(err)
			}
			w.out.ok("Removed %s.", e.src.ID)
		}
	}
	more, err := w.p.yesNo("Link one more bank?", false)
	return !more, err
}

// pairComplete says the entry of kind lists every subsource the kind
// takes.
func (w *setup) pairComplete(kind string) bool {
	es, err := w.entries()
	if err != nil {
		return false
	}
	src, _ := silver.Get(kind)
	for _, e := range es {
		if e.src.Kind == kind && len(e.src.Subsources) == len(src.Subsources) {
			return true
		}
	}
	return false
}

// afterAdd names the next step for the silver file at abs, which an
// entry lists: its collector's first sign-in, or, when the file is there
// already, a load of that entry.
func (w *setup) afterAdd(abs string) error {
	es, err := w.entries()
	if err != nil {
		return err
	}
	for _, e := range es {
		for _, f := range e.feeds {
			if filepath.Clean(f.Path) != abs {
				continue
			}
			if !exists(f.Path) {
				w.printNext(f)
				return nil
			}
			fmt.Fprintln(w.out, "Its data is collected already.")
			return w.offerLoad(e)
		}
	}
	return nil
}

// printNext prints the first step of collecting a collector's data:
// the command on a line of its own, with space around it, and when to
// run it.
func (w *setup) printNext(f config.Feed) {
	c, _ := silver.CollectorNamed(f.Collector)
	w.runLater("To collect the data of "+cmp.Or(c.Display, f.Collector), w.opts.collect(config.CollectArgs(f)))
}

// offerLoad offers to load e's collected data, when the database exists
// and the setup has a Load action.
func (w *setup) offerLoad(e entry) error {
	if !w.goldExists() {
		w.out.help("It loads once the database exists (%s, then %s).", w.cmd("init"), w.cmd("load -a"))
		return nil
	}
	if w.opts.Actions.Load == nil {
		return nil
	}
	ok, err := w.p.yesNo(fmt.Sprintf("Load %s now?", e.src.ID), true)
	if err != nil || !ok {
		return err
	}
	return w.load(e.src.ID)
}

// load runs the load of one source, as `wealthdb load <id>` does. Its
// callers offer it only with a Load action.
func (w *setup) load(id string) error {
	// The load prints as the command does, past the screen.
	w.out.gap()
	err := w.opts.Actions.Load(id)
	w.out.outside()
	if err != nil {
		w.out.fail("The load of %s failed: %s", id, err.Error())
	}
	return nil
}

// remove asks, then removes e's data from the database, as `wealthdb
// reset <id>` does, then e itself. The file is read again first, so an
// edit by hand is kept. A config that names e elsewhere is refused
// before anything is removed.
func (w *setup) remove(e entry, loaded map[string]bool) error {
	ok, err := w.p.yesNo(fmt.Sprintf("Remove %s from the list and its data from the database? "+
		"The downloaded files stay.", e.src.ID), false)
	if err != nil || !ok {
		return err
	}
	// The file may have been edited by hand since the list was shown:
	// the removal applies to the file as it is now.
	if err := w.store.refresh(); err != nil {
		return w.explain(err)
	}
	if entryIndex(w.store.doc, e.src.ID) < 0 {
		return w.explain(gone(e.src.ID))
	}
	text, err := w.store.prepare(func(d *jsonedit.Doc) error { return removeEntry(d, e.src.ID) })
	if err != nil {
		var r *refusal
		if errors.As(err, &r) {
			w.out.fail("Cannot remove %s: the file names it elsewhere.\n  %s\nEdit %s first.",
				e.src.ID, r.Error(), w.store.path)
			return nil
		}
		return err
	}
	if loaded[e.src.ID] && w.opts.Actions.Reset != nil {
		w.out.gap()
		err := w.opts.Actions.Reset(e.src.ID)
		w.out.outside()
		if err != nil {
			w.out.fail("Cannot remove %s: %s", e.src.ID, err.Error())
			return nil
		}
	}
	if err := w.store.commit(text); err != nil {
		return err
	}
	w.out.ok("Removed %s.", e.src.ID)
	return nil
}

// explain prints a change the engine refuses and goes on; any other
// error stops the command.
func (w *setup) explain(err error) error {
	var r *refusal
	if errors.As(err, &r) {
		w.out.fail("Not saved: %s", r.Error())
		return nil
	}
	return err
}

// checkID takes a name a config entry can carry.
func checkID(s string) (string, error) {
	if !config.IDPattern.MatchString(s) {
		return "", errors.New("Use letters, digits, - and _ only.")
	}
	return s, nil
}

// goldPath is the database the config names, expanded; the default
// under the data folder when it names none.
func (w *setup) goldPath() string {
	var p string
	if _, err := w.store.doc.Decode(&p, "gold_db"); err != nil || p == "" {
		return filepath.Join(w.opts.DataRoot, "wealthdb.db")
	}
	return w.expand(p)
}

// goldExists says the database is there.
func (w *setup) goldExists() bool {
	_, err := os.Stat(w.goldPath())
	return !errors.Is(err, fs.ErrNotExist)
}

// loaded are the sources the database holds; none when there is no
// database or it cannot be read.
func (w *setup) loaded() map[string]bool {
	if w.opts.Actions.Loaded == nil || !w.goldExists() {
		return nil
	}
	ids, err := w.opts.Actions.Loaded(w.goldPath())
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// fxSource is the collector the setup offers for exchange rates, as
// the catalogue states it; false when no adapter registers it.
func fxSource() (silver.Collector, bool) {
	return silver.CollectorNamed("fred")
}

// statesFX says a source of es states exchange rates.
func statesFX(es []entry) bool {
	for _, e := range es {
		for _, f := range e.feeds {
			if c, ok := silver.CollectorNamed(f.Collector); ok && c.FX {
				return true
			}
		}
	}
	return false
}

// unconvertible are the currencies the database holds amounts in that
// no exchange rate connects to currency (Actions.Unconvertible); none
// when there is no database, it cannot be read, or the setup has no such
// action.
func (w *setup) unconvertible(currency string) []string {
	if w.opts.Actions.Unconvertible == nil || !w.goldExists() {
		return nil
	}
	out, err := w.opts.Actions.Unconvertible(w.goldPath(), currency)
	if err != nil {
		return nil
	}
	return out
}

// fxHint is the line that says the exchange-rate source is needed,
// while no source of es states exchange rates. It applies in three
// cases, the first that holds:
//
//   - the database holds amounts in a currency no rate connects to the
//     reports' (unconvertible): the line names the currencies;
//   - a source's accounts are usually in a currency other than the
//     reports', by its group;
//   - a source of no one country, which can hold accounts in any
//     currency. Market and reference data holds no account.
//
// Empty when none holds.
func (w *setup) fxHint(es []entry) string {
	currency := w.currency()
	fx, ok := fxSource()
	if currency == "" || !ok || statesFX(es) {
		return ""
	}
	if missing := w.unconvertible(currency); len(missing) > 0 {
		return fmt.Sprintf("The reports are in %s, and the database holds amounts in %s with no exchange rate to %s. "+
			"Add %s so the reports can convert them.", currency, joinAnd(missing), currency, fx.Display)
	}
	var other, anywhere *silver.Collector
	for _, e := range es {
		for _, f := range e.feeds {
			c, ok := silver.CollectorNamed(f.Collector)
			if !ok || c.Group == silver.GroupMarketData {
				continue
			}
			switch cur := c.Group.Currency(); {
			case cur == "" && anywhere == nil:
				anywhere = &c
			case cur != "" && cur != currency && other == nil:
				other = &c
			}
		}
	}
	switch {
	case other != nil:
		return fmt.Sprintf("The reports are in %s and %s reports in %s. Add %s so the reports can convert.",
			currency, other.Display, other.Group.Currency(), fx.Display)
	case anywhere != nil:
		return fmt.Sprintf("The reports are in %s. %s can hold accounts in other currencies. Add %s so the reports can convert them.",
			currency, anywhere.Display, fx.Display)
	}
	return ""
}

// joinAnd is the names joined for a sentence: "a", "a and b",
// "a, b and c".
func joinAnd(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
