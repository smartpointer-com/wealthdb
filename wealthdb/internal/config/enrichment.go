package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// SpendingConfig is the `spending` block of wealthdb.cfg.
//
// The model endpoint that prices a category per merchant signature
// lives here too, beside the knobs that decide which rows ever reach
// it, the way `symbol_resolution` groups its own model with its own
// overrides.
type SpendingConfig struct {
	// Accounts is the spending scope's only exception mechanism.
	// Absent ⇒ every account counts; only an entry here takes one out.
	Accounts *SpendingAccounts `json:"accounts,omitempty"`
	// InternalTransferMatching tunes the matcher that pairs the two
	// legs of an own-account move so neither counts as spending.
	// Absent ⇒ the defaults below.
	InternalTransferMatching *SpendingTransferMatching `json:"internal_transfer_matching,omitempty"`
	// Rules are the deployment's own entries in the rule tier: a
	// case-insensitive pattern over a row's narrative (counterparty
	// and description) and the category a match places. Nothing
	// here can be an engine constant, because what identifies these
	// rows is personal text: the account holder's own name on a wire
	// to their account at an untracked bank, an own account number,
	// the legal entity of an exchange the holder also tracks, a fund
	// the holder subscribes to, a lawyer or a tax office paid by
	// wire. The category may be any valid spend_detailed value,
	// vendored or delta (canonical.ValidSpendDetailed): a rule is the
	// holder's own local input and the model never sees it, so the
	// model tier's vendored-only restriction is not this one. Absent
	// ⇒ nothing is marked. Compiled by Validate; read through
	// Config.SpendRules.
	Rules []SpendingRule `json:"rules,omitempty"`
	// Pins is an optional path to the CSV ledger of per-transaction
	// category pins — the top of the precedence lattice, for the row
	// nothing else can classify. Expanded like EquityTransfers; a
	// missing file is a no-op. See docs/DESIGN.md §13.10 and
	// internal/spending/pins.go.
	Pins string `json:"pins,omitempty"`
	// TransferOverrides is an optional path to the CSV ledger of manual
	// match / unmatch decisions for the internal-transfer matcher — the
	// surface for a pair the data cannot settle, in either direction —
	// and of reimburse lines, which link a repayment to its outflow.
	// Expanded like Pins; a missing file is a no-op. See
	// internal/gold/transferoverridefile.go.
	TransferOverrides string `json:"transfer_overrides,omitempty"`
	// Categorization configures the model tier driven by `wealthdb
	// categorize`. Absent ⇒ the command refuses, the way
	// resolve-symbols refuses without `symbol_resolution.model`; the
	// deterministic tiers are unaffected and keep running on load.
	Categorization *SpendingCategorization `json:"categorization,omitempty"`

	// rules is Rules compiled, filled by Validate so a bad rule fails
	// the load and the pass never compiles anything itself.
	rules []CompiledSpendRule
}

// SpendingRule is one entry of `spending.rules` as written in the
// file: `match`, a regular expression, `category`, the spend_detailed
// value a match places, and an optional `scope` narrowing WHERE and
// WHEN the rule is allowed to fire.
type SpendingRule struct {
	Match    string             `json:"match"`
	Category string             `json:"category"`
	Scope    *SpendingRuleScope `json:"scope,omitempty"`
	// Far names a `declared_accounts` entry as the other side of the
	// move the rule places, admitted beside `internal_transfer` alone:
	// only an own-account move HAS a far side. It is what makes a rule
	// say where the money went rather than only that it stayed the
	// holder's; the cash flow statement then places the move by the
	// declared account's wrapper instead of drawing it as a move to a
	// destination nothing identifies (docs/CASHFLOW.md §4).
	Far string `json:"far,omitempty"`
	// AssetClass is what the capital went INTO, stated only where the
	// rule places `investment` — the one verdict of this family whose
	// section is `investing`. It is the EXPOSURE dimension alone
	// (docs/TAXONOMY.md §2), not the 2-D pair `instrument_overrides`
	// insists on: that block classifies a holding, and the cash flow
	// statement's investing class reads one dimension.
	//
	// It exists because the investing class is otherwise the
	// instrument's, and a bank payment order names none — so capital
	// deployed into a destination the product holds in a source that
	// carries no transactions has no road onto its own class and draws
	// as "Untracked investments" (cashflow_txn_nodes).
	AssetClass string `json:"asset_class,omitempty"`
}

