# wealthdb: Implementation plan

Companion to [DESIGN.md](DESIGN.md). DESIGN.md covers the
architecture, schema, and external contracts; this document
covers how the Go code is laid out, how packages depend on each
other, and how tests run. Nothing here changes the
user-observable behaviour specified in DESIGN.md.

## 1. Repository layout

```
wealthdb/
├── go.mod                              module github.com/ptu/wealthdb
├── go.sum
├── cmd/
│   └── wealthdb/
│       ├── main.go                     entry: build dispatcher, blank-imports adapters
│       ├── flags.go                    top-level flag parsing (-c, -r, -v)
│       ├── dispatch.go                 subcommand routing + exit-code mapping
│       └── cmd_{config,init,load,reset,positions,status,snapshots,help}.go
├── internal/
│   ├── canonical/
│   │   ├── types.go                    AccountChange, InstrumentChange, PositionChange,
│   │   │                               CashBalanceChange, FxRateChange, TransactionChange,
│   │   │                               Status, Window
│   │   ├── enums.go                    AssetClass, AccountKind, TxKind, BalanceKind, FxMode
│   │   └── decimal.go                  thin wrapper over shopspring/decimal
│   ├── silver/
│   │   ├── adapter.go                  Adapter, Connection, SnapshotStream, TransactionStream
│   │   ├── registry.go                 Register(name, factory) / Get(name)
│   │   ├── schwab/
│   │   │   ├── adapter.go              init() registers; opens *sql.DB on the silver SQLite
│   │   │   ├── status.go               Status() + ChangeWindow()
│   │   │   ├── snapshots.go            Snapshots() iterator
│   │   │   ├── transactions.go         Transactions() iterator
│   │   │   ├── kindmap.go              TRADE → buy/sell etc. (table-driven)
│   │   │   ├── classmap.go             EQUITY/ETF/... → AssetClass
│   │   │   ├── ids.go                  account hash, instrument key handling
│   │   │   └── *_test.go               fixture SQLite, hand-rolled rows
│   │   ├── ubs/
│   │   │   ├── adapter.go, status.go, snapshots.go, transactions.go
│   │   │   ├── kindmap.go              events.kind dispatch
│   │   │   ├── classmap.go             SDFI Tp → AssetClass
│   │   │   ├── narrative.go            MT940 :86: prefix parser
│   │   │   └── *_test.go
│   │   ├── swissquote/
│   │   │   ├── adapter.go, status.go, snapshots.go, transactions.go
│   │   │   ├── kindmap.go              transaction_type dispatch
│   │   │   ├── classmap.go             XLS section header → AssetClass
│   │   │   ├── txhash.go               synthetic transaction_external_id
│   │   │   └── *_test.go
│   │   └── auto/
│   │       └── detect.go               kind="auto" sniffing via sqlite_master
│   ├── gold/
│   │   ├── migrations/                 SQL lives here (Go embed needs local path)
│   │   │   ├── 0001_initial.sql
│   │   │   ├── 0020_fx_views.sql       fx_norm / fx_daily currency-conversion views
│   │   │   └── 0021_report_macros.sql  report_* table macros (single source of truth)
│   │   ├── schema.go                   //go:embed migrations/*.sql; Migrate()
│   │   ├── open.go                     Open(path, mode); maps to DuckDB access_mode
│   │   ├── writer.go                   inserts/upserts per canonical type, batched
│   │   ├── load.go                     the DESIGN.md §8.1 orchestration
│   │   ├── reset.go
│   │   ├── status.go                   gold-side queries for `wealthdb status`
│   │   ├── snapshots.go                `wealthdb snapshots` query
│   │   ├── positions.go                as-of query
│   │   ├── fxpriority.go               SetFxPriorities: stamp silver_sources.fx_priority on load
│   │   └── *_test.go                   in-memory DuckDB
│   ├── config/
│   │   ├── config.go                   types + JSON load/save
│   │   ├── expand.go                   ~ / $HOME expansion
│   │   ├── validate.go                 schema validation, ID uniqueness, ISO 4217
│   │   └── *_test.go
│   ├── pathmode/
│   │   ├── pathmode.go                 DESIGN.md §4.10 detection logic
│   │   └── pathmode_test.go
│   ├── wizard/
│   │   ├── wizard.go                   `wealthdb config` interactive flow
│   │   ├── prompt.go                   prompt helpers over io.Reader/Writer (mockable)
│   │   └── wizard_test.go              scripted stdin
│   ├── output/
│   │   ├── format.go                   Format enum + dispatch
│   │   ├── table.go                    postgres-style ASCII
│   │   ├── csv.go                      csv + csv_plain
│   │   ├── json.go
│   │   └── *_test.go                   golden-file
│   ├── errs/
│   │   └── errs.go                     ExitError{Code, Err}; dispatcher maps to exit code
│   └── version/
│       └── version.go                  populated via -ldflags at build
└── (top-level files per DESIGN.md §11: Dockerfile, wealthdb, wealthdb-test, README.md, ...)
```

