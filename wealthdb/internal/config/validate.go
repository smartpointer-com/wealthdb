package config

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// IDPattern restricts silver_source_id values to slug-style
// strings — DESIGN.md §5.1 spec. Exported so the setup wizard can
// reject bad ids at prompt time against the same shape.
var IDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

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
	if !IsLikelyISO4217(c.DefaultCurrency) {
		return fmt.Errorf("config: default_currency %q is not a 3-letter ISO 4217 code", c.DefaultCurrency)
	}

	known := silver.Kinds() // empty during tests that don't blank-import adapters; we tolerate that
	seenIDs := make(map[string]bool, len(c.SilverSources))
	for i, s := range c.SilverSources {
		if !IDPattern.MatchString(s.ID) {
			return fmt.Errorf("config: silver_sources[%d].id %q must match %s", i, s.ID, IDPattern.String())
		}
		if seenIDs[s.ID] {
			return fmt.Errorf("config: duplicate silver_sources[].id %q", s.ID)
		}
		seenIDs[s.ID] = true

		if s.Kind == "" {
			return fmt.Errorf("config: silver_sources[%d].kind is required", i)
		}
		if len(known) > 0 && !slices.Contains(known, s.Kind) && s.Kind != "auto" {
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

	// portfolio_overrides: same shape rules as account_overrides
	// but keyed by portfolio_external_id. tax_wrapper is the only
	// dimension wired through today.
	for sourceID, perPortfolio := range c.PortfolioOverrides {
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: portfolio_overrides[%q]: no silver_sources[].id matches", sourceID)
		}
		for portfolioID, ov := range perPortfolio {
			if portfolioID == "" {
				return fmt.Errorf("config: portfolio_overrides[%q]: empty portfolio_external_id key", sourceID)
			}
			if ov.TaxWrapper == "" {
				return fmt.Errorf("config: portfolio_overrides[%q][%q]: tax_wrapper must be set", sourceID, portfolioID)
			}
			if !canonical.TaxWrapper(ov.TaxWrapper).Valid() {
				return fmt.Errorf("config: portfolio_overrides[%q][%q]: invalid tax_wrapper %q", sourceID, portfolioID, ov.TaxWrapper)
			}
		}
	}

	// account_overrides: every outer key must name a declared
	// silver source (catches typos early); every inner key must be
	// non-empty (an empty account_external_id can't match anything
	// and is almost always user error); typed fields validate
	// against the canonical enums.
	for sourceID, perAccount := range c.AccountOverrides {
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: account_overrides[%q]: no silver_sources[].id matches", sourceID)
		}
		for acctID, ov := range perAccount {
			if acctID == "" {
				return fmt.Errorf("config: account_overrides[%q]: empty account_external_id key", sourceID)
			}
			if ov.Nickname == "" && ov.Category == "" &&
				ov.TaxWrapper == "" && ov.ManagementStyle == "" {
				return fmt.Errorf("config: account_overrides[%q][%q]: at least one of nickname, category, tax_wrapper, or management_style must be set", sourceID, acctID)
			}
			if ov.TaxWrapper != "" && !canonical.TaxWrapper(ov.TaxWrapper).Valid() {
				return fmt.Errorf("config: account_overrides[%q][%q]: invalid tax_wrapper %q", sourceID, acctID, ov.TaxWrapper)
			}
			if ov.ManagementStyle != "" && !canonical.ManagementStyle(ov.ManagementStyle).Valid() {
				return fmt.Errorf("config: account_overrides[%q][%q]: invalid management_style %q", sourceID, acctID, ov.ManagementStyle)
			}
		}
	}
	// instrument_overrides: same shape rules as account_overrides but
	// keyed by instrument_external_id. Both asset_class (the exposure)
	// and vehicle are required and must form an admitted taxonomy pair.
	for sourceID, perInstrument := range c.InstrumentOverrides {
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: instrument_overrides[%q]: no silver_sources[].id matches", sourceID)
		}
		for instrID, ov := range perInstrument {
			if instrID == "" {
				return fmt.Errorf("config: instrument_overrides[%q]: empty instrument_external_id key", sourceID)
			}
			if ov.AssetClass == "" || ov.Vehicle == "" {
				return fmt.Errorf("config: instrument_overrides[%q][%q]: asset_class and vehicle must both be set", sourceID, instrID)
			}
			if !canonical.AssetClass(ov.AssetClass).Valid() {
				return fmt.Errorf("config: instrument_overrides[%q][%q]: invalid asset_class %q", sourceID, instrID, ov.AssetClass)
			}
			if !canonical.Vehicle(ov.Vehicle).Valid() {
				return fmt.Errorf("config: instrument_overrides[%q][%q]: invalid vehicle %q", sourceID, instrID, ov.Vehicle)
			}
			if !canonical.ValidTaxonomyPair(canonical.AssetClass(ov.AssetClass), canonical.Vehicle(ov.Vehicle)) {
				return fmt.Errorf("config: instrument_overrides[%q][%q]: (%q, %q) is not an admitted taxonomy pair", sourceID, instrID, ov.AssetClass, ov.Vehicle)
			}
		}
	}

	// inception_overrides: source ids must name a declared silver
	// source (catches typos early); portfolio/account ids must be
	// non-empty; every value must parse as YYYY-MM-DD. Portfolio /
	// account ids can't be checked against gold here (no DB access at
	// load), so a typo'd inner key silently no-ops — a known v1 gap.
	if o := c.InceptionOverrides; o != nil {
		for sourceID, d := range o.Sources {
			if !seenIDs[sourceID] {
				return fmt.Errorf("config: inception_overrides.sources[%q]: no silver_sources[].id matches", sourceID)
			}
			if _, err := parseYYYYMMDD(d); err != nil {
				return fmt.Errorf("config: inception_overrides.sources[%q]: %w", sourceID, err)
			}
		}
		if err := validateInceptionNested("portfolios", o.Portfolios, seenIDs); err != nil {
			return err
		}
		if err := validateInceptionNested("accounts", o.Accounts, seenIDs); err != nil {
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
	for sourceID, ov := range c.ReturnsPolicyOverrides {
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: returns_policy_overrides[%q]: no silver_sources[].id matches", sourceID)
		}
		if ov == nil {
			continue
		}
		if ov.FlowRegime != nil {
			if _, err := returns.ParseRegime(*ov.FlowRegime); err != nil {
				return fmt.Errorf("config: returns_policy_overrides[%q].flow_regime: %w", sourceID, err)
			}
		}
		if ov.AccountsGrain != nil {
			if _, err := returns.ParseAccountsGrainMode(*ov.AccountsGrain); err != nil {
				return fmt.Errorf("config: returns_policy_overrides[%q].accounts_grain: %w", sourceID, err)
			}
		}
	}

	if m := c.ReturnsTransferMatching; m != nil {
		if err := validateMatchKnobs("returns_transfer_matching", m.WindowDays, m.TolerancePct); err != nil {
			return err
		}
	}

	// spending: the account-scope overrides get the same treatment as
	// returns_exclude — declared source, non-empty ids — plus the one
	// check that shape cannot express: an account listed on both sides
	// has no defensible answer, and gold's spend_account_scope is
	// keyed so it could hold only one of them. Reject it here rather
	// than let a primary-key violation surface mid-load.
	if sp := c.Spending; sp != nil {
		if a := sp.Accounts; a != nil {
			if err := validateIDListNested("spending.accounts", "include", a.Include, seenIDs); err != nil {
				return err
			}
			if err := validateIDListNested("spending.accounts", "exclude", a.Exclude, seenIDs); err != nil {
				return err
			}
			for sourceID, ids := range a.Include {
				excluded := make(map[string]bool, len(a.Exclude[sourceID]))
				for _, id := range a.Exclude[sourceID] {
					excluded[id] = true
				}
				for _, id := range ids {
					if excluded[id] {
						return fmt.Errorf("config: spending.accounts[%q]: %q is listed in both include and exclude", sourceID, id)
					}
				}
			}
			// A duplicate WITHIN one list is the same class of problem
			// as the overlap above: syncAccountScope inserts one row
			// per listed id into a table keyed (source, account), so a
			// repeat raises a primary-key violation mid-load, after
			// every source has already been written. The shared
			// id-list check cannot make it — returns_exclude and
			// returns_hide fold their lists into sets, where a repeat
			// is harmless.
			for _, grain := range []struct {
				name string
				m    map[string][]string
			}{{"include", a.Include}, {"exclude", a.Exclude}} {
				for sourceID, ids := range grain.m {
					seen := make(map[string]bool, len(ids))
					for _, id := range ids {
						if seen[id] {
							return fmt.Errorf("config: spending.accounts.%s[%q]: %q is listed twice",
								grain.name, sourceID, id)
						}
						seen[id] = true
					}
				}
			}
		}
		if m := sp.InternalTransferMatching; m != nil {
			if err := validateMatchKnobs("spending.internal_transfer_matching", m.WindowDays, m.TolerancePct); err != nil {
				return err
			}
		}
		rules, err := compileSpendRules(sp.Rules)
		if err != nil {
			return err
		}
		sp.rules = rules
		// The context level decides how much of a transaction leaves the
		// machine, so a typo must not fall back to a default — silently
		// resolving "descriptors" to `merchant` would under-deliver, and
		// resolving an unknown name to anything wider would over-share.
		// The sample cap is bounded whether or not the level reads it.
		if cz := sp.Categorization; cz != nil {
			if !ValidSpendContext(cz.Context) {
				return fmt.Errorf("config: spending.categorization.context %q is not one of %q, %q, %q",
					cz.Context, SpendContextMerchant, SpendContextDescriptor, SpendContextTransaction)
			}
			if cz.DescriptorSamples != nil && (*cz.DescriptorSamples < 0 || *cz.DescriptorSamples > 20) {
				return fmt.Errorf("config: spending.categorization.descriptor_samples %d out of range [0, 20]", *cz.DescriptorSamples)
			}
		}
	}

	// web: optional dockerized BI server. Only the port needs a
	// shape check; an absent block or zero port means "use the
	// default" (DefaultWebPort), resolved at read time.
	if c.Web != nil && c.Web.Port != 0 && (c.Web.Port < 1 || c.Web.Port > 65535) {
		return fmt.Errorf("config: web.port %d is out of range (1-65535)", c.Web.Port)
	}

	return nil
}