// SpendingRuleScope narrows a rule to part of the ledger. Every field
// is optional and an omitted one does not constrain; a rule with no
// scope, or an empty one, matches everywhere.
//
// It exists because a pattern specific enough for one booking is
// rarely specific enough for the whole future. `^\s*closing\s*$` is
// exactly right for one mortgage settlement on one account in one
// month, and a liability the day another bank writes "Closing" on
// something else. Scoping is how a rule can be surgical instead of
// permanent: state the account and the month, and a later row that
// merely reads the same falls through to be asked about rather than
// being silently swept into last year's answer.
//
// `From` and `To` are inclusive ISO dates (YYYY-MM-DD) compared
// against the transaction's own day, so a single-day range is written
// with both set the same.
type SpendingRuleScope struct {
	Source    string `json:"source,omitempty"`    // silver_source_id
	Portfolio string `json:"portfolio,omitempty"` // portfolio_external_id
	Account   string `json:"account,omitempty"`   // account_external_id
	From      string `json:"from,omitempty"`      // inclusive, YYYY-MM-DD
	To        string `json:"to,omitempty"`        // inclusive, YYYY-MM-DD
}

// CompiledSpendRule is a SpendingRule after Validate: the pattern
// compiled case-insensitively, the category checked, the scope's dates
// resolved to the unix bounds the pass compares against. What the
// enrichment pass consumes.
type CompiledSpendRule struct {
	Match      *regexp.Regexp
	Category   string
	AssetClass string
	Far        string
	Scope      CompiledSpendScope
}

// CompiledSpendScope is a SpendingRuleScope with its dates resolved.
// `From`/`To` are unix seconds, inclusive of the whole named day; zero
// means unbounded on that side. It is plain data: whether a row is
// inside a scope is decided by spending.RuleScope, next to the rule
// tier that asks the question.
type CompiledSpendScope struct {
	Source    string
	Portfolio string
	Account   string
	From      int64
	To        int64
}

// SpendingAccounts lists the account-scope overrides, keyed by
// silver_source_id, in the same shape as returns_exclude. `include`
// is a no-op on an account already in the population by default (a
// wallet whose outflows really are spending); `exclude` fences a cash
// or card account out (a card whose spending is not the household's).
// An account may not appear in both.
type SpendingAccounts struct {
	Include map[string][]string `json:"include,omitempty"` // source_id -> [account_external_id...]
	Exclude map[string][]string `json:"exclude,omitempty"` // source_id -> [account_external_id...]
}

// SpendingTransferMatching are the internal-transfer matcher's knobs
// for its banded passes — the phases that infer a pair from two figures
// landing near each other, first among the pairs a narrative names. The
// phases that assert one outright, from the override ledger or from a
// reference the source stamped on both legs, are not banded and these do
// not reach them.
//
// Pointer fields distinguish "not set" (use the default) from an
// explicit value.
type SpendingTransferMatching struct {
	// WindowDays is the max day distance between the two legs of a
	// pair. Defaults to the same 5 days the returns matcher uses: the
	// two share one matching core, and a spending pass that banded
	// differently would call the same movement internal in one report
	// and external in the other.
	WindowDays *int `json:"window_days,omitempty"`
	// TolerancePct is the relative amount tolerance in percent of the
	// larger leg (an absolute floor of 0.01 always applies, so 0 means
	// exact-to-a-cent). Covers a transfer fee deducted in transit.
	TolerancePct *float64 `json:"tolerance_pct,omitempty"`
	// Names say which narratives name which account as the other side of a
	// movement (docs/SPENDING.md §3). A pair one of whose legs names the
	// other's account is claimed ahead of the plain amount pass. Absent
	// leaves the matcher pairing on amount and day alone.
	Names []SpendingMatchName `json:"names,omitempty"`
}

// SpendingMatchName is one spending.internal_transfer_matching.names entry:
// a leg whose narrative matches Match names Source as its other side — or,
// with Account set, that one account of it.
type SpendingMatchName struct {
	Source  string `json:"source"`
	Account string `json:"account,omitempty"`
	Match   string `json:"match"`
	re      *regexp.Regexp
}

// Pattern is Match compiled; nil until Validate has run.
func (n SpendingMatchName) Pattern() *regexp.Regexp { return n.re }

// Defaults for the spending matcher's knobs, deliberately equal to
// DefaultTransferMatchWindowDays / DefaultTransferMatchTolerancePct.
const (
	DefaultSpendMatchWindowDays   = DefaultTransferMatchWindowDays
	DefaultSpendMatchTolerancePct = DefaultTransferMatchTolerancePct
)

// SpendingCategorization is the `spending.categorization` block: the
// model tier's endpoint and the one knob that decides how much of a
// transaction leaves the machine.
type SpendingCategorization struct {
	// Model is the LLM endpoint asked for a category per merchant
	// signature. Same shape and same API support as
	// `symbol_resolution.model`; `wealthdb categorize` refuses without
	// it.
	Model *ModelConfig `json:"model,omitempty"`
	// Context selects how much of a transaction reaches the prompt:
	// SpendContextMerchant, SpendContextDescriptor or
	// SpendContextTransaction. Empty ⇒ the default.
	Context string `json:"context,omitempty"`
	// DescriptorSamples caps the raw narratives sent per merchant at
	// the two context levels that send any. It is deliberately
	// range-checked even at the default level, where nothing reads it:
	// a mis-typed value that only fails once the context is widened
	// fails at the least convenient moment.
	DescriptorSamples *int `json:"descriptor_samples,omitempty"`
	// FencePersonNames keeps a signature that is a bare person's name
	// off a non-card account out of the model tier. Nil ⇒ the default,
	// which is ON: the product is published and most deployments will
	// point `categorize` at a remote endpoint, where a person's name is
	// the one signature shape that is PII by itself.
	//
	// Turning it off is for a model endpoint on this machine, where
	// nothing leaves and a person-shaped payer is just another payer
	// worth naming. It is a pointer so that an explicit `false`
	// survives a round-trip through the config writer, which a plain
	// bool with omitempty would drop.
	FencePersonNames *bool `json:"fence_person_names,omitempty"`
}

