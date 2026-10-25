package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// columnSpec describes one named output column. T is the row
// type the Extract function takes, such as gold.PositionRow or
// gold.TransactionRow.
type columnSpec[T any] struct {
	Name    string
	Header  string // empty ⇒ same as Name (use for dynamic headers like value_<CCY>)
	Align   output.Alignment
	Extract func(T) string
	// Privacy controls how the cell is redacted when the
	// caller runs in privacy mode (`-p` / `--privacy` on the
	// readout subcommands). PrivacyNone leaves the cell
	// unchanged.
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

// PrivacyClass tags a column with the redaction shape applied
// when `-p` / `--privacy` is on. Mirrors what a banking-app
// privacy toggle does: hide identifying numbers, amounts, and
// share counts; leave taxonomy / dates / currency labels alone.
type PrivacyClass int

const (
	// PrivacyNone: never redacted (default).
	PrivacyNone PrivacyClass = iota
	// PrivacyAccountID: account / portfolio / relationship /
	// transaction identifiers. Redacted by redactAccountID —
	// stars in the middle, last 2-4 chars exposed (length-
	// dependent so very short IDs don't reveal too much), IBAN-
	// style 2-letter country code preserved at the front.
	// Purely-alphabetic strings pass through unredacted, since
	// at this layer they're typically bank-assigned taxonomic
	// labels (UBS "Savings" / "Brokerage").
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
	// PrivacyPseudonym: a name a person typed — a typed account's or
	// portfolio's name or id, a nickname, a manual holding's name and
	// key, a typed source's transaction or lot id, a config category.
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

// header returns the column's display label — Header if set,
// else Name. Used so registries can carry a stable lookup name
// while rendering a -x/--currency-suffixed header.
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
	flagValue = strings.TrimSpace(flagValue)
	if adds, removes, isDelta := parseColumnsDelta(flagValue); isDelta {
		return columnsByName(applyColumnsDelta(defaults, adds, removes), registry)
	}
	switch flagValue {
	case "", "default":
		return columnsByName(defaults, registry)
	case "all":
		out := make([]columnSpec[T], len(registry))
		copy(out, registry)
		return out, nil
	}
	names := strings.Split(flagValue, ",")
	for i, n := range names {
		names[i] = strings.TrimSpace(n)
	}
	return columnsByName(names, registry)
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

// accountLabel is an account or portfolio column's text: the display
// name where gold has one (a Schwab account number, a UBS portfolio
// label), else the external id.
func accountLabel(displayName *string, id string) string {
	if displayName != nil && *displayName != "" {
		return *displayName
	}
	return id
}

// accountRef is what an account column reads off a row: the account's
// source, its display name, its nickname and its external id.
type accountRef struct {
	source         string
	name, nickname *string
	id             string
}

// typed reports whether the account's display name may be the
// customer's own words: its source's kind says so (typedAccountLabel),
// its source names an account by the bank's nickname
// (nicknameLabelKinds), or the name is the account's nickname.
func (a accountRef) typed(kindOf func(string) string) bool {
	if typedAccountLabel(kindOf, a.source) {
		return true
	}
	if a.name == nil || *a.name == "" {
		return false
	}
	return nicknameLabelKinds[kindOf(a.source)] || a.nickname != nil && *a.name == *a.nickname
}

// idClass is how -p redacts the account's id: as a pseudonym whatever
// its shape where its holder chose it (typedAccountID), else by its
// shape (lineClass).
func (a accountRef) idClass(kindOf func(string) string) PrivacyClass {
	if typedAccountID(kindOf, a.source) {
		return PrivacyPseudonym
	}
	return a.lineClass(kindOf)
}

// lineClass is how -p redacts an id by its shape: the account's own id,
// or one its source gives a portfolio, a line or a lot (typedIDCol). An
// id of a source whose names may be typed can hold typed words too: a
// manual config key, a crypto tracker's `<user>:<wallet label>`. Such an
// id redacts as one only when it is wholly id-shaped
// (isLikelyAccountID), and prints as a pseudonym otherwise. A line's id
// is the provider's even where the source's account ids are typed.
func (a accountRef) lineClass(kindOf func(string) string) PrivacyClass {
	if a.typed(kindOf) && !isLikelyAccountID(a.id) {
		return PrivacyPseudonym
	}
	return PrivacyAccountID
}

// accountCol is a report's account column: the account's label
// (accountLabel), redacted as an account id under -p. A label that may
// be the customer's own words (accountRef.typed), or the label of an
// account whose id is (typedAccountID), gives way to the account's id
// under -p, redacted as idClass says: the id-shape test
// passes a typed name such as a person's, but an id redacts. kindOf
// maps a source to its silver kind (sourceKinds); only privacy mode
// reads it.
func accountCol[T any](kindOf func(string) string, get func(T) accountRef) columnSpec[T] {
	col := accountIDCol("account", kindOf, get)
	col.Extract = func(r T) string {
		a := get(r)
		return accountLabel(a.name, a.id)
	}
	col.PrivacyExtract = func(r T) string {
		a := get(r)
		if a.typed(kindOf) || typedAccountID(kindOf, a.source) {
			return a.id
		}
		return accountLabel(a.name, a.id)
	}
	return col
}

// accountIDCol is a report's account-id column, named name: the
// account's external id, redacted as idClass says under -p.
func accountIDCol[T any](name string, kindOf func(string) string, get func(T) accountRef) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignLeft, Privacy: PrivacyAccountID,
		Extract:     func(r T) string { return get(r).id },
		PrivacyFunc: func(r T) PrivacyClass { return get(r).idClass(kindOf) },
		Pseudonym:   func(r T) (string, string) { return typedPseudonym(kindOf, get(r).source) }}
}

