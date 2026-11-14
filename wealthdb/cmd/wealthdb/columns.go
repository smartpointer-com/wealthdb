package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// columnSpec describes one named output column. T is the row
// type the Extract function takes, such as gold.PositionRow or
// gold.TransactionRow.
type columnSpec[T any] struct {
	Name   string
	Header string // empty ⇒ same as Name (use for dynamic headers like value_<CCY>)
	// Kind is what the cells are. It sets the alignment, the kind a
	// filter condition reads, the order of a sort and the default
	// privacy class.
	Kind ColumnKind
	// Unit is the currency of a money column's figures: the holding's
	// own, or the output currency, whose header ends in _<CCY>.
	Unit MoneyUnit
	// Twin names the column in the holding's own currency that this
	// output-currency column converts; MCP sorts read the twin
	// (columnLookup.resolve).
	Twin    string
	Extract func(T) string
	// Privacy controls how the cell is redacted when the
	// caller runs in privacy mode (`-p` / `--privacy` on the
	// readout subcommands). PrivacyNone leaves the cell
	// unchanged, unless the column's kind hides it (class).
	Privacy PrivacyClass
	// PrivacyFunc, when non-nil, replaces Privacy for per-row
	// decisions. Used where the right class depends on row
	// context — typically silver_source: e.g. UBS portfolio
	// labels ("Savings") are bank-assigned categories and stay
	// legible, while a cointracking portfolio's name is its
	// holder's words and prints as a pseudonym whatever its shape
	// (portfolioCol).
	PrivacyFunc func(T) PrivacyClass
	// PrivacyExtract, when non-nil, replaces Extract in privacy mode:
	// the value the row shows there, which Privacy or PrivacyFunc then
	// redacts. Used where the legible value has no safe redacted form
	// but another field of the row has one, as for an account label the
	// customer may have typed (accountCol).
	PrivacyExtract func(T) string
	// Pseudonym is the prefix of a PrivacyPseudonym cell's pseudonym and
	// the source the value is the words of (pseudonyms.of): the kind of
	// the source whose holder typed the value (typedPseudonym) and that
	// source, or what the value is ("category", "nickname") and no
	// source. Nil reads as the column's name and no source.
	Pseudonym func(T) (prefix, source string)
}

// ColumnKind is what a column's cells are.
type ColumnKind int

const (
	TextColumn ColumnKind = iota
	CountColumn
	NumberColumn   // a legible figure: years, a ratio such as P/E
	PercentColumn  // a figure in percent, its header ending in _pct
	MoneyColumn    // an amount of money, redacted as money
	QuantityColumn // a share count, redacted as a quantity
	DateColumn     // a date or a period, read through the date grammar
)

// figure reports whether the kind's cells are figures: every kind but
// text and dates.
func (k ColumnKind) figure() bool { return k != TextColumn && k != DateColumn }

// align is how a column of the kind pads: text and dates to the left,
// figures to the right.
func (k ColumnKind) align() output.Alignment {
	if k.figure() {
		return output.AlignRight
	}
	return output.AlignLeft
}

// MoneyUnit is the currency of a money column's figures.
type MoneyUnit int

const (
	OwnCurrency    MoneyUnit = iota // the holding's own
	OutputCurrency                  // the report's output currency
)

// class is the column's privacy class: Privacy where it is set, else
// the class of its kind. Money and a quantity are hidden; any other
// kind is legible.
func (c columnSpec[T]) class() PrivacyClass {
	switch {
	case c.Privacy != PrivacyNone:
		return c.Privacy
	case c.Kind == MoneyColumn:
		return PrivacyMoney
	case c.Kind == QuantityColumn:
		return PrivacyQuantity
	}
	return PrivacyNone
}

// PrivacyClass tags a column with the redaction shape applied
// when `-p` / `--privacy` is on. Mirrors what a banking-app
// privacy toggle does: hide identifying numbers, amounts, and
// share counts; leave taxonomy / dates / currency labels alone.
type PrivacyClass int