// DefaultFencePersonNames is what an absent `fence_person_names`
// resolves to. On, by decision: a deployment that wants a person-shaped
// signature sent to a model says so, and nothing sends one silently.
const DefaultFencePersonNames = true

// The context levels, in increasing order of what leaves the machine.
//
//   - merchant:    the merchant signature alone — a folded, truncated,
//     reference-number-free string. No amounts, no dates, no accounts.
//   - descriptor:  plus the raw narratives the signature was folded
//     from, which carry the spelling, the branch, the city.
//   - transaction: plus date, amount, account kind, and the signatures
//     of what was bought around it.
//
// The DEFAULT IS THE MOST PRIVATE LEVEL, and that is a decision rather
// than a starting point. Every level above it improves the model's
// accuracy on ambiguous merchants and widens what a third-party
// endpoint learns about a household in exchange. A deployment that
// wants the trade is free to make it explicitly; nothing makes it
// silently. The transfer fence (spending.TransferShaped) is
// independent of this knob and gates CANDIDACY, so a wire or P2P
// narrative bearing a person's name is never sent at any level.
const (
	SpendContextMerchant    = "merchant"
	SpendContextDescriptor  = "descriptor"
	SpendContextTransaction = "transaction"
)

// DefaultSpendContext is the context level an absent or empty
// `spending.categorization.context` resolves to.
const DefaultSpendContext = SpendContextMerchant

// DefaultSpendDescriptorSamples is the per-merchant narrative cap when
// `descriptor_samples` is omitted. Three is enough to show a merchant's
// spelling variants without turning one prompt into a transaction log.
const DefaultSpendDescriptorSamples = 3

// SpendContextLevels and IncomeContextLevels spell each family's
// accepted levels for an error message. They sit beside the predicates
// so a level added to one is added to the other in the same edit.
const (
	SpendContextLevels  = "merchant | descriptor | transaction"
	IncomeContextLevels = "payer (or merchant) | descriptor | transaction"
)

// ValidSpendContext reports whether s names a context level. The empty
// string is accepted: an omitted field means the default.
func ValidSpendContext(s string) bool {
	switch s {
	case "", SpendContextMerchant, SpendContextDescriptor, SpendContextTransaction:
		return true
	}
	return false
}

// ValidIncomeContext is ValidSpendContext for the income family, which
// admits SpendContextPayer as well.
func ValidIncomeContext(s string) bool {
	return s == SpendContextPayer || ValidSpendContext(s)
}

// SpendCategorization returns the categorization block, which may be
// nil — its accessors handle that.
func (c *Config) SpendCategorization() *SpendingCategorization {
	if c.Spending == nil {
		return nil
	}
	return c.Spending.Categorization
}

// ContextLevel returns the effective context level. Only call after
// Validate has vetted the name (Load does); a nil receiver or an empty
// field reports the default.
func (s *SpendingCategorization) ContextLevel() string {
	if s == nil || s.Context == "" {
		return DefaultSpendContext
	}
	if s.Context == SpendContextPayer {
		return SpendContextMerchant
	}
	return s.Context
}

// Samples returns the effective per-merchant narrative cap
// (`descriptor_samples`).
func (s *SpendingCategorization) Samples() int {
	if s == nil || s.DescriptorSamples == nil {
		return DefaultSpendDescriptorSamples
	}
	return *s.DescriptorSamples
}

// PersonNameFence reports whether the person-shape arm of the fence is
// on. An absent block, or an absent field, means on — a missing
// configuration must never be the thing that sends a person's name to a
// third party.
func (s *SpendingCategorization) PersonNameFence() bool {
	if s == nil || s.FencePersonNames == nil {
		return DefaultFencePersonNames
	}
	return *s.FencePersonNames
}

// CategorizationModel returns the configured model endpoint, or nil
// when no categorization block (or no model inside one) is set.
func (s *SpendingCategorization) CategorizationModel() *ModelConfig {
	if s == nil {
		return nil
	}
	return s.Model
}

// SpendAccountScope returns the include and exclude maps the
// enrichment pass stamps into gold. A nil block scopes nothing, which
// leaves every account in.
func (c *Config) SpendAccountScope() (include, exclude map[string][]string) {
	if c.Spending == nil || c.Spending.Accounts == nil {
		return nil, nil
	}
	return c.Spending.Accounts.Include, c.Spending.Accounts.Exclude
}

