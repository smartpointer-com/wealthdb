# equityzen

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to EquityZen with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
EquityZen offers no read-only sub-scope, so nothing but this codebase's
own discipline restricts the session to reading. If malicious code were
ever introduced into this repository, its dependency chain, or the
container images it runs, it could act on your accounts with your full
authority and cause **irreversible financial damage, up to the total loss
of the assets reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach EquityZen's terms of
service; verifying that your use is permitted is likewise your
responsibility.

**No warranty; no liability.** This software is provided “AS IS”, without
warranty of any kind, express or implied, including but not limited to the
implied warranties of merchantability, fitness for a particular purpose,
title, and non-infringement. To the maximum extent permitted by applicable
law, **SmartPointer AG and the contributors accept no responsibility for,
and shall not be liable for, any claim, damages, or other liability** —
whether in an action of contract, tort, or otherwise — arising from, out
of, or in connection with this software or its use, including without
limitation unauthorized or erroneous transactions, loss of funds or other
assets, credential or data compromise, account suspension or termination,
and any direct, indirect, incidental, special, consequential, or punitive
damages. Your use is entirely at your own risk. See
[LICENSE](../../LICENSE) for the governing terms. This software is not
affiliated with, endorsed by, or sponsored by EquityZen or any other
financial institution; nothing in this repository is financial, legal, or
tax advice.

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
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for shared collector
conventions.

## Status

**Implemented end-to-end.** Every verb is built; `explore` / `login` /
`download` / `load` are exercised against the live portal and real bronze
(`prune` is a pure file walk). The **gold adapter**
(`wealthdb/internal/silver/equityzen/`) is registered with the gold engine
and gold migration `0015` admits the source. Build, test, and adapter
registration are already wired — the collector is auto-discovered by the
repo Makefile — so the one remaining step to pull EquityZen into a gold run
is a `wealthdb.cfg` `silver_sources` entry pointing at the silver DB.

| Verb | Status | Notes |
| --- | --- | --- |
| `explore`  | **implemented** | Camoufox + VNC discovery harness (redacted HAR + crash-safe network log + click log, opt-in `--trace`). Pre-fills the login form (login-path-gated, fill-once + clear/verify, Firefox password-manager disabled; never submits). Already run; re-run only when a selector changes. |
| `login`    | **implemented** | Headless CLI flow: headed Camoufox under Xvfb (no VNC), email/password (`submitLogIn`) + stdin TOTP prompt → Submit-button click (`loginTotp`), persistent profile. Renews silently if the session is still valid. Verified end-to-end. |
| `download` | **implemented** | Headed Camoufox under Xvfb. Captures `getBuyerInvestments` per stage (Ongoing/Closed/Exited tabs) + `getMyInvestmentDetails` per offering → bronze JSON. `--dry-run` (read-only) verified; each offering's document PDF blobs (capital-account statements, K-1s) are fetched via the session by default, with a `--no-documents` opt-out. **Download-avoidant** (`collectorkit.docdedup`), chosen per document class: parsed / restatement-prone documents (statements, K-1s, reports) are always fetched and content-compared (a restated one is kept, an unchanged one hardlinked for disk reclaim), while executed-once legal / offering documents (an explicit, curated allow-list) are hardlinked in rather than re-fetched. `--documents-force` bypasses it. |
| `load`     | **implemented** | SQLite silver (`migrations/0001_initial.sql`): offerings (immutable) / positions (event-sourced) / cash_flows / tax_documents / capital_account_statements / k1_documents. Parses statement + K-1 PDFs (`statements.py`, `pdftotext`); injects fund NAVs as positions revaluation events. Idempotent (`--force` deletes silver + rebuilds from bronze). |
| `prune`    | **implemented** | Reclaims bronze disk via the shared `collectorkit.prune` engine. Deletes non-complete dumps (crashed downloads with no terminal `run.json`) and strips `screenshots/` (the `download --debug` captures) from complete dumps; keeps every load input. `--dry-run` previews; `--min-age-hours` guards an in-flight download. |

The gold adapter is registered with the gold engine and projects this
silver into the canonical `accounts` / `instruments` / `positions` /
`transactions` tables (see DESIGN.md §6). Enabling the source in a gold
run is then just the `wealthdb.cfg` `silver_sources` entry above.

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
#    record a redacted HAR + network/click logs so login/download can
#    be written (--trace adds a Playwright trace, which nothing redacts).
./equityzen explore --fresh

# 4. Mint the session. Prompts for the TOTP code on stdin once; the
#    Camoufox profile keeps later runs from re-prompting (session lives
#    until an explicit logout).
./equityzen login

# 5. Pull a fresh bronze dump (offerings + positions + cash flows). The
#    document PDFs (capital-account statements, K-1s) that load parses
#    for fund NAVs + tax-basis capital are fetched by default; pass
#    --no-documents to skip them. Re-runs are download-avoidant:
#    executed-once legal/offering docs are hardlinked from a prior bronze
#    run instead of re-fetched; parsed / restatement-prone docs
#    (statements, K-1s, reports) are always re-fetched and
#    content-compared. --documents-force re-fetches all.
./equityzen download

# 6. Ingest bronze into the SQLite silver.
./equityzen load
```

Probe / iterate without firing a 2FA push or writing data:

```sh
./equityzen login --check          # exits 0 if session valid, 1 if not
./equityzen download --dry-run     # walks the GraphQL reads, exports nothing
./equityzen load --force           # delete silver + rebuild from all bronze
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
`load --force` rebuild. From a complete dump it reclaims one thing:
`screenshots/`, the DOM + screenshot captures `download --debug` writes
(the portfolio list page, and any offering whose detail query never fired).
`load` never reads them. (The other diagnostics live outside bronze —
`login --screenshot-dir` screenshots and the `explore` verb's `/debug`
HAR/trace/click log.) Every load input is therefore
untouched — the `investments.json`, `offerings/*/detail.json`, and the
`documents/<deal-slug>/*.pdf` / `.zip` blobs of a complete dump (each either
freshly fetched or hardlinked from a prior run by `collectorkit.docdedup`)
are structurally out of scope, so silver stays reproducible.
An in-flight guard (`--min-age-hours`, default 1, keyed on recent write
activity) keeps it from removing a download that is still running.
`prune` runs host-side (a pure file walk needs none of the image's
deps), so — unlike `download` / `load` — it bypasses the wrapper's
single-writer guard and can reclaim disk while a `login` / `download`
container is mid-flight; the container `entrypoint.sh` keeps a `prune)`
arm too, for a direct `docker run`.

## Read-only

See [CLAUDE.md](CLAUDE.md). EquityZen is a **live marketplace**: the
portal exposes order-placement, Express-Deal sell, reserve/IOI, funding,
and e-sign surfaces — all out of scope. This toolkit only reads the
buyer's own holdings + documents, and issues only read GraphQL queries
plus the two auth mutations. Pre-IPO company names, SPV/fund names, share
counts, basis / FMV figures, and K-1 contents are PII and never enter the
repo.
