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
// no reference or size funds, no tags, no custody, and the default
// settings. The load writes it into gold's exposure tables
// (gold.SyncExposure).
type ExposureConfig struct {
	// Proxies maps a fund reference to the fund whose composition stands
	// in for it. Keyed by isin:…, cusip:… or symbol:…, as `wealthdb
	// exposure funds` prints a holding's fund.
	Proxies map[string]ExposureProxy `json:"proxies,omitempty"`
	// ReferenceFunds are broad funds fetched for their lines alone: they
	// widen the security master a directly held stock finds its sector
	// and country in. They need not be held.
	ReferenceFunds []string `json:"reference_funds,omitempty"`
	// SizeFunds are broad funds whose weights size the stocks no market
	// cap sizes, each with the share of its markets' value it covers in
	// percent: 100 for an all-cap fund, 85 for a large and mid cap one.
	// They are fetched like reference funds and need not be held.
	SizeFunds map[string]float64 `json:"size_funds,omitempty"`
	// Tags is the path to the tags ledger, a CSV of what no public
	// source can know: a private holding's country, currency and sector
	// (exposure_tags.go). Expanded like equity_transfers; a missing file
	// holds no tags.
	Tags TagsPath `json:"tags,omitempty"`
	// Custody names who keeps each source's assets, keyed by
	// silver_source_id.
	Custody map[string]ExposureCustody `json:"custody,omitempty"`
	// MaxGapDays is how far a composition may lie from a day and still
	// describe it, before or after; default canonical.DefaultMaxGapDays,
	// so a report from the previous year still qualifies.
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

// ExposureCustody is one `exposure.custody` entry.
type ExposureCustody struct {
	Custodian string `json:"custodian"`
	Country   string `json:"country,omitempty"`
}

// maxLeverage bounds a proxy's leverage factor; past it a typo is likelier
// than a fund.
const maxLeverage = 10

// validateExposure checks the exposure block: every fund reference
// parses, no proxy chains or points at itself, every custody entry names
// a configured source. The tags ledger is checked where a load reads it
// (ExposureRows).
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
	sized := map[string]string{}
	for key, cov := range e.SizeFunds {
		ref, err := canonical.ParseFundRef(key)
		if err != nil {
			return fmt.Errorf("config: exposure.size_funds: %w", err)
		}
		if prev, dup := sized[ref]; dup {
			return fmt.Errorf("config: exposure.size_funds: %q and %q name the same fund", prev, key)
		}
		sized[ref] = key
		if math.IsNaN(cov) || cov < 50 || cov > 100 {
			return fmt.Errorf("config: exposure.size_funds[%q] %v: want the share of its markets it covers, 50 to 100", key, cov)
		}
	}
	for sourceID, cu := range e.Custody {
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: exposure.custody[%q]: no silver_sources[].id matches", sourceID)
		}
		if strings.TrimSpace(cu.Custodian) == "" {
			return fmt.Errorf("config: exposure.custody[%q]: custodian is required", sourceID)
		}
		if cu.Country != "" && !isCountryCode(cu.Country) {
			return fmt.Errorf("config: exposure.custody[%q]: country %q is not an ISO 3166 alpha-2 code", sourceID, cu.Country)
		}
	}
	return nil
}

// isCountryCode reports whether s is a country code of the exposure
// vocabulary, upper-case as gold stores it.
func isCountryCode(s string) bool {
	c, ok := canonical.CountryByCode(s)
	return ok && c.Code == s
}

func sectorNames() string {
	var names []string
	for _, s := range canonical.Sectors() {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

// ExposureRows is the exposure block as gold stores it, in a stable
// order, with every fund reference in the form gold builds, and the tags
// ledger read and checked. Call after Validate. An absent block yields
// the default settings alone.
func (c *Config) ExposureRows() (canonical.ExposureRows, error) {
	out := canonical.ExposureRows{Settings: canonical.ExposureSettings{
		MaxGapDays: canonical.DefaultMaxGapDays,
		AgedDays:   canonical.DefaultAgedDays,
		PartialPct: canonical.DefaultPartialPct,
	}}
	e := c.Exposure
	if e == nil {
		return out, nil
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
	for key, cov := range e.SizeFunds {
		ref, _ := canonical.ParseFundRef(key)
		out.SizeFunds = append(out.SizeFunds, canonical.ExposureSizeFund{FundRef: ref, CoveragePct: cov})
	}
	slices.SortFunc(out.SizeFunds, func(a, b canonical.ExposureSizeFund) int { return strings.Compare(a.FundRef, b.FundRef) })
	sources := map[string]bool{}
	for _, s := range c.SilverSources {
		sources[s.ID] = true
	}
	tags, err := readTagLedgerFile(string(e.Tags), sources)
	if err != nil {
		return out, err
	}
	out.Tags = tags
	for _, src := range slices.Sorted(maps.Keys(e.Custody)) {
		cu := e.Custody[src]
		out.Custody = append(out.Custody, canonical.ExposureCustody{SilverSourceID: src,
			Custodian: strings.TrimSpace(cu.Custodian), Country: cu.Country})
	}
	return out, nil
}
