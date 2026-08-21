// Package config parses the wealthdb JSON config file. See
// docs/DESIGN.md §5 for the schema and defaults.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Config is the in-memory shape of the wealthdb config file.
// Field paths in the file are tilde / $HOME-expanded at Load time;
// callers always see absolute paths.
type Config struct {
	GoldDB          string         `json:"gold_db"`
	DefaultCurrency string         `json:"default_currency"`
	SilverSources   []SilverSource `json:"silver_sources"`
	// EquityTransfers is an optional path to a CSV ledger of equity
	// transfers in/out of a tracked account that the collectors don't
	// capture as valued flows — typically appreciated securities moved
	// between custodians, whose value would otherwise read as in-account
	// performance. The loader turns each row into a canonical
	// transfer_in/transfer_out transaction at load time. Absent ⇒ no
	// ledger. See docs/DESIGN.md §13.10 and internal/loader/transfers.go.
	EquityTransfers string `json:"equity_transfers,omitempty"`
	// AccountOverrides replace the per-account
	// `nickname` and `account_category` columns adapters would
	// otherwise emit. Keyed by silver_source_id (outer) and then
	// account_external_id (inner). Either field of the value may
	// be empty/omitted; an empty value is treated as "no override
	// for that column". The loader applies overrides AFTER the
	// adapter has stamped its own values, so config wins on
	// overlap. See docs/DESIGN.md §13.9.
	AccountOverrides map[string]map[string]AccountOverride `json:"account_overrides,omitempty"`
	// PortfolioOverrides is the portfolio-grain counterpart of
	// AccountOverrides. Keyed by silver_source_id (outer) and then
	// portfolio_external_id (inner). The override applies to every
	// account whose `portfolio_external_id` matches — useful when
	// a whole CT portfolio sits inside an IRA / trust / Stiftung
	// wrapper and stamping the tax_wrapper on every wallet
	// individually would be churn. Per-account overrides still win
	// over portfolio overrides on the same column.
	PortfolioOverrides map[string]map[string]PortfolioOverride `json:"portfolio_overrides,omitempty"`
	// InstrumentOverrides pins the `asset_class` of individual
	// instruments, for holdings the adapters' structured signals
	// and name heuristics misclassify — e.g. an exchange-traded
	// commodity trust whose security name doesn't give away what
	// it holds. Keyed by silver_source_id (outer) and then
	// instrument_external_id (inner — copy it from the gold
	// `instruments` table). The loader applies these AFTER the
	// adapter has classified, to both the instrument dimension and
	// every position row referencing it, so config wins on
	// overlap. See docs/DESIGN.md §13.9.
	InstrumentOverrides map[string]map[string]InstrumentOverride `json:"instrument_overrides,omitempty"`
	// InceptionOverrides pins the returns-window START date per silver
	// source, portfolio, or account, so an entity's track record can
	// begin at its first real capital instead of a tiny pre-history
	// dust base (an account-opening gift, a stub position) that would
	// otherwise dominate its time-weighted return. Keyed by grain:
	// sources[source_id], portfolios[source_id][portfolio_external_id],
	// accounts[source_id][account_external_id]. Values are YYYY-MM-DD
	// (UTC midnight). When an entity is computed, the floor is resolved
	// most-specific-first (account → its portfolio → source); the global
	// grain is never anchored. It can only move an anchor LATER, never
	// earlier. Absent block ⇒ every entity keeps its data-derived
	// inception. Works for every source, not just crypto. See
	// docs/DESIGN.md §5 and internal/gold entityWindow.
	InceptionOverrides *InceptionOverrides `json:"inception_overrides,omitempty"`
	// ReturnsExclude omits whole accounts or portfolios from HIGHER-grain
	// return aggregates (sources, global) while still reporting them at their
	// own grain. Use it to keep holdings tracked in a shared login that
	// belong to another person out of source/global
	// returns. Keyed by grain: portfolios[source_id] and accounts[source_id]
	// each map to a list of external ids. An excluded account is also omitted
	// from its portfolio's row; an excluded portfolio still shows its own row.
	// Returns only — holdings / net-worth are unaffected (a proper owner
	// dimension is future work). Absent ⇒ nothing excluded. See docs/DESIGN.md
	// §5.5 and internal/gold groupAccounts.
	ReturnsExclude *ReturnsExclude `json:"returns_exclude,omitempty"`
	// ReturnsHide suppresses accounts' or portfolios' OWN return rows at
	// every grain while their values and flows keep contributing to every
	// aggregate — the display mirror of ReturnsExclude (which removes an
	// entity from the coarse-grain math instead). Same grain-keyed shape as
	// returns_exclude. Use it for plumbing whose flows matter but whose own
	// return is noise (per-source policies already hide whole conduit
	// sources; this block covers deployment-specific ids). Absent ⇒ nothing
	// hidden beyond policy. See docs/DESIGN.md §5.7 and internal/gold
	// entityHidden.
	ReturnsHide *ReturnsHide `json:"returns_hide,omitempty"`
	// ReturnsPolicyOverrides adjusts a silver source's registered
	// ReturnsPolicy, keyed by silver_sources[].id (NOT adapter kind — two
	// sources sharing an adapter override independently). Partial: only the
	// set fields change; every other knob keeps what the source's silver
	// package registers. `flow_regime` replaces the whole flow
	// classification with the named regime's canonical kind sets — the
	// escape hatch for a deployment whose data completeness differs from
	// the registered default (e.g. pin a flow-counting source back to
	// "nav_only" when its silver carries no transaction history).
	// `accounts_grain` sets the per-account display mode ("normal" |
	// "blanked" | "hidden"). Absent block ⇒ registered policies apply
	// unchanged. See docs/DESIGN.md §5.6 and internal/gold newAccountData.
	ReturnsPolicyOverrides map[string]*ReturnsPolicyOverride `json:"returns_policy_overrides,omitempty"`
	// ReturnsTransferMatching enables the opt-in cross-source transfer
	// matcher: an unmatched external leg whose counterparty leg exists in
	// another source (opposite sign, same native currency, equal amount
	// within the tolerance, within the day window) nets out of every return
	// aggregate that contains BOTH legs, while finer grains keep counting
	// each leg. Absent block or enabled=false ⇒ the matcher never runs and
	// returns are byte-identical to the per-source heuristics alone. See
	// docs/DESIGN.md §5.8.
	ReturnsTransferMatching *ReturnsTransferMatching `json:"returns_transfer_matching,omitempty"`
	// SymbolResolution groups the per-deployment knobs that drive
	// `wealthdb resolve-symbols`: the LLM endpoint and the
	// user-authored override list. Both fields inside are optional;
	// the subcommand fails loudly if Model is unset and
	// --overrides-only wasn't passed.
	SymbolResolution *SymbolResolutionConfig `json:"symbol_resolution,omitempty"`
	// Web configures the optional dockerized Metabase BI server
	// (`wealthdb web …`). Absent/omitted = not configured; `wealthdb
	// web start` refuses with a pointer to this block. The server is
	// host-orchestrated (Docker isn't reachable from inside the
	// engine container) and reads a read-only *snapshot* of gold, so
	// it never contends for the single-writer lock. See web/DESIGN.md.
	Web *WebConfig `json:"web,omitempty"`
}

