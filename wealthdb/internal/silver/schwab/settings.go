package schwab

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// MarkGuard finds a bad quote in a position's marks: a spike that
// reverts (docs/adapters/schwab.md §4.1). SpikePct is the move, in
// percent of the previous mark, past which a mark may be a spike;
// RevertPct how close, in percent, one of the next two marks must come
// back to the previous mark for it to be one. A zero field takes
// defaultMarkGuard's value.
type MarkGuard struct {
	SpikePct, RevertPct float64
}

// defaultMarkGuard reads a move of more than 10% that comes back within
// 2% of the previous mark as a spike.
var defaultMarkGuard = MarkGuard{SpikePct: 10, RevertPct: 2}

// WithDefaults fills each zero field from defaultMarkGuard.
func (g MarkGuard) WithDefaults() MarkGuard {
	if g.SpikePct == 0 {
		g.SpikePct = defaultMarkGuard.SpikePct
	}
	if g.RevertPct == 0 {
		g.RevertPct = defaultMarkGuard.RevertPct
	}
	return g
}

// settings is the `schwab` object of a schwab-kind silver source in the
// config: the guard on the api feed's overnight marks
// (docs/adapters/schwab.md §4.1). An absent field takes its default
// (MarkGuard.WithDefaults).
type settings struct {
	// MarkSpikePct is the move, in percent of a position's previous
	// mark, past which a mark may be a spike. Default 10.
	MarkSpikePct *float64 `json:"mark_spike_pct,omitempty"`
	// MarkRevertPct is how close, in percent, a later mark must come back
	// to the previous mark for the move to read as a spike. Default 2;
	// below MarkSpikePct.
	MarkRevertPct *float64 `json:"mark_revert_pct,omitempty"`
}

// markGuard is the guard the settings state. A field they leave out
// stays zero, which Open reads as its default.
func (s settings) markGuard() MarkGuard {
	var g MarkGuard
	if s.MarkSpikePct != nil {
		g.SpikePct = *s.MarkSpikePct
	}
	if s.MarkRevertPct != nil {
		g.RevertPct = *s.MarkRevertPct
	}
	return g
}

// parseSettings reads a source's `schwab` object (silver.Settings): a
// schwab-kind source's only, each percentage above zero, and the
// confirmation band below the hold threshold once the defaults fill
// what the object leaves out. It yields the MarkGuard Open reads.
func parseSettings(raw json.RawMessage, kind string) (any, error) {
	var s settings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("schwab: %w", err)
	}
	if kind != kindName {
		return nil, fmt.Errorf("schwab: applies to a source of kind schwab only, not %q", kind)
	}
	for _, f := range []struct {
		name string
		v    *float64
	}{{"mark_spike_pct", s.MarkSpikePct}, {"mark_revert_pct", s.MarkRevertPct}} {
		if f.v != nil && !(*f.v > 0) {
			return nil, fmt.Errorf("schwab: %s %v must be above 0", f.name, *f.v)
		}
	}
	if g := s.markGuard().WithDefaults(); g.RevertPct >= g.SpikePct {
		return nil, fmt.Errorf("schwab: mark_revert_pct %v must be below mark_spike_pct %v", g.RevertPct, g.SpikePct)
	}
	return s.markGuard(), nil
}

// markGuardOf is the guard a source's settings state (parseSettings);
// the zero guard, which takes the defaults, where they state none.
func markGuardOf(spec silver.OpenSpec) MarkGuard {
	g, _ := spec.Settings.(MarkGuard)
	return g
}
