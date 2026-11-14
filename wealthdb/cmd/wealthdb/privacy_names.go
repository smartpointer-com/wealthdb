package main

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

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
	return columnSpec[T]{Name: name, Privacy: PrivacyAccountID,
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
			return gold.NoPortfolioLabel
		}
		return accountLabel(display, id)
	}
	return columnSpec[T]{Name: name, Privacy: PrivacyAccountID,
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

// entityCols are a report's entity and entity_id columns, read off the
// row's source, entity id and label (get): account where the entity is
// an account, which -p shows as accountCol and accountIDCol do, its
// nickname from nicknames; portfolio where it is a portfolio, as
// portfolioCol and typedIDCol do.
func entityCols[T any](kindOf func(string) string, nicknames accountNicknames,
	get func(T) (source, id, label string)) (account, portfolio [2]columnSpec[T]) {
	ref := func(r T) accountRef {
		source, id, label := get(r)
		return accountRef{source: source, name: &label, nickname: nicknames.of(source, id), id: id}
	}
	entity := accountCol(kindOf, ref)
	entity.Name = "entity"
	account = [2]columnSpec[T]{entity, accountIDCol("entity_id", kindOf, ref)}
	portfolio = [2]columnSpec[T]{
		portfolioCol("entity", kindOf, func(r T) (string, string, *string) {
			source, id, label := get(r)
			return source, id, &label
		}),
		typedIDCol("entity_id", kindOf, func(r T) (string, string) {
			source, id, _ := get(r)
			return source, id
		}),
	}
	return account, portfolio
}

// eitherCol is a column that reads as a where first holds and as b
// elsewhere: a row's entity is one of two kinds, each with its own
// redaction. Both columns set PrivacyFunc and Pseudonym.
func eitherCol[T any](name string, first func(T) bool, a, b columnSpec[T]) columnSpec[T] {
	pick := func(r T) columnSpec[T] {
		if first(r) {
			return a
		}
		return b
	}
	return columnSpec[T]{Name: name, Privacy: PrivacyAccountID,
		Extract:     func(r T) string { return pick(r).Extract(r) },
		PrivacyFunc: func(r T) PrivacyClass { return pick(r).PrivacyFunc(r) },
		PrivacyExtract: func(r T) string {
			if c := pick(r); c.PrivacyExtract != nil {
				return c.PrivacyExtract(r)
			}
			return pick(r).Extract(r)
		},
		Pseudonym: func(r T) (string, string) { return pick(r).Pseudonym(r) }}
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

// holdingIDCol is col, whose cells identify a holding: its position key
// or instrument id. A typed holding's (typedHolding) can carry its
// holder's words and an account's or a contract's id, so -p redacts it
// as an account id (redactAccountID): its words stay and any id in it
// masks. get gives the row's source and key; every other row keeps
// col's own rule. A holding's name takes holdingNameCol instead.
func holdingIDCol[T any](col columnSpec[T], kindOf func(string) string, get func(T) (source, key string)) columnSpec[T] {
	class, classOf := col.class(), col.PrivacyFunc
	col.PrivacyFunc = func(r T) PrivacyClass {
		if source, key := get(r); key != "" && typedHolding(kindOf, source) {
			return PrivacyAccountID
		}
		if classOf != nil {
			return classOf(r)
		}
		return class
	}
	return col
}

// holdingNameCol is col, whose cells name a holding. -p hides money and
// identifiers, never what is held, so a name prints as it is. A typed
// holding's (typedHolding) name can carry an account's or a contract's
// id, so -p masks the runs of it that can be one (redactIDRuns) and
// keeps the rest: a name is never one id, so a single word such as
// `Unit7` stays whole. get gives the row's source and key; every other
// row keeps col's own rule.
func holdingNameCol[T any](col columnSpec[T], kindOf func(string) string, get func(T) (source, key string)) columnSpec[T] {
	return typedNameCol(col, func(r T) bool {
		source, key := get(r)
		return key != "" && typedHolding(kindOf, source)
	})
}

// typedNameCol is col with the rows typed reports masked by their id
// runs under -p (holdingNameCol); every other row keeps col's own rule.
func typedNameCol[T any](col columnSpec[T], typed func(T) bool) columnSpec[T] {
	class, classOf := col.class(), col.PrivacyFunc
	shown := col.Extract
	if col.PrivacyExtract != nil {
		shown = col.PrivacyExtract
	}
	col.PrivacyFunc = func(r T) PrivacyClass {
		switch {
		case typed(r):
			return PrivacyNone // PrivacyExtract masks its ids
		case classOf != nil:
			return classOf(r)
		}
		return class
	}
	col.PrivacyExtract = func(r T) string {
		if !typed(r) {
			return shown(r)
		}
		// The content scrub runs ahead of the runs, as in rowsToTable,
		// so an address in the name still reads as one.
		return redactIDRuns(scrubSensitiveIDs(shown(r)))
	}
	return col
}

// holdingCols are a holding's position_key and name columns: key and
// name read them off a row that source says the source of. -p redacts a
// typed holding's key as an account id (holdingIDCol) and masks an id
// typed into its name (holdingNameCol).
func holdingCols[T any](kindOf func(string) string, source, key, name func(T) string) (keyCol, nameCol columnSpec[T]) {
	holding := func(r T) (string, string) { return source(r), key(r) }
	return holdingIDCol(strCol("position_key", key), kindOf, holding),
		holdingNameCol(strCol("name", name), kindOf, holding)
}

// holdingKeyCol is a key column. A security key is a public identifier
// and stays legible under -p, as a public position_key does. A holding
// no key names reads as `holding:<source>:<id>`, which -p hides. A typed
// holding's (typedHolding) id redacts as an account id, as its
// instrument id does, and the prefix and the source id before it stay;
// any other's masks whole.
func holdingKeyCol[T any](name string, kindOf func(string) string, get func(T) string) columnSpec[T] {
	// typedID splits a typed holding's key into the legible prefix and
	// the id; ok is false for any other key.
	typedID := func(r T) (prefix, id string, ok bool) {
		key := get(r)
		source, id, holding := holdingKeyParts(key)
		if !holding || id == "" || !typedHolding(kindOf, source) {
			return "", "", false
		}
		return strings.TrimSuffix(key, id), id, true
	}
	return columnSpec[T]{Name: name, Extract: get,
		PrivacyFunc: func(r T) PrivacyClass {
			_, _, holding := holdingKeyParts(get(r))
			if _, _, typed := typedID(r); holding && !typed {
				return PrivacyFreeText
			}
			// A security key stays, and PrivacyExtract redacts a typed
			// holding's id.
			return PrivacyNone
		},
		PrivacyExtract: func(r T) string {
			prefix, id, ok := typedID(r)
			if !ok {
				return get(r)
			}
			// The content scrub runs ahead of the redaction, as in
			// rowsToTable, so an address in the id still reads as one.
			return prefix + redactAccountID(scrubSensitiveIDs(id))
		}}
}

// holdingKeyCols are a holding key's column, named name (holdingKeyCol),
// and the name column beside it, which label reads. -p masks an id typed
// into a typed holding's name, as in holdingCols.
func holdingKeyCols[T any](name string, kindOf func(string) string,
	key, label func(T) string) (keyCol, nameCol columnSpec[T]) {
	return holdingKeyCol(name, kindOf, key), holdingNameCol(strCol("name", label), kindOf, keyHolding(key))
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