## 2. Dependency direction

Strictly one-way. Anything below can import anything above:

```
canonical                          (zero deps)
   ↑
silver  (interface only)           imports canonical
   ↑
silver/{schwab,ubs,swissquote}     imports silver + canonical
   ↑
gold                               imports canonical (NOT silver)
   ↑
cmd/wealthdb/cmd_*.go                imports gold + silver + config + wizard + output + errs
```

Notable: **`gold` does not import `silver`.** Gold's writer
takes canonical `*Change` values, not adapter handles. The load
orchestration that bridges silver→gold lives in
`cmd/wealthdb/cmd_load.go` (or a thin `internal/loader/` package if
it grows). This matches the DESIGN.md §6.1 "plugins never touch
the gold database" rule from the other side: gold doesn't need
to know plugins exist.

## 3. Key implementation decisions

### 3.1 `canonical/` is a separate package from `silver/`

DESIGN.md §6.2's interface sketch put `*Change` types inside
`silver/`. They actually describe the canonical row shape, not
the adapter contract — they belong in their own zero-dep package.
Effects:
- Adapter packages depend on `canonical` for types and `silver`
  for the interface.
- The gold writer depends only on `canonical`, not on `silver`.
- Tests for canonical types pull in no plugin infrastructure.

Trade-off: `silver.SnapshotBatch` now holds
`canonical.PositionChange`. Marginal verbosity at the call site.
Worth it for the dependency hygiene.

### 3.2 SQL migrations live under `internal/gold/migrations/`

Go's `//go:embed` can't traverse up the file tree. Migrations
sit next to `schema.go` so the embed directive is local. DESIGN.md
§11 sketched migrations at repo root; this is the small revision.
Engineers will still find them via the obvious search path.

### 3.3 Exit codes via typed error

Each subcommand returns `error`. The dispatcher does:

```go
var ex *errs.ExitError
if errors.As(err, &ex) { os.Exit(ex.Code) }
fmt.Fprintln(os.Stderr, err); os.Exit(1)
```

Subcommand code wraps errors with `errs.ExitError{Code: N, Err: ...}`
when the DESIGN.md §4.10 table demands a specific code (2 through
6), plain `fmt.Errorf(...)` otherwise. Avoids threading an int
through every function call.

### 3.4 `pathmode/` as a small standalone package

~50 lines, but the kind of code that's easy to get subtly wrong
(directory-vs-file `unix.Access` ordering, `os.Stat` races) and
deserves focused tests. Lives separately so cmd code can compute
the mode before opening any DB.

### 3.5 Pull-model iterators over `*sql.Rows`

Each adapter's `Snapshots()` and `Transactions()` returns a
struct holding open `*sql.Rows`. `Next()` batches up to ~256 rows
and returns a `SnapshotBatch`. `Close()` closes the underlying
rows. Natural Go shape; avoids buffering whole windows in memory.

### 3.6 Decimal at the boundary

DuckDB DECIMAL ↔ Go: the `go-duckdb` driver surfaces these as
`*big.Rat` or `string` depending on options. Canonical types use
`shopspring/decimal.Decimal` (well-known, exact, JSON-friendly);
conversion happens at the driver boundary. Bare `float64` is
forbidden by DESIGN.md §7.1.

### 3.7 Adapter registration via blank imports — only in main

```go
import (
    _ "github.com/ptu/wealthdb/internal/silver/schwab"
    _ "github.com/ptu/wealthdb/internal/silver/ubs"
    _ "github.com/ptu/wealthdb/internal/silver/swissquote"
    _ "github.com/ptu/wealthdb/internal/silver/auto"
)
```

Library code never blank-imports. Tests selectively
register/deregister via registry test hooks.

### 3.8 Driver choices

- **SQLite (read silver)**: `modernc.org/sqlite` (pure Go).
  DuckDB already pulls in CGO, so this doesn't simplify the build
  — but it avoids a second SQLite shared library. Happy to swap
  to `mattn/go-sqlite3` if maturity matters more.
- **DuckDB (read/write gold)**: `github.com/duckdb/duckdb-go/v2`
  (CGO; the official Go DuckDB driver, formerly `marcboeker/go-duckdb`).

### 3.9 No public API

