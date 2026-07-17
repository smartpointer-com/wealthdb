# angellist

A read-only collector for the [AngelList](https://angellist.com) venture
investor portal — the **limited-partner** book of SPVs and fund
deals: per-vehicle commitment, capital called (contributed), invested,
distributions (realized), fair value, plus portfolio totals (IRR / TVPI /
DPI) and unfunded commitments.

AngelList's public API (`docs.angellist.com`) is the **fund-admin / GP**
surface, not an LP surface, and the LP web login is gated by an invisible
Turnstile/reCAPTCHA challenge that **Camoufox cannot pass** (the
automation fingerprint is flagged, not the IP). So this collector uses a
**bring-your-own-cookie** flow: a one-time by-hand login in a genuine
Firefox over VNC clears the challenge; the collector lifts that session
and drives the venture GraphQL API with it. See [DESIGN.md](DESIGN.md)
for the full story.

Part of the **wealthdb** suite — see
[the architecture overview](../../DESIGN.md) and
[collectors/README.md](../README.md).

## Status

Implemented and working end-to-end (`login` → `download` → `load` →
queryable silver).

| Verb | Status | Notes |
| --- | --- | --- |
| `login` | implemented | The auth path. If the saved profile still holds a valid session (cookie unexpired **and** confirmed live by a quick headless server probe — a cookie can be unexpired yet stale), the cookie is lifted and it exits immediately — no VNC. Otherwise: stock Mozilla Firefox under VNC; the login happens by hand (clears the anti-bot challenge); on close, the session cookie is lifted to `~/.secrets/angellist-cookies.json`. ~monthly (session ≈27 days). No unattended login (the SPA is bot-walled). `--check` probes only; `--fresh` re-logs in. Any K-1 / financial docs downloaded during the session save to `$XDG_DATA_HOME/wealthdb/angellist/angellist-documents/`. |
| `download`  | implemented | Headless Camoufox with the injected cookie drives the venture SPA and captures its GraphQL (positions, commitments, the funding-account ledger) + downloads tax documents. Browser-based because `/venture/graphql` needs a JS-signed `x-al-gql` header. Read-only. `--no-documents` skips the tax-document fetches (the run's dominant cost); the GraphQL captures and `run.json` are still written. |
| `load`      | implemented | Parses bronze `captures.jsonl` → SQLite silver: `offerings` (immutable identity) + `position_snapshots` (event-sourced valuation timeline) / `vehicles` / `portfolio_summary` / `portfolio_timeseries` / `commitments` / `funding_accounts` + `funding_transactions` (dated cash ledger); and parses K-1 CSVs in `angellist-documents/` → `k1_capital_accounts` / `tax_documents`. |
| `explore`   | implemented | Camoufox + VNC discovery harness (HAR + trace + click log, `--cookies`, `--dump-links`). Kept for re-discovery. |
| `prune`     | implemented | Reclaims bronze disk: deletes whole non-complete dumps (a crashed / interrupted `download`), and strips `screenshots/` (the `download --debug` captures) from complete dumps. A complete dump's load inputs are left intact. Runs host-side; `--dry-run` previews. |

Gold side is wired: the `wealthdb/internal/silver/angellist/` adapter
projects this silver into the canonical model and is registered with the
gold engine — see [DESIGN.md §"Gold mapping"](DESIGN.md). Enabling the
source in a run is then a `wealthdb.cfg` `silver_sources` entry.

## Quick start

```sh
# 1. Build the image (stock Firefox + Camoufox base; ~1-2 min on warm cache).
./angellist build

# 2. Lift a session. If the saved profile is still logged in, this exits at
#    once with the cookie refreshed. Otherwise it opens stock Firefox under
#    VNC; connect, log in (+2FA), confirm you reach your portfolio, then CLOSE
#    Firefox — the cookie is extracted automatically. Repeat ~monthly.
./angellist login
#   (already valid) login: existing AngelList session still valid — cookie lifted
#   (fresh login)   login: VNC ready on 127.0.0.1:<port>  (password at handoff)
#                   On this host, connect directly:  open vnc://localhost:<port>

# 3. Pull a fresh bronze dump (headless; ~15s; read-only GraphQL).
./angellist download

# 4. Ingest into the SQLite silver.
./angellist load
```

Iterate / probe without a fresh login:

```sh
./angellist download --dry-run    # navigate + capture, write no bronze
./angellist load --force          # delete silver + rebuild from all bronze
```

Override host mounts via env: `ANGELLIST_SECRETS_DIR`,
`ANGELLIST_DATA_DIR`, `ANGELLIST_DEBUG_DIR`.

### Reclaiming disk

```sh
./angellist prune --dry-run   # print the deletion plan, delete nothing
./angellist prune             # delete it
```

`prune` removes whole non-complete run dirs across the bronze tree: a
`download` that crashed before writing a terminal `run.json` (its
manifest carries `status: "in-progress"`, or is absent entirely). Because
`load` ingests any run dir that holds a `captures.jsonl` regardless of
manifest, such a partial dir would otherwise keep seeding silver; deleting
it surfaces on the next `load --force` rebuild. From a *complete* dump it
reclaims one thing: `screenshots/`, the per-route DOM + screenshot
captures `download --debug` writes; `load` reads only `captures.jsonl` and
`run.json`, so the dump's inputs are left byte-identical. (The `explore`
verb's diagnostics — HAR, Playwright trace, click log — are separate and
land under `/debug`, outside bronze.) The K-1/PDF documents at
`angellist-documents/` and the silver `angellist.db` sit at the bronze
root, not inside a run dir, so they are never touched. Runs host-side like
`load`, and an in-flight guard (`--min-age-hours`, default 1, keyed on
recent write activity) keeps it from removing a download that is still
running.

## Read-only & PII

See [CLAUDE.md](CLAUDE.md). The collector only navigates the venture LP
read surfaces and captures the GraphQL the SPA fetches — it never clicks
an invest/commit/fund/e-sign/settings control and stays off any
lead/admin surface. The data (incl. K-1-adjacent details and, on the
commitments surface, bank/wire instructions) is highly sensitive: it
lives only under `$XDG_DATA_HOME/wealthdb/angellist/` and `~/.secrets/`, never the
repo. Slugs/IDs are derived at runtime, never hardcoded.
