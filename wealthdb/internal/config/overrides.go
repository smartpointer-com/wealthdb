package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// AccountOverride is one per-account override entry. All fields
// are optional; an empty string means "don't override that
// column". AccountKind, TaxWrapper and ManagementStyle are validated
// against the canonical enums at config-load time; bad values fail the
// load rather than landing as gibberish in gold.
type AccountOverride struct {
	Nickname string `json:"nickname,omitempty"`
	Category string `json:"category,omitempty"`
	// AccountKind restates what the account is where its source types
	// it wrong, such as an account an aggregator files under the wrong
	// kind. A card, a mortgage or an overlay keeps its source's kind.
	// The wrapper is its own override.
	AccountKind string `json:"account_kind,omitempty"`
	TaxWrapper  string `json:"tax_wrapper,omitempty"`
	// TaxWrapperFrom (YYYY-MM-DD) dates TaxWrapper: from that day the
	// account has the override's wrapper, and before it the wrapper it
	// has without this override. Empty means the override holds for the
	// whole history. The day must be after 1970-01-01 (0 is unset) and
	// no later than today, since the dimension carries the override's
	// wrapper from the load on. The cash flow boundary reads the wrapper
	// as of each row's day (docs/CASHFLOW.md §3).
	TaxWrapperFrom  string `json:"tax_wrapper_from,omitempty"`
	ManagementStyle string `json:"management_style,omitempty"`
	// Exclude drops the account from gold entirely — the dimension
	// row and every fact keyed to it, swept once the load's streams
	// have drained. It is for an account a collector enumerates
	// but the relationship does not hold: a provider whose UI lists
	// its whole product menu per login reports the products the
	// holder never opened alongside the ones they did, and the
	// scraper cannot tell them apart. Such an account reaches gold
	// as a real account that happens to be empty, which is exactly
	// how a real account the provider under-reports also looks.
	//
	// The distinction is the holder's to make, never the loader's,
	// so nothing here infers it from emptiness. Excluding an account
	// that does hold something removes that money from every total,
	// which is why the load prints what each exclusion dropped.
	Exclude bool `json:"exclude,omitempty"`

	// taxWrapperFrom is TaxWrapperFrom as Unix seconds, compiled by
	// Validate.
	taxWrapperFrom int64
}

// HoldingAlias is one entry of HoldingAliases: Key is read as As, in
// Source, in Account or in every account when Account is empty.
type HoldingAlias struct {
	Source  string `json:"source"`
	Account string `json:"account,omitempty"`
	Key     string `json:"key"`
	As      string `json:"as"`
	Note    string `json:"note,omitempty"`
}

// DeclaredAccount is one `declared_accounts` entry: an account the
// holder states rather than the product observes. The kind and the
// wrapper are required because they are the whole point — the wrapper
// is what places a move to the account, and nothing else about it is
// ever read — and both are validated against the canonical enums. A
// declaration is unfalsifiable by the product's own instruments, which
// is why it carries no balance and the load prints what it stamped.
type DeclaredAccount struct {
	AccountKind string `json:"account_kind"`
	TaxWrapper  string `json:"tax_wrapper"`
	Nickname    string `json:"nickname,omitempty"`
	Currency    string `json:"currency,omitempty"`
}

// PortfolioOverride is one per-portfolio override entry. Applies
// to every account in gold whose `portfolio_external_id` matches.
// All fields are optional; the canonical-enum-typed ones are
// validated at config-load time.
//
// TaxWrapper restates the wrapper of every account in the portfolio,
// for a portfolio held inside a wrapper (retirement, trust or
// foundation), where the adapter's `taxable_personal` default is wrong
// for every wallet in it.
type PortfolioOverride struct {
	TaxWrapper string `json:"tax_wrapper,omitempty"`
	// Exclude drops the portfolio and every account inside it — the
	// portfolio's own dimension row, each account's, and every fact
	// keyed to those accounts. It is the portfolio-grain form of
	// AccountOverride.Exclude: a provider login can carry a whole
	// relationship the holder does not own, and naming its accounts
	// one by one is a roster to maintain that the provider can
	// silently add to.
	//
	// Returns-only exclusion is a different instrument: returns_exclude
	// keeps the money in net worth and removes it from the coarse-grain
	// return math, where this removes it from gold entirely.
	Exclude bool `json:"exclude,omitempty"`
}

