// Package config parses the wealthdb JSON config file. See
// docs/DESIGN.md §5 for the schema and defaults.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
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
	// ledger. See docs/DESIGN.md §13.9 and internal/loader/transfers.go.
	EquityTransfers string `json:"equity_transfers,omitempty"`
	// AccountOverrides replace the per-account
	// `nickname` and `account_category` columns adapters would
	// otherwise emit, and can drop an account outright
	// (AccountOverride.Exclude). Keyed by silver_source_id (outer)
	// and then account_external_id (inner). Either field of the
	// value may be empty/omitted; an empty value is treated as "no
	// override for that column". The loader applies overrides AFTER
	// the adapter has stamped its own values, so config wins on
	// overlap. See docs/DESIGN.md §13.8.
	AccountOverrides map[string]map[string]AccountOverride `json:"account_overrides,omitempty"`
	// DeclaredAccounts are the holder's own accounts at institutions the
	// product does not collect, keyed by an id the holder chooses. A
	// declaration is what lets a rule say where an own-account move
	// went (SpendingRule.Far) when the far side is nowhere in gold: the
	// load writes each one as an account row under the reserved source
	// canonical.DeclaredSourceID, and the cash flow statement then reads
	// the declared wrapper the way it reads a collected account's — a
	// household wrapper makes the move pool-internal, a vehicle wrapper
	// draws it as a crossing. Absent ⇒ no such accounts; a move to an
	// untracked account is then income or spending, which is what the
	// closed world the statement assumes says of it.
	DeclaredAccounts map[string]DeclaredAccount `json:"declared_accounts,omitempty"`
	// HoldingAliases name a holding a source keys two ways, so the gains
	// reports read both keys as one: a statement that shows a holding
	// as an overview line until a daily feed lists it by ISIN, say. The
	// load writes them into gold's holding_aliases table whole; the
	// gains reports map a key through it before pairing a bucket's
	// start with its end (docs/GAINS.md §5). Absent ⇒ no aliases.
	HoldingAliases []HoldingAlias `json:"holding_aliases,omitempty"`
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
	// overlap. See docs/DESIGN.md §13.8.
	InstrumentOverrides map[string]map[string]InstrumentOverride `json:"instrument_overrides,omitempty"`
	// TransactionInstruments links a row (a securities trade, or an
	// income or withholding row of a security) whose own feed states its
	// instrument in a way nothing else in the product can resolve — a
	// Swiss valor for a line the instrument dimension has no valor for,
	// a fund named before it was renamed, a ticker the symbol index
	// never saw, a statement name that fits two holdings.
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
	// link here, pin its classification there. See docs/DESIGN.md §13.8.
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
	// is a success, not an error. See docs/DESIGN.md §13.8.
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
	// own grain. Use it to keep accounts that are tracked but are not part
	// of the household's own wealth out of source/global returns. Keyed by
	// grain: portfolios[source_id] and accounts[source_id] each map to a
	// list of external ids. An excluded account is also omitted
	// from its portfolio's row; an excluded portfolio still shows its own row.
	// Returns only — holdings / net-worth are unaffected. Absent ⇒
	// nothing excluded. See docs/DESIGN.md §5.5 and internal/gold
	// groupAccounts.
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
	// Cashflow is the cash flow statement's knobs: which accounts are
	// in the household's cash pool, where a crossing to a given tax
	// wrapper lands, and the cash flow projection's look-back. It
	// inherits neither family's scope —
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
	// Lots configures the lot engine: the method it relieves lots by,
	// per source, portfolio or account, and the default reading of a
	// missing cost basis. Absent block ⇒ each source kind's registered
	// policy, fifo where it names no method, missing basis ignored. See
	// LotsConfig and docs/LOTS.md.
	Lots *LotsConfig `json:"lots,omitempty"`
	// MCP configures the optional MCP server (`wealthdb mcp …`),
	// which puts the read-only reports in front of AI agents. Absent
	// or not enabled means `wealthdb mcp start` refuses with a pointer
	// to this block. It reads live gold read-only, one short handle
	// per call. See mcp/DESIGN.md.
	MCP *MCPConfig `json:"mcp,omitempty"`
	// Exposure configures the exposure report: proxies for funds no
	// source describes, reference funds, size funds, the declarations
	// ledger for private holdings, custody, and how far a composition may
	// lie from a day.
	// Absent block ⇒ none of them and the default settings. See
	// ExposureConfig and docs/EXPOSURE.md §9.
	Exposure *ExposureConfig `json:"exposure,omitempty"`
	// Benchmarks adds named fund mixes to the built-in set, or redefines
	// a built-in one by its name (benchmarks.go). Absent block: the
	// built-in set alone.
	Benchmarks map[string]map[string]float64 `json:"benchmarks,omitempty"`
	// Yield configures the yield report: the public rate curves, the
	// tax withheld at source, the funds sliced by credit rating, coupon
	// conventions, the projection's horizon and the sectors' cycles.
	// Absent block ⇒ the built-in conventions and cycles and the default
	// horizon alone. See YieldConfig.
	Yield *YieldConfig `json:"yield,omitempty"`

	// The blocks in the engine's terms, each compiled once by Validate
	// and returned by its accessor. Nil on a Config never validated.
	lotsEngine    *lots.Config
	benchmarkDefs []canonical.Benchmark
	exposureRows  *canonical.ExposureRows
	yieldRows     *canonical.YieldRows
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
	for _, f := range c.pathFields() {
		if f.optional && *f.p == "" {
			continue
		}
		expanded, err := expandPath(*f.p, configDir)
		if err != nil {
			return nil, fmt.Errorf("config: %s: %w", f.key, err)
		}
		*f.p = expanded
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// pathField is one path-valued key of the file: key names it in an
// error, p points at its value. An optional path that is empty stays
// empty; a required one is expanded even then, so an empty value fails
// as a path.
type pathField struct {
	key      string
	p        *string
	optional bool
}

// pathFields lists every path-valued key of c, in the order Load
// expands them. A path missing here is not a load error: a ledger such
// as `~/ledgers/income.csv` reaches os.Open verbatim, the open fails
// with not-exist, and a missing ledger is a no-op by design, so every
// row in it would silently never apply.
func (c *Config) pathFields() []pathField {
	out := []pathField{
		{key: "gold_db", p: &c.GoldDB},
		{key: "equity_transfers", p: &c.EquityTransfers, optional: true},
	}
	if c.Exposure != nil {
		out = append(out, pathField{key: "exposure.declarations", p: (*string)(&c.Exposure.Declarations), optional: true})
	}
	if c.Spending != nil {
		out = append(out,
			pathField{key: "spending.pins", p: &c.Spending.Pins, optional: true},
			pathField{key: "spending.transfer_overrides", p: &c.Spending.TransferOverrides, optional: true})
	}
	if c.Income != nil {
		out = append(out, pathField{key: "income.pins", p: &c.Income.Pins, optional: true})
	}
	for i := range c.SilverSources {
		s := &c.SilverSources[i]
		out = append(out, pathField{key: fmt.Sprintf("silver_sources[%d].path", i), p: &s.Path, optional: true})
		for j := range s.Subsources {
			out = append(out, pathField{key: fmt.Sprintf("silver_sources[%d].subsources[%d].path", i, j),
				p: &s.Subsources[j].Path})
		}
	}
	return out
}
