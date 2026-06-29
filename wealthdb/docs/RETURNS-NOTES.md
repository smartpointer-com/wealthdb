# `wealthdb returns` — implementation notes

Companion to the design proposal (rev. 2). Records, per the build's autonomy
brief: assumptions, places the implementation deviated from the spec (spec
assumed → what the code showed → what was done → why), and anything deferred.
All examples synthetic (CLAUDE.md §4).

Build order: (1) `internal/returns/` pure math, (2) macros — *planned as
migration `0026`; reused the existing `report_accounts_history` +
`report_transactions` instead, no new migration — see the M2 section below*,
(3) `internal/gold/returns.go` + `cmd/wealthdb/cmd_returns.go`. Commit at each
milestone with tests green.

---

## Locked decisions (resolved §4 items — implemented as given)

1. Mortgage / net-negative entities → excluded from return rollups; per-entity
   twr/mwr = n/a + `nonpositive_base`; reported on a separate liability line.
2. Canonical headline bucket = daily where snapshots are dense, else monthly
   (per entity); the since-inception cumulative TWR uses it independent of
   `--period`. **⚠ SUPERSEDED** — replaced by snapshot-aligned bucketing (chain
   over actual valuation days); fixed calendar buckets collapsed the chain for
   sparse-snapshot flow-bearing sources. See "Snapshot-aligned headline" below.