// portfolioCol is a report's portfolio column, named name: the
// portfolio's display name, else its id, and "(no portfolio)" for the
// accounts of a source outside any (an empty id). -p redacts it as
// portfolioNamePrivacy says. A typed source's portfolio prints the
// pseudonym of its id, as its portfolio-id column does where that id is
// not id-shaped.
func portfolioCol[T any](name string, kindOf func(string) string,
	get func(T) (source, id string, display *string)) columnSpec[T] {
	label := func(r T) string {
		_, id, display := get(r)
		if id == "" {
			return "(no portfolio)"
		}
		return accountLabel(display, id)
	}
	return columnSpec[T]{Name: name, Align: output.AlignLeft, Privacy: PrivacyAccountID,
		Extract: label,
		PrivacyFunc: func(r T) PrivacyClass {
			source, id, _ := get(r)
			return portfolioNamePrivacy(kindOf, source, id)
		},
		PrivacyExtract: func(r T) string {
			if source, id, _ := get(r); id != "" && typedAccountLabel(kindOf, source) {
				return id
			}
			return label(r)
		},
		Pseudonym: func(r T) (string, string) {
			source, _, _ := get(r)
			return typedPseudonym(kindOf, source)
		}}
}

// portfolioNamePrivacy is the redaction class of a portfolio column's
// value (portfolioCol), from the portfolio's source and id. The value
// mixes different conventions across sources:
//
//   - the sentinel row for a source's orphan accounts renders the
//     literal "(no portfolio)". It is a structural marker with no
//     row content behind it, so it stays legible for every source.
//   - a source whose names may be typed (typedAccountLabel) shows
//     the customer's words: the cointracking adapter emits the
//     free-form CT account name as display_name, and a manual or
//     declared portfolio is named, or keyed, by hand. These can be
//     anything — one word or several — so they print as a pseudonym
//     whatever their shape, the id fallback too (portfolioCol). A
//     tracker's `cu_<user>` id names its user and prints as one too
//     (typedIDCol).
//   - UBS / other Swiss-source adapters emit bank-assigned
//     portfolio category labels (Savings / Brokerage / …).
//     These pass through under the PrivacyAccountID heuristic
//     because their info content is taxonomy, not identifier.
//
// kindOf resolves a silver_source_id to its silver_kind — the
// config-side id is free-form, so matching the id string instead
// would silently lose redaction for a source named anything but
// the literal adapter name.
func portfolioNamePrivacy(kindOf func(string) string, source, id string) PrivacyClass {
	if id == "" {
		return PrivacyNone
	}
	if typedAccountLabel(kindOf, source) {
		return PrivacyPseudonym
	}
	return PrivacyAccountID
}