// SpendRules returns the compiled `spending.rules`, or nil when none
// are set. Only call after Validate has compiled them (Load does).
func (c *Config) SpendRules() []CompiledSpendRule {
	if c.Spending == nil {
		return nil
	}
	return c.Spending.rules
}

// SpendPins returns the expanded `spending.pins` path, or "" when the
// ledger is not configured.
func (c *Config) SpendPins() string {
	if c.Spending == nil {
		return ""
	}
	return c.Spending.Pins
}

// SpendTransferOverrides returns the expanded
// `spending.transfer_overrides` path, or "" when the ledger is not
// configured. The matcher it steers is shared with returns, so the
// ledger is read once and handed to both.
func (c *Config) SpendTransferOverrides() string {
	if c.Spending == nil {
		return ""
	}
	return c.Spending.TransferOverrides
}

// SpendMatching returns the spending matcher block, which may be nil —
// its Window and Tolerance accessors handle that.
func (c *Config) SpendMatching() *SpendingTransferMatching {
	if c.Spending == nil {
		return nil
	}
	return c.Spending.InternalTransferMatching
}

// Window returns the effective day window.
func (m *SpendingTransferMatching) Window() int {
	if m == nil || m.WindowDays == nil {
		return DefaultSpendMatchWindowDays
	}
	return *m.WindowDays
}

// Tolerance returns the effective relative tolerance in percent.
func (m *SpendingTransferMatching) Tolerance() float64 {
	if m == nil || m.TolerancePct == nil {
		return DefaultSpendMatchTolerancePct
	}
	return *m.TolerancePct
}

// IncomeConfig is the `income` block of wealthdb.cfg — the inflow
// family's half of what `spending` configures.
//
// Four fields where spending has six. The two that are missing are
// missing on purpose: `internal_transfer_matching` and
// `transfer_overrides` govern the ONE matcher both families read, and
// a second set of knobs would let the same wire be internal on one
// side and external on the other (docs/INCOME.md, decision 8).
type IncomeConfig struct {
	// Accounts is the income scope's only exception mechanism, in
	// `spending.accounts`' shape and with its own contents. The two
	// questions have different exceptions: an account excluded from
	// spending because its outflows double-count giving is not thereby
	// an account whose inflows are not income.
	Accounts *SpendingAccounts `json:"accounts,omitempty"`
	// Rules are the deployment's own entries in the income rule tier:
	// an employer's name to INCOME_SALARY, a pension fund to
	// INCOME_RETIREMENT_PENSION, a benefits agency to
	// INCOME_GOVERNMENT_BENEFITS, a relative to `gift`, the holder's
	// own untracked bank to `internal_transfer`, a private debt fund's
	// distribution to INCOME_INTEREST_EARNED. The value field is named
	// `type` rather than `category`, because that is what the income
	// surface calls it everywhere else. Any valid income value,
	// vendored, extension or delta: a rule is local input the model
	// never sees.
	Rules []IncomeRule `json:"rules,omitempty"`
	// Pins is an optional path to the income pins ledger — the
	// spending ledger's format with `income_detailed` where
	// `spend_detailed` was.
	Pins string `json:"pins,omitempty"`
	// Categorization configures the income model tier. Absent ⇒ it
	// INHERITS `spending.categorization` whole: one household, one
	// local model, and no reason to configure the same endpoint twice.
	// Resolved in one place, IncomeCategorization().
	Categorization *SpendingCategorization `json:"categorization,omitempty"`

	// rules is Rules compiled, filled by Validate.
	rules []CompiledSpendRule
}

// IncomeRule is one entry of `income.rules`. Identical to SpendingRule
// but for the value field's name: the income surface says `type` where
// the spending one says `category`, and a config is read far more
// often than it is written.
type IncomeRule struct {
	Match string             `json:"match"`
	Type  string             `json:"type"`
	Scope *SpendingRuleScope `json:"scope,omitempty"`
	// Far is SpendingRule.Far for the inbound leg: the declared account
	// the money came FROM. The income overlay has no far columns, so
	// the spending pass carries it onto its own overlay for the row
	// (docs/CASHFLOW.md §4).
	Far string `json:"far,omitempty"`
	// AssetClass is SpendingRule.AssetClass for the return leg, stated
	// only where the rule places `capital_return`.
	AssetClass string `json:"asset_class,omitempty"`
}