Everything under `internal/`. We are an application, not a
library. If a future external consumer needs `canonical/`, move
it then.

### 3.10 What's not in v1

- No Cobra / urfave-cli — `flag` stdlib is enough.
- No `log/slog` configuration beyond the default text handler.
- No generated mocks (`gomock`, `mockery`) — hand-roll the small
  fakes that gold tests need.
- No property-based or fuzz tests.

## 4. Testing strategy

### 4.1 Philosophy

Mock at seams you don't own; use the real thing for everything
else. SQLite and DuckDB are embeddable, fast, and hermetic — no
reason to mock them, and doing so would paper over the SQL bugs
we most want to catch.

- **Adapter tests** use real silver SQLite fixtures (synthetic,
  small).
- **Gold tests** use real in-memory DuckDB (`:memory:`).
- **Load-orchestration tests** use both ends real: temp silver
  SQLite into fresh in-memory DuckDB.
- **Mocks** reserved for time, filesystem permission checks,
  stdin/stdout (wizard), and the silver registry (so gold tests
  can install scripted fake adapters).

### 4.2 Pyramid

```
                 ┌─────────────────────────────────────────┐
                 │  E2E via in-process dispatch            │  thin top
                 │  (Run(args, stdin, stdout, stderr)      │
                 │  with golden-file output assertions)    │
                 └─────────────────────────────────────────┘
       ┌────────────────────────────────────────────────────────┐
       │  Integration: load orchestration                       │  load.go,
       │  silver SQLite fixture → in-memory DuckDB,             │  reset.go,
       │  multi-cycle: first load, incremental, amend window,   │  cross-cuts
       │  silver-backwards, reset, partial-failure              │
       └────────────────────────────────────────────────────────┘
 ┌──────────────────────────────────────────────────────────────────────┐
 │  Per-package unit tests                                              │
 │   - silver/<bank>/*_test.go    real SQLite fixtures, table-driven    │
 │   - gold/*_test.go             real :memory: DuckDB                  │
 │   - canonical, output, config, pathmode, wizard, errs (focused)     │
 └──────────────────────────────────────────────────────────────────────┘
```

Integration tests live in-path under `go test ./...` — no build
tags.

### 4.3 Adapter tests

Each bank package owns `testdata/silver/<scenario>.sql` scripts
that build a fixture SQLite at the latest migration. Helper:

```go
db := newSilverFixture(t, "schwab", "two_snapshots_with_amend")
adapter, _ := silver.Get("schwab")
conn, _ := adapter.Open(ctx, db.Path)
```

Coverage:
- `Status` — correct oldest/latest timestamps; `-1` sentinel when
  the silver is empty.
- `ChangeWindow(-1)` covers everything; `ChangeWindow(t)` covers
  only strictly newer.
- `Snapshots(window)` — right `*Change` counts per snapshot;
  routing rules (Schwab CASH_EQUIVALENT → CashBalanceChange);
  unknown `assetType` lands as `other` with raw type in payload.
- `Transactions(window)` — right `kind` mapping per row.
- `kindmap` / `classmap` / `narrative.go` get their own pure
  table-driven tests with no SQLite needed.

**PII discipline.** Fixture rows use synthetic identifiers
(`ACC0001`, ISIN `XX0000000001`, amounts like `1234.56`). Same
standard as the sibling silver repos. Fixture files are reviewed
for PII before commit.

### 4.4 Gold tests

In-memory DuckDB per test:

```go
gold := goldtest.Open(t)              // :memory:, migrations applied
gold.Writer().UpsertAccount(...)
gold.Writer().InsertPosition(...)
got := gold.PositionsAsOf(asOf, "USD", FxModeHistoric)
```

Coverage:
- Schema migrations apply; idempotent on re-run.
- Writer upsert merges by natural PK; `last_seen_at` advances but
  never retreats (the DESIGN.md §8.4 guard).
- Positions as-of query with multiple silvers at independent
  latest snapshots.
- FX conversion via the `fx_norm` / `fx_daily` views: flat nearest
  rate at or before the target day (no interpolation); direct and
  reciprocal both resolve from `fx_norm`; CHF-then-USD triangulation
  for crosses; a missing rate yields NULL (empty cell, not an error).
  Golden table-driven (target_day × pair × mode → expected rate).
- Reset: FK delete order correct; subsequent re-load works with
  watermark reset to `-1`.

### 4.5 Load-orchestration tests

Fixtures:

```
testdata/scenarios/
├── first_load_empty/                       silver + expected gold-state JSON
├── first_load_one_snapshot/
├── incremental_two_snapshots/
├── amend_window_drops_phantom_event/
├── silver_went_backwards/
├── multi_source_independent_cursors/
└── partial_failure_rolls_back/
```

