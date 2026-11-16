package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// InceptionOverrides is the `inception_overrides` block of wealthdb.cfg.
// Each map value is a YYYY-MM-DD date (UTC midnight). All three maps are
// optional. Keys are the same stable external ids the rest of the config
// uses (silver_source_id, portfolio_external_id, account_external_id) —
// copy them from the `entity_id` column of `wealthdb returns <grain>`.
type InceptionOverrides struct {
	Sources    map[string]string            `json:"sources,omitempty"`    // source_id -> date
	Portfolios map[string]map[string]string `json:"portfolios,omitempty"` // source_id -> portfolio_external_id -> date
	Accounts   map[string]map[string]string `json:"accounts,omitempty"`   // source_id -> account_external_id -> date

	// The three maps as Unix seconds (UTC midnight), compiled by Validate.
	sources              map[string]int64
	portfolios, accounts map[string]map[string]int64
}

// Epochs returns the dates as Unix seconds (UTC midnight), in the three
// maps the returns engine resolves against, as Validate compiled them.
// A nil receiver, or one never validated, returns three nil maps.
func (o *InceptionOverrides) Epochs() (sources map[string]int64, portfolios, accounts map[string]map[string]int64) {
	if o == nil {
		return nil, nil, nil
	}
	return o.sources, o.portfolios, o.accounts
}

// compile checks the block and keeps its dates as Unix seconds: source
// ids must name a declared silver source (catches typos early);
// portfolio/account ids must be non-empty; every value must parse as
// YYYY-MM-DD. Portfolio / account ids can't be checked against gold
// here (no DB access at load), so a typo'd inner key silently no-ops.
func (o *InceptionOverrides) compile(seenIDs map[string]bool) error {
	var sources map[string]int64
	if len(o.Sources) > 0 {
		sources = make(map[string]int64, len(o.Sources))
	}
	for _, sourceID := range slices.Sorted(maps.Keys(o.Sources)) {
		d := o.Sources[sourceID]
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: inception_overrides.sources[%q]: no silver_sources[].id matches", sourceID)
		}
		e, err := parseYYYYMMDD(d)
		if err != nil {
			return fmt.Errorf("config: inception_overrides.sources[%q]: %w", sourceID, err)
		}
		sources[sourceID] = e
	}
	portfolios, err := compileNestedDates("inception_overrides", "portfolios", o.Portfolios, seenIDs)
	if err != nil {
		return err
	}
	accounts, err := compileNestedDates("inception_overrides", "accounts", o.Accounts, seenIDs)
	if err != nil {
		return err
	}
	o.sources, o.portfolios, o.accounts = sources, portfolios, accounts
	return nil
}

// ReturnsExclude is the `returns_exclude` block of wealthdb.cfg. Each map is
// keyed by silver_source_id and lists the external ids to omit from higher-grain
// return aggregates. Both maps are optional. Keys are the stable external ids
// (portfolio_external_id / account_external_id) from the `entity_id` column.
type ReturnsExclude struct {
	Portfolios map[string][]string `json:"portfolios,omitempty"` // source_id -> [portfolio_external_id...]
	Accounts   map[string][]string `json:"accounts,omitempty"`   // source_id -> [account_external_id...]
}

// ReturnsHide is the `returns_hide` block of wealthdb.cfg — the display
// mirror of `returns_exclude`: listed accounts / portfolios keep contributing
// their values and flows to every aggregate, but emit no rows of their own at
// any grain. Same shape and keys as ReturnsExclude.
type ReturnsHide struct {
	Portfolios map[string][]string `json:"portfolios,omitempty"` // source_id -> [portfolio_external_id...]
	Accounts   map[string][]string `json:"accounts,omitempty"`   // source_id -> [account_external_id...]
}

// Sets turns the hide lists into source-keyed membership sets the returns
// engine tests against. A nil receiver returns two nil maps.
func (h *ReturnsHide) Sets() (portfolios, accounts map[string]map[string]bool) {
	if h == nil {
		return nil, nil
	}
	return excludeSets(h.Portfolios), excludeSets(h.Accounts)
}

// Sets turns the exclude lists into source-keyed membership sets the returns
// engine tests against. A nil receiver returns two nil maps.
func (e *ReturnsExclude) Sets() (portfolios, accounts map[string]map[string]bool) {
	if e == nil {
		return nil, nil
	}
	return excludeSets(e.Portfolios), excludeSets(e.Accounts)
}

func excludeSets(in map[string][]string) map[string]map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string]bool, len(in))
	for src, ids := range in {
		set := make(map[string]bool, len(ids))
		for _, id := range ids {
			set[id] = true
		}
		out[src] = set
	}
	return out
}