// InstrumentOverride is one per-instrument override entry, validated
// against the canonical enums at config-load time. It pins the 2-D
// taxonomy pair (TAXONOMY.md) for a holding the adapters misclassify —
// e.g. an exchange-traded commodity trust whose name gives nothing
// away. Both fields are required and must form an admitted pair (e.g.
// a bond ETF is `fixed_income` × `etf`).
type InstrumentOverride struct {
	AssetClass string `json:"asset_class,omitempty"`
	Vehicle    string `json:"vehicle,omitempty"`
}

// Supersession is the `supersession` block of wealthdb.cfg: per
// source, per account_external_id, the YYYY-MM-DD from which that
// source's rows are dropped because another source carries the account
// from then on.
type Supersession struct {
	Accounts map[string]map[string]string `json:"accounts,omitempty"` // source_id -> account_external_id -> date

	// ends is Accounts as Unix seconds (UTC midnight), compiled by
	// Validate.
	ends map[string]map[string]int64
}

// compile checks the block and keeps its dates as Unix seconds. It has
// the checks of inception_overrides' nested grains: a source id that
// names nothing is a typo worth failing on, and a date that will not
// parse would silently supersede nothing.
func (s *Supersession) compile(seenIDs map[string]bool) error {
	ends, err := compileNestedDates("supersession", "accounts", s.Accounts, seenIDs)
	if err != nil {
		return err
	}
	s.ends = ends
	return nil
}

// Epochs returns the dates Validate compiled, as Unix seconds (UTC
// midnight), keyed source -> account, with no entry for a source that
// names no account. Each call returns fresh maps, so a caller may keep
// one. A nil receiver, or one never validated, returns a nil map.
func (s *Supersession) Epochs() map[string]map[string]int64 {
	if s == nil || s.ends == nil {
		return nil
	}
	out := make(map[string]map[string]int64, len(s.ends))
	for source, accounts := range s.ends {
		if len(accounts) > 0 {
			out[source] = maps.Clone(accounts)
		}
	}
	return out
}

// DeclaredAccountChanges is `declared_accounts` as the account rows
// the load stamps, sorted by id, every one under
// canonical.DeclaredSourceID. Both seen-at columns carry `now`: a
// declaration is a statement, not an observation, so config always
// wins on a re-stamp.
func (c *Config) DeclaredAccountChanges(now int64) []canonical.AccountChange {
	out := make([]canonical.AccountChange, 0, len(c.DeclaredAccounts))
	for _, id := range slices.Sorted(maps.Keys(c.DeclaredAccounts)) {
		d := c.DeclaredAccounts[id]
		wrapper := canonical.TaxWrapper(d.TaxWrapper)
		name := id
		if d.Nickname != "" {
			name = d.Nickname
		}
		row := canonical.AccountChange{
			SilverSourceID:    canonical.DeclaredSourceID,
			AccountExternalID: id,
			AccountKind:       canonical.AccountKind(d.AccountKind),
			DisplayName:       &name,
			TaxWrapper:        &wrapper,
			FirstSeenAt:       now,
			LastSeenAt:        now,
		}
		if d.Nickname != "" {
			n := d.Nickname
			row.Nickname = &n
		}
		if d.Currency != "" {
			ccy := d.Currency
			row.BaseCurrency = &ccy
		}
		out = append(out, row)
	}
	return out
}

// HoldingAliasRows is the holding_aliases block as gold stores it.
func (c *Config) HoldingAliasRows() []canonical.HoldingAlias {
	out := make([]canonical.HoldingAlias, 0, len(c.HoldingAliases))
	for _, a := range c.HoldingAliases {
		out = append(out, canonical.HoldingAlias{SilverSourceID: a.Source, AccountExternalID: a.Account,
			FromKey: a.Key, ToKey: a.As})
	}
	return out
}

// TaxWrapperFromEpoch is TaxWrapperFrom at UTC midnight, as Validate
// compiled it; 0 when unset or never validated.
func (o AccountOverride) TaxWrapperFromEpoch() int64 {
	return o.taxWrapperFrom
}

// validatePortfolioOverrides checks portfolio_overrides: same shape rules as
// account_overrides but keyed by portfolio_external_id, with tax_wrapper and
// exclude as the only fields.
func (c *Config) validatePortfolioOverrides(seenIDs map[string]bool) error {
	return validateNested("portfolio_overrides", "portfolio_external_id", c.PortfolioOverrides, seenIDs, false,
		func(path, _, _ string, ov PortfolioOverride) error {
			if ov.TaxWrapper == "" && !ov.Exclude {
				return fmt.Errorf("config: %s: tax_wrapper or exclude must be set", path)
			}
			// Same contradiction the account grain refuses: the rows a
			// wrapper would be stamped on are the rows the exclusion
			// removes.
			if ov.Exclude && ov.TaxWrapper != "" {
				return fmt.Errorf("config: %s: exclude cannot be combined with tax_wrapper — the portfolio is dropped, so there is no row to stamp", path)
			}
			if ov.TaxWrapper != "" && !canonical.TaxWrapper(ov.TaxWrapper).Valid() {
				return fmt.Errorf("config: %s: invalid tax_wrapper %q", path, ov.TaxWrapper)
			}
			return nil
		})
}