// DefaultWebPort is the host loopback port the Metabase server is
// published on when `web.port` is omitted. 3000 is Metabase's own
// default.
const DefaultWebPort = 3000

// WebConfig is the optional `web` block of wealthdb.cfg, driving the
// dockerized Metabase server managed by `wealthdb web`. The lifecycle
// runs host-side; the host wrapper reads these settings back via the
// `web-config` subcommand (the one component that already parses this
// JSON). Keep the field names in sync with web/web.
type WebConfig struct {
	// Enabled gates `wealthdb web start`. False (or an omitted block)
	// means the server is not configured.
	Enabled bool `json:"enabled"`
	// Port is the loopback port Metabase is published on
	// (127.0.0.1:Port and [::1]:Port → container :3000). Zero/omitted
	// → DefaultWebPort.
	Port int `json:"port,omitempty"`
}

// SymbolResolutionConfig is the `symbol_resolution` block of
// wealthdb.cfg. Groups everything specific to the resolve-symbols
// subcommand so the top-level config file doesn't sprout one
// field per concern.
type SymbolResolutionConfig struct {
	// Model configures the LLM endpoint used to back-fill missing
	// instrument tickers. Required for normal `resolve-symbols`
	// runs; can be omitted when only --overrides-only is used.
	Model *ModelConfig `json:"model,omitempty"`
	// Overrides is the user-authored ticker-mapping override list.
	// Each entry replaces (or suppresses) a row in
	// symbol_resolutions under model_name='manual-override'.
	// Applied at the start of every `wealthdb resolve-symbols`
	// invocation (including --overrides-only). Use cases:
	// correcting an LLM resolution that was wrong, or seeding
	// tickers the LLM can't infer (e.g. private-fund proxies).
	Overrides []SymbolOverride `json:"overrides,omitempty"`
}

