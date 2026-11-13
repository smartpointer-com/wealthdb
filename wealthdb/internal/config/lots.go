package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// LotsConfig is the `lots` block of wealthdb.cfg: which method the lot
// engine relieves lots by, at each grain, and how a reader treats a
// missing cost basis by default. Keys are the external ids every other
// override uses. Precedence: account, then portfolio, then source, then
// the source kind's own method (average for the shadow kinds), then the
// global one. Absent block ⇒ each kind's registered policy, fifo where
// it names no method, missing basis ignored. See docs/LOTS.md §7.
type LotsConfig struct {
	// Method is the global method: fifo (the default), lifo, hifo, lofo
	// or average.
	Method string `json:"method,omitempty"`
	// MissingBasis is the default of `--missing-basis`: "ignore" (a
	// figure that needs a missing cost is blank) or "zero" (a missing
	// cost counts as 0).
	MissingBasis string                                 `json:"missing_basis,omitempty"`
	Sources      map[string]LotsSourceConfig            `json:"sources,omitempty"`
	Portfolios   map[string]map[string]LotsMethodConfig `json:"portfolios,omitempty"`
	Accounts     map[string]map[string]LotsMethodConfig `json:"accounts,omitempty"`
	// Basis states the cost basis of an instrument's lots the engine
	// opens without one in a source: a seed, a receipt nothing pairs, a
	// transfer in with no ledger cost. Shares that reach a custodian
	// from a transfer agent arrive with no cost; the holder knows it.
	Basis []LotsBasisConfig `json:"basis,omitempty"`
}

// LotsBasisConfig is one `lots.basis` entry: PerUnit in the
// instrument's currency, Acquired the acquisition date (YYYY-MM-DD) a
// lot without one takes, Account one key account or "" for every
// account of the source.
type LotsBasisConfig struct {
	Source     string  `json:"source"`
	Account    string  `json:"account,omitempty"`
	Instrument string  `json:"instrument"`
	PerUnit    float64 `json:"per_unit"`
	Acquired   string  `json:"acquired,omitempty"`
	Note       string  `json:"note,omitempty"`
}