// validateDeclaredAccounts checks declared_accounts: every id is a
// well-formed key, and the kind and the wrapper — the two fields the
// statement reads — are both present and both in their enums. The
// currency is checked only for shape, like default_currency.
func (c *Config) validateDeclaredAccounts(map[string]bool) error {
	for _, id := range slices.Sorted(maps.Keys(c.DeclaredAccounts)) {
		d := c.DeclaredAccounts[id]
		if !IDPattern.MatchString(id) {
			return fmt.Errorf("config: declared_accounts[%q]: id must match %s", id, IDPattern.String())
		}
		if d.AccountKind == "" {
			return fmt.Errorf("config: declared_accounts[%q]: account_kind is required", id)
		}
		if !canonical.AccountKind(d.AccountKind).Valid() {
			return fmt.Errorf("config: declared_accounts[%q]: invalid account_kind %q", id, d.AccountKind)
		}
		if d.TaxWrapper == "" {
			return fmt.Errorf("config: declared_accounts[%q]: tax_wrapper is required — it is what places a move to the account", id)
		}
		if !canonical.TaxWrapper(d.TaxWrapper).Valid() {
			return fmt.Errorf("config: declared_accounts[%q]: invalid tax_wrapper %q", id, d.TaxWrapper)
		}
		if d.Currency != "" && !IsLikelyISO4217(d.Currency) {
			return fmt.Errorf("config: declared_accounts[%q]: currency %q is not a 3-letter ISO 4217 code", id, d.Currency)
		}
	}
	return nil
}

// validateHoldingAliases checks holding_aliases: every entry names a
// configured source and two different keys, no key is aliased twice in
// one scope, and no alias points at a key that is itself aliased, so a
// lookup is one step.
func (c *Config) validateHoldingAliases(seenIDs map[string]bool) error {
	seen := map[[3]string]bool{}
	from := map[[2]string]bool{}
	for i, a := range c.HoldingAliases {
		switch {
		case !seenIDs[a.Source]:
			return fmt.Errorf("config: holding_aliases[%d]: no silver_sources[].id matches source %q", i, a.Source)
		case a.Key == "" || a.As == "":
			return fmt.Errorf("config: holding_aliases[%d]: key and as are required", i)
		case a.Key == a.As:
			return fmt.Errorf("config: holding_aliases[%d]: key %q is its own alias", i, a.Key)
		}
		scope := [3]string{a.Source, a.Account, a.Key}
		if seen[scope] {
			return fmt.Errorf("config: holding_aliases[%d]: key %q is aliased twice in source %q", i, a.Key, a.Source)
		}
		seen[scope] = true
		from[[2]string{a.Source, a.Key}] = true
	}
	for i, a := range c.HoldingAliases {
		if from[[2]string{a.Source, a.As}] {
			return fmt.Errorf("config: holding_aliases[%d]: as %q is itself an aliased key in source %q", i, a.As, a.Source)
		}
	}
	return nil
}

// declares reports whether an id names a declared account, for the
// rules that point at one.
func (c *Config) declares(id string) bool {
	_, ok := c.DeclaredAccounts[id]
	return ok
}