// SymbolOverride is one entry under `symbol_resolution.overrides`.
// Mirrors the symbol_resolutions PK + value columns. Two modes:
//
//   - Correction: set `symbol` to the right ticker. The sync UPSERTs
//     this row into symbol_resolutions, winning over any LLM result.
//   - Suppression: set `delete: true` (and omit `symbol`). The sync
//     DELETEs any row with this PK from symbol_resolutions. Use for
//     descriptions where no real ticker exists (US Treasury CUSIPs,
//     private structured products, currency-line placeholders) so
//     the LLM's wrong guess stops surfacing.
type SymbolOverride struct {
	SilverSourceID string `json:"silver_source_id"`
	LookupKind     string `json:"lookup_kind"` // 'instrument_external_id' or 'name'
	LookupValue    string `json:"lookup_value"`
	Symbol         string `json:"symbol,omitempty"`
	Delete         bool   `json:"delete,omitempty"`
}

// ModelConfig is the `symbol_resolution.model` block of
// wealthdb.cfg. The only API shape supported today is the
// OpenAI-compatible Chat Completions endpoint
// (`api: "openai-completions"`); ThinkingFormat lets the
// resolve-symbols pipeline strip R1-style `<think>` blocks from
// the response before parsing.
type ModelConfig struct {
	BaseURL        string `json:"baseUrl"`
	API            string `json:"api"`
	APIKey         string `json:"apiKey,omitempty"`
	Name           string `json:"name"`
	ThinkingFormat string `json:"thinkingFormat,omitempty"`
}

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
	FxPriority *int              `json:"fx_priority,omitempty"`
	Subsources []SilverSubsource `json:"subsources,omitempty"`
	// Relationships pairs cross-subsource entity identities under
	// a single user-chosen label. Used by the UBS adapter to link
	// the web `banking_relationship_id` (opaque SPA token) to the
	// PSN `relationship_id` (SFTPCHxx, etc.). Optional.
	Relationships []RelationshipPair `json:"relationships,omitempty"`
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

// AccountOverride is one per-account override entry. All fields
// are optional; an empty string means "don't override that
// column". TaxWrapper and ManagementStyle are validated against
// the canonical enums at config-load time; bad values fail the
// load rather than landing as gibberish in gold.
type AccountOverride struct {
	Nickname        string `json:"nickname,omitempty"`
	Category        string `json:"category,omitempty"`
	TaxWrapper      string `json:"tax_wrapper,omitempty"`
	ManagementStyle string `json:"management_style,omitempty"`
}

// PortfolioOverride is one per-portfolio override entry. Applies
// to every account in gold whose `portfolio_external_id` matches.
// All fields are optional; the canonical-enum-typed ones are
// validated at config-load time.
//
// Today only the `taxable_personal` → portfolio-specific wrapper
// override is wired through (the main use case: a CT portfolio
// held inside an IRA / 401k / trust / Stiftung wrapper where the
// adapter's `taxable_personal` default is wrong for every wallet
// in the portfolio). Other dimensions (nickname, management_style)
// can be added here if a use case emerges.
type PortfolioOverride struct {
	TaxWrapper string `json:"tax_wrapper,omitempty"`
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

// InceptionOverrides is the `inception_overrides` block of wealthdb.cfg.
// Each map value is a YYYY-MM-DD date (UTC midnight). All three maps are
// optional. Keys are the same stable external ids the rest of the config
// uses (silver_source_id, portfolio_external_id, account_external_id) —
// copy them from the `entity_id` column of `wealthdb returns <grain>`.
type InceptionOverrides struct {
	Sources    map[string]string            `json:"sources,omitempty"`    // source_id -> date
	Portfolios map[string]map[string]string `json:"portfolios,omitempty"` // source_id -> portfolio_external_id -> date
	Accounts   map[string]map[string]string `json:"accounts,omitempty"`   // source_id -> account_external_id -> date
}

// Epochs parses the YYYY-MM-DD values to Unix-seconds (UTC midnight),
// producing the three maps the returns engine resolves against. Only
// call after Validate, which has already verified the date format
// (parse errors here are therefore ignored — a bad date can't reach
// this point). A nil receiver returns three nil maps.
func (o *InceptionOverrides) Epochs() (sources map[string]int64, portfolios, accounts map[string]map[string]int64) {
	if o == nil {
		return nil, nil, nil
	}
	if len(o.Sources) > 0 {
		sources = make(map[string]int64, len(o.Sources))
		for id, d := range o.Sources {
			e, _ := parseYYYYMMDD(d)
			sources[id] = e
		}
	}
	return sources, inceptionEpochsNested(o.Portfolios), inceptionEpochsNested(o.Accounts)
}

func inceptionEpochsNested(in map[string]map[string]string) map[string]map[string]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string]int64, len(in))
	for src, m := range in {
		inner := make(map[string]int64, len(m))
		for id, d := range m {
			e, _ := parseYYYYMMDD(d)
			inner[id] = e
		}
		out[src] = inner
	}
	return out
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
	// registered one: "flow_complete", "crypto_partial", or "nav_only".
	FlowRegime *string `json:"flow_regime,omitempty"`
	// AccountsGrain names the per-account display mode that replaces the
	// registered one: "normal" (full rows), "blanked" (values shown, TWR/MWR
	// n/a), or "hidden" (no rows anywhere; values and flows still aggregate).
	AccountsGrain *string `json:"accounts_grain,omitempty"`
}

