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
	"regexp"
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
	// otherwise emit, and can drop an account outright
	// (AccountOverride.Exclude). Keyed by silver_source_id (outer)
	// and then account_external_id (inner). Either field of the
	// value may be empty/omitted; an empty value is treated as "no
	// override for that column". The loader applies overrides AFTER
	// the adapter has stamped its own values, so config wins on
	// overlap. See docs/DESIGN.md §13.9.
	AccountOverrides map[string]map[string]AccountOverride `json:"account_overrides,omitempty"`
	// PortfolioOverrides is the portfolio-grain counterpart of
	// AccountOverrides. Keyed by silver_source_id (outer) and then
	// portfolio_external_id (inner). The override applies to every
	// account whose `portfolio_external_id` matches — useful when
	// a whole CT portfolio sits inside an IRA / trust / Stiftung
	// wrapper and stamping the tax_wrapper on every wallet
	// individually would be churn, or when a whole portfolio should
	// not be in gold at all (PortfolioOverride.Exclude). Per-account
	// overrides still win over portfolio overrides on the same column.
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
	// TransactionInstruments links a securities trade whose own feed
	// states its instrument in a way nothing else in the product can
	// resolve — a Swiss valor for a line the instrument dimension has
	// no valor for, a fund named before it was renamed, a ticker the
	// symbol index never saw.
	//
	// Keyed by silver_source_id (outer) and then by the TOKEN the
	// adapter looked up and failed on (inner) — read it from
	// `wealthdb transactions -C +instrument_hint`, which prints exactly
	// what a row was looked up by. The value is the
	// `instrument_external_id` gold holds, copied from the gold
	// `instruments` table.
	//
	// Applied by the loader after the adapter has resolved what it can,
	// so config wins on overlap, and only on the rows a load touches —
	// `wealthdb reload <source>` is what re-applies it against history.
	// An entry matching nothing is a silent no-op, as the other
	// override families' are: a token that stops appearing because the
	// adapter learned to resolve it is a success, not an error.
	//
	// It states the IDENTITY only. What the instrument IS remains
	// `instrument_overrides`' question, and the two compose: pin the
	// link here, pin its classification there.
	TransactionInstruments map[string]map[string]string `json:"transaction_instruments,omitempty"`
	// Supersession ends a silver source's account at a date, because
	// something else carries it from there. An account can outlive its
	// source — a deposit account whose bank is acquired keeps running
	// under the collector for the acquirer, a holding moves custodian —
	// and gold keys an account on (silver_source_id,
	// account_external_id), so the two sources are two accounts and BOTH
	// count. Naming the date the later source takes over stops the older
	// one contributing from there: positions, cash balances and
	// transactions alike. It also writes one zero row AT that date for
	// whatever the account still held, because gold carries a key
	// forward until something supersedes it and a source that simply
	// stops reporting supersedes nothing.
	//
	// What is cut is what the ADAPTER read. An `equity_transfers` row
	// dated on or after the date is refused by name instead, failing the
	// load rather than silently dropping a hand-written capital flow —
	// see loader.rejectSupersededTransfers.
	//
	// It cuts by DATE, not by dropping the account, so a statement or a
	// dump straddling the handover still contributes the part that
	// precedes it. It bounds a series at the END; InceptionOverrides
	// below bounds the start, and the two are different knobs.
	//
	// An entry matching nothing is a silent no-op, as the other override
	// families' are — a source that stops emitting the account on its own
	// is a success, not an error. See docs/DESIGN.md §13.9.
	Supersession *Supersession `json:"supersession,omitempty"`
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
	// Spending groups the per-deployment knobs of the spending
	// feature: which accounts spending counts, and how hard the
	// internal-transfer matcher tries to pair the two legs of an
	// own-account move. Absent block ⇒ every account
	// counts and the matcher runs on its defaults. See
	// internal/spending.
	Spending *SpendingConfig `json:"spending,omitempty"`
	// Income is the inflow side of the same question: which accounts
	// count as receiving, which narratives name a known payer, and
	// which model is asked about the rest. Its shapes mirror
	// `spending`'s; what it deliberately does NOT have is matcher
	// knobs or a transfer-override ledger, because there is one
	// matcher and both families read its verdicts.
	Income *IncomeConfig `json:"income,omitempty"`
	// Cashflow is the cash flow statement's two knobs: which accounts
	// are in the household's cash pool, and where a crossing to a
	// given tax wrapper lands. It inherits neither family's scope —
	// cashflow reads their verdicts rather than their bases, so an
	// account one of them excludes is not thereby outside the pool.
	// Absent block ⇒ every account is pooled and the engine's own
	// household boundary stands. See docs/CASHFLOW.md.
	Cashflow *CashflowConfig `json:"cashflow,omitempty"`
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
// wealthdb.cfg, and the identically shaped
// `spending.categorization.model` block. The only API shape
// supported today is the OpenAI-compatible Chat Completions
// endpoint (`api: "openai-completions"`). Stripping R1-style
// `<think>` blocks off a response is unconditional hygiene in
// both pipelines, not something a field turns on.
type ModelConfig struct {
	BaseURL string `json:"baseUrl"`
	API     string `json:"api"`
	APIKey  string `json:"apiKey,omitempty"`
	Name    string `json:"name"`
	// ThinkingFormat is accepted and ignored: nothing reads it,
	// and `<think>` stripping runs for every model regardless.
	// It stays declared because Load decodes with
	// DisallowUnknownFields, so dropping the field turns any
	// config that still sets `thinkingFormat` into a hard parse
	// failure on every subcommand — remove it only together with
	// the key in the deployment's config file.
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
}

