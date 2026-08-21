# `wealthdb returns` — method and rationale

The design decisions of record behind `wealthdb returns` (TWR / MWR):
the conventions, the non-obvious choices, and what is deliberately
deferred. [DESIGN.md §10.9](DESIGN.md) is the user-facing summary; this
is the "why". All examples synthetic (CLAUDE.md §4).

The engine is **source-agnostic**: `internal/returns/` holds the pure
math (period Modified-Dietz, TWR chaining, XIRR, onboarding/closure
subsumption) and takes zero source names or branches. Each source's
domain knowledge enters through a pluggable `ReturnsPolicy` (see
"Pluggable per-source policy" below).

## Grain and scope

- **Account-grain is exact; coarse grains** (portfolios / sources /
  global) **are best-effort** — heuristic transfer netting plus synthetic
  onboarding for staggered inception. Returns are **not additive across
  grains**: `global == Σ accounts + hidden plumbing` is a *value* identity
  (guarded by a reconciliation test; AccountsGrainHidden / returns_hide rows
  are in global but emit no accounts-grain row), never a return identity.
- **Mortgage / net-negative entities** are excluded from return rollups;
  per-entity twr/mwr = n/a + `nonpositive_base`, reported on a separate
  liability line.
- **The NAV-only source** (manual) reports value-growth TWR tagged
  `nav_only` + `nav_only_capital_call_risk`; MWR is `mwr_no_flows`.
  A blended aggregate MWR is still computed and tagged
  `mwr_incomplete_flows` — disclose, don't refuse. carta / equityzen count
  the real boundary flows of their double-entry ledgers (see "Pluggable
  per-source policy"); a window of theirs with no observed flow keeps
  `nav_only_capital_call_risk` via the `CapitalCallRisk` knob.

## The math (`internal/returns`)

- **All returns math is float64.** Money is exact (DECIMAL) up to the
  gold→returns boundary and floated only here; XIRR is inherently
  iterative and Modified-Dietz an approximation. No precision claim beyond
  ~1e-7 on rates.
- **Snapshot-aligned headline.** The since-inception cumulative TWR chains
  Modified-Dietz over the entity's **actual valuation (snapshot) days**
  (`canonicalChainBounds`), not a fixed daily/monthly calendar grid
  (`--period` still controls the per-bucket display rows). *Why:* a fixed
  calendar grid detonates the geometric chain for sparse-snapshot,
  flow-bearing sources. When a run of deposits lands in a gap with no
  snapshot, the deposit's calendar bucket sees a flow with no value move
  (Dietz ≈ −F/base, a sub-(−100%) sub-period) while the value jump shows
  up at the next snapshot a bucket later — chaining the poisoned factor
  drives the since-inception TWR orders of magnitude below −100%. Breaking
  at real valuation days keeps each flow in the same sub-period as the
  value change it causes (textbook TWR at valuation dates). A carried tail
  past the last snapshot is excluded; a window with no interior valuation
  falls back to a single `[winFrom, winTo]` bucket. Dense (daily-snapshot)
  sources are unchanged. Guard: `TestRunReturnsSparseSnapshotNoChainCollapse`.
- **Onboarding = the unexplained remainder.** A staggered constituent's
  synthetic onboarding flow is `firstValue − realDebutFunding`, injected
  only when `> tol` — not a binary suppress. This subsumes the binary case
  (full real funding → 0 → suppressed) and also handles *partial* real
  funding (inject only the uncovered opening).
- **XIRR:** Newton-Raphson (analytic derivative) from a 10% guess, with a
  bisection fallback scanning `[−0.9999, 100]` at 0.01 resolution for the
  first sign-change bracket. The rate is floored at −0.9999 (never worse
  than ~−100%). `ErrNoFlows` / `ErrNoSignChange` / `ErrNoConverge` map to
  `mwr_*` quality flags; a no-external-flow window is `mwr_no_flows` and
  never calls XIRR (avoids the `[−V0, +V1]` holding-period collapse
  masquerading as an IRR).
- **Sign convention.** `value_outccy` already carries the canonical sign
  (`canonical.ApplyCanonicalSign`, pinned by test), so Dietz
  `F_i = +value_outccy` and XIRR `cf = −value_outccy`, with no per-kind
  exception.

## Value spine and flows

SQL assembles the inputs by reusing existing report macros — no dedicated
returns migration:

- **Value spine:** `report_accounts_history(p_ccy)` is the per-account
  carry-forward series; its ASOF inner join omits pre-inception days, so a
  boundary before an account's first snapshot reads NULL ("not yet alive",
  not 0). Every grain is driven off the per-account spine (aggregated in
  Go), which is what synthetic onboarding needs.
  - A vanished account reads **0 after its last emitted row**, not its last
    carried value — post-disappearance absence ≠ pre-inception NULL.
    `valueAt` returns 0 past the last row and flags `dropped_while_nonzero`,
    so `global == Σ accounts` reconciles.
  - **Mid-series** disappearance (an account vanishes for a snapshot or
    two, then reappears) is carried across the gap in Go, whereas the
    macros read 0 inside the gap. Left as-is: it is in the
    economically-sensible direction, and the headline / terminal still
    reconcile.