// typedIDCol is a report's column, named name, of an id a source gives
// a portfolio, a line or a lot, which -p redacts by its shape
// (accountRef.lineClass). A typed source builds such ids from its
// holder's words: a tracker's portfolio id names its user, a manual
// exit's line is `exit:<position key>`. Such an id prints as a
// pseudonym unless it is wholly id-shaped.
func typedIDCol[T any](name string, kindOf func(string) string, get func(T) (source, id string)) columnSpec[T] {
	ref := func(r T) accountRef {
		source, id := get(r)
		return accountRef{source: source, id: id}
	}
	col := accountIDCol(name, kindOf, ref)
	col.PrivacyFunc = func(r T) PrivacyClass { return ref(r).lineClass(kindOf) }
	return col
}

// typedPseudonym is a Pseudonym of a value a source's holder typed: its
// prefix is the source's kind, or the source itself where gold holds no
// kind for it, as for the declared accounts. The source keeps the same
// text of two sources apart.
func typedPseudonym(kindOf func(string) string, source string) (prefix, src string) {
	if kind := kindOf(source); kind != "" {
		return kind, source
	}
	return source, source
}

// typedHoldingKinds are the silver kinds whose holdings are named by
// hand: a manual silver's name, position key and instrument id are its
// holder's words. The declared accounts hold nothing.
var typedHoldingKinds = map[string]bool{"manual": true}

// typedHolding reports whether a source's holdings are named by hand:
// the source's kind is one of typedHoldingKinds.
func typedHolding(kindOf func(string) string, source string) bool {
	return typedHoldingKinds[kindOf(source)]
}

// typedHoldingCol is col, whose cells name a holding: under -p a typed
// holding's (typedHolding) cell prints as the pseudonym of key, the
// holding's key, so its name, position key and instrument id read as
// one pseudonym. get gives the row's source and key; a blank cell stays
// blank, and every other row keeps col's own rule.
func typedHoldingCol[T any](col columnSpec[T], kindOf func(string) string, get func(T) (source, key string)) columnSpec[T] {
	class, classOf, extract := col.Privacy, col.PrivacyFunc, col.Extract
	if col.PrivacyExtract != nil {
		extract = col.PrivacyExtract
	}
	typed := func(r T) (string, bool) {
		source, key := get(r)
		return key, key != "" && typedHolding(kindOf, source) && extract(r) != ""
	}
	col.PrivacyFunc = func(r T) PrivacyClass {
		if _, ok := typed(r); ok {
			return PrivacyPseudonym
		}
		if classOf != nil {
			return classOf(r)
		}
		return class
	}
	col.PrivacyExtract = func(r T) string {
		if key, ok := typed(r); ok {
			return key
		}
		return extract(r)
	}
	col.Pseudonym = func(r T) (string, string) {
		source, _ := get(r)
		return typedPseudonym(kindOf, source)
	}
	return col
}

// holdingCols are a holding's position_key and name columns: key and
// name read them off a row that source says the source of. -p prints a
// typed holding's key and name as one pseudonym (typedHoldingCol).
func holdingCols[T any](kindOf func(string) string, source, key, name func(T) string) (keyCol, nameCol columnSpec[T]) {
	holding := func(r T) (string, string) { return source(r), key(r) }
	return typedHoldingCol(strCol("position_key", key), kindOf, holding),
		typedHoldingCol(strCol("name", name), kindOf, holding)
}

// holdingKeyCol is a key column. A security key is a public identifier
// and stays legible under -p, as a public position_key does. A holding
// no key names reads as `holding:<source>:<id>`, which -p hides: a
// typed holding's (typedHolding) prints as the pseudonym of its id, as
// its instrument id does, and any other's masks whole.
func holdingKeyCol[T any](name string, kindOf func(string) string, get func(T) string) columnSpec[T] {
	col := columnSpec[T]{Name: name, Align: output.AlignLeft, Extract: get,
		PrivacyFunc: func(r T) PrivacyClass {
			if _, _, ok := holdingKeyParts(get(r)); ok {
				return PrivacyFreeText
			}
			return PrivacyNone
		}}
	return typedHoldingCol(col, kindOf, keyHolding(get))
}

