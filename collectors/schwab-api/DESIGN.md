# schwab-api: Design

Part of the **wealthdb** suite — see [the architecture overview](../../DESIGN.md) for the bronze → silver → gold model and [collectors/README.md](../README.md) for shared collector conventions.

## 1. Audience and scope

This document describes the Schwab-specific design of
`schwab-api`: how the OAuth fetch loop, the source-shaped silver
schema, and the temporal model are put together. Where a choice is
specific to Schwab's API it is called out so a reader adapting the
pattern to another backend can substitute the equivalent.

## 2. The three layers

See [the architecture overview](../../DESIGN.md) for the
bronze → silver → gold model and the layer-ownership boundaries this
toolkit inherits. The rest of this document covers only how
`schwab-api` realises its bronze and silver.

## 3. The toolkit

schwab-api follows the fleet lifecycle
([collectors/README.md](../README.md#the-lifecycle)): `login.py`,
`download.py`, `load.py` and `prune.py`, plus the manual
`recompress.py`. This section covers what is specific to Schwab.

### 3.1 `login.py` — authentication lifecycle

Schwab uses OAuth 2.0 with a 7-day refresh-token cap. Only an
interactive browser grant renews it. `login.py` runs that grant,
exchanges the code, and writes the token file.

**Why a separate tool?** Auth is interactive, episodic (weekly for
Schwab) and has its own failure modes. Inside the fetch loop it would
raise an interactive prompt in an unattended run.

**Choices that generalise:**
- `--check` reports the token's age, then proves the token with the
  cheapest authenticated call (`/accounts/accountNumbers`). It makes a
  failing `download` predictable.
- schwab-py's own interactive prompts ("Press ENTER to open the
  browser") are suppressed. They expire auth codes over SSH, in remote
  environments, or after any interruption.
- The token file is written with mode `0600` from its first byte. The
  code does not rely on the umask.

#### Failure discipline

The flow drives a 2FA challenge and consent pages that can lock the
account. A blind click on an unresolved challenge submits an empty or
stale code, and repeated failed 2FA verifications are a lockout trigger.
The flow therefore follows these rules. schwab-web's separate MFA code
follows them too.

- **No blind clicks.** An advance or consent click fires only on a page
  positively classified as part of the consent flow, by URL route or DOM
  heading. It never fires on a challenge, an interstitial or an unknown
  page.
- **Click budget and change check.** Every click loop checks that the
  page changed after a click. An unchanged page is never clicked again.
  A stalled or over-budget flow captures the page and fails.
- **MFA is a barrier.** Nothing drives the page while a challenge is
  unresolved. An empty code, or stdin at EOF, is terminal. A non-TTY
  stdin fails before any credential is submitted. The wrapper passes
  docker `-i` only when stdin and stdout are TTYs, so a piped run gets a
  closed stdin.
- **One shot per run.** One credential submission and at most one 2FA
  submission. Any failure after that is terminal: the provider's on-page
  message is logged verbatim and the run exits nonzero. Re-running is
  the recovery path.
- **Terminal pages end the run.** The gateway's notice route family
  (`…/ui/host/#/information/<code>`) stops all interaction at once
  (exit 7). A lockout message adds that Schwab must unlock the account.
  An unrecognized interstitial fails with a capture (exit 8) instead of
  a silent multi-minute wait.

Notice-page details:

- The gateway's content catalog maps one notice code to the
  identity-verification wording ("we need to verify your identity
  before proceeding … please call"). The flow treats it as a lockout.
  Two sibling codes show a generic "there's a problem logging you in"
  and are not lockouts.
- The notice text renders into the page's `#msgLabel` container through
  an async fetch, seconds after the route change. Message extraction
  polls for that container before it falls back to the body text. An
  immediate body read yields only footer boilerplate.

### 3.2 `download.py` — bronze fetch

Owns "talk to the bank, write what came back to disk verbatim".

Schwab specifics:
- One HTTPS round-trip per artefact kind: accounts, user prefs,
  positions, transactions (chunked into ≤1-year windows because the
  endpoint caps that), open orders.
- By default the run extends with a `/instruments` lookup for every
  symbol seen in positions and transactions; `--no-instruments` opts
  out. Because instrument metadata changes rarely, a high-cadence
  schedule can skip the extra round-trip via the opt-out. See §4.9.
- Writes one JSON file per (artefact kind, account, window) into a
  `<bronze-dir>/<UTC-timestamp>/` directory. Timestamp is the run-start
  time; subsequent runs get a new directory, never overwrite.
- Drops a `run.json` status manifest into the run dir: `in-progress`
  at run-dir creation, atomically overwritten with `complete` (plus a
  few counts, no identifiers) once every artefact is written. It is the
  completeness signal `prune` and `load` key on: `load` skips a dump
  `prune` counts as not complete (§3.4). The manifest lands before the
  first request, so a run that dies at the token refresh leaves an
  `in-progress` shell too. `--dry-run` creates no run dir, so it leaves
  no shell and no manifest.
- Never imports or calls write endpoints (`place_order`, etc.).
  This is enforced by code review and documented in `AGENTS.md`. An
  order rate limit of 0 on the Schwab app adds a safeguard.

**Bronze compression.** Each of the six data artefacts
(`account_numbers.json`, `user_preference.json`,
`accounts_positions.json`, every `transactions_NNN.json`,
`open_orders.json`, and the optional `instruments.json`) is
zstd-compressed in place as it lands
(`collectorkit.compress.compress_best_effort`: atomic tmp+rename,
decompress-and-sha256-verify before the plain file is unlinked, mtime
carried over), so the run dir fills with `.json.zst`. JSON payloads
compress to a small fraction of their raw size. Compression is
best-effort: a failure (disk full, missing codec) downgrades to a
warning and leaves the plain `.json` in place — every reader resolves
the on-disk variant via `compress.resolve_variant` (plain wins when
both forms coexist), so a half-adopted tree is a valid tree, not an
error state. `run.json` is **never** compressed: it is the
status-lifecycle manifest `prune` reads directly and must
stay greppable. Because the loader runs host-side and parses the JSON
in Python (SQLite silver, no engine to stream `.zst` natively), it
decompresses via `compress.open_text` inside the single `read_json`
funnel — a `.json.zst` and a plain `.json` yield the identical parsed
object, so silver is byte-identical either way. Pre-compression run
dirs are converted by the manual `recompress` verb (thin wrapper over
`collectorkit.recompress`: prune-grade safety envelope,
verify-then-unlink per file, reuses prune's completeness predicate,
run.json excluded from its patterns; never scheduled) — after a sweep,
a `load --force` rebuild must produce identical silver.

**Portable principles:**

- **Bronze is one timestamped directory per run, not a single mutable
  file.** Re-runs accumulate; never overwrite a prior run. The dump
  timestamp doubles as a primary key in silver later. Schwab uses
  `YYYYMMDDTHHMMSSZ`; pick something similar.
- **Re-fetch generous windows on every run, don't try to be
  incremental.** Silver does the dedup. This is cheaper than tracking
  "since when" cursors and survives missed runs trivially.
- **Filename should not leak identifiers.** Schwab returns account
  hashes which would be safe-ish, but they still stay out of
  filenames; account dimension lives inside payloads. Doing `ls` on
  a bronze dir reveals only artefact kinds, not which accounts are
  involved.
- **Filter to what matters at the dump layer, not silver.** For
  Schwab open orders, the dump keeps only non-terminal statuses
  and discards everything else — order history is out of
  scope. Data that would only be thrown away is not captured.
- **A `--dry-run` mode that validates auth and lists accounts but
  doesn't fetch.** It refreshes the stored token and signs in to
  nothing, so it serves as a connectivity test that fires no 2FA.

### 3.3 `load.py` — bronze → silver

Owns "parse bronze JSON, apply migrations, insert into SQLite".

Three discipline points:

1. **Apply pending migrations on startup**, before any data load.
   Read `MAX(silver_schema_version) FROM schema_meta`, compare to
   the highest numbered file in `migrations/`, execute any forward
   migrations in order. Silver must always be at the latest schema;
   no backward-compat in queries.

2. **One dump = one transaction.** Open a SQLite transaction at the
   start of loading a dump directory; record the dump in `dump_runs`
   *as the last statement* before commit. A failure anywhere in the
   middle rolls everything back, so the next run re-attempts the
   whole dump. This makes the loader idempotent without any retry
   logic in user code.

3. **Skip already-loaded dumps** by checking `dump_runs` at the top
   of the load function. The unique `snapshot_at` PK on `dump_runs`
   ensures re-running on the same bronze dir is a no-op. A dump that
   `prune` counts as not complete (§3.4) is skipped too and stays
   pending: the loader classifies it with prune's own predicate. Its
   artefacts are partial: loading them would record the dump as done
   with, say, accounts but no positions.

The loader is also where minor "cleanup-on-the-way-in" lives —
specifically, stripping per-request noise fields that would otherwise
defeat content-based dedup (see §4.7).

Every bronze read goes through the single `read_json` funnel, which
resolves the on-disk variant (`compress.resolve_variant`: plain `.json`
wins over `.json.zst`) and decompresses by suffix in memory. The two
`transactions_*` enumerations resolve the same way (they strip any
`.zst`/`.gz` suffix to recover the logical name, dedup, then resolve —
so a plain + `.zst` twin that coexist ingest exactly once, plain
winning). A compressed bronze tree therefore loads to byte-identical
silver as its uncompressed form (§3.2).

### 3.4 `prune.py` — reclaiming bronze

A thin wrapper over the shared `collectorkit.prune` engine, run
host-side like `load`. The safety envelope is the engine's
([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)).

A run dir holds the JSON load inputs, the `run.json` manifest, and,
only under `download --debug`, a `screenshots/` subdir with the HTTP
trace. `debug_subdirs` is that one subdir. The browser captures and
traces belong to `login.py` and land in the debug dir outside bronze,
which the wrapper passes to `prune` as `--debug-dir`. So `prune` has
three categories: `screenshots/` of complete dumps, whole non-complete
dumps (a `download` that crashed or was interrupted), and idle entries
of the debug dir.

Completeness comes from the `run.json` status. A dump carrying no
status falls back to the presence of `open_orders.json`, the last
unconditional artefact a complete run writes. The in-flight guard
(`--min-age-hours`) protects a long transaction backfill that is still
writing `transactions_NNN.json`. `load` applies the same predicate, so
a non-complete dump was never loaded and deleting it never changes
silver.

Bronze compression is inert to `prune` for a manifest-bearing dump —
classification is status-based, and the data artefacts are load inputs
whether `.json` or `.json.zst`. The one interaction: a dump carrying no
status is judged complete by the presence of its terminal data artefact
`open_orders.json`, which a `recompress` sweep may leave as
`open_orders.json.zst`. So `_is_complete` resolves the on-disk variant
(plain **or** `.zst`), not a fixed `.json` name — a fixed name would flip
a recompressed dump to NON_COMPLETE and prune it. Compressing a dump is
the separate manual `recompress` verb's job — it rewrites load inputs
(which `prune` never does), so it lives behind the same completeness
envelope plus a verify-then-unlink rule and is never scheduled (§3.2).

## 4. Silver schema design

### 4.1 Storage: SQLite + JSON1

SQLite chosen over alternatives:
- vs. DuckDB: SQLite is universal (`sqlite3` CLI on every Unix box),
  has a stable file format with a 25-year track record, and is OLTP-
  shaped, which matches silver's "append-only single-writer" usage.
  DuckDB is better for analytics; that's why gold uses it instead.
- vs. Postgres / MySQL: server-process overhead has no benefit at
  personal-portfolio scale. Single-file portability wins.
- vs. flat files / parquet: silver needs cheap point queries and
  cheap incremental writes. Parquet is for gold-style scans.

JSON1 is a non-optional companion. `json_extract` is fast enough at
this scale that speed alone never forces a column out of `payload`.
See §4.2.

### 4.2 Semi-relational pattern

Every silver table has the same shape:

```
<stable filter columns>  -- promoted, indexed, used as PK
payload TEXT NOT NULL    -- the rest, as canonical-JSON
```

Stable filter columns are the *minimum* needed to:
- Form the row's primary key (uniqueness + clustering).
- Answer the dominant access pattern with an index probe rather
  than a full scan.

For Schwab silver:
- `positions`: `(snapshot_at, account_external_id, instrument_key)`
- `transactions`: `(activity_id)` PK + secondary index on
  `(account_external_id, timestamp)`
- everything else is in `payload` — instrument descriptions, prices,
  fee types, transaction subtypes, etc.

`positions` also carries two cost-basis columns. They hold Schwab's
adjusted tax-lot basis, so an adapter reads it without a
`json_extract` per row:

| Column | Source field | Meaning |
| --- | --- | --- |
| `average_cost` | `averagePrice` | Cost per unit, as sent. |
| `unrealized_gain_loss` | `longOpenProfitLoss`, or `shortOpenProfitLoss` when `shortQuantity` > 0 | Open P/L in USD, as sent. |

The holding's total cost is market value minus open P/L, which needs
no per-asset-class scale. The header of
[migrations/0005_position_cost.sql](migrations/0005_position_cost.sql)
gives the price convention of `average_cost`, the meaning of zero and
NULL, and why `averageLongPrice` stays in `payload`.

**Why not promote more columns?** Because the upstream's schema
evolves. Schwab adds new fields all the time; UBS varies what each
MT message type carries; both sometimes change types (string ↔
object). A relational schema would force a migration for every
upstream change. A JSON payload absorbs all of them silently.

**Why promote anything at all?** Because adapters in `wealthdb`
need to filter rows efficiently. `WHERE snapshot_at = ? AND
account_external_id = ?` should be an index probe, not a JSON scan.

**When to promote a JSON field into a real column** (decided per
table, not as a universal rule):
- The query pattern needs it as a filter or join key, or an adapter
  reads it on every row.
- It's present on *every* row of that table (no nulls from sparse
  fields — promoting a field that's only on 60% of rows produces a
  half-useful column).
- It's in the source's stable surface (not a generated/random ID).

When in doubt, leave it in `payload`.

### 4.3 Temporal model: monotemporal on source-time

Silver stores facts at **the time the source claims they happened**,
not the time they were ingested. The ingest time only matters for
debugging and is *not* a queryable column.

Why monotemporal rather than bitemporal? Bitemporal databases solve
a real problem (auditing "what did we believe at time X about events
at time Y"), but at the cost of doubling the model's complexity.
For personal portfolio analysis, the question "what was true" suffices
and bitemporal is overkill.

The price of monotemporal: when an upstream source amends a past
event, the prior version is lost. That loss is accepted; see §5.

### 4.4 Two row archetypes: snapshots and events

Every silver table is exactly one of these.

**Snapshot tables** represent "state at a moment". One row per
(entity, snapshot_at). For Schwab: `accounts`, `user_preference`,
`account_balances`, `positions`, `open_orders`, and the optional
`instruments` table (see §4.9).

- PK = composite, **`snapshot_at` first**, then entity columns.
- The composite PK *is* the as-of index — no separate index needed.
  The natural query is:
  ```sql
  WITH latest(ts) AS (
      SELECT MAX(snapshot_at) FROM positions WHERE snapshot_at <= :as_of
  )
  SELECT p.* FROM positions p JOIN latest ON p.snapshot_at = latest.ts;
  ```
  The CTE form is canonical because some optimisers mis-plan the
  inner MAX as correlated; writing it as a CTE forces a one-shot
  scalar evaluation.
- Snapshots are append-only. Each run produces a new row per entity
  per dump, regardless of whether the entity changed.
- *Exception:* very-slow-changing snapshots (e.g. account-number
  mappings) use content-based dedup at load time — insert a new row
  only when the canonical-JSON payload differs from the most recent
  row for that entity. Direct text comparison, no hash column. This
  is a per-table optimisation, not a universal rule.

**Event tables** represent things that happened. One row per
source-stable event ID. For Schwab: `transactions` only.

- PK = source's event ID (Schwab `activityId`).
- Load semantics: **window-DELETE-then-INSERT, in one transaction**,
  per `(account, time-window)`. The dump emits one file per
  `(account, non-overlapping window)`; the loader replaces exactly
  that range.
  ```sql
  BEGIN;
  DELETE FROM transactions
    WHERE account_external_id = :acct
      AND timestamp >= :window_start AND timestamp <= :window_end;
  INSERT INTO transactions VALUES (...);  -- one per event in payload
  COMMIT;
  ```
- *Not* row-level upsert. Why: upserting on `activityId` misses
  *upstream removals*. If the bank retracts a transaction (cancelled
  trade backdated out, settlement reversal), upsert leaves a phantom
  row. Window-replace catches it.
- One wrinkle: Schwab sometimes returns a transaction whose `time`
  falls *outside* the request window. JOURNAL entries in particular
  appear to be selected by posting/settlement date while the row's
  `time` is the underlying event time, which can be days earlier. A
  prior dump may already hold such a row at its out-of-window
  timestamp, where the window DELETE won't reach it, so the re-fetch
  would PK-collide on `activity_id`. The loader therefore also DELETEs
  the exact `activity_id`s it is about to INSERT (chunked under the 999
  host-param cap), which keeps the window-DELETE removal-detection
  semantics for in-window rows and clears the boundary stragglers.
- Index on `(account_external_id, timestamp)` (composite). Time-range
  queries that filter by account take the index probe; the single-
  column `timestamp` index would be redundant.

**Why not three archetypes (e.g. "slowly changing dimension")?** The
two-archetype model covers every table here. Resist adding a third until
a real third pattern shows up.

### 4.5 Indexing strategy: minimal

Resist the urge to add secondary indexes for cross-cut queries
like "this instrument over time" or "this account over time".

At personal-portfolio scale:
- 50 positions × daily × 10 years = 180k rows.
- A secondary index has ~180k entries — same order as the table.
- A range-scan over a time-clustered table doing 18k rows for one
  year is sub-millisecond on any modern hardware.
- A composite index saves no I/O when the cardinality of the secondary
  key approaches the cardinality of the time key.

**Rule: one index per table** (usually the PK is enough). Add a
secondary index only when a query is demonstrably slow, never
prophylactically.

### 4.6 Identifier translation

Schwab returns account identifiers in two forms:
- **Plaintext account number** — embedded inside positions,
  transactions, orders, balances payloads.
- **Hash** — returned from `/accounts/accountNumbers` and used in the
  REST path for per-account endpoints.

The mapping is stable per Schwab app. silver uses the **hash** as
`account_external_id` everywhere. The loader builds a
`{plaintext: hash}` lookup from `account_numbers.json` at the start
of each dump load and translates plaintext→hash before insert.

The plaintext stays inside the JSON `payload` for traceability. One
promoted column carries it: `accounts.account_number` (migration 0004),
which lets gold bridge schwab-api accounts to schwab-web's without a
`json_extract` per row. Every other table keys on the hash, so
`account_external_id` never holds a real Schwab account number.

**For other banks:**
- UBS PSN doesn't hash account numbers, but it does have a
  `relationship_id` dimension (there can be multiple banking relationships
  under one SFTP login, distinguished by the `SFTPCH<NN>` / `SFTPCH<MM>`
  server-ID in filenames). silver needs to carry that as a stable
  column on every row, since transactions for relationship 1 and 2
  are otherwise indistinguishable.
- Pick *one* identifier per logical account dimension and stick with
  it consistently across silver tables. The wealthdb gold layer
  will map per-broker IDs onto its canonical `accounts.account_id`.

### 4.7 Per-source noise filtering

Silver is allowed to drop fields that are pure per-request noise. The
test: if the field changes on every API call regardless of the
underlying state, it's noise.

Schwab example: `streamerInfo[*].schwabClientCorrelId` in the
`/userPreference` response. Schwab regenerates it per call. Without
stripping, content-based dedup of `user_preference` would *never*
deduplicate, even when the underlying preferences haven't
changed for years.

Implementation in `load.py`:

```python
def _strip_user_preference_noise(pref: dict) -> dict:
    """Drop per-request noise; bronze keeps the original."""
    out = copy.deepcopy(pref)
    for entry in out.get("streamerInfo") or []:
        entry.pop("schwabClientCorrelId", None)
    return out
```

Strip on the way in; store the stripped form. Bronze retains the
original for anyone who genuinely needs to trace a specific request.

**Don't strip:**
- Anything that could carry semantic information ("status" fields,
  "type" fields, anything that might be queried later).
- Anything that *could* be noise but isn't provably so — silver
  faithfulness defaults to "preserve, dedup based on the noisy form".

### 4.8 Schema versioning via migrations

`migrations/NNNN_<slug>.sql` numbered files in the repo. Each file:
- Contains DDL changes and any necessary data backfill.
- Ends with `INSERT INTO schema_meta (silver_schema_version, applied_at)
  VALUES (N, CAST(strftime('%s','now') AS INTEGER));` — this is the
  loader's "migration completed" marker.

A crash mid-migration can leave a partial schema. The recovery is
`load --force`, which deletes the silver DB and rebuilds it from bronze.

The loader:
1. On startup, reads the maximum `silver_schema_version` from
   `schema_meta` (treats absent table as version 0).
2. For each migration file with a higher number, in numeric order,
   executes the entire file as one `executescript` call.
3. Subsequent silver writes happen against the upgraded schema.

**Silver is always at the latest schema.** Never write code that
handles "if column X exists, use it, else fall back". If the migration
hasn't run, fix the migration. If it has, the column always exists.

### 4.9 Optional enrichment artefacts

A subtler design wrinkle: Schwab's positions and transactions endpoints
emit `description` for `COLLECTIVE_INVESTMENT`, `FIXED_INCOME`,
`OPTION`, and `CURRENCY` rows, but **omit it for `EQUITY` rows**.
Strict source-faithfulness would inherit this inconsistency into
silver and pass it downstream. Two coherent ways out:

1. Drop `description` from the silver projection of *all* asset
   classes (level-down to the lowest-common-denominator).
2. Fill it in for equities by hitting a second Schwab endpoint that
   *does* return descriptions (`/marketdata/v1/instruments`).

schwab-api takes (2): the `/instruments` lookup runs by default, with a
`download.py --no-instruments` opt-out, because the metadata is
slow-changing and the extra round-trip needn't fire on every dump.
The fetched payload lands in a
separate bronze file (`instruments.json`), consistent with the
"one Schwab response per file" convention. The silver loader populates
an `instruments` table when that bronze file is present; absence is
not an error.

**Generalisable pattern: optional enrichment bronze artefacts.**

- Gated by a dedicated flag on the dump tool (here default-on, with a `--no-instruments` opt-out).
- Live in their own bronze file; don't get merged into the
  state/event artefacts.
- Map to their own silver table; do not back-fill columns into
  existing tables.
- The opt-out lets it run on a slower cadence than the state/event
  dump (Schwab's instrument metadata only really changes on
  corporate-naming events).

This pattern accepts a small deviation from "silver mirrors source
faithfully": silver may *add* missing data when the source supplies
it through a sibling endpoint. It does not change data Schwab did
return, and it does not invent any data Schwab did not provide.

**Schwab quirk worth knowing about** when implementing the same
pattern for another data class: class-share tickers are spelled
differently on different Schwab endpoints. `/accounts` and
`/transactions` emit the **dot form** (e.g. `BRK.B`); `/instruments`
only indexes the **slash form** (`BRK/B`). `fetch_instruments` sends
*both* forms when a sent symbol contains `.`, and rewrites returned
`/` back to `.` on the symbol field so silver joins cleanly across
artefacts. The same kind of cross-endpoint inconsistency may bite in
other places; treat fetch-side normalisation as a first-class concern
when bridging artefact types.

**Local synthesis when no API supplies the field.** A related pattern:
when the source's endpoints omit a field that downstream tools need,
but the field can be deterministically constructed from other data the
source *does* provide, silver may synthesise it. Schwab returns bond
transferItems with `maturityDate` and `variableRate` but no description,
and option transferItems with the four contract coordinates
(`underlyingSymbol`, `expirationDate`, `strikePrice`, `putCall`) but
again no description. `load_synthesized_instruments` builds the
descriptions locally at load time — Treasury CUSIPs decode via their
six-character prefix into the issuer family — and writes them into the
same `instruments` silver table that the API-sourced rows live in.

The two enrichment paths coexist via a defer-to-API rule: when a symbol
appears in both the dump's `instruments.json` *and* a transactions
transferItem, synthesis skips it and lets the API row stand. The
silver consumer sees one table; the source is transparent.

### 4.10 Tax wrapper: not exposed

The Schwab Trader API does **not** expose the tax-registration / wrapper
of an account (IRA, Roth IRA, Rollover IRA, SEP-IRA, Inherited IRA,
Coverdell ESA, UTMA/UGMA, 529, taxable, trust, etc.) as a structured
field. No endpoint that touches accounts carries one:

| Endpoint | Wrapper-shaped field? |
|---|---|
| `GET /trader/v1/accounts` and `/accounts/{hash}`, with or without `fields=…` | No. The `securitiesAccount.type` field is one of {`CASH`, `MARGIN`} — that is *margin enablement*, not tax treatment. The 9 returned keys are `accountNumber`, `type`, `roundTrips`, `isDayTrader`, `isClosingOnlyRestricted`, `pfcbFlag`, plus three balance blocks. Undocumented projections (`accountSubType`, `registrationType`, `type2`, `registration`, `all`, …) return the same 9 keys; unknown projection values are silently ignored. |
| `GET /trader/v1/accounts/accountNumbers` | No. Just `{accountNumber, hashValue}`. |
| `GET /trader/v1/userPreference` per-account entry | The structured `type` field returns `BROKERAGE` — useless for discrimination. The only wrapper signal is the **`nickName`** free-text field. |

The silver `accounts` table already promotes `nickName` as a real
column (`nickname`, populated by `_build_account_metadata` from
`userPreference.accounts[*].nickName`; see migration 0003). No further
field is available to promote.

The two structured signals that exist (`account_type` ∈ {`CASH`,
`MARGIN`}, the free-text `nickname`) are both promoted as silver
columns. How gold interprets these columns — including the
nickname-token → `tax_wrapper` inference and config overrides — is
owned by the wealthdb Schwab adapter — see
[the adapter doc](../../wealthdb/docs/adapters/schwab.md).

**Do not** invent a structured-field column on silver to hold a
derived wrapper. Silver's contract is "what the source said"; the
source said nothing structured here.

## 5. What silver deliberately omits

- **Bitemporal model.** See §4.3.
- **Audit trail of pre-amendment events.** Window-DELETE-then-INSERT
  loses prior versions of an event. Accepted for personal scale.
- **Secondary indexes for cross-cuts.** See §4.5.
- **Foreign keys between silver tables.** A loader that processes
  artefacts in a different order should still succeed. The logical
  relationships (positions.account_external_id → accounts.account_external_id)
  are inviolate semantically but unenforced structurally.
- **Multi-tenancy.** One silver DB per Schwab developer-app. A
  multi-tenant deployment would switch to a server DB (Postgres) and
  one schema per tenant rather than packing them into one SQLite file.
- **Computed/derived columns.** No `value_in_usd`, no `is_settled`.
  Those belong to gold. Silver stores what the source said; gold
  computes derivatives.

## 6. Gold-layer expectations

How gold reads and projects this silver is owned by the wealthdb
Schwab adapter — see [the adapter doc](../../wealthdb/docs/adapters/schwab.md).
The implications that bear on *silver schema design* here:

- **Silver schema is a public contract.** Bumping a silver schema
  version may require updating the corresponding wealthdb adapter.
  Communicate breaking changes.
- **Promote anything the adapter needs as a filter or join key.**
  If gold ends up doing `json_extract(...)` on every row during
  silver→gold transforms, that's a sign the column should be
  promoted.
- **Don't over-design for gold.** Adapters can handle reasonable
  joins, type coercion, and JSON extraction. Don't materialize
  "convenience columns" silver doesn't otherwise need.

## 7. Notes for other-bank backends

Reusable wholesale:
- The monotemporal source-time model.
- Snapshot vs event archetype distinction.
- One transaction per dump load, dump_runs as last write.
- SQLite + JSON1 with semi-relational tables.
- Composite PK starting with `snapshot_at` on snapshot tables.
- Window-DELETE-then-INSERT for event tables.
- `migrations/NNNN_*.sql` versioning.
- Per-request noise stripping in the loader.

Will need adaptation:
- **Auth ceremony.** Schwab is OAuth-with-7-day-refresh; UBS is RSA
  keypair. `login.py` will look very different. Some banks may not
  even need a separate auth tool.
- **Bronze artefact layout.** Schwab emits homogeneous JSON; UBS
  emits a zip of heterogeneous SWIFT-MT and XML files per order
  type. The "one timestamped directory per run" frame applies, but
  file naming and parsing differ entirely.
- **Identifier scheme.** Schwab gives both plaintext and hash; UBS
  gives plaintext account numbers and a relationship dimension.
  Each silver picks its own `<thing>_external_id` convention.
- **Per-asset-class shape divergence.** Schwab returns one position
  shape, one transaction shape (with `transferItems[]` for legs).
  UBS returns separate XML files per asset class (FX, options,
  metals, etc.) with different fields each. UBS silver may need
  more tables, or a `kind`-discriminated column in fewer tables, or
  a per-asset-class JSON shape in `payload` — pick after looking at
  real bronze samples.

A new collector's build order is the phased playbook in
[NEW-COLLECTOR-PROMPT.md](../../NEW-COLLECTOR-PROMPT.md#the-kickoff-prompt-template). Its
core rule holds here too: inspect real bronze before designing silver.
