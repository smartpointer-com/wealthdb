package config

import (
	"fmt"
	"regexp"

	"github.com/ptu/wealthdb/internal/silver"
)

// idPattern restricts silver_source_id values to slug-style
// strings — DESIGN.md §5.1 spec.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// symbolOverrideShapeRe matches the ticker shape we'll accept in
// `symbol_overrides[].symbol`. Same surface form the runtime
// validator in cmd_resolve_symbols uses for LLM responses (kept
// in sync intentionally — overrides are held to the same shape
// rules so they don't sneak past the read-time COALESCE as
// garbage). 1-12 chars uppercase letters/digits/dot/hyphen.
var symbolOverrideShapeRe = regexp.MustCompile(`^[A-Z0-9.\-]{1,12}$`)

// Validate checks the structural requirements on a parsed Config.
// Returns a non-nil error describing the first failure; runs no
// I/O.
func (c *Config) Validate() error {
	if c.GoldDB == "" {
		return fmt.Errorf("config: gold_db is required")
	}
	if c.DefaultCurrency == "" {
		return fmt.Errorf("config: default_currency is required")
	}
	if !isLikelyISO4217(c.DefaultCurrency) {
		return fmt.Errorf("config: default_currency %q is not a 3-letter ISO 4217 code", c.DefaultCurrency)
	}

	known := silver.Kinds() // empty during tests that don't blank-import adapters; we tolerate that
	seenIDs := make(map[string]bool, len(c.SilverSources))
	for i, s := range c.SilverSources {
		if !idPattern.MatchString(s.ID) {
			return fmt.Errorf("config: silver_sources[%d].id %q must match %s", i, s.ID, idPattern.String())
		}
		if seenIDs[s.ID] {
			return fmt.Errorf("config: duplicate silver_sources[].id %q", s.ID)
		}
		seenIDs[s.ID] = true

		if s.Kind == "" {
			return fmt.Errorf("config: silver_sources[%d].kind is required", i)
		}
		if len(known) > 0 && !kindIsKnown(s.Kind, known) && s.Kind != "auto" {
			return fmt.Errorf("config: silver_sources[%d].kind %q not registered (known: %v)", i, s.Kind, known)
		}

		// Single-file form OR subsources form — exactly one.
		hasPath := s.Path != ""
		hasSubs := len(s.Subsources) > 0
		switch {
		case !hasPath && !hasSubs:
			return fmt.Errorf("config: silver_sources[%d]: one of `path` or `subsources` is required", i)
		case hasPath && hasSubs:
			return fmt.Errorf("config: silver_sources[%d]: `path` and `subsources` are mutually exclusive", i)
		}
		for j, sub := range s.Subsources {
			if sub.Kind == "" {
				return fmt.Errorf("config: silver_sources[%d].subsources[%d].kind is required", i, j)
			}
			if sub.Path == "" {
				return fmt.Errorf("config: silver_sources[%d].subsources[%d].path is required", i, j)
			}
		}
		for j, rel := range s.Relationships {
			if rel.Label == "" {
				return fmt.Errorf("config: silver_sources[%d].relationships[%d].label is required", i, j)
			}
			if rel.WebID == "" && rel.PSNID == "" {
				return fmt.Errorf("config: silver_sources[%d].relationships[%d]: at least one of web_id or psn_id must be set", i, j)
			}
		}
	}

	// symbol_resolution.overrides: every source must be declared,
	// every kind must be one of the two discriminator values used
	// by the symbol_resolutions table, every lookup_value must be
	// non-empty, every symbol must look ticker-shaped (unless the
	// entry is a `delete: true` suppression). Also reject duplicate
	// (source, kind, value) tuples so the downstream UPSERT loop
	// can't surprise us with last-write-wins.
	if c.SymbolResolution != nil {
		seenOverrideKey := map[string]bool{}
		for i, o := range c.SymbolResolution.Overrides {
			if !seenIDs[o.SilverSourceID] {
				return fmt.Errorf("config: symbol_resolution.overrides[%d]: no silver_sources[].id matches %q", i, o.SilverSourceID)
			}
			if o.LookupKind != "instrument_external_id" && o.LookupKind != "name" {
				return fmt.Errorf("config: symbol_resolution.overrides[%d].lookup_kind %q must be 'instrument_external_id' or 'name'", i, o.LookupKind)
			}
			if o.LookupValue == "" {
				return fmt.Errorf("config: symbol_resolution.overrides[%d].lookup_value is required", i)
			}
			switch {
			case o.Delete && o.Symbol != "":
				return fmt.Errorf("config: symbol_resolution.overrides[%d]: `delete: true` is mutually exclusive with `symbol`", i)
			case o.Delete:
				// Deletion entry — nothing else to validate.
			default:
				if !symbolOverrideShapeRe.MatchString(o.Symbol) {
					return fmt.Errorf("config: symbol_resolution.overrides[%d].symbol %q must be 1-12 chars of uppercase letters/digits/dots/hyphens", i, o.Symbol)
				}
			}
			k := o.SilverSourceID + "\x00" + o.LookupKind + "\x00" + o.LookupValue
			if seenOverrideKey[k] {
				return fmt.Errorf("config: symbol_resolution.overrides[%d]: duplicate (silver_source_id, lookup_kind, lookup_value) tuple", i)
			}
			seenOverrideKey[k] = true
		}
	}

	// account_overrides: every outer key must name a declared
	// silver source (catches typos early); every inner key must be
	// non-empty (an empty account_external_id can't match anything
	// and is almost always user error).
	for sourceID, perAccount := range c.AccountOverrides {
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: account_overrides[%q]: no silver_sources[].id matches", sourceID)
		}
		for acctID, ov := range perAccount {
			if acctID == "" {
				return fmt.Errorf("config: account_overrides[%q]: empty account_external_id key", sourceID)
			}
			if ov.Nickname == "" && ov.Category == "" {
				return fmt.Errorf("config: account_overrides[%q][%q]: at least one of nickname or category must be set", sourceID, acctID)
			}
		}
	}
	return nil
}

// isLikelyISO4217 does a sanity check, not a full ISO 4217 lookup:
// exactly 3 uppercase ASCII letters. Pragmatically enough at this
// scale; we don't want a vendored currency-code table in the binary.
func isLikelyISO4217(s string) bool {
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

func kindIsKnown(want string, known []string) bool {
	for _, k := range known {
		if k == want {
			return true
		}
	}
	return false
}