// holdingKeyCols are a holding key's column, named name (holdingKeyCol),
// and the name column beside it, which label reads: under -p a typed
// holding's name prints as its key's pseudonym, as in holdingCols.
func holdingKeyCols[T any](name string, kindOf func(string) string,
	key, label func(T) string) (keyCol, nameCol columnSpec[T]) {
	return holdingKeyCol(name, kindOf, key), typedHoldingCol(strCol("name", label), kindOf, keyHolding(key))
}

// keyHolding reads the holding a row's key names (holdingKeyParts): its
// source and id, both blank for a security key.
func keyHolding[T any](key func(T) string) func(T) (source, id string) {
	return func(r T) (string, string) {
		source, id, _ := holdingKeyParts(key(r))
		return source, id
	}
}

// holdingKeyParts splits the key gold gives a holding no security key
// names, `holding:<source>:<id>`; ok is false for a security key.
func holdingKeyParts(key string) (source, id string, ok bool) {
	rest, ok := strings.CutPrefix(key, gold.HoldingKeyPrefix)
	if !ok {
		return "", "", false
	}
	source, id, _ = strings.Cut(rest, ":")
	return source, id, true
}

// typedCategories are the account categories the config's account
// overrides set (overrideCategories): the holder's own words, where a
// bank's category is its product label.
type typedCategories map[string]bool

// overrideCategories are the categories cfg's account overrides set.
func overrideCategories(cfg *config.Config) typedCategories {
	out := typedCategories{}
	for _, accounts := range cfg.AccountOverrides {
		for _, ov := range accounts {
			if ov.Category != "" {
				out[ov.Category] = true
			}
		}
	}
	return out
}

// accountCategoryCol is a report's account_category column, blank for
// nil. -p prints one of categories as a pseudonym; a bank's stays.
func accountCategoryCol[T any](categories typedCategories, get func(T) *string) columnSpec[T] {
	col := textCol("account_category", get)
	col.PrivacyFunc = func(r T) PrivacyClass {
		if categories[strOrEmpty(get(r))] {
			return PrivacyPseudonym
		}
		return PrivacyNone
	}
	col.Pseudonym = func(T) (string, string) { return "category", "" }
	return col
}

// typedLabelKinds are the silver kinds whose account display names may
// be the customer's own words: a crypto tracker's wallet labels, a
// manual silver's account labels.
var typedLabelKinds = map[string]bool{"cointracking": true, "manual": true}

// nicknameLabelKinds are the silver kinds whose account display name is
// the bank roster's nickname where it has one: the customer's words. A
// config nickname replaces the account's nickname in gold but not its
// display name, so the kind says what comparing the two cannot.
var nicknameLabelKinds = map[string]bool{"chase": true, "firstcitizens": true, "raiffeisen_at": true}

// typedIDKinds are the silver kinds whose account id is a name its
// holder chose rather than one the provider issues: an AngelList
// account's id is the holder's portal slug, which can spell a name.
var typedIDKinds = map[string]bool{"angellist": true}

// typedAccountID reports whether a source's account ids are its holder's
// words: the source's kind is one of typedIDKinds.
func typedAccountID(kindOf func(string) string, source string) bool {
	return typedIDKinds[kindOf(source)]
}

// typedAccountLabel reports whether the display names of a source's
// accounts may be the customer's own words rather than the bank's
// product labels: the source's kind is one of typedLabelKinds, or the
// source holds the declared accounts, which the config names.
func typedAccountLabel(kindOf func(string) string, source string) bool {
	return source == canonical.DeclaredSourceID || typedLabelKinds[kindOf(source)]
}

// moneyCol is a money column: two decimals, blank for nil, redacted as
// money. header "" prints the name.
func moneyCol[T any](name, header string, get func(T) *string) columnSpec[T] {
	return columnSpec[T]{Name: name, Header: header, Align: output.AlignRight, Privacy: PrivacyMoney,
		Extract: func(r T) string { return formatCents(get(r)) }}
}

