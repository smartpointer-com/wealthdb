# `wealthdb returns` — implementation notes

Companion to the design proposal (rev. 2). Records, per the build's autonomy
brief: assumptions, places the implementation deviated from the spec (spec
assumed → what the code showed → what was done → why), and anything deferred.
All examples synthetic (CLAUDE.md §4).

Build order: (1) `internal/returns/` pure math, (2) migration `0026` macros,
(3) `internal/gold/returns.go` + `cmd/wealthdb/cmd_returns.go`. Commit at each
milestone with tests green.

---

## Locked decisions (resolved §4 items — implemented as given)

1. Mortgage / net-negative entities → excluded from return rollups; per-entity
   twr/mwr = n/a + `nonpositive_base`; reported on a separate liability line.
2. Canonical headline bucket = daily where snapshots are dense, else monthly
   (per entity); the since-inception cumulative TWR uses it independent of
   `--period`.
3. NAV-only sources (manual, carta, equityzen) → value-growth TWR + `nav_only`
   + `nav_only_capital_call_risk`; MWR = `mwr_no_flows`. Blended aggregate MWR
   computed + tagged `mwr_incomplete_flows` (disclose, don't refuse).
4. UBS stamp-duty fall-through → adapter fix (defensive: add Swiss tokens to the
   tax mapping; confirm default fall-through is `other`, never deposit/withdrawal).
5. Netting ε = max(1.00 outCcy, 0.5% of larger leg); window ±3 calendar days;
   FX-normalized via value_outccy; named constants.

---

## Field decisions / deviations

### M1 — `internal/returns/` (pure math)

- **Onboarding dedup injects the unexplained remainder, not a binary suppress.**
  Spec (§2.7/GAP2): "suppress the synthetic onboarding flow when a real funding
  flow in the debut bucket already explains the opening value." Implemented as
  `synthetic = firstValue − realDebutFunding`, injected only when `> tol`. Why:
  subsumes the binary case (full real funding → 0 → suppressed) and also handles
  *partial* real funding correctly (inject only the uncovered opening), which the
  binary rule would mis-handle by double-counting or under-crediting. Same intent,
  strictly more correct.
- **Canonical headline bucket uses a median-inter-snapshot-gap heuristic**
  (`denseGapDays = 4`): daily if median gap ≤ 4 days, else monthly. The spec said
  "daily where snapshots support it, else monthly" without a rule; this is the
  operationalization. Tunable constant; documented.
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

## Deferred / out of scope (TODO for review)

- Real capital-call/distribution flows for carta/equityzen (would make their
  TWR/MWR trustworthy beyond onboarding) — needs adapter work to expose the
  non-sentinel leg; tracked via `nav_only_capital_call_risk`.
- `--gross-of-fees` (v2); returns ship net-of-fees / after-tax.