// validateAccountOverrides checks account_overrides: every outer key must
// name a declared silver source (catches typos early); every inner key must
// be non-empty (an empty account_external_id can't match anything and is
// almost always a typo); typed fields validate against the canonical
// enums. A valid tax_wrapper_from is compiled onto its override.
func (c *Config) validateAccountOverrides(seenIDs map[string]bool) error {
	return validateNested("account_overrides", "account_external_id", c.AccountOverrides, seenIDs, false,
		func(path, sourceID, acctID string, ov AccountOverride) error {
			if ov.Nickname == "" && ov.Category == "" && ov.AccountKind == "" &&
				ov.TaxWrapper == "" && ov.ManagementStyle == "" && !ov.Exclude {
				return fmt.Errorf("config: %s: at least one of nickname, category, account_kind, tax_wrapper, management_style, or exclude must be set", path)
			}
			// Excluding an account and stamping a column on it are
			// contradictory instructions: the row the column would go
			// on is the row the exclusion removes. Refused rather than
			// silently resolved, because either half could be the one
			// that was meant.
			if ov.Exclude && (ov.Nickname != "" || ov.Category != "" || ov.AccountKind != "" ||
				ov.TaxWrapper != "" || ov.ManagementStyle != "") {
				return fmt.Errorf("config: %s: exclude cannot be combined with a column override — the account is dropped, so there is no row to stamp", path)
			}
			// A kind that follows its source (AccountKind.FollowsSource)
			// is no target: an overlay is synthetic, and a card's or a
			// mortgage's kind is the liability's.
			switch kind := canonical.AccountKind(ov.AccountKind); {
			case ov.AccountKind == "":
			case !kind.Valid() || kind == canonical.AccountKindOverlay:
				return fmt.Errorf("config: %s: invalid account_kind %q", path, ov.AccountKind)
			case kind.FollowsSource():
				return fmt.Errorf("config: %s: account_kind %q: a liability's kind follows its source", path, ov.AccountKind)
			}
			if ov.TaxWrapper != "" && !canonical.TaxWrapper(ov.TaxWrapper).Valid() {
				return fmt.Errorf("config: %s: invalid tax_wrapper %q", path, ov.TaxWrapper)
			}
			ov.taxWrapperFrom = 0
			if ov.TaxWrapperFrom != "" {
				if ov.TaxWrapper == "" {
					return fmt.Errorf("config: %s: tax_wrapper_from dates a tax_wrapper, and none is set", path)
				}
				from, err := parseYYYYMMDD(ov.TaxWrapperFrom)
				if err != nil {
					return fmt.Errorf("config: %s: tax_wrapper_from: %w", path, err)
				}
				// The dimension carries the override's wrapper from the
				// load on, so a day ahead would date it falsely; a day on
				// or before the epoch is no day a wrapper changed on.
				if from <= 0 || from > time.Now().Unix() {
					return fmt.Errorf("config: %s: tax_wrapper_from %s is after today or not after 1970-01-01", path, ov.TaxWrapperFrom)
				}
				ov.taxWrapperFrom = from
			}
			if ov.ManagementStyle != "" && !canonical.ManagementStyle(ov.ManagementStyle).Valid() {
				return fmt.Errorf("config: %s: invalid management_style %q", path, ov.ManagementStyle)
			}
			c.AccountOverrides[sourceID][acctID] = ov
			return nil
		})
}

// validateInstrumentOverrides checks instrument_overrides: same shape rules
// as account_overrides but keyed by instrument_external_id. Both asset_class
// (the exposure) and vehicle are required and must form an admitted taxonomy
// pair.
func (c *Config) validateInstrumentOverrides(seenIDs map[string]bool) error {
	return validateNested("instrument_overrides", "instrument_external_id", c.InstrumentOverrides, seenIDs, false,
		func(path, _, _ string, ov InstrumentOverride) error {
			if ov.AssetClass == "" || ov.Vehicle == "" {
				return fmt.Errorf("config: %s: asset_class and vehicle must both be set", path)
			}
			if !canonical.AssetClass(ov.AssetClass).Valid() {
				return fmt.Errorf("config: %s: invalid asset_class %q", path, ov.AssetClass)
			}
			if !canonical.Vehicle(ov.Vehicle).Valid() {
				return fmt.Errorf("config: %s: invalid vehicle %q", path, ov.Vehicle)
			}
			if !canonical.ValidTaxonomyPair(canonical.AssetClass(ov.AssetClass), canonical.Vehicle(ov.Vehicle)) {
				return fmt.Errorf("config: %s: (%q, %q) is not an admitted taxonomy pair", path, ov.AssetClass, ov.Vehicle)
			}
			return nil
		})
}

// validateTransactionInstruments checks transaction_instruments: the source
// must be declared, the lookup token must be non-empty, and so must the
// instrument it names. The instrument id cannot be checked against gold here
// (no DB access at config load) — the same known gap the families above
// carry — so a typo'd id resolves to a dimension row that is not there.
func (c *Config) validateTransactionInstruments(seenIDs map[string]bool) error {
	return validateNested("transaction_instruments", "lookup token", c.TransactionInstruments, seenIDs, false,
		func(path, sourceID, token, instrID string) error {
			if strings.TrimSpace(token) == "" {
				return fmt.Errorf("config: transaction_instruments[%q]: empty lookup token key", sourceID)
			}
			if strings.TrimSpace(instrID) == "" {
				return fmt.Errorf("config: %s: empty instrument_external_id", path)
			}
			return nil
		})
}