// outCcyTwin is the output-currency twin of a money column in the
// holding's own currency: named <name>_outccy, printed <name>_<CCY>.
// MCP sorts read the twin (columnLookup.resolve).
func outCcyTwin[T any](name, outCcy string, get func(T) *string) columnSpec[T] {
	return moneyCol(name+"_outccy", name+"_"+outCcy, get)
}

// outCcyCol is a money figure only the output currency carries: its own
// name, printed <name>_<CCY>.
func outCcyCol[T any](name, outCcy string, get func(T) *string) columnSpec[T] {
	return moneyCol(name, name+"_"+outCcy, get)
}

// quantityCol is a quantity column, redacted as a quantity.
func quantityCol[T any](name string, get func(T) *string) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignRight, Privacy: PrivacyQuantity,
		Extract: func(r T) string { return strOrEmpty(get(r)) }}
}

// textCol is a legible text column, blank for nil.
func textCol[T any](name string, get func(T) *string) columnSpec[T] {
	return strCol(name, func(r T) string { return strOrEmpty(get(r)) })
}

// nicknameCol is a nickname column, blank for nil. A nickname is the
// customer's own words by definition, so -p prints it as a pseudonym.
func nicknameCol[T any](name string, get func(T) *string) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignLeft, Privacy: PrivacyPseudonym,
		Extract:   func(r T) string { return strOrEmpty(get(r)) },
		Pseudonym: func(T) (string, string) { return "nickname", "" }}
}

// strCol is a legible text column.
func strCol[T any](name string, get func(T) string) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignLeft, Extract: get}
}

// qualityCol is a report's quality column.
func qualityCol[T any](get func(T) string) columnSpec[T] { return strCol("quality", get) }

// floatMoneyCol is a money figure in the output currency, named name and
// printed name_<CCY>, two decimals, blank for nil, redacted as money.
func floatMoneyCol[T any](name, outCcy string, get func(T) *float64) columnSpec[T] {
	return columnSpec[T]{Name: name, Header: name + "_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
		Extract: func(r T) string { return fixedOrBlank(get(r), 2) }}
}

// floatValCol is floatMoneyCol of a figure every row has.
func floatValCol[T any](name, outCcy string, get func(T) float64) columnSpec[T] {
	return floatMoneyCol(name, outCcy, func(r T) *float64 { v := get(r); return &v })
}

// priceCol is a price per unit, or per 100 of a bond's face, four
// decimals, redacted as money.
func priceCol[T any](get func(T) *float64) columnSpec[T] {
	return columnSpec[T]{Name: "price", Align: output.AlignRight, Privacy: PrivacyMoney,
		Extract: func(r T) string { return fixedOrBlank(get(r), 4) }}
}

// decimalCol is a legible figure with two decimals, blank for nil: years,
// a ratio such as P/E.
func decimalCol[T any](name string, get func(T) *float64) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignRight, Extract: func(r T) string { return fixedOrBlank(get(r), 2) }}
}

// tagNote is a tag a column prints and what it means: a quality tag, a
// basis.
type tagNote struct{ tag, meaning string }

// tagNotes are tags as the usage texts list them: aligned, each meaning
// wrapped beside its tag.
func tagNotes(tags []tagNote) string {
	width := 0
	for _, q := range tags {
		width = max(width, len(q.tag))
	}
	var b strings.Builder
	for _, q := range tags {
		tag := q.tag
		for _, line := range wrapWords(q.meaning, 72-4-width-2) {
			fmt.Fprintf(&b, "\n    %-*s  %s", width, tag, line)
			tag = ""
		}
	}
	return b.String()
}

// tagList are tags as the MCP help lists them: one "- tag: meaning."
// line each.
func tagList(tags []tagNote) string {
	var b strings.Builder
	for _, q := range tags {
		b.WriteString("\n- " + q.tag + ": " + q.meaning + ".")
	}
	return b.String()
}

// fixedOrBlank is a figure with prec decimals, blank for nil.
func fixedOrBlank(p *float64, prec int) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'f', prec, 64)
}