- **Flows:** `report_transactions(from, to, p_ccy)` converts `net_amount`
  to the output currency at `occurred_at`; adapter kind and portfolio come
  from `silver_sources` + the history rows.

## Netting and closure (coarse grains)

- **Netting** runs only over transfer-like kinds (`transfer_in/out`,
  `journal`), never deposit/withdrawal: a deposit+withdrawal of equal size
  can be a genuine pair of external movements, so netting them would cancel
  real external capital. An internal move booked as deposit/withdrawal
  surfaces as two external flows, not silently cancelled. Netting is
  deterministic — `netOwnedTransfers` sorts stably by `(|amount|, day, id)`.
  ε = max(1.00 outCcy, 0.5% of the larger leg); window ±3 calendar days;
  FX-normalized via `value_outccy`; named constants.
- **Cross-source transfer matching** (opt-in, DESIGN.md §5.8) is the one
  sanctioned crossing of that deposit/withdrawal line: a pair explicitly
  matched ACROSS sources (opposite sign, same native currency, equal
  amount within tolerance, day window) nets in entities containing both
  legs live — tagged `cross_source_netted=N` — because the cross-source
  evidence is exactly what the within-entity heuristic lacks. Matched
  legs are withheld from `netOwnedTransfers`' candidate pool, matching
  operates on native amounts (partitions derive identical pairs whenever
  their attached-flow universes coincide), and `--netting off` disables
  cross-source drops along with the heuristic netter. Off ⇒
  byte-identical.
- **Explicit closure** is detected only when an account's value series ends
  in a ~0 carry tail (`|v| < valueTol`) — mere staleness never triggers it.
  The per-source `ClosureScope` knob decides how the exit is accounted:
  - `ClosureSubsumeDrains` (default): flows dated inside the flat zero-carry
    tail show no ΔV and are subsumed as strays (`subsumesAtClosure`); the
    zeroing value drop itself is the exit signal, offset by real drains that
    preceded the zeroing snapshot. Safe when the ledger may be missing or
    mis-dated around a closure.
  - `ClosureLedgerExact` (carta / equityzen): zero-tail flows are real dated
    exit legs — those ledgers put the proceeds on the zeroing day itself —
    so they are KEPT and no closure synthetic is booked: the realized-vs-
    last-mark delta shows as return (a late escrow release after the zeroing
    likewise books as gain). A flow-less zeroing reads as a full loss under
    either scope, tagged via `CapitalCallRisk`.

  The closure day is the zero tail's END, so the machinery engages when the
  window reaches it. The exit-day zero snapshot itself comes from the
  adapters (`silver.ClosureMarkerBatch`): when a carta / equityzen portfolio
  fully empties, the disposition date's batch replays the previous
  snapshot's positions at zero value — an empty batch is invisible to gold's
  history and as-of queries, which would otherwise carry the pre-exit marks
  forward as phantom value. The closure mirror of an inception anchor: the
  track record ends at a real, dated zero.

## Staggered-inception subsumption

*(referenced by DESIGN.md §10.9)* At coarse grains a constituent that
joins the aggregate value spine mid-window ("staggered inception") must
have each boundary-crossing dollar counted **exactly once**, in the same
bucket as the value change it causes. An account fed only by month-end
snapshots (a pension or 3a account, say) can be funded mid-month and
first appear weeks or months later, so a naive near-debut funding dedup
never fires and the engine
books *both* the synthetic onboarding and the real funding — driving the
chained TWR below −100%.

**Mechanism (`subsumesAt` + `entityFlows`):**

- A constituent debuting at `d > winFrom` has every own external flow dated
  `≤ d` **dropped** from the aggregate flow series; onboarding books the
  **full first-snapshot value** at `d`. An account already alive at
  `winFrom` keeps all in-window flows and gets no onboarding.
- The **closure mirror** (`lastNonzeroDay`): a closing constituent's flows
  dated after its last non-zero carried value — anywhere in the terminal
  zero-carry tail — land where the series shows no ΔV and are subsumed as
  strays under the default `ClosureScope` (the zeroing value drop is the
  exit signal); `ClosureLedgerExact` keeps them as the real exit legs.
- **Netting interaction:** transfer/journal netting runs over the full
  candidate set (pre-debut legs included) **before** subsumption, so
  genuine internal pairs annihilate and only a constituent's own surviving
  pre-debut/closure capital is subsumed. A surviving journal-OUT sits on an
  *alive* account whose value series reflects the drop, so it is real, not
  an orphaned phantom.