const (
	// PrivacyNone: never redacted (default).
	PrivacyNone PrivacyClass = iota
	// PrivacyAccountID: account / portfolio / relationship /
	// transaction identifiers, and a manual holding's keys
	// (holdingIDCol). Redacted by redactAccountID: every id in the
	// value, however it is spaced or punctuated, turns to stars but
	// for its last 2-4 chars (length-dependent so very short IDs don't
	// reveal too much) and an IBAN-style 2-letter country code at the
	// front. Words and runs of up to four digits stay, since at this
	// layer they are bank-assigned taxonomic labels (UBS "Savings" /
	// "Brokerage") or the words a manual holding's key spells.
	PrivacyAccountID
	// PrivacyFreeText: free-form strings that can carry a person's
	// name and that the household did not type — a statement
	// narrative and its folds (a wire, a P2P transfer, a cheque
	// payee, a merchant) — and the instrument id of a holding no
	// public key names, unless a person typed it (holdingKeyCol).
	// Rendered as "***" verbatim, in every output format.
	//
	// Unlike PrivacyAccountID this class is shape-blind: it never
	// asks whether the value looks like an identifier, because a
	// narrative never does. Any shape-based rule would pass
	// exactly the values that matter here — a payee narrative is
	// multi-word, so an identifier heuristic lets it straight
	// through.
	PrivacyFreeText
	// PrivacyPseudonym: a name a person typed about an account — a typed
	// account's or portfolio's name or id, a nickname, a typed source's
	// transaction or lot id, a config category. A holding's name is no
	// such name: it prints as it is, a manual holding's with any id in
	// it masked (holdingNameCol).
	// Rendered as a pseudonym, stable within one rendering
	// (pseudonyms), in every output format. Shape-blind like
	// PrivacyFreeText, but the rows stay apart: the same value reads
	// the same in every row and column, and two values read apart.
	PrivacyPseudonym
	// PrivacyQuantity: share counts. Rendered as "***" verbatim.
	PrivacyQuantity
	// PrivacyMoney: any monetary amount (prices, balances,
	// market values, net amounts, fees, etc.). Rendered as
	// "*****.**" verbatim.
	PrivacyMoney
)

// hidesFigure reports whether the class hides a figure whole: money and
// a quantity.
func (p PrivacyClass) hidesFigure() bool { return p == PrivacyMoney || p == PrivacyQuantity }

// header returns the column's display label — Header if set,
// else Name. Used so registries can carry a stable lookup name
// while rendering a currency-suffixed header such as
// `total_value_USD`.
func (c columnSpec[T]) header() string {
	if c.Header != "" {
		return c.Header
	}
	return c.Name
}

// resolveColumns implements the -C / --columns parsing every report
// shares:
//
//   - empty / "default" → the per-command defaults list
//   - "all"             → every registered column
//   - "+adds,...-removes,..." → delta against the defaults
//   - otherwise         → a comma-separated absolute list
//
// Unknown names produce a helpful error listing every column the
// command knows about.
func resolveColumns[T any](flagValue string, defaults []string, registry []columnSpec[T]) ([]columnSpec[T], error) {
	e := parseColumnsExpr(flagValue, false)
	switch {
	case e.delta:
		return columnsByName(applyColumnsDelta(defaults, registryNames(e.adds, registry), registryNames(e.removes, registry)), registry)
	case e.defaults:
		return columnsByName(defaults, registry)
	case e.all:
		out := make([]columnSpec[T], len(registry))
		copy(out, registry)
		return out, nil
	}
	return columnsByName(registryNames(e.names, registry), registry)
}

// columnsExpr is a parsed column expression, the grammar the CLI's -C
// and MCP's columns share: the defaults, every column, a delta against
// the defaults, or a list of names. Each side resolves the names its
// own way: the CLI exactly, MCP tolerantly with notes.
type columnsExpr struct {
	defaults, all, delta bool
	adds, removes, names []string
}

