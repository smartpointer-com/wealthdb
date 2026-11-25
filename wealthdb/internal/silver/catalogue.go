package silver

import (
	"cmp"
	"path"
	"slices"
	"strings"
)

// Group is where the setup lists a collector: a country, the
// collectors that serve any country, or market and reference data,
// which comes after the banks.
type Group int

// The groups, in the order the setup lists them.
const (
	GroupAustria Group = iota
	GroupSwitzerland
	GroupUnitedStates
	GroupWorldwide
	GroupMarketData
)

// String is the group's heading.
func (g Group) String() string {
	switch g {
	case GroupAustria:
		return "Austria"
	case GroupSwitzerland:
		return "Switzerland"
	case GroupUnitedStates:
		return "United States"
	case GroupWorldwide:
		return "Worldwide"
	case GroupMarketData:
		return "Market and reference data"
	}
	return "Other"
}

// Currency is the currency the accounts of a country's banks are
// usually in; empty for a group of no one country.
func (g Group) Currency() string {
	switch g {
	case GroupAustria:
		return "EUR"
	case GroupSwitzerland:
		return "CHF"
	case GroupUnitedStates:
		return "USD"
	}
	return ""
}

// Collector is one collector whose silver a kind reads: what the setup
// offers to pick, and the config entry a pick writes. Each adapter
// package states its own (Source.Collectors).
type Collector struct {
	// Name is the collector's directory under collectors/, the word
	// after `wealthdb-collect`.
	Name string
	// Display is the name a person recognises: the bank's, and the
	// channel where a bank has two.
	Display string
	// Group is where the setup lists it.
	Group Group
	// Covers says in a few words what it reads, where Display alone
	// does not.
	Covers string
	// Subsource is the subsource kind an entry of the kind lists the
	// collector's silver under; empty for an entry of one path.
	Subsource string
	// Files are the silver files the collector writes in its data
	// folder. Each is an entry of its own, named after the file: the
	// first always, the others once they exist.
	Files []string
	// PerItem: the collector keeps one silver per linked login, at
	// <item>/<item>.db in its data folder, and each is an entry of its
	// own.
	PerItem bool
	// Logins is the verb that lists a per-item collector's logins, with
	// the names its --item flag takes; empty when it has none.
	Logins string
	// Start is what follows `wealthdb-collect <name>` in the command
	// that starts collecting: the verb and its flags, `{item}` standing
	// for a per-item login's name. Empty: `login`.
	Start string
	// FX: its silver holds exchange rates, so the reports can convert
	// the currencies they cover.
	FX bool
	// Kind is the kind that reads the collector's silver; Register
	// fills it.
	Kind string
}

// Catalogue is every registered kind's collectors, by group and then by
// display name.
func Catalogue() []Collector {
	registryMu.RLock()
	defer registryMu.RUnlock()
	var out []Collector
	for _, s := range registry {
		out = append(out, s.Collectors...)
	}
	slices.SortFunc(out, func(a, b Collector) int {
		return cmp.Or(cmp.Compare(a.Group, b.Group), strings.Compare(strings.ToLower(a.Display), strings.ToLower(b.Display)))
	})
	return out
}

// CollectorNamed is the catalogue's collector of that name.
func CollectorNamed(name string) (Collector, bool) {
	for _, c := range Catalogue() {
		if c.Name == name {
			return c, true
		}
	}
	return Collector{}, false
}

// CollectorOf is the collector that writes the silver file at p for a
// source of kind, as one of its subsources of subKind (empty for the
// path form): the only collector of the kind, or the one among several
// whose file the name of p is.
func CollectorOf(kind, subKind, p string) (Collector, bool) {
	var found []Collector
	for _, c := range Catalogue() {
		if c.Kind != kind || subKind != "" && c.Subsource != subKind {
			continue
		}
		found = append(found, c)
	}
	if len(found) == 1 {
		return found[0], true
	}
	base := path.Base(p)
	for _, c := range found {
		if slices.Contains(c.Files, base) {
			return c, true
		}
	}
	return Collector{}, false
}