// strOrEmpty is a nullable text, blank for nil.
func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// dateCol is a date column, blank where the epoch is 0: a source that
// has produced no observations at all.
func dateCol[T any](name string, get func(T) int64) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignLeft, Extract: func(r T) string {
		if epoch := get(r); epoch != 0 {
			return formatDate(epoch)
		}
		return ""
	}}
}

// datePtrCol is a date column, blank for nil.
func datePtrCol[T any](name string, get func(T) *int64) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignLeft, Extract: func(r T) string {
		if epoch := get(r); epoch != nil {
			return formatDate(*epoch)
		}
		return ""
	}}
}

// formatDate is an epoch second's UTC date.
func formatDate(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02")
}

// pctCol is a figure already in percent, named name and printed
// name_pct. A percentage is a proportion, not an amount: -p leaves it.
func pctCol[T any](name string, get func(T) *float64) columnSpec[T] {
	return columnSpec[T]{Name: name, Header: name + "_pct", Align: output.AlignRight,
		Extract: func(r T) string { return formatPercent(get(r)) }}
}

// ratioCol is a ratio, named name and printed name_pct in percent, n/a
// for nil. A ratio is a proportion, not an amount: it survives -p,
// which keeps the privacy twin of a report readable.
func ratioCol[T any](name string, get func(T) *float64) columnSpec[T] {
	return columnSpec[T]{Name: name, Header: name + "_pct", Align: output.AlignRight,
		Extract: func(r T) string { return formatPct(get(r)) }}
}

// formatPercent is a figure already in percent with two decimals, blank
// for nil.
func formatPercent(p *float64) string { return fixedOrBlank(p, 2) }

// countCol is a legible count.
func countCol[T any](name string, get func(T) int64) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignRight, Extract: func(r T) string { return fmt.Sprintf("%d", get(r)) }}
}

// intPtrCol is a legible count, blank for nil.
func intPtrCol[T any](name string, get func(T) *int64) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignRight, Extract: func(r T) string { return intOrEmpty(get(r)) }}
}

// intOrEmpty is a nullable count, blank for nil.
func intOrEmpty(p *int64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%d", *p)
}

// yesNoCol is a stated flag, blank where the source states nothing.
func yesNoCol[T any](name string, get func(T) *bool) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignLeft, Extract: func(r T) string { return yesNo(get(r)) }}
}