Onboarding still legitimately recognizes **untracked pre-existing
capital** — a late account with no funding transactions at all: booking its first value as onboarding is
correct, not a double-count. Guard: `TestStaggeredJournalFundedNoPhantom`.

## Pluggable per-source policy

*(referenced by DESIGN.md §10.9 and `internal/returns/returnspolicy.go`)*

Source economics differ — NAV-only tiny bases, crypto sweep churn, drained
closures, appreciated in-kind transfers, conduit cash — so a single
hardcoded model comes out wrong for most sources, and a pile of per-source
`if` branches in shared engine code leaks one source's behavior into
another (at the global grain every constituent merges into one entity). The
one source-blind engine instead reads a declarative `ReturnsPolicy` per
source:

- **Registry.** `internal/returns/returnspolicy.go` defines `ReturnsPolicy`
  (a `Flow FlowPolicy` member plus the knobs) and a `kind`-keyed registry
  (`RegisterPolicy` / `ReturnsPolicyFor`). Each source declares its policy
  in `internal/silver/<source>/policy.go` and registers it from that
  package's `init()` — the same pattern as the adapter registry. Resolution
  is by adapter kind, so a policy is **source-scoped even at the global
  grain** (each constituent keeps its origin's policy inside the merged
  entity), which makes the cross-grain leak structurally impossible.
  `DefaultReturnsPolicy()` reproduces current behavior, so a
  recognised-but-unmigrated source is a strict no-op.
- **Config overrides.** A deployment can adjust a source's registered policy
  from wealthdb.cfg (`returns_policy_overrides`, DESIGN.md §5.6), keyed by
  silver_source_id and composed at the engine's single resolution point
  (`newAccountData`), so the CLI and the materialized `report_returns` always
  agree: `flow_regime` replaces the flow classification with a regime's
  canonical kind sets (`FlowPolicyForRegime`); `accounts_grain` sets the
  per-account display mode (normal / blanked / hidden). The `returns_hide`
  config block (DESIGN.md §5.7) hides specific account/portfolio ids the
  same way — rows suppressed, values and flows still aggregated. The escape hatch for data completeness
  that differs from the registered default (e.g. a flow-counting source whose
  silver carries no transactions pins back to `nav_only`).
- **UBS knobs (live).** UBS's policy sets four source-scoped knobs:
  `OnboardScope = OnboardPerEntityOnce` (books the relationship's inception
  step-up once, net of same-day negative sibling funding drops, floored at
  0 — an internal cash→securities move onboards nothing); `ConduitKinds =
  [cash]` (cash feeds the value spine but emits no per-account onboarding);
  `ExternalOnly = true` (honored in silver — UBS pre-tags external/internal
  and demotes internal rows to a non-flow kind, so the engine's
  `ExternalOnly`/`ClassifyFlow` branch is inert for UBS; the silver
  classifier combines the parser's internal markers, the own-counter-IBAN
  rule, a direction-symmetric interbank-rail promotion era-gated to the
  MT940-covered era, and a same-day offset veto that recognises a mirror
  leg on another own account across all three feeds — see
  `pdfCashIsExternal` / `buildSameDayOffsetVeto`. The deep backfill era
  before any MT940 coverage stays conservative in BOTH directions: its
  inbound capital rides the onboarding step-ups, so one-sided or symmetric
  counting there fabricates return — that era's honest fix is
  counterparty-complete data, tracked separately);
  `Inception = InceptionFirstRealSnapshot` (anchors the window past sparse
  cash-only pre-history, so a tiny opening base cannot inflate the return).
- **Flow classification** is one member of the policy: banks / pension =
  flow-complete; crypto = fiat flows only (transfer legs excluded); carta /
  equityzen = boundary deposit/withdrawal only (the holding legs of their
  double-entry pairs — buy/sell/contribution/distribution — stay internal,
  or every event would cancel to a net-0 flow; `CapitalCallRisk` keeps the
  honesty tag on flow-less windows; `ClosureLedgerExact` books full exits at
  their real proceeds); manual = NAV-only.
- **Dormant knobs.** `SpineDensity`, `NettingTol`, `InKindJumpTol`, the
  `NavOnly` mirror, and the `ClassifyFlow` / `OnboardAmount` hooks are
  defined and defaulted but not yet consumed — NAV-only and crypto handling
  still ride `Flow.Regime`, not dedicated knobs. Forward stubs.

**Abandoned — residual onboarding.** An earlier attempt reconciled
per-constituent onboarding with conduit cash deposits by subtracting counted
inflows from the onboarding amount. It was implemented and **reverted**:
unwinnable, because the conduit's gross throughput dwarfs debut values, so
there is nothing to net against. The per-entity-once step-up
(`groupOnboardStep`) replaced it; no residual/throughput arithmetic remains
(grep-clean).

