package wizard

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/jsonedit"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// A field is one member of the config whose value is a choice, a
// switch, a number or the path of a file: the settings the setup edits
// beyond the basics. A setting that takes a list or a map of objects is
// edited in the file.
type field struct {
	path  []any
	label string
	// does says in a line or two what the setting changes.
	does string
	kind fieldKind
	// choices are the values of a choice, each with what it means.
	choices []choice
	// min and max bound a number; unit follows it ("days").
	min, max float64
	unit     string
	// dflt is the value the engine takes when the member is not set;
	// nil when there is none.
	dflt any
	// example is the name of the file the hint for a file shows.
	example string
	doc     string
}

type fieldKind int

const (
	kindChoice fieldKind = iota // one of choices, written as the string
	kindSwitch                  // on or off, written as a boolean
	kindWhole                   // a whole number from min to max
	kindNumber                  // a number from min to max
	kindFile                    // the path of a file in the data folder
)

type choice struct{ value, does string }

// reportOptions are the reports' settings that take one value each.
var reportOptions = []field{
	{path: []any{"lots", "method"}, label: "Lot method", kind: kindChoice, dflt: string(canonical.BasisMethodFIFO), doc: "docs/LOTS.md §3.2",
		does: "Which purchase a sale is taken from, for the gains and the cost basis.",
		choices: []choice{
			{"fifo", "the earliest purchase first"},
			{"lifo", "the latest purchase first"},
			{"hifo", "the purchase with the highest cost first"},
			{"lofo", "the purchase with the lowest cost first"},
			{"average", "every purchase alike, at the average cost"},
		}},
	{path: []any{"lots", "missing_basis"}, label: "Missing cost basis", kind: kindChoice, dflt: string(lots.MissingIgnore),
		doc:  "docs/GAINS.md §8",
		does: "How the gains read a purchase whose cost no source states.",
		choices: []choice{
			{"ignore", "a figure that needs it stays blank"},
			{"zero", "it counts as 0: the conservative reading for tax"},
		}},
	{path: []any{"spending", "internal_transfer_matching", "window_days"}, label: "Transfer window", kind: kindWhole,
		min: 0, max: 30, unit: "days", dflt: float64(config.DefaultSpendMatchWindowDays), doc: "docs/SPENDING.md §3",
		does: "How many days apart the two sides of a move between your own accounts\nmay be, so that neither counts as spending or income."},
	{path: []any{"spending", "internal_transfer_matching", "tolerance_pct"}, label: "Transfer tolerance", kind: kindNumber,
		min: 0, max: 5, unit: "%", dflt: float64(config.DefaultSpendMatchTolerancePct), doc: "docs/SPENDING.md §3",
		does: "How much the two amounts of such a move may differ, in percent."},
	{path: []any{"returns_transfer_matching", "enabled"}, label: "Transfers in returns", kind: kindSwitch, dflt: false,
		doc:  "docs/DESIGN.md §5.8",
		does: "On: a move between two of your accounts nets out of every return that\ncovers both. Off: each side counts as money in or out."},
	{path: []any{"cashflow", "projection", "lookback_months"}, label: "Cash flow look-back", kind: kindWhole,
		min: 1, max: config.MaxCashflowLookbackMonths, unit: "months", dflt: float64(config.DefaultCashflowLookbackMonths),
		doc:  "docs/CASHFLOW.md §12",
		does: "How many past months the cash flow projection reads."},
	{path: []any{"yield", "horizon_months"}, label: "Income projection", kind: kindWhole,
		min: 1, max: canonical.MaxHorizonMonths, unit: "months", dflt: float64(canonical.DefaultHorizonMonths),
		doc:  "docs/YIELD.md §8",
		does: "How many months ahead the income projection shows by default."},
	{path: []any{"exposure", "max_gap_days"}, label: "Fund composition age", kind: kindWhole,
		min: 1, max: 3650, unit: "days", dflt: float64(canonical.DefaultMaxGapDays), doc: "docs/EXPOSURE.md §4",
		does: "How many days a fund's published composition may be from the day of an\nexposure report and still be used."},
}

