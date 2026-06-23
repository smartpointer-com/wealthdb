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
**bring-your-own-cookie** flow: you log in once in a genuine Firefox over
VNC, and the collector lifts that session and drives the venture GraphQL
API with it. See [DESIGN.md](DESIGN.md) for the full story.

Part of the **wealthdb** suite — see
[the architecture overview](../../ARCHITECTURE.md) and
[collectors/README.md](../README.md).

## Status

Implemented and working end-to-end (`login` → `download` → `load` →
queryable silver).

| Verb | Status | Notes |
| --- | --- | --- |
| `login` | implemented | The auth path (formerly `byo-login`). If the saved profile still holds a valid session, the cookie is lifted and it exits immediately — no VNC. Otherwise: stock Mozilla Firefox under VNC; you log in by hand (clears the anti-bot challenge); on close, the session cookie is lifted to `~/.secrets/angellist-cookies.json`. ~monthly (session ≈27 days). No unattended login (the SPA is bot-walled). `--check` probes only; `--force` re-logs in. Any K-1 / financial docs you download in the session save to `$XDG_DATA_HOME/wealthdb/angellist/angellist-documents/`. |
| `download`  | implemented | Headless Camoufox with the injected cookie drives the venture SPA and captures its GraphQL (positions, commitments, the funding-account ledger) + downloads tax documents. Browser-based because `/venture/graphql` needs a JS-signed `x-al-gql` header. Read-only. |
| `load`      | implemented | Parses bronze `captures.jsonl` → SQLite silver: `offerings` (immutable identity) + `position_snapshots` (event-sourced valuation timeline) / `vehicles` / `portfolio_summary` / `portfolio_timeseries` / `commitments` / `funding_accounts` + `funding_transactions` (dated cash ledger); and parses K-1 CSVs in `angellist-documents/` → `k1_capital_accounts` / `tax_documents`. |
| `explore`   | implemented | Camoufox + VNC discovery harness (HAR + trace + click log, `--cookies`, `--dump-links`). Kept for re-discovery. |

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
./angellist load --force          # re-load snapshots already in dump_runs
```

Override host mounts via env: `ANGELLIST_SECRETS_DIR`,
`ANGELLIST_DATA_DIR`, `ANGELLIST_DEBUG_DIR`.

## Read-only & PII

See [CLAUDE.md](CLAUDE.md). The collector only navigates the venture LP
read surfaces and captures the GraphQL the SPA fetches — it never clicks
an invest/commit/fund/e-sign/settings control and stays off any
lead/admin surface. The data (incl. K-1-adjacent details and, on the
commitments surface, bank/wire instructions) is highly sensitive: it
lives only under `$XDG_DATA_HOME/wealthdb/angellist/` and `~/.secrets/`, never the
repo. Slugs/IDs are derived at runtime, never hardcoded.