## UBS stamp-duty routing

The `cash_movement` default fall-through is `signedDepositWithdrawal`
(deposit/withdrawal by sign), **not** `other`. Stamp duty is routed to `tax`
by a substring match on the distinctive terms (`TIMBRE`, `UMSATZABGABE`,
`STEMPEL`, `STAMP`) anywhere in the narrative — a first-word match can't
catch "DROIT DE TIMBRE" (first word "DROIT", shared with "droit de garde"
custody fees). The default is deliberately **not** changed to `other`: that
would mis-route genuine unlabeled wires (real external capital) out of the
flow series — the worse error. Residual: stamp-duty variants without those
terms still leak to deposit/withdrawal (defensive coverage, no live sample).

## Quality flags

The `quality` column is the honesty surface: every n/a carries a reason and
every approximation is tagged. Computed set:

`since_data_inception`, `configured_inception`, `partial_window`,
`staggered_inception`, `accounts_grain_meaningless`, `empty_bucket`,
`carried_forward`, `boundary_same_snapshot`, `stale_snapshot`,
`dropped_while_nonzero`, `dietz_degenerate`, `nonpositive_base`,
`mwr_no_flows`, `mwr_no_sign_change`, `mwr_nonunique`,
`mwr_no_converge`, `mwr_incomplete_flows`, `mwr_negative_net_capital`,
`unmatched_transfers=N`, `cross_source_netted=N`, `journal_present`,
`nav_only`, `nav_only_capital_call_risk`, `crypto_unclassified_transfers`,
`unknown_adapter_policy`, `fx_clamped_flow`, `pre_fx_history`,
`flows_before_inception`, `after_tax`.

`configured_inception` marks a window truncated to a configured
inception override (DESIGN.md §5.4) rather than the data's own start.
`accounts_grain_meaningless` blanks TWR/MWR on accounts-grain rows of
sweep sources (a single crypto wallet's return is noise; the coarser
grains stay valid). Conduit plumbing goes further: `AccountsGrainHidden`
sources (the deposit banks) and `returns_hide` ids emit no rows at all
while still feeding every aggregate.
`mwr_negative_net_capital` reports XIRR as n/a because the
window's net invested capital (opening base plus net external flow) is
zero or below.

`stale_snapshot` marks a bucket (or the summary row) whose end-day
valuation rests on a snapshot older than 3× the entity's median
snapshot gap — the feed-died-mid-bucket case that `empty_bucket` /
`carried_forward` cannot see, since the bucket itself still holds
snapshots. The cadence is inferred per entity, so a daily feed flags
after days and a quarterly manual source only after months, with no
per-source configuration; with fewer than 3 observed gaps no cadence
is inferred and the flag stays off.

`unknown_adapter_policy` is unreachable via the real pipeline
(`silver_sources.silver_kind` has a CHECK constraint admitting only the
known adapter kinds, each of which registers a policy from its `init()`).
That invariant once silently broke — three collectors shipped into the
CHECK without a `policy.go` — so it is now enforced by a guard test
(`TestEveryCheckKindHasAdapterAndPolicy`) that cross-checks the CHECK list
against the adapter and policy registries (fred exempt: a pure FX reference
source that never reaches the returns engine). The flag itself stays as
defense-in-depth, unit-tested at the policy-resolution level.

`flows_before_inception` marks an entity whose observed external ledger
flows reach back before its resolved inception (data-derived, policy
anchor, or a §5.4 `inception_overrides` floor): those flows sit outside
every window the entity can produce, so the measurement cannot see them.
With a configured override the flag documents exactly what the override
excludes; without one it means the transaction backfill reaches further
back than the value spine supports (the case the §5.4 override is the
remedy for). Purely diagnostic — no flow handling changes — and anchored
on the inception, so an explicit `--from` / windowed-partition clip never
raises it.

Deferred flags: `corp_action_present` / `corp_action_split_timing`,
`dormant_carryforward`.

## Deferred / out of scope

- **Per-source policy migrations** still pending — each its own reviewed
  change with a per-source before/after: schwab appreciated-transfer
  handling folded into the policy + `ClassifyFlow`; manual onto the
  `NavOnly` knob (it derives NAV-only from `Flow.Regime`); svb
  drained-closure handling. End state: no source-named code in the returns
  calculation — all source-specificity is a declarative policy (+ rare
  hook) co-located in `internal/silver/<source>/`. (Done: carta / equityzen
  count their ledgers' real boundary flows — the pairs moved from the
  funding sentinel onto the custody account, making them spine-visible —
  with `CapitalCallRisk` covering flow-less windows.)
- **`--gross-of-fees`** (v2); returns ship net-of-fees / after-tax.