// yesNo renders a stated flag, empty where the source states nothing.
func yesNo(b *bool) string {
	switch {
	case b == nil:
		return ""
	case *b:
		return "yes"
	default:
		return "no"
	}
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
// run's pseudonyms; nil gives the table a set of its own.
func rowsToTable[T any](rows []T, cols []columnSpec[T], privacy bool, format output.Format, names *pseudonyms) output.Table {
	if names == nil {
		names = newPseudonyms()
	}
	if privacy && format == output.FormatJSON {
		kept := make([]columnSpec[T], 0, len(cols))
		for _, c := range cols {
			if c.Privacy == PrivacyMoney || c.Privacy == PrivacyQuantity {
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
	for i, c := range cols {
		t.Columns[i] = c.header()
		t.Aligns[i] = c.Align
		extract[i] = c.Extract
		if privacy && c.PrivacyExtract != nil {
			extract[i] = c.PrivacyExtract
		}
	}
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cell := extract[i](r)
			if privacy {
				class := c.Privacy
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
					cell = applyPrivacy(cell, class, format)
				}
				// Defence-in-depth: mask structured bank
				// identifiers wherever they appear, including in
				// PrivacyNone columns the per-column pass skips
				// (e.g. a UBS account number surfacing in
				// position_key / instrument).
				cell = scrubSensitiveIDs(cell)
			}
			cells[i] = cell
		}
		t.Rows = append(t.Rows, cells)
	}
	return t
}

// applyPrivacy is the dispatcher for the per-column redaction
// classes. Empty cells stay empty.
//
// Format-specific behaviour for Quantity / Money:
//   - table:     visible placeholder ("***" / "*****.**")
//   - csv*:      empty string ⇒ ",," between separators
//   - json:      empty string (the caller in rowsToTable has
//     already dropped the column from the JSON output
//     so this branch isn't reached for JSON, but
//     returning "" is the safe fallback).
//
// AccountID is always the visible placeholder — every format
// needs row-identity to remain scannable. FreeText is likewise
// format-independent, but it masks the whole value: there is no
// safe slice of a narrative to expose, since the identifying part
// can sit anywhere in it. A Pseudonym cell takes its name from the
// run's pseudonyms, which rowsToTable holds and applies before this; a
// Pseudonym cell that reaches here all the same masks whole, so a
// caller without the pseudonyms fails closed.
func applyPrivacy(value string, class PrivacyClass, format output.Format) string {
	if value == "" {
		return value
	}
	switch class {
	case PrivacyAccountID:
		return redactAccountID(value)
	case PrivacyFreeText, PrivacyPseudonym:
		return freeTextPlaceholder
	case PrivacyQuantity:
		if format == output.FormatCSV || format == output.FormatCSVPlain {
			return ""
		}
		return "***"
	case PrivacyMoney:
		if format == output.FormatCSV || format == output.FormatCSVPlain {
			return ""
		}
		return "*****.**"
	}
	return value
}

// freeTextPlaceholder is what a PrivacyFreeText cell renders as —
// the same fixed star form PrivacyQuantity uses, so privacy mode
// keeps one visual vocabulary instead of a per-class one.
const freeTextPlaceholder = "***"

// pseudonyms are the names -p gives the values a person typed, in one
// rendering: a CLI run, or one MCP call's result set. A value prints as
// its prefix and a number, `manual-1`, counted per prefix in the order
// the values first appear. So the same value of one source reads the
// same in every row and column, and two values, or one text of two
// sources, read apart. The numbers follow the rendering's rows: another
// run may number the same value apart.
type pseudonyms struct {
	names map[[3]string]string
	count map[string]int
	given map[string]bool // the names given out, in normalizeCell's form
}

func newPseudonyms() *pseudonyms {
	return &pseudonyms{names: map[[3]string]string{}, count: map[string]int{}, given: map[string]bool{}}
}

// of is the pseudonym under prefix of value, the words of source (blank
// for a value that reads alike across sources); a blank value stays
// blank.
func (p *pseudonyms) of(prefix, source, value string) string {
	if value == "" {
		return ""
	}
	key := [3]string{prefix, source, value}
	if name, ok := p.names[key]; ok {
		return name
	}
	p.count[prefix]++
	name := prefix + "-" + strconv.Itoa(p.count[prefix])
	p.names[key] = name
	p.given[normalizeCell(name)] = true
	return name
}

// named reports whether s, in normalizeCell's form, is a name the set
// has given out (of).
func (p *pseudonyms) named(s string) bool { return p.given[s] }

// redactAccountID masks the middle of an identifier-shaped
// string, exposing only a length-appropriate trailing slice
// plus (for IBAN-style strings) the 2-letter country prefix.
//
// Rules:
//   - Strings with any non-alphanumeric character (spaces,
//     parens, slashes) pass through unchanged. Synthetic display
//     strings like "Portfolio overlay" / "(no portfolio)" /
//     "Brokerage (other)" stay legible.
//   - Strings that are alphanumeric-only AND contain at least
//     one digit are treated as bank-issued IDs and redacted.
//     Purely-alphabetic tokens (UBS portfolio LABELS like
//     "Savings" / "Brokerage") are left alone; their info
//     content is taxonomy, not identifier.
//   - Trailing-slice length scales down with the input so very
//     short IDs don't reveal a guessable amount:
//     length ≥ 8  → last 4
//     length 6-7  → last 3
//     length 4-5  → last 2
//     length ≤ 3  → fully starred
//   - Output length always matches the input. The redacted form
//     is visually a drop-in replacement for the original so
//     column widths and eyeball-scan shape are preserved
//     between privacy-on and privacy-off views.
//   - IBAN-shape (length 15-34, starts with two ASCII letters):
//     the 2-letter country code stays legible at the front, so
//     CH / DE / etc. is identifiable. The 15-34 bound matches
//     the real IBAN length range and excludes long alphanumeric
//     tokens (Schwab hashValues happen to be 64 hex chars and
//     can start with letters; those don't qualify as IBANs).
func redactAccountID(s string) string {
	// Synthetic-suffix IDs (currently only the UBS-adapter
	// "<portfolio_id>:overlay" form): redact the ID portion,
	// preserve the suffix verbatim — it's a structural marker,
	// not an identifier component.
	if i := strings.IndexByte(s, ':'); i > 0 {
		return redactAccountID(s[:i]) + s[i:]
	}
	if !isLikelyAccountID(s) {
		return s
	}
	n := len(s)
	if n <= 3 {
		return strings.Repeat("*", n)
	}
	suffixLen := 4
	switch {
	case n <= 5:
		suffixLen = 2
	case n <= 7:
		suffixLen = 3
	}
	suffix := s[n-suffixLen:]
	if n >= 15 && n <= 34 && isASCIILetter(s[0]) && isASCIILetter(s[1]) {
		return s[:2] + strings.Repeat("*", n-2-suffixLen) + suffix
	}
	return strings.Repeat("*", n-suffixLen) + suffix
}

// isLikelyAccountID matches the redactor's contract: at least 3
// chars, alphanumeric-only, and containing at least one digit —
// purely-alphabetic strings are bank-assigned taxonomic labels at
// this layer (UBS "Savings" / "Brokerage") and pass through.
// Strings with any non-alphanumeric character (spaces, parens,
// slashes) pass through unchanged; synthetic display strings like
// "Portfolio overlay" / "(no portfolio)" stay legible.
//
// The shape test is deliberately narrow, and stays that way: it
// decides account-id redaction for every command. A value that
// needs hiding whatever its shape belongs to a class that asks no
// shape question at all, rather than to a widened test here: a
// statement narrative to PrivacyFreeText, a customer-chosen label
// to PrivacyPseudonym.
func isLikelyAccountID(s string) bool {
	if len(s) < 3 {
		return false
	}
	hasDigit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		default:
			return false
		}
	}
	return hasDigit
}