// ledgerFiles are the settings that name a ledger: a file kept by hand
// that a load reads.
var ledgerFiles = []field{
	{path: []any{"spending", "pins"}, label: "Spending pins", kind: kindFile, example: "spending-pins.csv", doc: "docs/DESIGN.md §13.10",
		does: "A CSV file that sets the category of single payments no rule places."},
	{path: []any{"income", "pins"}, label: "Income pins", kind: kindFile, example: "income-pins.csv", doc: "docs/DESIGN.md §13.10",
		does: "The same for money coming in."},
	{path: []any{"equity_transfers"}, label: "Equity transfers", kind: kindFile, example: "equity-transfers.csv", doc: "docs/DESIGN.md §13.9",
		does: "A CSV file of shares moved into or out of an account that no bank states\nas a flow."},
	{path: []any{"exposure", "declarations"}, label: "Exposure declarations", kind: kindFile, example: "exposure-declarations.ndjson",
		doc:  "docs/EXPOSURE.md §9",
		does: "An NDJSON file of what a holding is made of, where no source says."},
}

// modelSent are the settings of what the AI model is sent.
var modelSent = []field{
	{path: []any{"spending", "categorization", "context"}, label: "What it is sent", kind: kindChoice, dflt: config.DefaultSpendContext,
		doc: "docs/SPENDING.md §5", does: "How much of a payment the model sees when it sorts spending.",
		choices: []choice{
			{"merchant", "the merchant's name alone: the most private"},
			{"descriptor", "also the bank's text for the payment"},
			{"transaction", "also its date, amount, account kind and nearby payments"},
		}},
	{path: []any{"spending", "categorization", "descriptor_samples"}, label: "Bank texts per merchant", kind: kindWhole,
		min: 0, max: 20, dflt: float64(config.DefaultSpendDescriptorSamples), doc: "docs/SPENDING.md §5",
		does: "With descriptor or transaction: how many of the bank's texts it sees for\none merchant."},
	{path: []any{"spending", "categorization", "fence_person_names"}, label: "Hold back people's names", kind: kindSwitch,
		dflt: config.DefaultFencePersonNames, doc: "docs/SPENDING.md §5",
		does: "On: a payee that is a person's bare name is never sent. Turn it off only\nfor a model on this computer."},
}

// editedPaths are the members the setup's editors write: the models and
// every field. A setting edited in the file counts as set only for what
// lies outside them.
func editedPaths() [][]any {
	var out [][]any
	for _, u := range modelUses {
		out = append(out, u.path)
	}
	for _, fs := range [][]field{reportOptions, ledgerFiles, modelSent} {
		for _, f := range fs {
			out = append(out, f.path)
		}
	}
	return out
}

// fieldKeys are the dotted paths of fields.
func fieldKeys(fields []field) []string {
	var out []string
	for _, f := range fields {
		out = append(out, dotted(f.path))
	}
	return out
}

// saved is the value at f's member, and whether there is one.
func (w *setup) saved(f field) (any, bool) {
	var v any
	ok, err := w.store.doc.Decode(&v, f.path...)
	return v, ok && err == nil
}

// show writes a value of f as a person reads it.
func (f field) show(v any) string {
	var s string
	switch x := v.(type) {
	case bool:
		s = onOffWord(x)
	case float64:
		s = plain(x)
	case string:
		s = x
	default:
		s = fmt.Sprint(x)
	}
	switch f.unit {
	case "":
	case "%":
		s += "%"
	default:
		s += " " + f.unit
	}
	return s
}

// plain writes a number as short as it reads.
func plain(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }

// value is what f holds, as a person reads it.
func (w *setup) value(f field) string {
	if v, ok := w.saved(f); ok {
		if f.kind == kindFile {
			return tilde(fmt.Sprint(v))
		}
		return f.show(v)
	}
	if f.dflt == nil {
		return "not set"
	}
	return f.show(f.dflt) + " (the default)"
}