// parseColumnsExpr parses expr. foldKeywords reads "default" and "all"
// in any case. A list drops its empty names.
func parseColumnsExpr(expr string, foldKeywords bool) columnsExpr {
	expr = strings.TrimSpace(expr)
	if adds, removes, isDelta := parseColumnsDelta(expr); isDelta {
		return columnsExpr{delta: true, adds: adds, removes: removes}
	}
	keyword := expr
	if foldKeywords {
		keyword = strings.ToLower(expr)
	}
	switch keyword {
	case "", "default":
		return columnsExpr{defaults: true}
	case "all":
		return columnsExpr{all: true}
	}
	var e columnsExpr
	for _, n := range strings.Split(expr, ",") {
		if n = strings.TrimSpace(n); n != "" {
			e.names = append(e.names, n)
		}
	}
	return e
}

// registryNames reads each name as the server's columnLookup.exact does:
// a registry name or a header as printed (value_USD for value), any
// case. A name that is neither is left for columnsByName to refuse.
func registryNames[T any](names []string, registry []columnSpec[T]) []string {
	l := columnLookup{cols: make([]reportColumn, len(registry))}
	for i, c := range registry {
		l.cols[i] = reportColumn{name: c.Name, header: c.header()}
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n
		if j := l.exact(n); j >= 0 {
			out[i] = registry[j].Name
		}
	}
	return out
}