// ReturnsTransferMatching is the `returns_transfer_matching` block of
// wealthdb.cfg. Pointer fields distinguish "not set" (use the default) from
// an explicit value.
type ReturnsTransferMatching struct {
	// Enabled turns the matcher on. False (or an absent block) keeps returns
	// byte-identical to the per-source heuristics alone.
	Enabled bool `json:"enabled"`
	// WindowDays is the max day distance between the two legs of a pair
	// (default 5: cross-border wires settle within a business week; wider
	// windows raise the false-pair risk).
	WindowDays *int `json:"window_days,omitempty"`
	// TolerancePct is the relative amount tolerance in percent of the larger
	// leg (default 0.5; an absolute floor of 0.01 always applies, so 0 means
	// exact-to-a-cent). Covers wire fees deducted in transit.
	TolerancePct *float64 `json:"tolerance_pct,omitempty"`
}

// Defaults for the returns_transfer_matching knobs when the block is enabled
// with fields omitted.
const (
	DefaultTransferMatchWindowDays   = 5
	DefaultTransferMatchTolerancePct = 0.5
)

// Window returns the effective day window.
func (m *ReturnsTransferMatching) Window() int {
	if m == nil || m.WindowDays == nil {
		return DefaultTransferMatchWindowDays
	}
	return *m.WindowDays
}

// Tolerance returns the effective relative tolerance in percent.
func (m *ReturnsTransferMatching) Tolerance() float64 {
	if m == nil || m.TolerancePct == nil {
		return DefaultTransferMatchTolerancePct
	}
	return *m.TolerancePct
}

// Regime returns the parsed FlowRegime and whether one is set. Only call
// after Validate has vetted the name (Load does); an unvetted name reports
// unset. A nil receiver reports unset.
func (o *ReturnsPolicyOverride) Regime() (returns.Regime, bool) {
	if o == nil || o.FlowRegime == nil {
		return 0, false
	}
	r, err := returns.ParseRegime(*o.FlowRegime)
	if err != nil {
		return 0, false
	}
	return r, true
}

// AccountsGrainMode returns the parsed AccountsGrain display mode and whether
// one is set, under the same only-after-Validate contract as Regime.
func (o *ReturnsPolicyOverride) AccountsGrainMode() (returns.AccountsGrainMode, bool) {
	if o == nil || o.AccountsGrain == nil {
		return 0, false
	}
	m, err := returns.ParseAccountsGrainMode(*o.AccountsGrain)
	if err != nil {
		return 0, false
	}
	return m, true
}

// Load reads and parses the JSON config at the given path,
// expands `~` and `$HOME` in path fields, resolves relative paths
// against the config file's directory, and validates structural
// requirements. The returned Config is ready to use.
func Load(path string) (*Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: resolve %q: %w", path, err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", absPath, err)
	}

	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", absPath, err)
	}

	// Expand path-valued fields. Done before validation so any
	// path-shape checks see the resolved value.
	configDir := filepath.Dir(absPath)
	expanded, err := expandPath(c.GoldDB, configDir)
	if err != nil {
		return nil, fmt.Errorf("config: gold_db: %w", err)
	}
	c.GoldDB = expanded
	if c.EquityTransfers != "" {
		expanded, err := expandPath(c.EquityTransfers, configDir)
		if err != nil {
			return nil, fmt.Errorf("config: equity_transfers: %w", err)
		}
		c.EquityTransfers = expanded
	}
	for i := range c.SilverSources {
		if c.SilverSources[i].Path != "" {
			expanded, err := expandPath(c.SilverSources[i].Path, configDir)
			if err != nil {
				return nil, fmt.Errorf("config: silver_sources[%d].path: %w", i, err)
			}
			c.SilverSources[i].Path = expanded
		}
		for j := range c.SilverSources[i].Subsources {
			expanded, err := expandPath(c.SilverSources[i].Subsources[j].Path, configDir)
			if err != nil {
				return nil, fmt.Errorf("config: silver_sources[%d].subsources[%d].path: %w", i, j, err)
			}
			c.SilverSources[i].Subsources[j].Path = expanded
		}
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
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
	out := silver.OpenSpec{Path: s.Path}
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

// parseYYYYMMDD turns a YYYY-MM-DD string into a Unix-seconds
// timestamp at UTC midnight.
func parseYYYYMMDD(s string) (int64, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, fmt.Errorf("invalid YYYY-MM-DD %q: %w", s, err)
	}
	return t.UTC().Unix(), nil
}
