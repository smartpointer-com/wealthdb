package config

import (
	"bytes"
	"encoding/json"
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
	// a single label the config chooses. Used by the UBS adapter to link
	// the web `banking_relationship_id` (opaque SPA token) to the
	// PSN `relationship_id` (SFTPCHxx, etc.). Optional. The ubs adapter
	// reads and checks it (silver.Settings).
	Relationships json.RawMessage `json:"relationships,omitempty"`
	// Schwab tunes the schwab adapter. Only a schwab-kind source takes
	// it. Optional. The schwab adapter reads and checks it
	// (silver.Settings).
	Schwab json.RawMessage `json:"schwab,omitempty"`

	// settings is what the source's kind parsed from its own key, as
	// Validate compiled it; nil where it states none.
	settings any
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

// ToSilverOpenSpec converts a config.SilverSource into the silver.OpenSpec
// the adapter contract expects: its id, path and subsources, and the
// settings its kind parsed while Validate compiled the config
// (compileSettings).
func (s *SilverSource) ToSilverOpenSpec() silver.OpenSpec {
	out := silver.OpenSpec{ID: s.ID, Path: s.Path, Settings: s.settings}
	for _, sub := range s.Subsources {
		out.Subsources = append(out.Subsources, silver.Subsource{
			Kind: sub.Kind,
			Path: sub.Path,
		})
	}
	return out
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
	known := silver.Kinds() // empty in tests that import no adapter, which admits any kind
	seenIDs := make(map[string]bool, len(c.SilverSources))
	for i := range c.SilverSources {
		s := &c.SilverSources[i]
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
		if err := s.compileSettings(i); err != nil {
			return nil, err
		}
	}
	return seenIDs, nil
}

// compileSettings parses the source's keys that a kind reads
// (silver.SettingsFor), the relationships then the schwab object, and
// keeps what its own kind's parser yields. A key's owner checks it on a
// source of any kind, and may refuse it there. In a binary without the
// kind that reads a key, the key is refused. A kind reads one key
// (silver.Source.Settings), so a source keeps at most one value. A new
// key needs a field of SilverSource and an entry in this list. i is the
// source's index, for the errors.
func (s *SilverSource) compileSettings(i int) error {
	s.settings = nil
	for _, k := range []struct {
		key string
		raw json.RawMessage
	}{{"relationships", s.Relationships}, {"schwab", s.Schwab}} {
		if raw := bytes.TrimSpace(k.raw); len(raw) == 0 || string(raw) == "null" {
			continue
		}
		owner, st, ok := silver.SettingsFor(k.key)
		if !ok {
			return fmt.Errorf("config: silver_sources[%d].%s: no source kind reads it", i, k.key)
		}
		v, err := st.Parse(k.raw, s.Kind)
		if err != nil {
			return fmt.Errorf("config: silver_sources[%d].%w", i, err)
		}
		if owner == s.Kind {
			s.settings = v
		}
	}
	return nil
}