// CashflowConfig is the `cashflow` block of wealthdb.cfg: one block,
// three fields, all optional.
//
// It is deliberately small. Cashflow adds no categorisation tier and
// buys nothing from a model, so there is no endpoint to point at and
// no backlog to steer; the rules, pins and transfer overrides that
// decide what a row IS are the two families', and a verdict written
// there is what cashflow reads.
type CashflowConfig struct {
	// Accounts is the cash pool's only gate. An account listed here is
	// treated exactly like an account the product does not hold: a move
	// to it is a crossing into `vehicles · Unpaired transfers` rather
	// than an invisible internal step, and the balance memo does not
	// count it. That equivalence is what keeps an exclusion from
	// growing the residual forever.
	//
	// Exclusion only, and that is a decision rather than half a
	// feature: every account is pooled by default, so an `include`
	// could never fence anything and a knob that does nothing is worse
	// than one that is absent.
	Accounts *CashflowAccounts `json:"accounts,omitempty"`
	// Wrappers moves a tax wrapper across the household boundary,
	// keyed by wrapper and valued by where a crossing to it lands —
	// one of canonical.WrapperDestinations. An unlisted wrapper keeps
	// the engine default (canonical.WrapperBoundaries); an unknown
	// wrapper or destination fails the load naming the entry.
	//
	// Per WRAPPER, not per account. An account a source mis-labelled
	// is fixed with the existing per-account `tax_wrapper` override, so
	// that every consumer of the wrapper agrees about whose money it
	// is. The two knobs also take effect on different triggers: this
	// block is re-stamped by every enrichment pass, while an account
	// override reaches the rows a load actually touches, so a reload is
	// what applies one to history.
	Wrappers map[string]string `json:"wrappers,omitempty"`
	// Projection sets the cash flow projection (`cashflow projection`).
	Projection *CashflowProjection `json:"projection,omitempty"`
}

// CashflowProjection is the `cashflow.projection` block.
type CashflowProjection struct {
	// LookbackMonths is how many whole months before the current one
	// the cash flow projection's averages read, 1 to
	// MaxCashflowLookbackMonths; 0 for DefaultCashflowLookbackMonths.
	LookbackMonths int `json:"lookback_months,omitempty"`
}

// The cash flow projection's look-back: twelve months, so a seasonal expense
// is spread over its year, and at most three years.
const (
	DefaultCashflowLookbackMonths = 12
	MaxCashflowLookbackMonths     = 36
)

// CashflowLookback is the cash flow projection's look-back in months:
// cashflow.projection.lookback_months, else the default.
func (c *Config) CashflowLookback() int {
	if c != nil && c.Cashflow != nil && c.Cashflow.Projection != nil && c.Cashflow.Projection.LookbackMonths > 0 {
		return c.Cashflow.Projection.LookbackMonths
	}
	return DefaultCashflowLookbackMonths
}

// CashflowAccounts lists the accounts fenced out of the cash pool,
// keyed by silver_source_id in the shape the two families' scope
// blocks use.
type CashflowAccounts struct {
	Exclude map[string][]string `json:"exclude,omitempty"` // source_id -> [account_external_id...]
}

// CashflowAccountScope returns the pool's exclusions, in the (include,
// exclude) shape the pass stamps scopes in. The include map is always
// nil: the pool's default is every account, so there is nothing for an
// include to widen.
func (c *Config) CashflowAccountScope() (include, exclude map[string][]string) {
	if c.Cashflow == nil || c.Cashflow.Accounts == nil {
		return nil, nil
	}
	return nil, c.Cashflow.Accounts.Exclude
}

// CashflowWrappers returns the per-wrapper boundary overrides, or nil
// when none are set. Only call after Validate has vetted them (Load
// does): the pass composes them over canonical.WrapperBoundaries and
// stamps the result, so an unvetted destination would reach gold.
func (c *Config) CashflowWrappers() map[string]string {
	if c.Cashflow == nil {
		return nil
	}
	return c.Cashflow.Wrappers
}

// SpendContextPayer is the income spelling of the first context level,
// and what docs/INCOME.md uses. `merchant` is accepted from an income
// block as well, so that a `spending.categorization` block INHERITED
// whole validates without being rewritten. Both resolve to one
// constant rather than two code paths: the level decides how much of a
// TRANSACTION leaves the machine, and that question has one answer per
// level whichever family is asking.
const SpendContextPayer = "payer"

// IncomeAccountScope returns the include and exclude maps the income
// half of the pass stamps into gold.
func (c *Config) IncomeAccountScope() (include, exclude map[string][]string) {
	if c.Income == nil || c.Income.Accounts == nil {
		return nil, nil
	}
	return c.Income.Accounts.Include, c.Income.Accounts.Exclude
}

// IncomeRules returns the compiled `income.rules`. Only call after
// Validate has compiled them (Load does).
func (c *Config) IncomeRules() []CompiledSpendRule {
	if c.Income == nil {
		return nil
	}
	return c.Income.rules
}

// IncomePins returns the expanded `income.pins` path, or "" when the
// ledger is not configured.
func (c *Config) IncomePins() string {
	if c.Income == nil {
		return ""
	}
	return c.Income.Pins
}

