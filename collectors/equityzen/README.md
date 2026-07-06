# equityzen

A read-only collector for [equityzen.com](https://equityzen.com), the
pre-IPO secondary marketplace. Ingests a **buyer's** own holdings — the
single-company offerings they've invested in, each structured as a
**special-purpose vehicle (SPV)** that holds the underlying pre-IPO
shares — with per-offering basis, current fair-market value, share count,
and status, plus the purchase / distribution cash-flow ledger and tax
documents (K-1s, capital-account statements).

This is private, illiquid, non-public-market equity: no ticker, no
quotable price, an SPV/partnership wrapper. Same data shape as the
[`angellist`](../angellist/) LP collector and a sibling of
[`carta`](../carta/). See [DESIGN.md](DESIGN.md) for the full path
investigation, the observed GraphQL API surface, and the bronze → silver →
gold story.

EquityZen publishes no credentialed API for individual investors (the
"public API" on its status page is just uptime monitoring). But its portal
SPA is backed by a clean, buyer-scoped GraphQL API
(`POST /api/graphql/`) authenticated by the ordinary login session cookie.
So — like the other portal-only sources — this collector replays the
browser flow (TOTP login, React SPA) via Camoufox and drives the SPA's own
read operations.

Part of the **wealthdb** suite — see
[the architecture overview](../../ARCHITECTURE.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for shared collector
conventions.

## Status

**Implemented.** All four verbs work end-to-end against the live portal /
real bronze, and the **gold adapter** is in place
(`wealthdb/internal/silver/equityzen/` + gold migration `0015`). What
remains to fully wire it into the suite is operator config: a Makefile
target and a `wealthdb.cfg` `silver_sources` entry.

| Verb | Status | Notes |
| --- | --- | --- |
| `explore`  | **implemented** | Camoufox + VNC discovery harness (HAR + crash-safe network log + trace + click log). Pre-fills the login form (login-path-gated, fill-once + clear/verify, Firefox password-manager disabled; never submits). Already run; re-run only when a selector changes. |
| `login`    | **implemented** | Headless CLI flow: headed Camoufox under Xvfb (no VNC), email/password (`submitLogIn`) + stdin TOTP prompt → Submit-button click (`loginTotp`), persistent profile. Renews silently if the session is still valid. Verified end-to-end. |
| `download` | **implemented** | Headed Camoufox under Xvfb. Captures `getBuyerInvestments` per stage (Ongoing/Closed/Exited tabs) + `getMyInvestmentDetails` per offering → bronze JSON. `--dry-run` (read-only) verified; `--documents` fetches each offering's document PDF blobs (capital-account statements, K-1s) via the session. |
| `load`     | **implemented** | SQLite silver (`migrations/0001_initial.sql`): offerings (immutable) / positions (event-sourced) / cash_flows / tax_documents / capital_account_statements / k1_documents. Parses statement + K-1 PDFs (`statements.py`, `pdftotext`); injects fund NAVs as positions revaluation events. Idempotent (`--force` re-loads). |
| `prune`    | **implemented** | Reclaims bronze disk via the shared `collectorkit.prune` engine. Deletes non-complete dumps (crashed downloads with no terminal `run.json`); keeps every load input. No bronze-resident debug artefacts exist, so that is the sole target. `--dry-run` previews; `--min-age-hours` guards an in-flight download. |

The gold adapter projects this silver into the canonical
`accounts` / `instruments` / `positions` / `transactions` tables; see
DESIGN.md §6, and is registered with the gold engine. Enabling the
source in a run is then a `wealthdb.cfg` `silver_sources` entry
(operator config).

## Quick start

```sh
# 1. Build the image (builds on the shared base-camoufox image).
./equityzen build

# 2. Drop credentials into the env file. chmod 0600.
#    cat > ~/.secrets/equityzen.env <<'EOF'
#    EQUITYZEN_EMAIL='your-equityzen-email'      # alias: EQUITYZEN_USERNAME
#    EQUITYZEN_PASSWORD='your-equityzen-password'  # single-quote it
#    EOF

# 3. (One-time, dev) Discovery: drive the live buyer UI over VNC and
#    record HAR + trace + clicks so login/download can be written.
./equityzen explore --fresh

# 4. Mint the session. Prompts for the TOTP code on stdin once; the
#    Camoufox profile keeps later runs from re-prompting (session lives
#    until an explicit logout).
./equityzen login

# 5. Pull a fresh bronze dump (offerings + positions + cash flows). Add
#    --documents to also fetch the document PDFs (capital-account
#    statements, K-1s) that load parses for fund NAVs + tax-basis capital.
./equityzen download --documents

# 6. Ingest bronze into the SQLite silver.
./equityzen load
```

Probe / iterate without firing a 2FA push or writing data:

```sh
./equityzen login --check          # exits 0 if session valid, 1 if not
./equityzen download --dry-run     # walks the GraphQL reads, exports nothing
./equityzen load --force           # re-ingest snapshots already in dump_runs
```

Override host mounts via env: `EQUITYZEN_SECRETS_DIR`,
`EQUITYZEN_DATA_DIR`, `EQUITYZEN_DEBUG_DIR`. EquityZen marks update on
new-round cadence (FMV is not daily), so a weekly/monthly run is plenty;
same-day re-runs add nothing.

### Reclaiming disk

```sh
./equityzen prune --dry-run   # print the deletion plan, delete nothing
./equityzen prune             # delete it
```

`prune` removes whole non-complete run dirs across the bronze tree — a
walk that crashed before writing its terminal `run.json` (its
`status` is `"in-progress"`, or the manifest is absent entirely).
`load` would otherwise keep re-ingesting the partial `investments.json`
/ `offerings/` such a dir holds; deleting one surfaces on the next
`load --force` rebuild. There is nothing else to reclaim: `download`
writes **no** bronze-resident debug artefact (its diagnostics live
externally — `login --debug-dir` screenshots and the `explore` verb's
`/debug` HAR/trace/click log), so `debug_subdirs` is empty and a
complete dump has nothing pruned. Every load input is therefore
untouched — the `investments.json`, `offerings/*/detail.json`, and the
re-downloaded `documents/<deal-slug>/*.pdf` / `.zip` blobs of a complete
dump are structurally out of scope, so silver stays reproducible.
An in-flight guard (`--min-age-hours`, default 1, keyed on recent write
activity) keeps it from removing a download that is still running.
Unlike a host-venv collector, `prune` runs inside the container (like
`load`), so the wrapper's single-writer guard refuses it while a
`login` / `download` is live — run it between refreshes.

## Read-only

See [CLAUDE.md](CLAUDE.md). EquityZen is a **live marketplace**: the
portal exposes order-placement, Express-Deal sell, reserve/IOI, funding,
and e-sign surfaces — all out of scope. This toolkit only reads the
buyer's own holdings + documents, and issues only read GraphQL queries
plus the two auth mutations. Pre-IPO company names, SPV/fund names, share
counts, basis / FMV figures, and K-1 contents are PII and never enter the
repo.