func columnsByName[T any](names []string, registry []columnSpec[T]) ([]columnSpec[T], error) {
	index := make(map[string]columnSpec[T], len(registry))
	for _, c := range registry {
		index[c.Name] = c
	}
	out := make([]columnSpec[T], 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		c, ok := index[n]
		if !ok {
			return nil, fmt.Errorf("unknown column %q; available: %s", n, joinColumnNames(registry))
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--columns produced an empty list")
	}
	return out, nil
}

// moneyCol is a money column in the holding's own currency: two
// decimals, blank for nil, redacted as money.
func moneyCol[T any](name string, get func(T) *string) columnSpec[T] {
	return kindCol(name, MoneyColumn, func(r T) string { return formatCents(get(r)) })
}

// outCcyTwin is the output-currency twin of a money column in the
// holding's own currency: named <name>_outccy, printed <name>_<CCY>.
// MCP sorts read the twin (columnLookup.resolve).
func outCcyTwin[T any](name, outCcy string, get func(T) *string) columnSpec[T] {
	c := outCcyCol(name, outCcy, get)
	c.Name, c.Twin = name+"_outccy", name
	return c
}

// outCcyCol is a money figure only the output currency carries: its own
// name, printed <name>_<CCY>.
func outCcyCol[T any](name, outCcy string, get func(T) *string) columnSpec[T] {
	return inOutCcy(moneyCol(name, get), outCcy)
}

// outCcyColAs is outCcyCol printed header: a figure whose header is no
// <name>_<CCY> (returns' start_<CCY> for start_value).
func outCcyColAs[T any](name, header, outCcy string, get func(T) *string) columnSpec[T] {
	c := outCcyCol(name, outCcy, get)
	c.Header = header
	return c
}

// valueCol is a holding's value in the output currency, named value:
// market_value's twin.
func valueCol[T any](outCcy string, get func(T) *string) columnSpec[T] {
	c := outCcyCol("value", outCcy, get)
	c.Twin = "market_value"
	return c
}

// inOutCcy is the money column c in the output currency: printed
// <name>_<CCY>.
func inOutCcy[T any](c columnSpec[T], outCcy string) columnSpec[T] {
	c.Header, c.Unit = c.Name+"_"+outCcy, OutputCurrency
	return c
}

// kindCol is a column of kind whose cells get prints as they are.
func kindCol[T any](name string, kind ColumnKind, get func(T) string) columnSpec[T] {
	return columnSpec[T]{Name: name, Kind: kind, Extract: get}
}

// quantityCol is a quantity column, redacted as a quantity.
func quantityCol[T any](name string, get func(T) *string) columnSpec[T] {
	return kindCol(name, QuantityColumn, func(r T) string { return strOrEmpty(get(r)) })
}

// textCol is a legible text column, blank for nil.
func textCol[T any](name string, get func(T) *string) columnSpec[T] {
	return strCol(name, func(r T) string { return strOrEmpty(get(r)) })
}

// nicknameCol is a nickname column, blank for nil. A nickname is the
// customer's own words by definition, so -p prints it as a pseudonym.
func nicknameCol[T any](name string, get func(T) *string) columnSpec[T] {
	return columnSpec[T]{Name: name, Privacy: PrivacyPseudonym,
		Extract:   func(r T) string { return strOrEmpty(get(r)) },
		Pseudonym: func(T) (string, string) { return "nickname", "" }}
}

// strCol is a legible text column.
func strCol[T any](name string, get func(T) string) columnSpec[T] {
	return kindCol(name, TextColumn, get)
}

// privateCol is a text column -p redacts as class.
func privateCol[T any](name string, class PrivacyClass, get func(T) string) columnSpec[T] {
	c := strCol(name, get)
	c.Privacy = class
	return c
}

// asDate marks c as a date column: its cells are dates or periods.
func asDate[T any](c columnSpec[T]) columnSpec[T] {
	c.Kind = DateColumn
	return c
}

// qualityCol is a report's quality column.
func qualityCol[T any](get func(T) string) columnSpec[T] { return strCol("quality", get) }

// floatOwnMoneyCol is a money figure in its own currency, two decimals,
// blank for nil, redacted as money.
func floatOwnMoneyCol[T any](name string, get func(T) *float64) columnSpec[T] {
	return kindCol(name, MoneyColumn, func(r T) string { return fixedOrBlank(get(r), 2) })
}

// floatMoneyCol is floatOwnMoneyCol in the output currency, printed
// name_<CCY>.
func floatMoneyCol[T any](name, outCcy string, get func(T) *float64) columnSpec[T] {
	return inOutCcy(floatOwnMoneyCol(name, get), outCcy)
}

// floatValCol is floatMoneyCol of a figure every row has.
func floatValCol[T any](name, outCcy string, get func(T) float64) columnSpec[T] {
	return floatMoneyCol(name, outCcy, func(r T) *float64 { v := get(r); return &v })
}

// priceCol is a price per unit, or per 100 of a bond's face, four
// decimals, redacted as money.
func priceCol[T any](get func(T) *float64) columnSpec[T] {
	return kindCol("price", MoneyColumn, func(r T) string { return fixedOrBlank(get(r), 4) })
}

// decimalCol is a legible figure with two decimals, blank for nil: years,
// a ratio such as P/E.
func decimalCol[T any](name string, get func(T) *float64) columnSpec[T] {
	return kindCol(name, NumberColumn, func(r T) string { return fixedOrBlank(get(r), 2) })
}

// dateCol is a date column, blank where the epoch is 0: a source that
// has produced no observations at all.
func dateCol[T any](name string, get func(T) int64) columnSpec[T] {
	return asDate(strCol(name, func(r T) string {
		if epoch := get(r); epoch != 0 {
			return formatDate(epoch)
		}
		return ""
	}))
}

// datePtrCol is a date column, blank for nil.
func datePtrCol[T any](name string, get func(T) *int64) columnSpec[T] {
	return asDate(strCol(name, func(r T) string {
		if epoch := get(r); epoch != nil {
			return formatDate(*epoch)
		}
		return ""
	}))
}

// pctCol is a figure already in percent, named name and printed
// name_pct. A percentage is a proportion, not an amount: -p leaves it.
func pctCol[T any](name string, get func(T) *float64) columnSpec[T] {
	return percentCol(name, name+"_pct", func(r T) string { return formatPercent(get(r)) })
}

// percentCol is a percent column whose cells get prints; header ""
// prints the name, which then ends in _pct.
func percentCol[T any](name, header string, get func(T) string) columnSpec[T] {
	c := kindCol(name, PercentColumn, get)
	c.Header = header
	return c
}

// ratioCol is a ratio, named name and printed name_pct in percent, n/a
// for nil. A ratio is a proportion, not an amount: it survives -p,
// which keeps the privacy twin of a report readable.
func ratioCol[T any](name string, get func(T) *float64) columnSpec[T] {
	return percentCol(name, name+"_pct", func(r T) string { return formatPct(get(r)) })
}

// ratioOrBlankCol is a ratio in percent, blank for nil, whose name
// already says it is one: header "" prints the name. Like ratioCol it
// survives -p.
func ratioOrBlankCol[T any](name, header string, get func(T) *float64) columnSpec[T] {
	return percentCol(name, header, func(r T) string { return formatPctOrBlank(get(r)) })
}

// countCol is a legible count.
func countCol[T any](name string, get func(T) int64) columnSpec[T] {
	return kindCol(name, CountColumn, func(r T) string { return fmt.Sprintf("%d", get(r)) })
}

// intPtrCol is a legible count, blank for nil.
func intPtrCol[T any](name string, get func(T) *int64) columnSpec[T] {
	return kindCol(name, CountColumn, func(r T) string { return intOrEmpty(get(r)) })
}

// yesNoCol is a stated flag, blank where the source states nothing.
func yesNoCol[T any](name string, get func(T) *bool) columnSpec[T] {
	return strCol(name, func(r T) string { return yesNo(get(r)) })
}

func joinColumnNames[T any](registry []columnSpec[T]) string {
	return strings.Join(columnNames(registry), ", ")
}

// columnNames is a registry's column names, in order.
func columnNames[T any](registry []columnSpec[T]) []string {
	names := make([]string, len(registry))
	for i, c := range registry {
		names[i] = c.Name
	}
	return names
}

// rowsToTable assembles an output.Table from a typed row slice
// and a column set. Every report funnels its rows through this on
// the way to the formatter. The `format` drives privacy mode's
// per-format behaviour:
//
//   - table        — redacted cells render as visible placeholders
//     ("****1234" / "***" / "*****.**") so the
//     eye can still scan rows.
//   - csv / csv_plain — quantity / money cells render empty
//     (",," between separators) so downstream
//     parsers see "missing" rather than the
//     literal asterisks. Account-ID columns
//     still render the visible placeholder so
//     row identity is preserved for grep / awk.
//   - json         — quantity / money columns are DROPPED from
//     the output entirely (the keys don't appear)
//     so JSON consumers see only fields they're
//     allowed to know. Account-ID columns still
//     surface the visible placeholder.
//
// `privacy` is false ⇒ no redaction regardless of format. Under privacy
// a column's PrivacyExtract, where set, gives the cell in place of its
// Extract, and a PrivacyPseudonym cell takes its name from names, the
// run's pseudonyms; nil gives the table a set of its own. A figure that
// rounds to zero prints unsigned (unsignedZero) in every format.
func rowsToTable[T any](rows []T, cols []columnSpec[T], privacy bool, format output.Format, names *pseudonyms) output.Table {
	if names == nil {
		names = newPseudonyms()
	}
	if privacy && format == output.FormatJSON {
		kept := make([]columnSpec[T], 0, len(cols))
		for _, c := range cols {
			if c.class().hidesFigure() {
				continue
			}
			kept = append(kept, c)
		}
		cols = kept
	}
	t := output.Table{
		Columns: make([]string, len(cols)),
		Aligns:  make([]output.Alignment, len(cols)),
	}
	extract := make([]func(T) string, len(cols))
	classes := make([]PrivacyClass, len(cols))
	for i, c := range cols {
		t.Columns[i] = c.header()
		t.Aligns[i] = c.Kind.align()
		classes[i] = c.class()
		extract[i] = c.Extract
		if privacy && c.PrivacyExtract != nil {
			extract[i] = c.PrivacyExtract
		}
		if figure := extract[i]; c.Kind.figure() {
			extract[i] = func(r T) string { return unsignedZero(figure(r)) }
		}
	}
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cell := extract[i](r)
			if privacy {
				class := classes[i]
				if c.PrivacyFunc != nil {
					class = c.PrivacyFunc(r)
				}
				if class == PrivacyPseudonym {
					prefix, source := c.Name, ""
					if c.Pseudonym != nil {
						prefix, source = c.Pseudonym(r)
					}
					cell = names.of(prefix, source, cell)
				} else {
					// Defence-in-depth: mask structured bank
					// identifiers and addresses wherever they appear,
					// including in PrivacyNone columns the per-column
					// pass skips (e.g. a UBS account number surfacing
					// in position_key / instrument). It runs first:
					// the per-column pass would star a five-digit
					// postal code, and the address shape would no
					// longer match.
					cell = applyPrivacy(scrubSensitiveIDs(cell), class, format)
				}
			}
			cells[i] = cell
		}
		t.Rows = append(t.Rows, cells)
	}
	return t
}