// validateInceptionNested checks one grain map of inception_overrides
// (portfolios or accounts): every source id must be declared, every
// inner id non-empty, every value a YYYY-MM-DD date.
func validateInceptionNested(grain string, m map[string]map[string]string, seenIDs map[string]bool) error {
	for sourceID, inner := range m {
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: inception_overrides.%s[%q]: no silver_sources[].id matches", grain, sourceID)
		}
		for id, d := range inner {
			if id == "" {
				return fmt.Errorf("config: inception_overrides.%s[%q]: empty external-id key", grain, sourceID)
			}
			if _, err := parseYYYYMMDD(d); err != nil {
				return fmt.Errorf("config: inception_overrides.%s[%q][%q]: %w", grain, sourceID, id, err)
			}
		}
	}
	return nil
}

// compileSpendRules compiles spending.rules, naming the offending entry
// by index and text on failure. The pattern is compiled
// case-insensitively; one that matches the empty string — "", ".*",
// "^" — is refused, since it would fire on every row and re-label the
// whole spending population. The category may be any value the
// taxonomy knows (canonical.ValidSpendDetailed), vendored or delta, in
// its own case-sensitive spelling. A consumption category is allowed
// on purpose: the transfer fence keeps person- and IBAN-shaped
// narratives away from the model, and a household that pays a lawyer,
// a contractor or a tax office by wire has every such row fenced —
// a rule is the only instrument short of a per-transaction pin that
// can place a recurring counterparty. The vendored-only restriction
// belongs to the model tier, which guards what the model may say; a
// rule is the holder's own local input, and the model never sees it.
func compileSpendRules(rules []SpendingRule) ([]CompiledSpendRule, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make([]CompiledSpendRule, 0, len(rules))
	for i, r := range rules {
		re, err := regexp.Compile("(?i)" + r.Match)
		if err != nil {
			return nil, fmt.Errorf("config: spending.rules[%d].match %q: %w", i, r.Match, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("config: spending.rules[%d].match %q matches the empty string and would mark every row", i, r.Match)
		}
		if !canonical.ValidSpendDetailed(r.Category) {
			return nil, fmt.Errorf("config: spending.rules[%d].category %q is not a spend_detailed value: case-sensitive, in the taxonomy's own spelling (a vendored detailed value or one of the deltas, docs/SPENDING.md §2)",
				i, r.Category)
		}
		out = append(out, CompiledSpendRule{Match: re, Category: r.Category})
	}
	return out, nil
}

// validateMatchKnobs bounds one transfer matcher's knobs. Both matchers
// (returns_transfer_matching, spending.internal_transfer_matching) run on
// the same core and are checked whether or not their block is enabled or
// even reachable, so a mis-typed value fails at load rather than lying in
// wait: the window cap keeps it from pairing unrelated month-apart flows,
// the tolerance cap from pairing unrelated amounts.
func validateMatchKnobs(block string, windowDays *int, tolerancePct *float64) error {
	if windowDays != nil && (*windowDays < 0 || *windowDays > 30) {
		return fmt.Errorf("config: %s.window_days %d out of range [0, 30]", block, *windowDays)
	}
	if tolerancePct != nil && (*tolerancePct < 0 || *tolerancePct > 5) {
		return fmt.Errorf("config: %s.tolerance_pct %g out of range [0, 5]", block, *tolerancePct)
	}
	return nil
}

// validateIDListNested checks one grain map of an id-list block — the
// returns_exclude / returns_hide grains, and spending.accounts' include /
// exclude sides: every source id must be declared, every listed id
// non-empty.
func validateIDListNested(block, grain string, m map[string][]string, seenIDs map[string]bool) error {
	for sourceID, ids := range m {
		if !seenIDs[sourceID] {
			return fmt.Errorf("config: %s.%s[%q]: no silver_sources[].id matches", block, grain, sourceID)
		}
		for _, id := range ids {
			if id == "" {
				return fmt.Errorf("config: %s.%s[%q]: empty external-id in list", block, grain, sourceID)
			}
		}
	}
	return nil
}

// IsLikelyISO4217 does a sanity check, not a full ISO 4217 lookup:
// exactly 3 uppercase ASCII letters. Pragmatically enough at this
// scale; we don't want a vendored currency-code table in the binary.
// Exported so the setup wizard validates currency input identically.
func IsLikelyISO4217(s string) bool {
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