Each drives the orchestrator end-to-end and diffs the resulting
gold state against a JSON snapshot. Covers the IVM-shaped
behaviour that's hardest to verify any other way: window-DELETE
removing phantoms, watermark advance, refusal on backwards
motion, per-source independence.

### 4.6 CLI tests

In-process. `cmd/wealthdb/dispatch.go` exposes
`Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int`;
`main()` is a one-liner around it. Tests build `args`, route
stdio to buffers, assert on exit code and output. Golden files
under `testdata/golden/` catch formatter regressions.

Wizard tests script stdin lines and verify the resulting JSON
config. `prompt.go` works against `io.Reader`/`Writer`, so no
real TTY is needed.

### 4.7 What gets mocked

| Seam | Why | How |
| --- | --- | --- |
| `time.Now()` | Audit timestamps would make snapshot comparisons fragile | Inject a `Clock` interface; default `realClock{}`, tests use `fixedClock{t}` |
| Filesystem permissions for `pathmode` | Need to test "read-only mount" without `chmod` in CI | Wrap `unix.Access` behind a small interface |
| Stdin/stdout for `wizard` | Deterministic interactive driving | Already via `io.Reader`/`Writer` |
| `silver.Connection` in gold-only tests | Gold tests shouldn't depend on an adapter | 30-line in-test fake yielding a scripted sequence of batches |

No mocked DuckDB, no mocked SQLite, no mocked `*sql.Tx`, no
generated-mock framework.

## 5. Running tests

Tests run **inside the container**; the host stays clean of Go
toolchain and DB headers. The companion wrapper `./wealthdb-test`
(sibling to `./wealthdb`) routes `go test` into the container with
argv passed through verbatim:

```sh
./wealthdb-test ./...                                 # full suite
./wealthdb-test ./internal/gold/...                   # one package tree
./wealthdb-test ./internal/gold/ -run TestPositionsAsOf
./wealthdb-test ./internal/gold/ -run TestFx -v -count=1
./wealthdb-test ./... -race
```

Exit code is `go test`'s exit code.

### 5.1 Wrapper shape

```sh
#!/bin/sh
# wealthdb-test — run `go test` inside the wealthdb container
set -eu
REPO="$(cd "$(dirname "$0")" && pwd)"

mkdir -p "$HOME/.cache/wealthdb-test/go-build" \
         "$HOME/.cache/wealthdb-test/go-mod"

docker run --rm \
    -u "$(id -u):$(id -g)" \
    -e HOME="$HOME" \
    -e GOCACHE="$HOME/.cache/wealthdb-test/go-build" \
    -e GOMODCACHE="$HOME/.cache/wealthdb-test/go-mod" \
    -v "$REPO:$REPO" \
    -v "$HOME/.cache/wealthdb-test:$HOME/.cache/wealthdb-test" \
    -w "$REPO" \
    --entrypoint go \
    wealthdb:latest \
    test "$@"
```

### 5.2 One image, one tag

Single-stage Dockerfile produces `wealthdb:latest`. The image
carries the Go toolchain, gcc/g++ (for CGO), and the built
binary. Both `./wealthdb` and `./wealthdb-test` use this same
image; the test wrapper just overrides the entrypoint with
`--entrypoint go`. See DESIGN.md §12.4 for why multi-stage isn't
worth it at this project's scale.

### 5.3 Isolated Go cache

`$HOME/.cache/wealthdb-test/` is dedicated to wealthdb; it
does not share with any host-level `$HOME/.cache/go-build` a
host Go toolchain might use. Trade-off: if you already have the
same modules cached elsewhere, you'll download them once on the
first test run. Flip the env vars in the wrapper if you'd
rather share.

### 5.4 CI shape

- `./wealthdb-test ./...` — full suite, runs in seconds.
- `./wealthdb-test ./... -race` — second pass with race detector.
- No `-tags=slow`; reserved for any future test needing a large
  on-disk DuckDB. Empty at v1.

## 6. Open structural questions

- **Where does the load orchestration live long-term?** Start in
  `cmd/wealthdb/cmd_load.go`. Promote to `internal/loader/` if it
  outgrows ~150 lines or wants tests independent of CLI parsing.
- **Whether to keep `auto/` as a separate adapter or fold into
  `silver/`.** It doesn't implement `Adapter` — it inspects a
  silver DB and returns the *name* of the right adapter. Could
  also live as a helper function in `silver/registry.go`. Defer
  the decision until the first non-trivial detection rule lands.
