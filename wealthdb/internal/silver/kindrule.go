package silver

import (
	"regexp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// KindRule is one row of a source's kind table: a verb of the source's
// own, with its text and a condition, and the verdicts gold reads of the
// rows it matches: the canonical kind and what the row does to the lots
// of its instrument. A table is read top down, and the first rule a row
// meets holds for it (Classify).
type KindRule struct {
	// Verb is the source's own word for the row; "" matches every row.
	Verb string
	// Text, where set, must match the row's text.
	Text *regexp.Regexp
	// When is a condition the row must meet beyond its words.
	When Cond
	// Kind is the row's kind; "" where the rule states none, and the
	// adapter's reading of a verb it does not know holds.
	Kind canonical.TxKind
	// Action is what the row does to the lots of its instrument; ""
	// where its kind says it (canonical.TxAction).
	Action canonical.TxAction
}

// Cond is a condition a kind rule puts on a row beyond its words.
type Cond uint8

const (
	// Always holds for every row.
	Always Cond = iota
	// Cash holds for a row that moves cash: a currency, not an asset
	// held in units.
	Cash
)

// KindRow is what a kind table reads of a row.
type KindRow struct {
	Verb string
	Text string
	Cash bool
}

// Classify is the first rule of rules that row meets, and false where it
// meets none.
func Classify(rules []KindRule, row KindRow) (KindRule, bool) {
	for _, r := range rules {
		if r.Verb != "" && r.Verb != row.Verb {
			continue
		}
		if r.Text != nil && !r.Text.MatchString(row.Text) {
			continue
		}
		if r.When == Cash && !row.Cash {
			continue
		}
		return r, true
	}
	return KindRule{}, false
}
