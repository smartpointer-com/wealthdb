package config

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// SilverSource is one entry under `silver_sources` in the config
// file. Most sources are single-file: set `path`. Multi-source
// adapters (UBS = web + PSN) instead set `subsources`, with one
// entry per backing silver. Each subsource is optional; at least
// one must be present when `subsources` is used. Path is empty
// in the multi-source form.
type SilverSource struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
	// FxPriority ranks this source when several sources publish an
	// FX rate for the same day: lower = higher priority (so the
	// account-level rate is used and a reference source like fred
	// fills only the days no account source covered). Absent/null =
	// minimum priority. Ties (equal or both-null) break by the order
	// sources appear in the config — earlier wins. See FxSourceOrder.
	FxPriority *int `json:"fx_priority,omitempty"`
	// TaxableWrapper is the taxable wrapper this source's taxable
	// accounts ACTUALLY sit in.
	//
	// An adapter emits `taxable_personal` as its generic taxable
	// answer, because a bank feed says what a product is and never who
	// holds it — a jointly held account and a personally held one are
	// the same product. Whose it is, is the deployment's fact, and
	// this is where it is stated: once per source rather than once per
	// account, so an account opened later is right on the load that
	// first sees it.
	//
	// It rewrites `taxable_personal` and NOTHING ELSE. An account the
	// adapter placed in a retirement, trust or custodial wrapper keeps
	// it, which is what makes a blanket statement safe to make. A
	// per-account `account_overrides` entry still wins over this.
	TaxableWrapper string            `json:"taxable_wrapper,omitempty"`
	Subsources     []SilverSubsource `json:"subsources,omitempty"`
	// Relationships pairs cross-subsource entity identities under
	// a single user-chosen label. Used by the UBS adapter to link
	// the web `banking_relationship_id` (opaque SPA token) to the
	// PSN `relationship_id` (SFTPCHxx, etc.). Optional.
	Relationships []RelationshipPair `json:"relationships,omitempty"`
	// Schwab tunes the schwab adapter. Only a schwab-kind source takes
	// it. Optional.
	Schwab *SchwabSettings `json:"schwab,omitempty"`
}

// SchwabSettings is the `schwab` object of a schwab-kind silver source:
// the guard on the api feed's overnight marks (docs/adapters/schwab.md
// §4.1). An absent field takes its default (silver.MarkGuard.WithDefaults).
type SchwabSettings struct {
	// MarkSpikePct is the move, in percent of a position's previous
	// mark, past which a mark may be a spike. Default 10.
	MarkSpikePct *float64 `json:"mark_spike_pct,omitempty"`
	// MarkRevertPct is how close, in percent, a later mark must come back
	// to the previous mark for the move to read as a spike. Default 2;
	// below MarkSpikePct.
	MarkRevertPct *float64 `json:"mark_revert_pct,omitempty"`
}

// markGuard is the guard the settings state. A field they leave out
// stays zero, which the adapter reads as its default. A nil receiver
// states nothing.
func (s *SchwabSettings) markGuard() silver.MarkGuard {
	var g silver.MarkGuard
	if s == nil {
		return g
	}
	if s.MarkSpikePct != nil {
		g.SpikePct = *s.MarkSpikePct
	}
	if s.MarkRevertPct != nil {
		g.RevertPct = *s.MarkRevertPct
	}
	return g
}

// FxSourceOrder returns the silver_source_ids ordered by FX precedence,
// highest priority first: ascending fx_priority with absent/null treated
// as minimum priority (sorted last), ties broken by the order sources are
// declared in the config. The gold FX resolver uses this to prefer one
// source's rate over another's on days both cover.
func (c *Config) FxSourceOrder() []string {
	type item struct {
		id  string
		pri int
		idx int
	}
	items := make([]item, len(c.SilverSources))
	for i, s := range c.SilverSources {
		pri := math.MaxInt // absent/null => minimum priority
		if s.FxPriority != nil {
			pri = *s.FxPriority
		}
		items[i] = item{id: s.ID, pri: pri, idx: i}
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].pri != items[b].pri {
			return items[a].pri < items[b].pri
		}
		return items[a].idx < items[b].idx // tie: declaration order
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.id
	}
	return out
}

// SilverSubsource is one entry under `silver_sources[].subsources`.
type SilverSubsource struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// RelationshipPair is one entry under `silver_sources[].relationships`.
// At least one of `web_id` or `psn_id` must be set. `label` is
// the canonical user-readable name the adapter stamps on canonical
// records. `psn_start_override`, when set (YYYY-MM-DD), overrides
// the auto-detected cutover date used to splice web↔PSN
// transactions for this relationship.
type RelationshipPair struct {
	Label            string `json:"label"`
	WebID            string `json:"web_id,omitempty"`
	PSNID            string `json:"psn_id,omitempty"`
	PSNStartOverride string `json:"psn_start_override,omitempty"`
}

// Lookup returns the named silver source from the config, or
// (nil, false) if no source with that ID is defined.
func (c *Config) Lookup(id string) (*SilverSource, bool) {
	for i := range c.SilverSources {
		if c.SilverSources[i].ID == id {
			return &c.SilverSources[i], true
		}
	}
	return nil, false
}