3. NAV-only sources (manual, carta, equityzen) → value-growth TWR + `nav_only`
   + `nav_only_capital_call_risk`; MWR = `mwr_no_flows`. Blended aggregate MWR
   computed + tagged `mwr_incomplete_flows` (disclose, don't refuse).
4. UBS stamp-duty fall-through → adapter fix (defensive: add Swiss tokens to the
   tax mapping; confirm default fall-through is `other`, never deposit/withdrawal).
5. Netting ε = max(1.00 outCcy, 0.5% of larger leg); window ±3 calendar days;
   FX-normalized via value_outccy; named constants.

---

## Field decisions / deviations

### UBS stamp-duty fix (locked decision 4) — mechanism deviation, outcome preserved

- Spec said: "add the standard Swiss tokens to the tax mapping AND confirm the
  default fall-through is `other`." **The code showed** the `cash_movement`
  default fall-through is `signedDepositWithdrawal` (deposit/withdrawal by
  sign), **not** `other` (the audit's "falls to TxKindOther" was imprecise).
- **What was done:** added a substring match for the distinctive stamp-duty
  terms (`TIMBRE`, `UMSATZABGABE`, `STEMPEL`, `STAMP`) in `cashMovementKind`,
  routing stamp duty to `tax`. **Did NOT** change the default fall-through to
  `other`.
- **Why (flagged loudly):** changing the cash-movement default from
  deposit/withdrawal to `other` would mis-route *genuine* unlabeled wires
  (real external capital) to `other`, silently dropping them from a returns
  flow series — the opposite, worse error. The locked decision's *outcome*
  ("stamp duty never routes to deposit/withdrawal") is achieved by the token
  match; the default for genuine movements is deliberately left as
  deposit/withdrawal. A first-word match couldn't catch "DROIT DE TIMBRE"
  (first word "DROIT", also used by "droit de garde" custody fees), so the fix
  matches the duty terms anywhere in the narrative instead.
- **Residual:** stamp-duty narrative variants not containing those terms would
  still leak to deposit/withdrawal; defensive coverage only (no live sample).

### M1 — `internal/returns/` (pure math)

- **Onboarding dedup injects the unexplained remainder, not a binary suppress.**
  Spec (§2.7/GAP2): "suppress the synthetic onboarding flow when a real funding
  flow in the debut bucket already explains the opening value." Implemented as
  `synthetic = firstValue − realDebutFunding`, injected only when `> tol`. Why:
  subsumes the binary case (full real funding → 0 → suppressed) and also handles
  *partial* real funding correctly (inject only the uncovered opening), which the
  binary rule would mis-handle by double-counting or under-crediting. Same intent,
  strictly more correct.
- **Snapshot-aligned headline** *(⚠ supersedes locked decision 2 — daily-if-dense-
  else-monthly)*. The since-inception cumulative TWR now chains Modified-Dietz over
  the entity's **actual valuation (snapshot) days** (`canonicalChainBounds`), not a
  fixed daily/monthly calendar grid. **Why the change:** fixed calendar buckets
  detonate the geometric chain for sparse-snapshot, flow-bearing sources. Concrete
  failure mode (a source with irregular ~monthly NAV snapshots): a
  run of deposits lands in a gap with no snapshot, so the deposit's calendar bucket
  sees a flow with no value move (Dietz ≈ `−F/base`, a sub-(−100%) sub-period) while
  the value jump shows up at the next snapshot a bucket later (a huge positive) —
  chaining the poisoned factor sent the since-inception TWR orders of magnitude
  below −100%. Breaking at the real valuation days keeps each flow in the same
  sub-period as the value change it causes (textbook TWR at valuation dates), so the
  bucket reads ≈0% and the headline is sane. A carried tail past the last snapshot is not a valuation
  sub-period and is excluded; a window with no interior valuation falls back to a
  single `[winFrom, winTo]` bucket. For dense (daily-snapshot) sources this is
  identical to the old daily bucket, so only sparse sources move. `CanonicalHeadline-
  Bucket` / `denseGapDays` removed as dead. Guard: `TestRunReturnsSparseSnapshotNo-
  ChainCollapse`.
- **All returns math is float64.** XIRR is inherently iterative; Modified-Dietz is
  an approximation. Money is exact (DECIMAL) up to the gold→returns boundary and
  floated only here, per the proposal. No precision claim beyond ~1e-7 on rates.
- **XIRR:** Newton-Raphson (analytic derivative) from a 10% guess; bisection
  fallback scanning `[-0.9999, 100]` at 0.01 resolution for the first sign-change
  bracket. Rate floored at −0.9999 (never report worse than ~−100%). Sentinels
  `ErrNoFlows` / `ErrNoSignChange` / `ErrNoConverge` map to mwr_* quality flags;
  the caller must treat a no-external-flow window as `mwr_no_flows` and not call
  XIRR (avoids the [-V0,+V1] holding-period collapse masquerading as an IRR).
- **`CapitalDirection` is documentary and pinned to `canonical.ApplyCanonicalSign`
  in a test** (proposal §3.B): for the cash-account entity model wealthdb uses,
  `Flow.Amount == CapitalDirection(kind)·|amount| == value_outccy`, so Dietz
  `F_i = +value_outccy` and XIRR `cf = -value_outccy` with no per-kind exception.

---

### M2 — macros: reused existing, no migration 0026 (mechanism deviation)

- Spec/build-order called for a new migration `0026` with `report_boundary_values`
  + `report_external_flows`. **Building showed the existing macros already
  provide both, cleaner:**
  - **Value spine:** `report_accounts_history(p_ccy)` is the per-account
    carry-forward value series and already omits pre-inception days (ASOF inner
    join) → NULL-by-absence, which is exactly the Fix #4 behavior
    `report_boundary_values` was meant to add. Driving *all* grains off the
    per-account spine (aggregated in Go) is also what the synthetic-onboarding
    mechanism needs, so per-source/global history macros aren't used for the
    return math (only for the value-identity reconciliation).
    - **Correction (review #1, fixed in M5):** the macro also stops emitting an
      account once a *later* same-source snapshot supersedes it without that
      account — i.e. **post-disappearance absence reads as 0, NOT the same as
      pre-inception NULL.** `valueAt` originally carried a vanished account
      forward at its last value (diverging from the macro and breaking
      `global == Σ accounts`); it now returns 0 after the last emitted row and
      flags `dropped_while_nonzero`.
  - **Flows:** `report_transactions(from,to,p_ccy)` already converts net_amount
    to outCcy at occurred_at; the adapter kind and portfolio come from two
    trivial lookups (`silver_sources`, and accounts via the history rows).
- **Result: no new migration, no schema bump, no duplicate FX SQL.** Net new
  query is one `DISTINCT snapshot day per account` over positions∪cash. Lower
  risk than authoring 0026.

### M3 — gold engine + CLI

- **Netting scope:** at coarse grains v1 nets only transfer-like kinds
  (transfer_in/out, journal), not deposit/withdrawal. A deposit+withdrawal of
  equal size could be a genuine pair of external movements; netting them risks
  cancelling real external capital, whereas transfer_in/out/journal are the
  unambiguous inter-account-move signals. Deposit/withdrawal netting deferred.
  **Ratified** (review #7) — coarse-grain returns do not net
  deposit/withdrawal moves; an internal move booked that way is surfaced as two
  external flows, not silently cancelled.
- **Explicit-closure proxy:** an account is treated as explicitly closed (→
  synthetic outflow + atomic spine-zero) only when its last snapshot value is
  ~0. Mere staleness never triggers closure (per §2.7). A closing transfer that
  doesn't drive the value to 0 isn't detected as closure in v1. *(M5: the
  synthetic outflow is now deduped against a real closing withdrawal — review #2.)*
- **Default columns adapt to --method** (show twr unless mwr-only, mwr unless
  twr-only) — a small UX improvement over a fixed default set.
- **`-C` is offered on every view** (the proposal omitted it on `global`, like
  holdings). The returns row shape is uniform, so there's no reason to special-
  case global; kept it available everywhere.
- **MWR "annualized" column** mirrors the MWR (XIRR is already an annual rate)
  for spans ≥ 1y and is blank for short spans — it is not a second solve.

### M4 — refactor + coverage pass

A post-implementation review (duplication/dead-code, coverage, doc accuracy, PII)
drove these changes:
- **Dedup:** extracted `investorCFs` (the investor cash-flow vector was assembled
  identically in `XIRR` and `MWRSignChanges`) and `acctKey` (the `src\x00acct`
  map key was built inline in ~4 places); `parseFloat`/`parseFloatPtr` now share
  a `parseFloat64` core; `entityFlows` reuses `flowsIn`; `closureDay` uses
  `math.Abs(v) < valueTol` instead of an open-coded epsilon.
- **Dead code removed:** `accountData.lastDay()` (never called); `approxEqual`
  moved out of production `synthetic.go` into the test helpers; the unused `key`
  parameter dropped from `computeEntityReturn`.
- **Hardening:** `bisectXIRR` now resets its bracket on a NaN/Inf sample instead
  of comparing a sign against a poisoned previous value (latent, not live, given
  current inputs).
- **Coverage:** `internal/returns` 76% → ~94%; added gold tests for `netTransfers`,
  the portfolios grain, staggered-inception synthetic onboarding (incl.
  `fundingNear` partial dedup), `mwr_incomplete_flows`/`mwr_nonunique`,
  `journal_present`/`crypto_unclassified_transfers`, `partial_window` +
  `--inception strict`, and `empty_bucket`.
- **Finding — `unknown_adapter_policy` is unreachable via the real pipeline.**
  `silver_sources.silver_kind` has a CHECK constraint admitting only the 12 known
  adapter kinds, so `FlowPolicyFor` never returns `Known=false` for a loaded
  source. The flag + the policy `default` branch are kept as defense-in-depth (a
  future adapter added to the CHECK but not to `FlowPolicyFor` would surface it),
  and the `default` is unit-tested at the `FlowPolicyFor` level; the gold-level
  flag emission is intentionally left uncovered.
- **Docs:** corrected the DESIGN §10.9 quality-flag list (added `mwr_no_converge`,
  `unknown_adapter_policy`, `partial_window`), scoped `--fx-mode`/`-d` away from
  `returns` in SKILL.md, and annotated this build-order line.
- **PII:** independent sweep of the added tests, docs, and all four commit
  messages — CLEAN (synthetic fixtures only).

### M5 — supervisor-review fix pass

The independent supervisor review (`returns-impl-review.md`: 25 confirmed / 3
partial / 0 refuted, no critical/high) drove these fixes — all in the
best-effort coarse-grain / closure / old-or-cross-currency zones; the verified
account-grain headline math was left untouched:
- **#1 disappearing account:** `valueAt` returns 0 after an account's last
  emitted row (matching the macro), so `global == Σ accounts` reconciles (gold
  reconciliation test vs `GlobalAsOf`); flags `dropped_while_nonzero`. The
  per-entity window is clamped to the latest available data day. *(`lastDay()`,
  removed as dead in M4, is reintroduced and now used.)*
- **#2 closure dedup:** the synthetic closure outflow is deduped against a real
  closing withdrawal/transfer_out (`closingNear`), mirroring onboarding — no more
  double-count on a "withdraw everything" closure.
- **#3 FX-clamp honesty:** emit `fx_clamped_flow` (a flow valued before its
  currency's first FX rate) and `pre_fx_history` (window starts before a held
  non-output currency's first rate), via a per-currency earliest-rate-day join.
- **#4 `boundary_same_snapshot`:** the *receiving* bucket (prior bucket empty +
  this bucket has a fresh snapshot) is now flagged — it is NOT subsumed by
  `empty_bucket` (which flags the flat donor bucket); the old "subsumed" note was
  wrong.
- **#6 deterministic netting:** `Flow.ID` (transaction id) added; `netTransfers`
  sorts stably by `(|amount|, day, id)`.
- **#7 sub-(-100%) loss:** XIRR returns `ErrNoConverge` (→ `mwr_no_converge`)
  instead of a clamped non-root ~-100%.
- **#8:** `unmatched_transfers=N` carries the count; the `mwr_%` column shows the
  *period* (de-annualized) figure, consistent with `twr_%` (`mwr_ann_%` holds the
  annualized XIRR).
- **#9 end-to-end gold tests** through `RunReturns`: disappearance reconciliation,
  closure dedup, coarse netting + `--netting off`, MWR error-flag mapping,
  FX-clamp flags, and a wiring-checked MWR.
- **Global `silver_source` bug (post-review):** the `global` grain spans all
  sources, so its row's `silver_source` is now empty — it was previously filled
  from `assets[0].src`, a non-deterministic map pick (the visible symptom was a
  random/non-sensical source on `returns global`). Regression-guarded by a
  multi-source global test. The accounts/sources/portfolios grains group by source
  so their `silver_source` is unaffected.

### M6 — staggered-inception double-count fix (pre-debut funding subsumed)

A confirmed bug: at coarse grains (`sources`/`portfolios`/`global`) the engine
**double-counted** capital for a constituent that joins the aggregate value spine
*mid-window* ("staggered inception"). The synthetic onboarding fired at the
account's full first-snapshot value AND the real funding flow was also counted,
because the old dedup (`fundingNear`, ±3 days) only suppressed funding within
three days of the debut *snapshot*. An account fed only by month-end snapshots can be funded mid-month and
first appear in the value spine weeks or months later, so the dedup never
fired. The flow-insensitive chained TWR broke too: the
real funding lands in an early bucket where the aggregate value series does not
yet include the account (it is not alive until debut), so that bucket reads `+F`
against `ΔV≈0` (strongly negative); onboarding then zeroes the debut bucket. The
two halves live in different buckets, so chaining compounded rather than
cancelled them — driving cumulative TWR below −100%.

**Principle implemented:** at every grain each dollar crossing the entity
boundary is counted exactly once, in the same bucket as the value change it
causes. A constituent's flows in a region the aggregate value series does NOT
reflect — *before its debut*, or *during its closure gap* — are **subsumed** by
the synthetic onboarding/closure amount instead of double-counted.

**Mechanism (`subsumesAt` + the reworked `entityFlows`):**
- A constituent debuting at `d > winFrom` has every own external flow dated `≤ d`
  dropped from the aggregate flow series (it is pre-spine-arrival, no visible
  ΔV); onboarding books the **full firstValue** at `d` for the whole arrival
  (`OnboardingFlow(d, firstValue, 0)` — the near-day `fundingNear` partial dedup
  is now redundant and removed). Accounts already alive at `winFrom` keep all
  in-window flows and get no onboarding (unchanged, exact).
- The **closure mirror** (`lastNonzeroDay`): a closing constituent's drains dated
  after its last non-zero carried value, up to the zeroing day `cd`, are
  subsumed by the synthetic closure outflow (the carried value is flat across
  that gap — no visible ΔV — so a drain there would double-count). `ClosureFlow`
  is now called with `realClosing=0` for the same reason.

**Netting interaction (the subtle part).** Transfer/journal
netting runs over the **full candidate set, pre-debut legs included**, BEFORE
subsumption, so genuine internal pairs still annihilate. Only legs that *survive*
netting as a constituent's own pre-debut/closure capital are then subsumed. This
is why `netTransfers` was refactored to a tagged `netOwnedTransfers` that carries
each leg's owning account + a `subsumed` flag through the greedy match unchanged
(`netTransfers` is kept as a thin ownership-free wrapper for its direct unit
test). The phantom the design warns about — dropping a pre-debut journal-IN while
its sibling journal-OUT remains — does *not* arise: a surviving journal-OUT sits
on an *alive* account whose value series genuinely reflects the −X drop, so the
−X is real, not orphaned. Pairs that don't net (different magnitudes / outside
±3 days) leave the OUT-leg as a real outflow and subsume only the IN-leg into the
late account's onboarding; capital is still counted once.

**Verified against a read-only copy of a populated gold DB:** constituents
funded well before their first snapshot no longer double-count, and
constituents with no funding transactions keep their onboarding flow.
`TestStaggeredJournalFundedNoPhantom` proves the netting interaction is
handled without a phantom.

### Known semantics on record

- **Mid-series disappearance (account vanishes for a snapshot or two, then
  reappears):** Go carries the account across the gap (its value series resumes
  after), whereas the macros read 0 inside the gap. This is left as-is — it is in
  the economically-sensible direction (it does not inflate the terminal value, and
  the end-disappearance case that broke `global == Σ accounts` is fixed by #1). The
  only consequence is that a per-bucket value at a boundary that lands *inside* a
  mid-series gap can differ from `report_*_history`; the headline and terminal
  reconcile.

### Deferred quality flags (the computed v1 set is below; these remain TODO)

Still deferred: `corp_action_present` / `corp_action_split_timing`,
`dormant_carryforward`, `flows_before_inception`, `stale_snapshot` (the principled
`empty_bucket`/`carried_forward` condition covers the common stale case; a
generous source-relative `stale_snapshot` threshold is still TODO).

Computed v1 set: `since_data_inception`, `partial_window`, `staggered_inception`,
`empty_bucket`, `carried_forward`, `boundary_same_snapshot`, `dropped_while_nonzero`,
`dietz_degenerate`, `nonpositive_base`, `mwr_no_flows`, `mwr_no_sign_change`,
`mwr_nonunique`, `mwr_no_converge`, `mwr_incomplete_flows`, `unmatched_transfers=N`,
`journal_present`, `nav_only`, `nav_only_capital_call_risk`,
`crypto_unclassified_transfers`, `unknown_adapter_policy`, `fx_clamped_flow`,
`pre_fx_history`, `after_tax`.

## Deferred / out of scope (TODO for review)

- Real capital-call/distribution flows for carta/equityzen (would make their
  TWR/MWR trustworthy beyond onboarding) — needs adapter work to expose the
  non-sentinel leg; tracked via `nav_only_capital_call_risk`.
- `--gross-of-fees` (v2); returns ship net-of-fees / after-tax.