// IncomeCategorization returns the block the income model tier runs
// on, resolving the inheritance in the one place that should know
// about it: `income.categorization` when set, `spending.categorization`
// otherwise, and nil when neither is.
//
// Inheritance is whole-block rather than per-field. A half-inherited
// endpoint — this deployment's model with that deployment's context
// level — is a configuration nobody wrote down, and the failure would
// be a quiet widening of what leaves the machine.
func (c *Config) IncomeCategorization() *SpendingCategorization {
	if c.Income != nil && c.Income.Categorization != nil {
		return c.Income.Categorization
	}
	return c.SpendCategorization()
}

// IncomeCategorizationKey names the config key an error about the
// income model tier should point at: the income block's own when it
// has one, and the block it inherited otherwise, so a reader is sent
// to the line they have to edit rather than to one that does not exist.
func (c *Config) IncomeCategorizationKey() string {
	if c.Income != nil && c.Income.Categorization != nil {
		return "income.categorization"
	}
	return "spending.categorization"
}

// validateSpending checks spending: the account-scope overrides get the same
// treatment as returns_exclude — declared source, non-empty ids — plus the
// one check that shape cannot express: an account listed on both sides has
// no defensible answer, and each family's scope table is keyed so it could
// hold only one of them. Reject it here rather than let a primary-key
// violation surface mid-load.
func (c *Config) validateSpending(seenIDs map[string]bool) error {
	if sp := c.Spending; sp != nil {
		if err := validateAccountScope("spending.accounts", sp.Accounts, seenIDs); err != nil {
			return err
		}
		if m := sp.InternalTransferMatching; m != nil {
			if err := validateMatchKnobs("spending.internal_transfer_matching", m.WindowDays, m.TolerancePct); err != nil {
				return err
			}
			if err := validateMatchNames(m.Names, seenIDs); err != nil {
				return err
			}
		}
		rules, err := compileRuleList("spending.rules", "category", "spend_detailed", "docs/SPENDING.md §2",
			canonical.SpendDetailedInvestment,
			spendingRuleList(sp.Rules), canonical.ValidSpendDetailed, c.declares, seenIDs)
		if err != nil {
			return err
		}
		sp.rules = rules
		if err := validateCategorization("spending.categorization", SpendContextLevels, sp.Categorization, ValidSpendContext); err != nil {
			return err
		}
	}
	return nil
}

// validateIncome checks income: the inflow family's half, validated by the
// same checks against its own vocabulary. The blocks spending has and this
// one does not — the matcher knobs and the transfer-override ledger — are
// absent by decision, not by omission, and a config naming them under
// `income` fails at unmarshal as an unknown field.
func (c *Config) validateIncome(seenIDs map[string]bool) error {
	if in := c.Income; in != nil {
		if err := validateAccountScope("income.accounts", in.Accounts, seenIDs); err != nil {
			return err
		}
		rules, err := compileRuleList("income.rules", "type", "income_detailed", "docs/INCOME.md §2",
			canonical.IncomeDetailedCapitalReturn,
			incomeRuleList(in.Rules), canonical.ValidIncomeDetailed, c.declares, seenIDs)
		if err != nil {
			return err
		}
		in.rules = rules
		if err := validateCategorization("income.categorization", IncomeContextLevels, in.Categorization, ValidIncomeContext); err != nil {
			return err
		}
	}
	return nil
}

// validateCashflow checks cashflow: the pool's exclusions get the
// account-scope treatment the two families' do, and the wrapper overrides
// are checked against the same two vocabularies gold's stamped table
// restates as CHECK constraints. The first two are rejected here rather
// than at the stamp, because a boundary that fails mid-load leaves the
// table half-written and the statement silently redrawn. The cash flow
// projection's look-back is held to 0 (the default) or 1 to
// MaxCashflowLookbackMonths.
func (c *Config) validateCashflow(seenIDs map[string]bool) error {
	if cf := c.Cashflow; cf != nil {
		if cf.Accounts != nil {
			if err := validateAccountScope("cashflow.accounts",
				&SpendingAccounts{Exclude: cf.Accounts.Exclude}, seenIDs); err != nil {
				return err
			}
		}
		for _, wrapper := range slices.Sorted(maps.Keys(cf.Wrappers)) {
			if !canonical.TaxWrapper(wrapper).Valid() {
				return fmt.Errorf("config: cashflow.wrappers[%q] is not a tax wrapper", wrapper)
			}
			if _, _, ok := canonical.ParseWrapperDestination(cf.Wrappers[wrapper]); !ok {
				return fmt.Errorf("config: cashflow.wrappers[%q]: %q is not a destination (want %s)",
					wrapper, cf.Wrappers[wrapper], strings.Join(canonical.WrapperDestinations, " | "))
			}
		}
		if p := cf.Projection; p != nil && (p.LookbackMonths < 0 || p.LookbackMonths > MaxCashflowLookbackMonths) {
			return fmt.Errorf("config: cashflow.projection.lookback_months %d: want 1 to %d months, or 0 for the default",
				p.LookbackMonths, MaxCashflowLookbackMonths)
		}
	}
	return nil
}