// ToSilverOpenSpec converts a config.SilverSource into the
// silver.OpenSpec the adapter contract expects. Lives here (not
// in silver) so config carries the JSON tags / parse logic and
// silver stays JSON-free. Returns an error when a relationship's
// psn_start_override fails to parse (YYYY-MM-DD).
func (s *SilverSource) ToSilverOpenSpec() (silver.OpenSpec, error) {
	out := silver.OpenSpec{ID: s.ID, Path: s.Path, MarkGuard: s.Schwab.markGuard()}
	for _, sub := range s.Subsources {
		out.Subsources = append(out.Subsources, silver.Subsource{
			Kind: sub.Kind,
			Path: sub.Path,
		})
	}
	for _, rel := range s.Relationships {
		var override int64
		if rel.PSNStartOverride != "" {
			t, err := parseYYYYMMDD(rel.PSNStartOverride)
			if err != nil {
				return silver.OpenSpec{}, fmt.Errorf(
					"silver_sources[%q].relationships[%q].psn_start_override: %w",
					s.ID, rel.Label, err)
			}
			override = t
		}
		out.Relationships = append(out.Relationships, silver.RelationshipPair{
			Label:            rel.Label,
			WebID:            rel.WebID,
			PSNID:            rel.PSNID,
			PSNStartOverride: override,
		})
	}
	return out, nil
}

// registeredKind refuses a kind no adapter registers, naming the kinds
// that are. An empty registry, in a test that imports no adapter, admits
// any kind.
func registeredKind(kind string, known []string) error {
	if len(known) == 0 || slices.Contains(known, kind) {
		return nil
	}
	return fmt.Errorf("%q not registered (known: %s)", kind, strings.Join(known, ", "))
}

// validateSources checks every silver_sources entry and returns the set of
// declared source ids the later sections check references against.
func (c *Config) validateSources() (map[string]bool, error) {
	known := silver.Kinds() // empty during tests that don't blank-import adapters; we tolerate that
	seenIDs := make(map[string]bool, len(c.SilverSources))
	for i, s := range c.SilverSources {
		if !IDPattern.MatchString(s.ID) {
			return nil, fmt.Errorf("config: silver_sources[%d].id %q must match %s", i, s.ID, IDPattern.String())
		}
		if seenIDs[s.ID] {
			return nil, fmt.Errorf("config: duplicate silver_sources[].id %q", s.ID)
		}
		if s.ID == canonical.DeclaredSourceID {
			return nil, fmt.Errorf("config: silver_sources[%d].id %q is reserved for `declared_accounts`", i, s.ID)
		}
		seenIDs[s.ID] = true

		if s.Kind == "" {
			return nil, fmt.Errorf("config: silver_sources[%d].kind is required", i)
		}
		if err := registeredKind(s.Kind, known); err != nil {
			return nil, fmt.Errorf("config: silver_sources[%d].kind %w", i, err)
		}

		// A taxable wrapper the deployment states for the source. Any
		// valid wrapper is accepted rather than only the taxable ones:
		// the field says what this source's generically-taxable
		// accounts really are, and a jurisdiction may spell that in a
		// wrapper the vocabulary already has for another purpose.
		if s.TaxableWrapper != "" && !canonical.TaxWrapper(s.TaxableWrapper).Valid() {
			return nil, fmt.Errorf("config: silver_sources[%d].taxable_wrapper %q is not a known tax wrapper",
				i, s.TaxableWrapper)
		}

		// Single-file form OR subsources form — exactly one.
		hasPath := s.Path != ""
		hasSubs := len(s.Subsources) > 0
		switch {
		case !hasPath && !hasSubs:
			return nil, fmt.Errorf("config: silver_sources[%d]: one of `path` or `subsources` is required", i)
		case hasPath && hasSubs:
			return nil, fmt.Errorf("config: silver_sources[%d]: `path` and `subsources` are mutually exclusive", i)
		}
		for j, sub := range s.Subsources {
			if sub.Kind == "" {
				return nil, fmt.Errorf("config: silver_sources[%d].subsources[%d].kind is required", i, j)
			}
			if sub.Path == "" {
				return nil, fmt.Errorf("config: silver_sources[%d].subsources[%d].path is required", i, j)
			}
		}
		for j, rel := range s.Relationships {
			if rel.Label == "" {
				return nil, fmt.Errorf("config: silver_sources[%d].relationships[%d].label is required", i, j)
			}
			if rel.WebID == "" && rel.PSNID == "" {
				return nil, fmt.Errorf("config: silver_sources[%d].relationships[%d]: at least one of web_id or psn_id must be set", i, j)
			}
			if rel.PSNStartOverride != "" {
				if _, err := parseYYYYMMDD(rel.PSNStartOverride); err != nil {
					return nil, fmt.Errorf("config: silver_sources[%d].relationships[%d].psn_start_override: %w", i, j, err)
				}
			}
		}
		if err := s.Schwab.validate(s.Kind); err != nil {
			return nil, fmt.Errorf("config: silver_sources[%d].schwab: %w", i, err)
		}
	}
	return seenIDs, nil
}

// validate checks a source's `schwab` object: a schwab-kind source's
// only, each percentage above zero, and the confirmation band below the
// hold threshold once the defaults fill what the object leaves out. A
// nil receiver is no object.
func (s *SchwabSettings) validate(kind string) error {
	if s == nil {
		return nil
	}
	if kind != "schwab" {
		return fmt.Errorf("applies to a source of kind schwab only, not %q", kind)
	}
	for _, f := range []struct {
		name string
		v    *float64
	}{{"mark_spike_pct", s.MarkSpikePct}, {"mark_revert_pct", s.MarkRevertPct}} {
		if f.v != nil && !(*f.v > 0) {
			return fmt.Errorf("%s %v must be above 0", f.name, *f.v)
		}
	}
	if g := s.markGuard().WithDefaults(); g.RevertPct >= g.SpikePct {
		return fmt.Errorf("mark_revert_pct %v must be below mark_spike_pct %v", g.RevertPct, g.SpikePct)
	}
	return nil
}