// ReturnsPolicyOverride is one entry of the `returns_policy_overrides` block
// of wealthdb.cfg: config-side adjustments to a source's registered
// ReturnsPolicy. Pointer fields distinguish "not set" (keep the registered
// value) from an explicit override; an empty object is a no-op.
type ReturnsPolicyOverride struct {
	// FlowRegime names the flow-classification regime that replaces the
	// registered one: "flow_complete" or "nav_only".
	FlowRegime *string `json:"flow_regime,omitempty"`
	// AccountsGrain names the per-account display mode that replaces the
	// registered one: "normal" (full rows), "blanked" (values shown, TWR/MWR
	// n/a), or "hidden" (no rows anywhere; values and flows still aggregate).
	AccountsGrain *string `json:"accounts_grain,omitempty"`

	// The two names parsed, by Validate; nil where unset.
	regime *returns.Regime
	grain  *returns.AccountsGrainMode
}

// ReturnsTransferMatching is the `returns_transfer_matching` block of
// wealthdb.cfg.
type ReturnsTransferMatching struct {
	// Enabled nets the transfer pairs the load stores out of the return
	// aggregates that hold both legs. False (or an absent block) reads no
	// pair.
	Enabled bool `json:"enabled"`
	// WindowDays and TolerancePct are accepted and ignored: the load's
	// matcher pairs the legs, under spending.internal_transfer_matching's
	// knobs. Config.Notices says so where either is set.
	WindowDays   *int     `json:"window_days,omitempty"`
	TolerancePct *float64 `json:"tolerance_pct,omitempty"`
}

// ignoredKnobsNotice is the notice of a block that sets window_days or
// tolerance_pct (Config.Notices).
const ignoredKnobsNotice = "config: returns_transfer_matching: window_days and tolerance_pct are ignored; " +
	"spending.internal_transfer_matching sets the matcher's window and tolerance"

// Regime returns the FlowRegime Validate parsed and whether one is set.
// A nil receiver, or one never validated, reports unset.
func (o *ReturnsPolicyOverride) Regime() (returns.Regime, bool) {
	if o == nil || o.regime == nil {
		return 0, false
	}
	return *o.regime, true
}

// AccountsGrainMode returns the AccountsGrain display mode Validate
// parsed and whether one is set, as Regime does.
func (o *ReturnsPolicyOverride) AccountsGrainMode() (returns.AccountsGrainMode, bool) {
	if o == nil || o.grain == nil {
		return 0, false
	}
	return *o.grain, true
}

// validateReturns checks the returns blocks: inception_overrides,
// supersession, returns_exclude / returns_hide and returns_policy_overrides.
func (c *Config) validateReturns(seenIDs map[string]bool) error {
	if o := c.InceptionOverrides; o != nil {
		if err := o.compile(seenIDs); err != nil {
			return err
		}
	}
	if s := c.Supersession; s != nil {
		if err := s.compile(seenIDs); err != nil {
			return err
		}
	}

	// returns_exclude / returns_hide: source ids must name a declared silver
	// source; listed portfolio/account ids must be non-empty. Portfolio/
	// account ids can't be checked against gold here (no DB access at load).
	if e := c.ReturnsExclude; e != nil {
		if err := validateIDListNested("returns_exclude", "portfolios", e.Portfolios, seenIDs); err != nil {
			return err
		}
		if err := validateIDListNested("returns_exclude", "accounts", e.Accounts, seenIDs); err != nil {
			return err
		}
	}
	if h := c.ReturnsHide; h != nil {
		if err := validateIDListNested("returns_hide", "portfolios", h.Portfolios, seenIDs); err != nil {
			return err
		}
		if err := validateIDListNested("returns_hide", "accounts", h.Accounts, seenIDs); err != nil {
			return err
		}
	}

	// returns_policy_overrides: source ids must name a declared silver
	// source; flow_regime and accounts_grain must name known values. An
	// empty or null override object is a no-op, not an error.
	for _, sourceID := range slices.Sorted(maps.Keys(c.ReturnsPolicyOverrides)) {
		ov := c.ReturnsPolicyOverrides[sourceID]
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: returns_policy_overrides[%q]: no silver_sources[].id matches", sourceID)
		}
		if ov == nil {
			continue
		}
		ov.regime, ov.grain = nil, nil
		if ov.FlowRegime != nil {
			r, err := returns.ParseRegime(*ov.FlowRegime)
			if err != nil {
				return fmt.Errorf("config: returns_policy_overrides[%q].flow_regime: %w", sourceID, err)
			}
			ov.regime = &r
		}
		if ov.AccountsGrain != nil {
			m, err := returns.ParseAccountsGrainMode(*ov.AccountsGrain)
			if err != nil {
				return fmt.Errorf("config: returns_policy_overrides[%q].accounts_grain: %w", sourceID, err)
			}
			ov.grain = &m
		}
	}

	return nil
}