// validateAccountScope is one family's `accounts` block: every source
// declared, no id in both lists, no id twice in one list.
//
// The duplicate check inside one list is the same class of problem as
// the overlap: syncAccountScope inserts one row per listed id into a
// table keyed (source, account), so a repeat raises a primary-key
// violation mid-load, after every source has already been written. The
// shared id-list check cannot make it — returns_exclude and
// returns_hide fold their lists into sets, where a repeat is harmless.
func validateAccountScope(key string, a *SpendingAccounts, seenIDs map[string]bool) error {
	if a == nil {
		return nil
	}
	if err := validateIDListNested(key, "include", a.Include, seenIDs); err != nil {
		return err
	}
	if err := validateIDListNested(key, "exclude", a.Exclude, seenIDs); err != nil {
		return err
	}
	for _, sourceID := range slices.Sorted(maps.Keys(a.Include)) {
		ids := a.Include[sourceID]
		excluded := make(map[string]bool, len(a.Exclude[sourceID]))
		for _, id := range a.Exclude[sourceID] {
			excluded[id] = true
		}
		for _, id := range ids {
			if excluded[id] {
				return fmt.Errorf("config: %s[%q]: %q is listed in both include and exclude", key, sourceID, id)
			}
		}
	}
	for _, grain := range []struct {
		name string
		m    map[string][]string
	}{{"include", a.Include}, {"exclude", a.Exclude}} {
		for _, sourceID := range slices.Sorted(maps.Keys(grain.m)) {
			ids := grain.m[sourceID]
			seen := make(map[string]bool, len(ids))
			for _, id := range ids {
				if seen[id] {
					return fmt.Errorf("config: %s.%s[%q]: %q is listed twice",
						key, grain.name, sourceID, id)
				}
				seen[id] = true
			}
		}
	}
	return nil
}

// validateCategorization checks one family's model-tier block. `levels`
// is the human-readable list the error names, since a rejected context
// is nearly always a spelling a reader can fix from the list alone.
//
// The context level decides how much of a transaction leaves the
// machine, so a typo must not fall back to a default — silently
// resolving "descriptors" to the narrowest level would under-deliver,
// and resolving an unknown name to anything wider would over-share.
// The sample cap is bounded whether or not the level reads it.
func validateCategorization(key, levels string, cz *SpendingCategorization, validContext func(string) bool) error {
	if cz == nil {
		return nil
	}
	if !validContext(cz.Context) {
		return fmt.Errorf("config: %s.context %q is not a context level (want %s)", key, cz.Context, levels)
	}
	if cz.DescriptorSamples != nil && (*cz.DescriptorSamples < 0 || *cz.DescriptorSamples > 20) {
		return fmt.Errorf("config: %s.descriptor_samples %d out of range [0, 20]", key, *cz.DescriptorSamples)
	}
	return nil
}

// ruleEntry is one config rule of either family, flattened so the
// compiler below sees one shape. The two differ only in what the value
// field is called in the file.
type ruleEntry struct {
	match      string
	value      string
	assetClass string
	far        string
	scope      *SpendingRuleScope
}

func spendingRuleList(rules []SpendingRule) []ruleEntry {
	out := make([]ruleEntry, 0, len(rules))
	for _, r := range rules {
		out = append(out, ruleEntry{r.Match, r.Category, r.AssetClass, r.Far, r.Scope})
	}
	return out
}

func incomeRuleList(rules []IncomeRule) []ruleEntry {
	out := make([]ruleEntry, 0, len(rules))
	for _, r := range rules {
		out = append(out, ruleEntry{r.Match, r.Type, r.AssetClass, r.Far, r.Scope})
	}
	return out
}

