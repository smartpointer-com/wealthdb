package config

import (
	"fmt"
	"maps"
	"slices"
	"time"

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

// LotsEngine is the `lots` block in the engine's terms. Validate has
// checked every value, so it cannot fail on a loaded config; it still
// reports a bad value for a Config built by hand.
func (c *Config) LotsEngine() (lots.Config, error) {
	out := lots.Config{Method: lots.FIFO}
	l := c.Lots
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
	if len(l.Sources) > 0 {
		out.Sources = make(map[string]lots.SourceConfig, len(l.Sources))
	}
	for id, s := range l.Sources {
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
	if out.Portfolios, err = lotsMethods("portfolios", l.Portfolios); err != nil {
		return out, err
	}
	if out.Accounts, err = lotsMethods("accounts", l.Accounts); err != nil {
		return out, err
	}
	for i, b := range l.Basis {
		rule := lots.BasisRule{Source: b.Source, Account: b.Account, Instrument: b.Instrument,
			Basis: lots.Basis{PerUnit: b.PerUnit, AcqDay: lots.NoDay}}
		if b.Acquired != "" {
			d, err := time.Parse("2006-01-02", b.Acquired)
			if err != nil {
				return out, fmt.Errorf("config: lots.basis[%d].acquired: %w", i, err)
			}
			rule.Basis.AcqDay = lots.DayOf(d.Unix())
		}
		out.Basis = append(out.Basis, rule)
	}
	return out, nil
}

func lotsMethods(grain string, in map[string]map[string]LotsMethodConfig) (map[string]map[string]lots.Method, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]map[string]lots.Method, len(in))
	for src, ids := range in {
		inner := make(map[string]lots.Method, len(ids))
		for id, mc := range ids {
			if id == "" {
				return nil, fmt.Errorf("config: lots.%s[%q]: empty id", grain, src)
			}
			m, err := lots.ParseMethod(mc.Method)
			if err != nil {
				return nil, fmt.Errorf("config: lots.%s[%q][%q].method: %w", grain, src, id, err)
			}
			inner[id] = m
		}
		out[src] = inner
	}
	return out, nil
}

// validateLots checks the `lots` block: every source id names a
// declared silver source, as returns_policy_overrides' do, and every
// method, mode, grain and missing-basis reading is one the engine
// knows.
func (c *Config) validateLots(seenIDs map[string]bool) error {
	l := c.Lots
	if l == nil {
		return nil
	}
	if l.MissingBasis != "" {
		if _, err := lots.ParseMissingBasis(l.MissingBasis); err != nil {
			return fmt.Errorf("config: lots.missing_basis: %w", err)
		}
	}
	for _, grain := range []struct {
		name string
		ids  []string
	}{
		{"sources", slices.Sorted(maps.Keys(l.Sources))},
		{"portfolios", slices.Sorted(maps.Keys(l.Portfolios))},
		{"accounts", slices.Sorted(maps.Keys(l.Accounts))},
	} {
		for _, id := range grain.ids {
			if !seenIDs[id] {
				return fmt.Errorf("config: lots.%s[%q]: no silver_sources[].id matches", grain.name, id)
			}
		}
	}
	seen := map[[3]string]bool{}
	for i, b := range l.Basis {
		switch {
		case !seenIDs[b.Source]:
			return fmt.Errorf("config: lots.basis[%d]: no silver_sources[].id matches source %q", i, b.Source)
		case b.Instrument == "":
			return fmt.Errorf("config: lots.basis[%d]: instrument is required", i)
		case !(b.PerUnit >= 0):
			return fmt.Errorf("config: lots.basis[%d]: per_unit must be 0 or more", i)
		}
		k := [3]string{b.Source, b.Account, b.Instrument}
		if seen[k] {
			return fmt.Errorf("config: lots.basis[%d]: a second basis for %q in source %q", i, b.Instrument, b.Source)
		}
		seen[k] = true
	}
	_, err := c.LotsEngine()
	return err
}
