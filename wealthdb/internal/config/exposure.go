package config

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// ExposureConfig is the `exposure` block of wealthdb.cfg: what the
// exposure report needs that no source knows. Absent block ⇒ no proxies,
// no reference funds, no tags, no custody, and the default settings. The
// load writes it into gold's exposure tables (gold.SyncExposure).
type ExposureConfig struct {
	// Proxies maps a fund reference to the fund whose composition stands
	// in for it. Keyed by isin:…, cusip:… or symbol:…, as `wealthdb
	// exposure funds` prints a holding's fund.
	Proxies map[string]ExposureProxy `json:"proxies,omitempty"`
	// ReferenceFunds are broad funds fetched for their lines alone: they
	// widen the security master a directly held stock finds its sector
	// and country in. They need not be held.
	ReferenceFunds []string `json:"reference_funds,omitempty"`
	// Tags state what an opaque holding is (a property, a private
	// fund): its sector, country and currency. Keyed by
	// silver_source_id (outer) and instrument_external_id (inner), as
	// instrument_overrides is.
	Tags map[string]map[string]ExposureTag `json:"tags,omitempty"`
	// Custody names who keeps each source's assets, keyed by
	// silver_source_id.
	Custody map[string]ExposureCustody `json:"custody,omitempty"`
	// MaxGapDays is how far a composition may lie from a day and still
	// describe it, before or after; default 400, so a report from the
	// previous year still qualifies.
	MaxGapDays int `json:"max_gap_days,omitempty"`
}

// ExposureProxy is one `exposure.proxies` entry.
type ExposureProxy struct {
	Fund string `json:"fund"`
	// Always makes the proxy replace the fund's own compositions; by
	// default it fills only the days the fund has none for.
	Always bool `json:"always,omitempty"`
	// Leverage scales the proxy's lines (default 1): a three-times fund
	// is three times its index and minus two times cash.
	Leverage *float64 `json:"leverage,omitempty"`
}

// ExposureTag is one `exposure.tags` entry.
type ExposureTag struct {
	Sector   string `json:"sector,omitempty"`
	Country  string `json:"country,omitempty"`
	Currency string `json:"currency,omitempty"`
}

// ExposureCustody is one `exposure.custody` entry.
type ExposureCustody struct {
	Custodian string `json:"custodian"`
	Country   string `json:"country,omitempty"`
}

// maxLeverage bounds a proxy's leverage factor; past it a typo is likelier
// than a fund.
const maxLeverage = 10