// sensitiveIDPatterns are the shapes that scrubSensitiveIDs masks
// wherever they appear in a cell, independent of the column's
// PrivacyClass: a bank's identifier shapes and a closing postal address.
// Each entry pairs a tight shape regex with a masker that preserves the
// match's length + separators so column widths and scan-shape survive
// (matching redactAccountID's contract).
//
// This is a content-based defence-in-depth layer over the per-column
// privacy machinery. The per-column pass reaches only the columns whose
// class asks for it. It cannot reach an identifier or an address inside
// a column that is PrivacyNone by design, notably position_key and
// symbol, which stay legible under -p because they normally hold public
// ISIN or ticker ids. A UBS mortgage uses its (private) account number
// as the instrument identity, so it surfaces there raw; this pass
// catches it by shape regardless of which column carries it.
var sensitiveIDPatterns = []struct {
	re   *regexp.Regexp
	mask func(string) string
}{
	{
		// UBS banking relationship / account base: a 4-digit branch,
		// a space, then the 8-digit account base (e.g. the
		// relationship_id "<branch> <base>"). It also prefixes the
		// UBS mortgage account number ("<branch> <base>.MMM <n>"),
		// which the match takes whole, tail included.
		// The digits-space-digits shape is distinctive in holdings
		// data — ISINs/CUSIPs carry no internal space, monetary
		// amounts have decimal separators, valor numbers are shorter
		// — so matching by shape rarely false-positives. Keeps the
		// last 4 characters for cross-referencing; masks the rest.
		re:   regexp.MustCompile(`\b\d{4} \d{8}(?:\.[A-Za-z0-9]+ \d+)?\b`),
		mask: maskID,
	},
	{
		// A postal address closing a label: a comma, a street and a
		// house number, a comma, a postal code of four or five digits
		// and a town, to the end of the value. A bank can name a
		// mortgage by the property. The product name before it stays.
		// web/sqlkit.py's ADDRESS_PATTERN is the same pattern for the
		// privacy twins, and both engines read it as RE2.
		re: regexp.MustCompile(`, [^,]* \d+[a-zA-Z]?, \d{4,5} .*$`),
		mask: func(m string) string {
			return strings.Map(func(r rune) rune {
				if unicode.IsLetter(r) || unicode.IsDigit(r) {
					return '*'
				}
				return r
			}, m)
		},
	},
}

// scrubSensitiveIDs masks structured bank identifiers and addresses
// wherever they appear in s. It runs on every cell under -p/--privacy
// that is not a pseudonym, before applyPrivacy, so one that leaks inside
// an otherwise-non-private column is caught even when that column's
// PrivacyClass is None.
func scrubSensitiveIDs(s string) string {
	for _, p := range sensitiveIDPatterns {
		s = p.re.ReplaceAllStringFunc(s, p.mask)
	}
	return s
}
