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
// no reference or size funds, no declarations, no custody, and the
// default settings. The load writes it into gold's exposure tables
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
	// Declarations is the path to the declarations ledger, NDJSON of
	// what no public source can know: where a private holding's value
	// is, as a split per dimension, its legal situs, and the yield
	// figures no source states (exposure_declarations.go). Expanded like
	// equity_transfers; a missing file holds no declarations.
	Declarations DeclarationsPath `json:"declarations,omitempty"`
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

// validateExposure checks the exposure block and compiles it
// (ExposureConfig.compile).
func (c *Config) validateExposure(seenIDs map[string]bool) error {
	rows, err := c.Exposure.compile(seenIDs)
	if err != nil {
		return err
	}
	c.exposureRows = &rows
	return nil
}

// compile checks the block and returns it as gold stores it, in a
// stable order, with every fund reference in the form gold builds:
// every fund reference parses, no proxy chains or points at itself,
// every custody entry names a configured source. The declarations
// ledger is read and checked where a load reads it (ExposureRows). A
// nil receiver yields the default settings alone.
func (e *ExposureConfig) compile(seenIDs map[string]bool) (canonical.ExposureRows, error) {
	out := canonical.ExposureRows{Settings: canonical.ExposureSettings{
		MaxGapDays: canonical.DefaultMaxGapDays,
		AgedDays:   canonical.DefaultAgedDays,
		PartialPct: canonical.DefaultPartialPct,
	}}
	if e == nil {
		return out, nil
	}
	if e.MaxGapDays < 0 || e.MaxGapDays > 3650 {
		return out, fmt.Errorf("config: exposure.max_gap_days %d: want 1 to 3650 days, or 0 for the default", e.MaxGapDays)
	}
	if e.MaxGapDays > 0 {
		out.Settings.MaxGapDays = e.MaxGapDays
	}
	proxied := map[string]string{}
	proxyOf := map[string]string{}
	proxyKeys := slices.Sorted(maps.Keys(e.Proxies))
	for _, key := range proxyKeys {
		p := e.Proxies[key]
		from, err := canonical.ParseFundRef(key)
		if err != nil {
			return out, fmt.Errorf("config: exposure.proxies: %w", err)
		}
		to, err := canonical.ParseFundRef(p.Fund)
		if err != nil {
			return out, fmt.Errorf("config: exposure.proxies[%q].fund: %w", key, err)
		}
		if from == to {
			return out, fmt.Errorf("config: exposure.proxies[%q]: a fund cannot be its own proxy", key)
		}
		if prev, dup := proxied[from]; dup {
			return out, fmt.Errorf("config: exposure.proxies: %q and %q name the same fund", prev, key)
		}
		proxied[from] = key
		proxyOf[key] = to
		lev := 1.0
		if p.Leverage != nil {
			l := *p.Leverage
			if l == 0 || math.IsNaN(l) || math.Abs(l) > maxLeverage {
				return out, fmt.Errorf("config: exposure.proxies[%q].leverage %v: want a non-zero factor within ±%d", key, l, maxLeverage)
			}
			lev = l
		}
		out.Proxies = append(out.Proxies, canonical.ExposureProxy{FundRef: from, ProxyRef: to, Always: p.Always, Leverage: lev})
	}
	for _, key := range proxyKeys {
		if _, chained := proxied[proxyOf[key]]; chained {
			return out, fmt.Errorf("config: exposure.proxies[%q]: its proxy %q has a proxy of its own; name the last one directly", key, e.Proxies[key].Fund)
		}
	}
	slices.SortFunc(out.Proxies, func(a, b canonical.ExposureProxy) int { return strings.Compare(a.FundRef, b.FundRef) })
	seenRef := map[string]bool{}
	for i, r := range e.ReferenceFunds {
		ref, err := canonical.ParseFundRef(r)
		if err != nil {
			return out, fmt.Errorf("config: exposure.reference_funds[%d]: %w", i, err)
		}
		if seenRef[ref] {
			return out, fmt.Errorf("config: exposure.reference_funds[%d]: %q is listed twice", i, r)
		}
		seenRef[ref] = true
		out.ReferenceFunds = append(out.ReferenceFunds, ref)
	}
	slices.Sort(out.ReferenceFunds)
	sized := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(e.SizeFunds)) {
		cov := e.SizeFunds[key]
		ref, err := canonical.ParseFundRef(key)
		if err != nil {
			return out, fmt.Errorf("config: exposure.size_funds: %w", err)
		}
		if prev, dup := sized[ref]; dup {
			return out, fmt.Errorf("config: exposure.size_funds: %q and %q name the same fund", prev, key)
		}
		sized[ref] = key
		if math.IsNaN(cov) || cov < 50 || cov > 100 {
			return out, fmt.Errorf("config: exposure.size_funds[%q] %v: want the share of its markets it covers, 50 to 100", key, cov)
		}
		out.SizeFunds = append(out.SizeFunds, canonical.ExposureSizeFund{FundRef: ref, CoveragePct: cov})
	}
	slices.SortFunc(out.SizeFunds, func(a, b canonical.ExposureSizeFund) int { return strings.Compare(a.FundRef, b.FundRef) })
	for _, sourceID := range slices.Sorted(maps.Keys(e.Custody)) {
		cu := e.Custody[sourceID]
		if !seenIDs[sourceID] {
			return out, fmt.Errorf("config: exposure.custody[%q]: no silver_sources[].id matches", sourceID)
		}
		if strings.TrimSpace(cu.Custodian) == "" {
			return out, fmt.Errorf("config: exposure.custody[%q]: custodian is required", sourceID)
		}
		if cu.Country != "" && !isCountryCode(cu.Country) {
			return out, fmt.Errorf("config: exposure.custody[%q]: country %q is not an ISO 3166 alpha-2 code", sourceID, cu.Country)
		}
		out.Custody = append(out.Custody, canonical.ExposureCustody{SilverSourceID: sourceID,
			Custodian: strings.TrimSpace(cu.Custodian), Country: cu.Country})
	}
	return out, nil
}

// isCountryCode reports whether s is a country code of the exposure
// vocabulary, upper-case as gold stores it.
func isCountryCode(s string) bool {
	c, ok := canonical.CountryByCode(s)
	return ok && c.Code == s
}

// joinNames is a vocabulary's names as a comma list.
func joinNames[T ~string](vs []T) string {
	names := make([]string, len(vs))
	for i, v := range vs {
		names[i] = string(v)
	}
	return strings.Join(names, ", ")
}

// ExposureRows is the exposure block as gold stores it, as Validate
// compiled it, with the declarations ledger read and checked. An absent
// block yields the default settings alone, and a Config never validated
// reads as one with no exposure block.
func (c *Config) ExposureRows() (canonical.ExposureRows, error) {
	if c.exposureRows == nil {
		return (*ExposureConfig)(nil).compile(nil)
	}
	out := *c.exposureRows
	if c.Exposure == nil {
		return out, nil
	}
	sources := map[string]bool{}
	for _, s := range c.SilverSources {
		sources[s.ID] = true
	}
	declarations, err := readDeclarationsFile(string(c.Exposure.Declarations), sources)
	if err != nil {
		return out, err
	}
	out.Declarations = declarations
	return out, nil
}