// shown is value for a list: what stands in for a value not set is
// dimmed.
func (w *setup) shown(f field) string {
	if _, ok := w.saved(f); ok {
		return w.value(f)
	}
	if f.dflt == nil {
		return w.out.Help("not set")
	}
	return f.show(f.dflt) + w.out.Help(" (the default)")
}

// fieldsSummary counts the fields set, for the menu.
func (w *setup) fieldsSummary(fields []field, none string) string {
	n := 0
	for _, f := range fields {
		if _, ok := w.saved(f); ok {
			n++
		}
	}
	if n == 0 {
		return none
	}
	return fmt.Sprintf("%d of %d set", n, len(fields))
}

// editFields lists fields with their values and edits the one picked,
// until Enter.
func (w *setup) editFields(intro string, fields []field) error {
	if intro != "" {
		w.out.help("%s", intro)
	}
	for {
		w.out.gap()
		for i, f := range fields {
			fmt.Fprintf(w.out, "  %s  %-26s %s\n", w.out.Key(letter(i)), f.label, w.shown(f))
		}
		ans, err := w.p.line(fmt.Sprintf("Type a letter (a to %s) to change one, or press Enter to go back: ", letter(len(fields)-1)))
		if err != nil {
			return err
		}
		if ans == "" {
			return nil
		}
		k, ok := letterIndex(strings.ToLower(ans))
		if !ok || k >= len(fields) {
			w.out.fail("  Type one of the letters on the list.")
			continue
		}
		if err := w.editField(fields[k]); err != nil {
			return err
		}
	}
}

// editField asks for one field's value. Enter keeps it. For a choice, a
// number or a file, - puts back the default (the member is removed); a
// switch takes on or off.
func (w *setup) editField(f field) error {
	// The title, what it does and its doc, then the choices between
	// blank lines, then the question.
	w.out.title(f.label)
	w.out.help("%s\nMore: %s", f.does, f.doc)
	cur, set := w.saved(f)
	reset := "- for the default"
	if f.dflt == nil {
		reset = "- to remove it"
	}
	var v any
	switch f.kind {
	case kindChoice:
		w.out.gap()
		for i, c := range f.choices {
			fmt.Fprintf(w.out, "  %s  %-12s %s\n", w.out.Key(strconv.Itoa(i+1)), c.value, w.out.Help(c.does))
		}
		for v == nil {
			ans, err := w.p.line(fmt.Sprintf("Type a number, %s, or press Enter to keep %s: ", reset, w.value(f)))
			if err != nil {
				return err
			}
			if ans == "" {
				return nil
			}
			if ans == "-" {
				if !set {
					return nil
				}
				return w.removeField(f)
			}
			if k, err := strconv.Atoi(ans); err == nil && k >= 1 && k <= len(f.choices) {
				v = f.choices[k-1].value
			} else if i := slices.IndexFunc(f.choices, func(c choice) bool { return strings.EqualFold(c.value, ans) }); i >= 0 {
				v = f.choices[i].value
			} else {
				w.out.fail("  Type one of the numbers on the list.")
			}
		}
	case kindSwitch:
		now, _ := cur.(bool)
		if !set {
			now, _ = f.dflt.(bool)
		}
		on, err := w.p.onOff(f.label, now)
		if err != nil {
			return err
		}
		v = on
	case kindWhole, kindNumber:
		var err error
		if v, err = w.askNumber(f, cur, set, reset); err != nil || v == nil {
			return err
		}
	case kindFile:
		var err error
		if v, err = w.askFile(f, cur, set); err != nil || v == nil {
			return err
		}
	}
	// The value it holds already, set or by default, is no change.
	if set && v == cur || !set && v == f.dflt {
		return nil
	}
	if err := w.store.apply(func(d *jsonedit.Doc) error { return d.Set(v, f.path...) }); err != nil {
		return w.explain(err)
	}
	w.out.ok("Saved: %s.", w.value(f))
	return nil
}