// Epochs parses the YYYY-MM-DD values to Unix-seconds (UTC midnight),
// keyed source -> account. Only call after Validate, which has already
// verified the date format. A nil receiver returns a nil map.
func (s *Supersession) Epochs() map[string]map[string]int64 {
	if s == nil || len(s.Accounts) == 0 {
		return nil
	}
	out := make(map[string]map[string]int64, len(s.Accounts))
	for source, accounts := range s.Accounts {
		for account, day := range accounts {
			e, err := parseYYYYMMDD(day)
			if err != nil {
				continue
			}
			if out[source] == nil {
				out[source] = make(map[string]int64, len(accounts))
			}
			out[source][account] = e
		}
	}
	return out
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
	// missing file is a no-op. See docs/DESIGN.md §13.11 and
	// internal/spending/pins.go.
	Pins string `json:"pins,omitempty"`
	// TransferOverrides is an optional path to the CSV ledger of manual
	// match / unmatch decisions for the internal-transfer matcher — the
	// surface for a pair the data cannot settle, in either direction.
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
}

// SpendingRuleScope narrows a rule to part of the ledger. Every field
// is optional and an omitted one does not constrain; a rule with no
// scope, or an empty one, matches everywhere — which is what every
// rule written before this existed does.
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
	Match    *regexp.Regexp
	Category string
	Scope    CompiledSpendScope
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
// or card account out (a card belonging to someone else on a shared
// login). An account may not appear in both.
type SpendingAccounts struct {
	Include map[string][]string `json:"include,omitempty"` // source_id -> [account_external_id...]
	Exclude map[string][]string `json:"exclude,omitempty"` // source_id -> [account_external_id...]
}

// SpendingTransferMatching are the internal-transfer matcher's knobs
// for its AMOUNT pass — the phase that infers a pair from two figures
// landing near each other. The phases that assert one outright, from
// the override ledger or from a reference the source stamped on both
// legs, are not banded and these do not reach them.
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
}

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
	if c.Spending != nil && c.Spending.Pins != "" {
		expanded, err := expandPath(c.Spending.Pins, configDir)
		if err != nil {
			return nil, fmt.Errorf("config: spending.pins: %w", err)
		}
		c.Spending.Pins = expanded
	}
	if c.Spending != nil && c.Spending.TransferOverrides != "" {
		expanded, err := expandPath(c.Spending.TransferOverrides, configDir)
		if err != nil {
			return nil, fmt.Errorf("config: spending.transfer_overrides: %w", err)
		}
		c.Spending.TransferOverrides = expanded
	}
	// The income family's one path. A ledger left unexpanded is not a
	// load error: `~/ledgers/income.csv` reaches os.Open verbatim, the
	// open fails with not-exist, and a missing pins file is a no-op by
	// design — so every pin in it would silently never apply.
	if c.Income != nil && c.Income.Pins != "" {
		expanded, err := expandPath(c.Income.Pins, configDir)
		if err != nil {
			return nil, fmt.Errorf("config: income.pins: %w", err)
		}
		c.Income.Pins = expanded
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
	// an employer's name to INCOME_WAGES, a pension fund to
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
}

// CashflowConfig is the `cashflow` block of wealthdb.cfg: one block,
// two fields, both optional.
//
// It is deliberately small. Cashflow adds no categorisation tier and
// buys nothing from a model, so there is no endpoint to point at and
// no backlog to steer; the rules, pins and transfer overrides that
// decide what a row IS are the two families', and a verdict written
// there is what cashflow reads.
type CashflowConfig struct {
	// Accounts is the cash pool's only gate. An account listed here is
	// treated exactly like an account the product does not hold: a move
	// to it is a crossing into `vehicles · Untracked accounts` rather
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
	// the engine default (canonical.DefaultWrapperSide); an unknown
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
// does): the pass composes them over canonical.DefaultWrapperSide and
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
