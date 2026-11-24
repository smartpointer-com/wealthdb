package wizard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/jsonedit"
)

// fieldLetter is the letter of the field labelled label in fields.
func fieldLetter(t *testing.T, fields []field, label string) string {
	t.Helper()
	for i, f := range fields {
		if f.label == label {
			return letter(i)
		}
	}
	t.Fatalf("no field %q", label)
	return ""
}

func TestReportOptions(t *testing.T) {
	h := newHarness(t)
	h.write(basics+"}\n", 0o600)
	h.section = "options"
	lot := fieldLetter(t, reportOptions, "Lot method")
	win := fieldLetter(t, reportOptions, "Transfer window")
	tol := fieldLetter(t, reportOptions, "Transfer tolerance")
	pairs := fieldLetter(t, reportOptions, "Transfers in returns")
	out, err := h.run(
		lot, "3", // hifo, by its number
		lot, "lifo", // by its name
		lot, "x", "", // not a choice, asked again; Enter keeps lifo
		win, "31", "2.5", "3", // out of range, not whole, then 3
		tol, "0.25%", // a number, with its sign
		pairs, "on",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.cfgOf()
	if cfg.Lots.Method != "lifo" || *cfg.Spending.InternalTransferMatching.WindowDays != 3 ||
		*cfg.Spending.InternalTransferMatching.TolerancePct != 0.25 || !cfg.ReturnsTransferMatching.Enabled {
		t.Errorf("not saved as typed:\n%s", h.read())
	}
	for _, w := range []string{
		"  a  Lot method                 fifo (the default)\n",
		// The title on a line of its own, the choices between blank lines.
		"\nLot method\nWhich purchase a sale is taken from, for the gains and the cost basis.\nMore: docs/LOTS.md §3.2\n\n  1  fifo ",
		"  5  average      every purchase alike, at the average cost\n\nType a number, - for the default",
		"Saved: hifo.", "Saved: lifo.", "  Type one of the numbers on the list.",
		"  Type a whole number from 0 to 30.", "Saved: 3 days.", "Saved: 0.25%.", "Saved: on.",
		"  a  Lot method                 lifo\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}

	// - puts back the default: the member goes, and each block it leaves
	// empty; a block that holds more keeps it.
	if _, err := h.run(lot, "-", win, "-", tol, "-", pairs, "off", ""); err != nil {
		t.Fatal(err)
	}
	d, _ := jsonedit.Parse([]byte(h.read()))
	if d.Has("lots") || d.Has("spending") {
		t.Errorf("an emptied block is left:\n%s", h.read())
	}
	if !d.Has("returns_transfer_matching", "enabled") {
		t.Errorf("off, a value of its own, was not written:\n%s", h.read())
	}
	h.cfgOf()

	// Enter at every field, the default typed, and - on a field that is
	// not set change nothing and say nothing was saved.
	before := h.read()
	out, err = h.run(lot, "", lot, "1", lot, "-", win, "5", win, "-", "")
	if err != nil {
		t.Fatal(err)
	}
	if h.read() != before || strings.Contains(out, "Saved") {
		t.Errorf("no change rewrote the file:\n%s\n%s", h.read(), out)
	}
}

// A ledger must be a file in the data folder, and is written through
// ${WEALTHDB_DATA_ROOT} when the variable names it.
func TestLedgerFiles(t *testing.T) {
	h := newHarness(t)
	h.rootFromEnv = true
	t.Setenv("WEALTHDB_DATA_ROOT", h.root)
	h.write(basics+"}\n", 0o600)
	h.collect("ledgers", "pins.csv")
	h.section = "ledgers"
	pins := fieldLetter(t, ledgerFiles, "Spending pins")
	eq := fieldLetter(t, ledgerFiles, "Equity transfers")
	outside := filepath.Join(filepath.Dir(h.root), "elsewhere.csv")
	out, err := h.run(
		pins, outside, "${WEALTHDB_DATA_ROOT}/ledgers/pins.csv",
		eq, filepath.Join(h.root, "ledgers", "equity.csv"),
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	text := h.read()
	for _, w := range []string{`"${WEALTHDB_DATA_ROOT}/ledgers/pins.csv"`,
		`"equity_transfers": "${WEALTHDB_DATA_ROOT}/ledgers/equity.csv"`} {
		if !strings.Contains(text, w) {
			t.Errorf("config lacks %s:\n%s", w, text)
		}
	}
	for _, w := range []string{
		"is not in the data folder",
		"There is no file there yet. A load reads it once it is there.",
		"  a  Spending pins              not set\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Count(out, "There is no file there yet") != 1 {
		t.Errorf("the note is said for a file that is there:\n%s", out)
	}
	if h.cfgOf().Spending.Pins != filepath.Join(h.root, "ledgers", "pins.csv") {
		t.Errorf("pins = %q", h.cfgOf().Spending.Pins)
	}
	// - on one not set goes back; on one set, it removes it.
	if out, _ := h.run(fieldLetter(t, ledgerFiles, "Income pins"), "-", ""); strings.Contains(out, "is not in the data folder") {
		t.Errorf("- was read as a path:\n%s", out)
	}
	if _, err := h.run(eq, "-", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.read(), "equity_transfers") {
		t.Errorf("- kept it:\n%s", h.read())
	}
}

// What the AI model is sent is set from its section, and counts as no
// setting of the file's.
func TestModelSent(t *testing.T) {
	h := newHarness(t)
	h.write(basics+`, "spending": {"categorization": {"model": {"baseUrl": "http://m/v1", "name": "m"}}}}`, 0o600)
	h.section = "model"
	ctx := fieldLetter(t, modelSent, "What it is sent")
	fence := fieldLetter(t, modelSent, "Hold back people's names")
	out, err := h.run("s", ctx, "2", fence, "off", "")
	if err != nil {
		t.Fatal(err)
	}
	cz := h.cfgOf().Spending.Categorization
	if cz.Context != "descriptor" || cz.FencePersonNames == nil || *cz.FencePersonNames {
		t.Errorf("not saved:\n%s", h.read())
	}
	if !strings.Contains(out, "Turn it off only\nfor a model on this computer.") {
		t.Errorf("output:\n%s", out)
	}
	w := &setup{store: openTestStore(t, h.cfg, "")}
	if got := w.state(fileSettings[3]); got != "not set" {
		t.Errorf("spending rules = %q", got)
	}
}

// Every field names a member the config takes, with a value of its kind
// that loads: the default, the first choice, the bounds.
func TestEveryFieldLoads(t *testing.T) {
	for _, fs := range [][]field{reportOptions, ledgerFiles, modelSent} {
		for _, f := range fs {
			var values []any
			switch f.kind {
			case kindChoice:
				for _, c := range f.choices {
					values = append(values, c.value)
				}
			case kindSwitch:
				values = []any{true, false}
			case kindWhole, kindNumber:
				values = []any{f.min, f.max}
			case kindFile:
				values = []any{"/x/ledger.csv"}
			}
			if f.dflt != nil {
				values = append(values, f.dflt)
			}
			for _, v := range values {
				d, _ := jsonedit.Parse([]byte(basics + "}"))
				if err := d.Set(v, f.path...); err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				p := filepath.Join(dir, "wealthdb.cfg")
				if err := os.WriteFile(p, d.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, _, err := Check(p); err != nil {
					t.Errorf("%s = %v: %v", dotted(f.path), v, err)
				}
			}
		}
	}
}