// askNumber asks for a number within f's bounds. nil: no change.
func (w *setup) askNumber(f field, cur any, set bool, reset string) (any, error) {
	what := "a whole number"
	if f.kind == kindNumber {
		what = "a number"
	}
	for {
		ans, err := w.p.line(fmt.Sprintf("Type %s from %s to %s, %s, or press Enter to keep %s: ",
			what, plain(f.min), plain(f.max), reset, w.value(f)))
		if err != nil {
			return nil, err
		}
		switch ans {
		case "":
			return nil, nil
		case "-":
			if !set {
				return nil, nil
			}
			return nil, w.removeField(f)
		}
		n, err := strconv.ParseFloat(strings.TrimSuffix(ans, "%"), 64)
		if err == nil && n >= f.min && n <= f.max && (f.kind == kindNumber || n == float64(int64(n))) {
			if set && cur == n {
				return nil, nil
			}
			return n, nil
		}
		w.out.fail("  Type %s from %s to %s.", what, plain(f.min), plain(f.max))
	}
}

// askFile asks for the path of a ledger, which must be a file in the
// data folder: the container engine mounts only the config and that
// folder, and the setup holds a config to what both kinds of engine can
// read. The path is written through ${WEALTHDB_DATA_ROOT} when the
// variable stands for that folder (dataPath). nil: no change.
func (w *setup) askFile(f field, cur any, set bool) (any, error) {
	w.out.help("The file must be in the data folder, %s.", tilde(w.opts.DataRoot))
	w.out.help("Type its path, such as %s.", tilde(filepath.Join(w.opts.DataRoot, "ledgers", f.example)))
	for {
		q := "Path (- removes it, Enter keeps it): "
		if !set {
			q = "Path (Enter to go back): "
		}
		ans, err := w.p.line(q)
		if err != nil {
			return nil, err
		}
		switch {
		case ans == "" || ans == "-" && !set:
			return nil, nil
		case ans == "-":
			return nil, w.removeField(f)
		}
		p, err := config.ExpandPath(ans, w.store.path)
		if err != nil {
			w.out.fail("  %s", err.Error())
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(w.opts.DataRoot), p)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			w.out.fail("  %s is not in the data folder, %s. Move the file there.", tilde(p), tilde(w.opts.DataRoot))
			continue
		}
		if _, err := os.Stat(p); err != nil {
			w.out.help("  There is no file there yet. A load reads it once it is there.")
		}
		v := w.dataPath(strings.Split(rel, string(filepath.Separator))...)
		if set && cur == v {
			return nil, nil
		}
		return v, nil
	}
}

// removeField removes f's member, and each block the removal leaves
// empty.
func (w *setup) removeField(f field) error {
	err := w.store.apply(func(d *jsonedit.Doc) error { return removeEmptied(d, f.path) })
	if err != nil {
		return w.explain(err)
	}
	w.out.ok("Saved: %s.", w.value(f))
	return nil
}

// removeEmptied removes the member at path, then each object on the way
// to it that the removal leaves empty.
func removeEmptied(d *jsonedit.Doc, path []any) error {
	if err := d.Remove(path...); err != nil {
		return err
	}
	for p := path[:len(path)-1]; len(p) > 0 && d.Has(p...) && d.Len(p...) == 0; p = p[:len(p)-1] {
		if err := d.Remove(p...); err != nil {
			return err
		}
	}
	return nil
}

func (w *setup) editReportOptions() error {
	return w.editFields("Settings of the reports that take one value each. Each shows its value,\nor the default the reports use.", reportOptions)
}

func (w *setup) editLedgerFiles() error {
	return w.editFields("Files kept by hand that a load reads. Each must be in the data folder.", ledgerFiles)
}

// editModelSent edits what the AI model is sent. income takes spending's
// block while it has none of its own.
func (w *setup) editModelSent() error {
	intro := ""
	if w.store.doc.Has("income", "categorization") {
		intro = "The income block has a categorization of its own in the file; these\nsettings are spending's."
	}
	return w.editFields(intro, modelSent)
}
