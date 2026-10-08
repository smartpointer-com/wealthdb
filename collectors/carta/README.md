# carta

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to Carta with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
Carta offers no read-only sub-scope, so nothing but this codebase's own
discipline restricts the session to reading. If malicious code were ever
introduced into this repository, its dependency chain, or the container
images it runs, it could act on your accounts with your full authority and
cause **irreversible financial damage, up to the total loss of the assets
reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach Carta's terms of
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
affiliated with, endorsed by, or sponsored by Carta or any other financial
institution; nothing in this repository is financial, legal, or tax
advice.

A read-only collector for [carta.com](https://carta.com), the
private-investment / cap-table platform. Ingests a **portfolio holder's**
own private-company holdings — equity / share certificates, stock options
(with strike + vesting), RSUs/RSAs, SAFEs / convertible notes — the fund-LP
capital account, the per-grant exercise-detail reports (date / shares /
strike / 409A FMV at exercise), and the document archive (K-1 /
capital-account statements / financials).

This is the first wealthdb source for **non-public-market equity**: no
ticker, no quotable price, and concepts (vesting, strike, 409A FMV) with no
existing home in the gold canonical model. See [DESIGN.md](DESIGN.md) for
the full path investigation and the bronze → silver → gold story.

Carta has a real REST API, but API access is invite-gated and (as of 2025)
SOC 2-gated for partners; the "consume your own data" carve-out is for
companies / investment firms, not individual shareholders. An individual
holder cannot self-provision credentials, so — like the other portal-only
sources and its sibling [`angellist`](../angellist/) — this collector
replays the browser flow (2FA login, React SPA) via Camoufox.

Part of the **wealthdb** suite — see
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for shared collector
conventions.

## Status

**Implemented end-to-end**:
`login` → `download` → `load` produces a populated silver DB.
`explore` mapped Carta's internal cookie-session **REST/JSON** API, which
both `download` and the silver schema follow.

| Verb | Status | Notes |
| --- | --- | --- |
| `explore`  | implemented | Camoufox + VNC discovery harness (HAR + crash-safe network log + trace + click log). Re-run if Carta changes its UI / endpoints. |
| `login`    | implemented | Camoufox SPA login + CLI-2FA on stdin, persistent profile; `--check` probes the session (no 2FA push). Clears Carta's Cloudflare front. |
| `download` | implemented | Authenticated REST/JSON walk of both families (cap-table holdings + grants + vesting + per-grant exercise-detail xlsx; fund LP capital account + cap-calls), plus the document archive (each `document_url` envelope followed to its signed CDN binary). Document fetches are download-avoidant via the shared `collectorkit.docdedup` engine: immutable archival reports (quarterly/annual financials) identical to a prior run are hardlinked in rather than re-fetched, while the documents `load` parses (capital-account statements, capital-call & distribution notices) and tax docs (K-1 / 1042-S) are always re-fetched and content-compared so a re-issue is never missed. `--documents-force` bypasses the index. `--no-documents` skips the documents pass entirely (the run's dominant cost), recording `skipped=true` in the manifest so a partial run is not read as one that found no documents. Read-only (GET only). `--dry-run` verifies discovery without writing. |
| `load`     | implemented | SQLite silver as **event-dated change deltas** (DESIGN.md §5.1): entities / securities (valued, with the FMV at exercise) / vesting / fund_metrics (quarterly NAV, fees, gains and carry parsed from each fund's statements) / capital_events / cash_flows (the dated money ledger) / documents / k1_capital_accounts (the K-1 tax year, period and tax capital account). Idempotent; migrations on startup. |

The top-level `Makefile` auto-discovers this collector (`make build-carta` /
`make test-carta`). The silver schema (`migrations/`) is the stable contract
the gold-side adapter (`wealthdb/internal/silver/carta/`, see
[`docs/adapters/carta.md`](../../wealthdb/docs/adapters/carta.md)) reads; it
consumes the event-dated delta model (DESIGN.md §6).

## Quick start

```sh
# 1. Build the image (builds on the shared base-camoufox image).
./carta build

# 2. Drop credentials into the env file. chmod 0600.
#    cat > ~/.secrets/carta.env <<'EOF'
#    CARTA_USERNAME=your-carta-login-email
#    CARTA_PASSWORD=your-carta-password
#    EOF

# 3. (One-time, dev) Discovery: drive the live holder UI over VNC and
#    record HAR + trace + clicks so login/download can be written.
./carta explore --fresh

# 4. Mint the session. Prompts for the 2FA code on stdin once; the
#    Camoufox profile keeps later runs from re-prompting ("remember this
#    device" skips 2FA on renewal; exact cookie lifetime not measured).
./carta login

# 5. Pull a fresh bronze dump (cap-table holdings, grants, vesting and
#    exercise details; the fund capital account and cap calls; documents).
./carta download

# 6. Ingest bronze into the SQLite silver.
./carta load
```

Probe / iterate without firing a 2FA push or writing data:

```sh
./carta login --check          # exits 0 if session valid, 1 if not
./carta download --dry-run     # walks navigation, exports nothing
./carta load --force           # delete silver + rebuild from all bronze
```

Override host mounts via env: `CARTA_SECRETS_DIR`, `CARTA_DATA_DIR`,
`CARTA_DEBUG_DIR`. Carta refreshes most data daily (~noon ET), so a nightly
run after that is the right cadence; same-day re-runs add nothing.

### Reclaiming disk

```sh
./carta prune --dry-run   # print the deletion plan, delete nothing
./carta prune             # delete it
```

`prune` removes whole non-complete run dirs across the bronze tree: a walk
that crashed leaves `run.json` with `status: "in-progress"` (a pre-status
run left no `run.json` at all), and `load` skips such dirs, so `prune`
reclaims them once quiescent. From a complete dump it strips only
`screenshots/` — the landing-page DOM + screenshot that `download --debug`
writes, which `load` never reads. (The `explore` diagnostics — HAR,
Playwright trace, click log — are separate: they go to `/debug`, outside
bronze, and prune never sees them.) A complete
dump's inputs (`entities/`, the document PDFs, `bootstrap/`, the manifest),
the side-loaded `<eid>-valuations.csv` / `<eid>-transactions.csv` files,
and the silver DB — all at the bronze root, not under a run dir — are never
touched, so silver stays reproducible; deleting a non-complete dump surfaces
on the next `load --force` rebuild. Runs host-side (a pure file walk needs no
container), so it can reclaim disk while a `download` is mid-flight; an
in-flight guard (`--min-age-hours`, default 1, keyed on recent write
activity) keeps it from removing a download that is still running.

## Read-only

See [AGENTS.md](AGENTS.md). The Carta holder UI exposes mutation surfaces
that **exercise options, sell/transfer shares, and move money** (plus
funding + tax-withholding setup, e-sign, account settings), and possibly an
issuer / company-admin or fund-admin console — all out of scope. This
toolkit only navigates, filters, and exports the portfolio holder's own holdings.
Private-company names, share counts, strikes, 409A FMVs, vesting schedules,
and tax documents are PII and never enter the repo.
