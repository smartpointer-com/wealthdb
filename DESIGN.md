# wealthdb: Architecture

Suite-wide view of how the monorepo's parts fit together. For the
deep design of the gold engine see
[wealthdb/docs/DESIGN.md](wealthdb/docs/DESIGN.md); for a given
source's internals see `collectors/<source>/DESIGN.md`.

## The three layers

The suite is a **bronze → silver → gold** pipeline. Each layer has
one owner and a stable contract with the next.

```
                 BRONZE                 SILVER                 GOLD
            (raw, as fetched)    (parsed, source-shaped)  (canonical, cross-bank)

 collectors/schwab-api/   JSON dumps  ─→  schwab-api.db   ─┐
 collectors/schwab-web/   PDF + CSV   ─→  schwab-web.db   ─┤
 collectors/ubs-psn/      MT5xx zips  ─→  ubs-psn.db      ─┤
 collectors/ubs-web/      PDF + CSV   ─→  ubs-web.db      ─┤
 collectors/swissquote/   XLS + PDF   ─→  swissquote.db   ─┤
 collectors/fidelity-web/ CSV + HTML  ─→  fidelity-web.db ─┤   wealthdb load
 collectors/relevate/     JSON        ─→  relevate.db     ─┼─────────────────→  wealthdb.db
 collectors/viac/         JSON + PDF  ─→  viac.db         ─┤   (DuckDB, canonical)
 collectors/cointracking/ CSV + JSON  ─→  cointracking.duckdb (DuckDB) ─┤
 collectors/angellist/    JSON        ─→  angellist.db    ─┤   → positions / transactions
 collectors/carta/        JSON + PDF  ─→  carta.db        ─┤   accounts / portfolios ...
 collectors/equityzen/    JSON + PDF  ─→  equityzen.db    ─┤
 collectors/manual/       CSV         ─→  manual.db       ─┤
 collectors/svb/          PDF stmts   ─→  svb.db          ─┤   (historical; loads via the fidelity adapter)
 collectors/fred/         JSON        ─→  fred.db         ─┘
```

- **Bronze** — exactly what the source returned, untouched. Owned
  by each collector's `download.py`. Lands under
  `$XDG_DATA_HOME/wealthdb/<source>/<UTC-timestamp>/` (XDG data dir;
  `$XDG_DATA_HOME` defaults to `~/.local/share` when unset).
- **Silver** — bronze parsed into a source-shaped SQLite, owned by
  each collector's `load.py`. One DB per source. JSON payloads
  carry through anything not promoted to a column, so source-format
  drift is absorbed here, not at gold.
- **Gold** — one canonical DuckDB, owned by the `wealthdb` engine.
  It reads every silver through a per-source adapter and merges
  them into cross-bank `accounts` / `positions` / `transactions` /
  `cash_balances` / `fx_rates` tables. **Sources only meet at gold.**

Silver is the **input contract** to gold: gold reads silver,
never writes it. Each silver schema is owned by its collector and
versioned with its own migrations.

## Why the split

- **Collectors are independent.** Each authenticates differently
  (OAuth, SFTP key, scraped session + MFA), runs on its own
  cadence, and can break or be rebuilt without touching the others
  or gold. A source outage degrades to "stale silver," not a
  pipeline failure.
- **Gold is provenance-preserving.** Every gold row carries its
  `silver_source_id` and a payload pointer back to the silver
  row(s) it derived from. Nothing is irreversibly transformed.
- **Snapshot-time semantics.** Position queries are as-of a date;
  each source independently contributes its latest snapshot ≤ that
  date (the daily history series resolves that per account instead,
  so a partial collector run carries the accounts it did not touch).
  FX is converted at query time (nearest historic rate), never baked
  in.

## Canonical model (gold)

Gold normalises every source into shared dimensions and facts. The
account taxonomy is **three orthogonal axes** so queries can slice
without conflating them:

- `account_kind` — technical container: brokerage / cash /
  safekeeping / custody / overlay / crypto / mortgage / other
  (with crypto_exchange / crypto_self_custody reserved for a
  future adapter that distinguishes them).
- `tax_wrapper` — tax/regulatory registration: taxable_personal,
  the US IRA family, 401k/403b/457b, 529, coverdell_esa,
  custodial_utma/ugma, trust_*, and the Swiss pillar_3a /
  vested_benefits / pillar_2, plus foundation / other.
- `management_style` — who places trades: self_directed /
  advisory / discretionary / automated.

Adapters populate whatever a source's silver carries; config-side
`account_overrides` fills the rest. A per-source adapter
(`wealthdb/internal/silver/<source>/`) is the only gold-side code
that knows a given silver's shape — it projects silver rows into
canonical change records and is where source-specific quirks are
resolved.

## Where to read more

| Topic | Document |
| --- | --- |
| Gold schema, CLI, plugin contract, load semantics, package layout | [wealthdb/docs/DESIGN.md](wealthdb/docs/DESIGN.md) |
| Returns method (TWR / MWR) and its rationale | [wealthdb/docs/RETURNS-NOTES.md](wealthdb/docs/RETURNS-NOTES.md) |
| Per-source adapter design (gold side) | [wealthdb/docs/adapters/](wealthdb/docs/adapters/) |
| A given source's bronze/silver internals | `collectors/<source>/DESIGN.md` |
| Agent ground rules (shared) | [CLAUDE.md](CLAUDE.md) |