func isASCIILetter(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// sensitiveIDPatterns are bank-identifier shapes that scrubSensitiveIDs
// masks wherever they appear in a rendered cell, independent of the
// column's PrivacyClass. Each entry pairs a tight shape regex with a
// masker that preserves the match's length + separators so column
// widths and scan-shape survive (matching redactAccountID's contract).
//
// This is a content-based defence-in-depth layer over the per-column
// privacy machinery. The per-column pass only redacts columns whose
// whole value IS an identifier (account_id, relationship_id, …); it
// can't reach an identifier that leaks inside a column that's
// PrivacyNone by design — notably position_key / symbol, which stay
// legible under -p because they normally hold public ISIN/ticker
// instrument ids. A UBS mortgage uses its (private) account number as
// the instrument identity, so it surfaces there raw; this pass catches
// it by shape regardless of which column carries it.
var sensitiveIDPatterns = []struct {
	re   *regexp.Regexp
	mask func(string) string
}{
	{
		// UBS banking relationship / account base: a 4-digit branch,
		// a space, then the 8-digit account base (e.g. the
		// relationship_id "<branch> <base>"). It also prefixes the
		// UBS mortgage account number ("<branch> <base>.MMM <n>").
		// The digits-space-digits shape is distinctive in holdings
		// data — ISINs/CUSIPs carry no internal space, monetary
		// amounts have decimal separators, valor numbers are shorter
		// — so matching by shape rarely false-positives. Keeps the
		// last 4 digits for cross-referencing; masks the rest.
		re:   regexp.MustCompile(`\b\d{4} \d{8}\b`),
		mask: func(m string) string { return "**** ****" + m[len(m)-4:] },
	},
}

// scrubSensitiveIDs masks structured bank identifiers wherever they
// appear in s. It runs on every cell under -p/--privacy as a final
// pass after applyPrivacy, so an identifier that leaks inside an
// otherwise-non-private column is caught even when that column's
// PrivacyClass is None.
func scrubSensitiveIDs(s string) string {
	for _, p := range sensitiveIDPatterns {
		s = p.re.ReplaceAllStringFunc(s, p.mask)
	}
	return s
}