// LotsSourceConfig overrides one source's method and its registered
// mode ("fill", "shadow", "off") and grain ("account", "portfolio").
type LotsSourceConfig struct {
	Method string `json:"method,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Grain  string `json:"grain,omitempty"`
}

// LotsMethodConfig is a portfolio's or an account's method.
type LotsMethodConfig struct {
	Method string `json:"method"`
}

// MissingBasis is the configured default reading of a missing cost
// basis: `lots.missing_basis`, else ignore.
func (c *Config) MissingBasis() lots.MissingBasis {
	if c == nil || c.Lots == nil || c.Lots.MissingBasis == "" {
		return lots.MissingIgnore
	}
	return lots.MissingBasis(c.Lots.MissingBasis)
}

// LotsEngine is the `lots` block in the engine's terms, as Validate
// compiled it. A Config never validated reads as one with no lots
// block: the engine's defaults.
func (c *Config) LotsEngine() lots.Config {
	if c.lotsEngine == nil {
		defaults, _ := (*LotsConfig)(nil).compile(nil)
		return defaults
	}
	return *c.lotsEngine
}

// validateLots checks the `lots` block and compiles it (compile). An
// absent block compiles to the engine's defaults.
func (c *Config) validateLots(seenIDs map[string]bool) error {
	eng, err := c.Lots.compile(seenIDs)
	if err != nil {
		return err
	}
	c.lotsEngine = &eng
	return nil
}

// compile checks the block and returns it in the engine's terms: every
// source id names a declared silver source, as returns_policy_overrides'
// do, and every method, mode, grain and missing-basis reading is one the
// engine knows. The keys are checked in order, so the first error is the
// same at every run. A nil receiver is the engine's defaults.
func (l *LotsConfig) compile(seenIDs map[string]bool) (lots.Config, error) {
	out := lots.Config{Method: lots.FIFO}
	if l == nil {
		return out, nil
	}
	if l.Method != "" {
		m, err := lots.ParseMethod(l.Method)
		if err != nil {
			return out, fmt.Errorf("config: lots.method: %w", err)
		}
		out.Method = m
	}
	if l.MissingBasis != "" {
		if _, err := lots.ParseMissingBasis(l.MissingBasis); err != nil {
			return out, fmt.Errorf("config: lots.missing_basis: %w", err)
		}
	}
	if len(l.Sources) > 0 {
		out.Sources = make(map[string]lots.SourceConfig, len(l.Sources))
	}
	for _, id := range slices.Sorted(maps.Keys(l.Sources)) {
		if !seenIDs[id] {
			return out, fmt.Errorf("config: lots.sources[%q]: no silver_sources[].id matches", id)
		}
		s := l.Sources[id]
		var sc lots.SourceConfig
		if s.Method != "" {
			m, err := lots.ParseMethod(s.Method)
			if err != nil {
				return out, fmt.Errorf("config: lots.sources[%q].method: %w", id, err)
			}
			sc.Method = &m
		}
		if s.Mode != "" {
			m, err := lots.ParseMode(s.Mode)
			if err != nil {
				return out, fmt.Errorf("config: lots.sources[%q].mode: %w", id, err)
			}
			sc.Mode = &m
		}
		if s.Grain != "" {
			g, err := lots.ParseGrain(s.Grain)
			if err != nil {
				return out, fmt.Errorf("config: lots.sources[%q].grain: %w", id, err)
			}
			sc.Grain = &g
		}
		out.Sources[id] = sc
	}
	var err error
	if out.Portfolios, err = lotsMethods("portfolios", l.Portfolios, seenIDs); err != nil {
		return out, err
	}
	if out.Accounts, err = lotsMethods("accounts", l.Accounts, seenIDs); err != nil {
		return out, err
	}
	seen := map[[3]string]bool{}
	for i, b := range l.Basis {
		switch {
		case !seenIDs[b.Source]:
			return out, fmt.Errorf("config: lots.basis[%d]: no silver_sources[].id matches source %q", i, b.Source)
		case b.Instrument == "":
			return out, fmt.Errorf("config: lots.basis[%d]: instrument is required", i)
		case !(b.PerUnit >= 0):
			return out, fmt.Errorf("config: lots.basis[%d]: per_unit must be 0 or more", i)
		}
		k := [3]string{b.Source, b.Account, b.Instrument}
		if seen[k] {
			return out, fmt.Errorf("config: lots.basis[%d]: a second basis for %q in source %q", i, b.Instrument, b.Source)
		}
		seen[k] = true
		rule := lots.BasisRule{Source: b.Source, Account: b.Account, Instrument: b.Instrument,
			Basis: lots.Basis{PerUnit: b.PerUnit, AcqDay: lots.NoDay}}
		if b.Acquired != "" {
			at, err := parseYYYYMMDD(b.Acquired)
			if err != nil {
				return out, fmt.Errorf("config: lots.basis[%d].acquired: %w", i, err)
			}
			rule.Basis.AcqDay = lots.DayOf(at)
		}
		out.Basis = append(out.Basis, rule)
	}
	return out, nil
}

// lotsMethods compiles one grain's source -> id -> method map
// (validateNested); nil for an empty one.
func lotsMethods(grain string, in map[string]map[string]LotsMethodConfig, seenIDs map[string]bool) (map[string]map[string]lots.Method, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]map[string]lots.Method, len(in))
	for src, ids := range in {
		out[src] = make(map[string]lots.Method, len(ids))
	}
	err := validateNested("lots."+grain, "id", in, seenIDs, false, func(path, src, id string, mc LotsMethodConfig) error {
		m, err := lots.ParseMethod(mc.Method)
		if err != nil {
			return fmt.Errorf("config: %s.method: %w", path, err)
		}
		out[src][id] = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