// compileRuleList compiles one family's `rules[]`, naming the offending
// entry by index and text on failure. `key` is the config path the
// errors point at, `valueField` and `valueNoun` the names that family
// gives its value column, and `doc` the section a reader is sent to.
//
// The pattern is compiled case-insensitively; one that matches the
// empty string — "", ".*", "^" — is refused, since it would fire on
// every row and re-label the whole population. The value may be
// anything `valid` admits — the family's own vocabulary, vendored or
// delta, in its case-sensitive spelling — and the predicate is
// family-fenced, so a spending rule naming an income value fails here
// rather than writing a value the family's reports cannot show. A
// spelling an earlier taxonomy held is refused like any other value
// outside it, and where its successor belongs to this family the error
// names what to write instead (canonical.RetiredDetailed).
//
// A consumption category is allowed on purpose: the transfer fence
// keeps person- and IBAN-shaped narratives away from the model, and a
// household that pays a lawyer, a contractor or a tax office by wire
// has every such row fenced — a rule is the only instrument short of a
// per-transaction pin that can place a recurring counterparty. The
// model-emittable restriction belongs to the model tier, which guards
// what the model may say; a rule is the holder's own local input, and
// the model never sees it.
//
// `investingValue` is the one value of this family's vocabulary whose
// cash flow section is `investing` — `investment` for spending,
// `capital_return` for income. An `asset_class` on any other value is
// refused rather than stored: nothing would read it, and a knob that
// does nothing is worse than one that is absent. A `far` is refused on
// the same terms beside anything but `internal_transfer`, and where it
// names no declared account: a far side that exists nowhere would be
// stamped and read by nothing. A scope's source names a silver source
// in seenIDs, since a misspelt one is a rule that never fires.
func compileRuleList(key, valueField, valueNoun, doc, investingValue string, rules []ruleEntry, valid, declares func(string) bool,
	seenIDs map[string]bool) ([]CompiledSpendRule, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make([]CompiledSpendRule, 0, len(rules))
	for i, r := range rules {
		re, err := regexp.Compile("(?i)" + r.match)
		if err != nil {
			return nil, fmt.Errorf("config: %s[%d].match %q: %w", key, i, r.match, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("config: %s[%d].match %q matches the empty string and would mark every row", key, i, r.match)
		}
		if !valid(r.value) {
			if retired, ok := canonical.RetiredDetailed(r.value); ok && valid(retired.Successor) {
				return nil, fmt.Errorf("config: %s[%d].%s %q is retired: use %s",
					key, i, valueField, r.value, retired.Use)
			}
			return nil, fmt.Errorf("config: %s[%d].%s %q is not a %s value: case-sensitive, in the taxonomy's own spelling (a vendored detailed value, an extension, or one of the deltas, %s)",
				key, i, valueField, r.value, valueNoun, doc)
		}
		if r.assetClass != "" {
			if r.value != investingValue {
				return nil, fmt.Errorf("config: %s[%d].asset_class is only meaningful beside %s %q, which is this family's one investing verdict; got %q",
					key, i, valueField, investingValue, r.value)
			}
			if err := canonical.StatedExposure(r.assetClass); err != nil {
				return nil, fmt.Errorf("config: %s[%d].asset_class: %w", key, i, err)
			}
		}
		if r.far != "" {
			if r.value != canonical.SpendDetailedInternalTransfer {
				return nil, fmt.Errorf("config: %s[%d].far is only meaningful beside %s %q, the one verdict with a far side; got %q",
					key, i, valueField, canonical.SpendDetailedInternalTransfer, r.value)
			}
			if !declares(r.far) {
				return nil, fmt.Errorf("config: %s[%d].far %q names no declared_accounts entry", key, i, r.far)
			}
		}
		if r.scope != nil && r.scope.Source != "" && !seenIDs[r.scope.Source] {
			return nil, fmt.Errorf("config: %s[%d].scope.source %q: no silver_sources[].id matches", key, i, r.scope.Source)
		}
		scope, err := compileSpendScope(r.scope)
		if err != nil {
			return nil, fmt.Errorf("config: %s[%d].scope: %w", key, i, err)
		}
		out = append(out, CompiledSpendRule{Match: re, Category: r.value, AssetClass: r.assetClass, Far: r.far, Scope: scope})
	}
	return out, nil
}

// compileSpendScope resolves a rule's optional scope. The dates become
// the unix bounds of the days they name — `from` at its first second,
// `to` at its last — so both ends are inclusive, and a one-day range is
// written with the two set the same. An inverted range is refused
// rather than silently matching nothing: a scope that can never admit a
// row is a typo every time, and a rule that never fires is invisible.
func compileSpendScope(sc *SpendingRuleScope) (CompiledSpendScope, error) {
	if sc == nil {
		return CompiledSpendScope{}, nil
	}
	out := CompiledSpendScope{
		Source: sc.Source, Portfolio: sc.Portfolio, Account: sc.Account,
	}
	if sc.From != "" {
		from, err := parseYYYYMMDD(sc.From)
		if err != nil {
			return out, fmt.Errorf("from: %w", err)
		}
		out.From = from
	}
	if sc.To != "" {
		to, err := parseYYYYMMDD(sc.To)
		if err != nil {
			return out, fmt.Errorf("to: %w", err)
		}
		out.To = to + 24*60*60 - 1
	}
	if out.From != 0 && out.To != 0 && out.To < out.From {
		return out, fmt.Errorf("to %q is before from %q; the scope could never admit a row", sc.To, sc.From)
	}
	return out, nil
}

// validateMatchNames compiles each spending matcher name and checks that it
// names a declared source: a name for a source that does not exist would
// silently name nothing.
func validateMatchNames(names []SpendingMatchName, seenIDs map[string]bool) error {
	for i := range names {
		n := &names[i]
		at := fmt.Sprintf("spending.internal_transfer_matching.names[%d]", i)
		if !seenIDs[n.Source] {
			return fmt.Errorf("config: %s: source %q is not a declared silver source", at, n.Source)
		}
		if strings.TrimSpace(n.Match) == "" {
			return fmt.Errorf("config: %s: match is empty", at)
		}
		re, err := regexp.Compile(n.Match)
		if err != nil {
			return fmt.Errorf("config: %s: match %q: %w", at, n.Match, err)
		}
		n.re = re
	}
	return nil
}
