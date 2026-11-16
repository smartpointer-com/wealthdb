package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// IDPattern restricts silver_source_id values to slug-style
// strings — DESIGN.md §5.1 spec. Exported so the setup wizard can
// reject bad ids at prompt time against the same shape.
var IDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// TickerShape is the strict ticker-shape pattern: 1-12 chars of
// uppercase ASCII letters, digits, dots, or hyphens. It captures the
// surface forms brokers emit (BRK.B, XDEW, IVV, EUNH.DE, etc.) and
// rejects obvious garbage like "BANK INT 07/30". Both
// `symbol_resolution.overrides[].symbol` and the symbols
// resolve-symbols accepts from a model are held to it, so neither
// slips past the read-time COALESCE as garbage.
var TickerShape = regexp.MustCompile(`^[A-Z0-9.\-]{1,12}$`)

// Validate checks the structural requirements on a parsed Config and
// compiles each block into the engine's terms its accessors return.
// Returns a non-nil error describing the first failure; runs no
// I/O.
func (c *Config) Validate() error {
	if c.GoldDB == "" {
		return fmt.Errorf("config: gold_db is required")
	}
	if c.DefaultCurrency == "" {
		return fmt.Errorf("config: default_currency is required")
	}
	if !IsLikelyISO4217(c.DefaultCurrency) {
		return fmt.Errorf("config: default_currency %q is not a 3-letter ISO 4217 code", c.DefaultCurrency)
	}

	seenIDs, err := c.validateSources()
	if err != nil {
		return err
	}
	for _, check := range []func(map[string]bool) error{
		c.validateSymbolResolution,
		c.validatePortfolioOverrides,
		c.validateAccountOverrides,
		c.validateDeclaredAccounts,
		c.validateHoldingAliases,
		c.validateInstrumentOverrides,
		c.validateTransactionInstruments,
		c.validateReturns,
		c.validateSpending,
		c.validateIncome,
		c.validateCashflow,
		c.validateLots,
		c.validateExposure,
		c.validateBenchmarks,
		c.validateYield,
	} {
		if err := check(seenIDs); err != nil {
			return err
		}
	}
	if err := c.validateWeb(); err != nil {
		return err
	}
	return c.validateMCP()
}

// parseYYYYMMDD turns a YYYY-MM-DD string into a Unix-seconds
// timestamp at UTC midnight.
func parseYYYYMMDD(s string) (int64, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, fmt.Errorf("invalid YYYY-MM-DD %q: %w", s, err)
	}
	return t.UTC().Unix(), nil
}

// compileNestedDates checks a source_id -> external_id -> YYYY-MM-DD
// map (validateNested): every inner key is non-empty, and every value
// parses. It returns the dates as Unix seconds at UTC midnight, in the
// same shape, with an entry for every source; nil for an empty map.
func compileNestedDates(block, grain string, m map[string]map[string]string, seenIDs map[string]bool) (map[string]map[string]int64, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]map[string]int64, len(m))
	for source, inner := range m {
		out[source] = make(map[string]int64, len(inner))
	}
	err := validateNested(block+"."+grain, "external-id", m, seenIDs, false, func(path, source, id, d string) error {
		e, err := parseYYYYMMDD(d)
		if err != nil {
			return fmt.Errorf("config: %s: %w", path, err)
		}
		out[source][id] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// validateNested checks a source_id -> external_id -> value map, the
// block at: every source id names a declared silver source (which
// catches a typo at load), and check passes every value, at its
// [source][id] path, and its two keys. Both levels are walked in key
// order, so the first error is the same at every run. Without strict,
// an empty inner key is refused, with noun naming it. With strict, the
// inner ids are account ids written exactly: a source names at least
// one, and none is blank or has space around it. Inner ids cannot be
// checked against gold here (no DB access at config load), so a
// mistyped one silently no-ops, as the other override families' do.
func validateNested[V any](at, noun string, m map[string]map[string]V, seenIDs map[string]bool, strict bool,
	check func(path, source, id string, v V) error) error {
	for _, sourceID := range slices.Sorted(maps.Keys(m)) {
		inner := m[sourceID]
		src := fmt.Sprintf("%s[%q]", at, sourceID)
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: %s: no silver_sources[].id matches", src)
		}
		if strict && len(inner) == 0 {
			return fmt.Errorf("config: %s: name at least one account", src)
		}
		for _, id := range slices.Sorted(maps.Keys(inner)) {
			switch {
			case !strict && id == "":
				return fmt.Errorf("config: %s: empty %s key", src, noun)
			case strict && strings.TrimSpace(id) == "":
				return fmt.Errorf("config: %s: an account id is empty", src)
			case strict && strings.TrimSpace(id) != id:
				return fmt.Errorf("config: %s[%q]: an account id has space around it", src, id)
			}
			if err := check(fmt.Sprintf("%s[%q]", src, id), sourceID, id, inner[id]); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateMatchKnobs bounds the transfer matcher's knobs
// (spending.internal_transfer_matching). They are checked whether or not
// the block is reachable, so a mis-typed value fails at load rather than
// lying in wait: the window cap keeps it from pairing unrelated month-apart
// flows, the tolerance cap from pairing unrelated amounts. There is no
// income or returns twin — one matcher runs, and the families read its
// pairs.
//
// Both knobs bound the matcher's banded passes — the named pass and the
// amount pass — which are the phases that guess. A pair the holder stated
// in the override ledger, or one the source asserted by stamping a
// reference on both legs, spends neither: neither is a guess, so there is
// no band to widen or narrow around it.
func validateMatchKnobs(block string, windowDays *int, tolerancePct *float64) error {
	if windowDays != nil && (*windowDays < 0 || *windowDays > 30) {
		return fmt.Errorf("config: %s.window_days %d out of range [0, 30]", block, *windowDays)
	}
	if tolerancePct != nil && (*tolerancePct < 0 || *tolerancePct > 5) {
		return fmt.Errorf("config: %s.tolerance_pct %g out of range [0, 5]", block, *tolerancePct)
	}
	return nil
}

// validateIDListNested checks one grain map of an id-list block — the
// returns_exclude / returns_hide grains, and the include / exclude sides
// of the spending, income and cashflow `accounts` blocks: every source
// id must be declared, every listed id non-empty.
func validateIDListNested(block, grain string, m map[string][]string, seenIDs map[string]bool) error {
	for _, sourceID := range slices.Sorted(maps.Keys(m)) {
		ids := m[sourceID]
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: %s.%s[%q]: no silver_sources[].id matches", block, grain, sourceID)
		}
		for _, id := range ids {
			if id == "" {
				return fmt.Errorf("config: %s.%s[%q]: empty external-id in list", block, grain, sourceID)
			}
		}
	}
	return nil
}

// IsLikelyISO4217 does a sanity check, not a full ISO 4217 lookup:
// exactly 3 uppercase ASCII letters. Pragmatically enough at this
// scale. A vendored currency table is deliberately absent.
// Exported so the setup wizard validates currency input identically.
func IsLikelyISO4217(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