// validateExposure checks the exposure block: every fund reference
// parses, no proxy chains or points at itself, every tag names a
// configured source and a vocabulary value, every custody entry a
// configured source.
func (c *Config) validateExposure(seenIDs map[string]bool) error {
	e := c.Exposure
	if e == nil {
		return nil
	}
	if e.MaxGapDays < 0 || e.MaxGapDays > 3650 {
		return fmt.Errorf("config: exposure.max_gap_days %d: want 1 to 3650 days, or 0 for the default", e.MaxGapDays)
	}
	proxied := map[string]string{}
	for key, p := range e.Proxies {
		from, err := canonical.ParseFundRef(key)
		if err != nil {
			return fmt.Errorf("config: exposure.proxies: %w", err)
		}
		to, err := canonical.ParseFundRef(p.Fund)
		if err != nil {
			return fmt.Errorf("config: exposure.proxies[%q].fund: %w", key, err)
		}
		if from == to {
			return fmt.Errorf("config: exposure.proxies[%q]: a fund cannot be its own proxy", key)
		}
		if prev, dup := proxied[from]; dup {
			return fmt.Errorf("config: exposure.proxies: %q and %q name the same fund", prev, key)
		}
		proxied[from] = key
		if p.Leverage != nil {
			l := *p.Leverage
			if l == 0 || math.IsNaN(l) || math.Abs(l) > maxLeverage {
				return fmt.Errorf("config: exposure.proxies[%q].leverage %v: want a non-zero factor within ±%d", key, l, maxLeverage)
			}
		}
	}
	for key, p := range e.Proxies {
		to, _ := canonical.ParseFundRef(p.Fund)
		if _, chained := proxied[to]; chained {
			return fmt.Errorf("config: exposure.proxies[%q]: its proxy %q has a proxy of its own; name the last one directly", key, p.Fund)
		}
	}
	seenRef := map[string]bool{}
	for i, r := range e.ReferenceFunds {
		ref, err := canonical.ParseFundRef(r)
		if err != nil {
			return fmt.Errorf("config: exposure.reference_funds[%d]: %w", i, err)
		}
		if seenRef[ref] {
			return fmt.Errorf("config: exposure.reference_funds[%d]: %q is listed twice", i, r)
		}
		seenRef[ref] = true
	}
	for sourceID, perInstrument := range e.Tags {
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: exposure.tags[%q]: no silver_sources[].id matches", sourceID)
		}
		for instrID, tag := range perInstrument {
			where := fmt.Sprintf("config: exposure.tags[%q][%q]", sourceID, instrID)
			if instrID == "" {
				return fmt.Errorf("config: exposure.tags[%q]: empty instrument_external_id key", sourceID)
			}
			if tag.Sector == "" && tag.Country == "" && tag.Currency == "" {
				return fmt.Errorf("%s: at least one of sector, country or currency must be set", where)
			}
			if tag.Sector != "" && !canonical.Sector(tag.Sector).Valid() {
				return fmt.Errorf("%s: sector %q is not one of %s", where, tag.Sector, sectorNames())
			}
			if tag.Country != "" {
				if _, ok := canonical.CountryByCode(tag.Country); !ok || tag.Country != strings.ToUpper(tag.Country) {
					return fmt.Errorf("%s: country %q is not an ISO 3166 alpha-2 code", where, tag.Country)
				}
			}
			if tag.Currency != "" && !IsLikelyISO4217(tag.Currency) {
				return fmt.Errorf("%s: currency %q is not a 3-letter ISO 4217 code", where, tag.Currency)
			}
		}
	}
	for sourceID, cu := range e.Custody {
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: exposure.custody[%q]: no silver_sources[].id matches", sourceID)
		}
		if strings.TrimSpace(cu.Custodian) == "" {
			return fmt.Errorf("config: exposure.custody[%q]: custodian is required", sourceID)
		}
		if cu.Country != "" {
			if _, ok := canonical.CountryByCode(cu.Country); !ok || cu.Country != strings.ToUpper(cu.Country) {
				return fmt.Errorf("config: exposure.custody[%q]: country %q is not an ISO 3166 alpha-2 code", sourceID, cu.Country)
			}
		}
	}
	return nil
}

func sectorNames() string {
	var names []string
	for _, s := range canonical.Sectors() {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

// ExposureRows is the exposure block as gold stores it, in a stable
// order, with every fund reference in the form gold builds. Call after
// Validate. An absent block yields the default settings alone.
func (c *Config) ExposureRows() canonical.ExposureRows {
	out := canonical.ExposureRows{Settings: canonical.ExposureSettings{
		MaxGapDays: canonical.DefaultMaxGapDays,
		AgedDays:   canonical.DefaultAgedDays,
		PartialPct: canonical.DefaultPartialPct,
	}}
	e := c.Exposure
	if e == nil {
		return out
	}
	if e.MaxGapDays > 0 {
		out.Settings.MaxGapDays = e.MaxGapDays
	}
	for key, p := range e.Proxies {
		from, _ := canonical.ParseFundRef(key)
		to, _ := canonical.ParseFundRef(p.Fund)
		lev := 1.0
		if p.Leverage != nil {
			lev = *p.Leverage
		}
		out.Proxies = append(out.Proxies, canonical.ExposureProxy{FundRef: from, ProxyRef: to, Always: p.Always, Leverage: lev})
	}
	slices.SortFunc(out.Proxies, func(a, b canonical.ExposureProxy) int { return strings.Compare(a.FundRef, b.FundRef) })
	for _, r := range e.ReferenceFunds {
		ref, _ := canonical.ParseFundRef(r)
		out.ReferenceFunds = append(out.ReferenceFunds, ref)
	}
	slices.Sort(out.ReferenceFunds)
	for _, src := range slices.Sorted(maps.Keys(e.Tags)) {
		for _, instr := range slices.Sorted(maps.Keys(e.Tags[src])) {
			t := e.Tags[src][instr]
			out.Tags = append(out.Tags, canonical.ExposureTag{SilverSourceID: src, InstrumentExternalID: instr,
				Sector: t.Sector, Country: t.Country, Currency: t.Currency})
		}
	}
	for _, src := range slices.Sorted(maps.Keys(e.Custody)) {
		cu := e.Custody[src]
		out.Custody = append(out.Custody, canonical.ExposureCustody{SilverSourceID: src,
			Custodian: strings.TrimSpace(cu.Custodian), Country: cu.Country})
	}
	return out
}
