# chase

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to chase.com with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
Chase offers no read-only sub-scope, so nothing but this codebase's own
discipline restricts the session to reading. If malicious code were ever
introduced into this repository, its dependency chain, or the container
images it runs, it could act on your accounts with your full authority and
cause **irreversible financial damage, up to the total loss of the assets
reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach Chase's terms of
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
affiliated with, endorsed by, or sponsored by JPMorgan Chase or any other
financial institution; nothing in this repository is financial, legal, or
tax advice.

A read-only collector for [chase.com](https://www.chase.com), the JPMorgan
Chase retail banking portal: **deposit accounts** (checking, and savings
if present) and **credit-card accounts**, their statement PDFs, and their
transaction history / exports. Card **management** — payments, autopay,
limits, disputes, lock/unlock — is out of scope, as is any investment
surface the same login may expose (see [DESIGN.md](DESIGN.md) §4). Both
products run the whole way through: the gold adapter projects a deposit
account as cash and a card as a revolving-credit liability. Like the other
portal-only sources, it replays the browser flow (2FA login, SPA) via
Camoufox — Chase fronts
its retail UI with the same Akamai-class bot defense that blocked vanilla
browsers outright at the two US siblings ([`fidelity-web`](../fidelity-web/),
[`schwab-web`](../schwab-web/)), so the harness builds on the shared
Camoufox base image from the start.

Part of the **wealthdb** suite — see
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for shared collector
conventions.

## Status

**Pipeline complete, bronze through gold.** The discovery harness mapped the
whole surface — the login/2FA flow (in-app-push, SMS, voice), the
hash-routed SPA's `/svc/` JSON API, and the statement / transaction /
export surfaces — and confirmed the session is **not persistent** (every
login needs a fresh 2FA). `login` / `download` / `load` / `prune` match the
sibling collectors and are driven by `wealthdb-collect chase …` /
`wealthdb-refresh chase`. **Credit-card read surfaces** — roster, detail,
transaction export, statements — are in scope as of the 2026-09-04
amendment (DESIGN.md §4): `download` captures them into bronze, the silver
load ingests them (roster, exports, statements — inventory, parsed period
balances and the pre-export transaction tail), and the gold adapter
projects them as revolving-credit liabilities alongside the deposit
accounts' cash. Card **management** is permanently out of scope.

| Verb | Status | Notes |
| --- | --- | --- |
| `explore`  | implemented | Camoufox + VNC discovery harness (HAR + crash-safe network log + click log + saved downloads + opt-in Playwright trace). |
| `download` | implemented | One-shot login + scrape, **2FA from the terminal** (no VNC): pre-fill → submit → stdin code → export each account's activity (CSV + QFX) + statement PDFs → bronze. Backed by `login.py`; the scrape is `download.py`'s `walk()`. |
| `vnc-login` | implemented | Fallback for `download` when the CLI 2FA can't drive a challenge control: exposes VNC for a by-hand sign-in. |
| `login`    | implemented | Folds into `download` (the session dies with Firefox — §F). `login --check` probes a session read-only. |
| `load`     | implemented | Bronze → SQLite silver (accounts, transactions, statements), both products. Deposit: joins the CSV running balance onto the QFX rows. Card: joins the CSV date/category/type onto the QFX descriptor. Both ledgers identify a row by **content plus an occurrence index**, never by the provider's `FITID` (kept in `payload.fitid`): each export format is fetched separately and can fail on its own, so a key only the QFX carries would give the same rows a second identity in a CSV-only run and double the ledger — and a card's FITIDs are not unique either. Card statements are parsed by their own pass: period balances for every era (each flagged with whether that period's transactions reached silver), and transactions below the export seam. The card export carries no running-balance column, so the export era's is reconstructed between those period balances; a balance the loader computed rather than read off a provider file is marked `payload.balance_basis`. Idempotent; `--force` rebuilds. |
| `prune`    | implemented | Thin wrapper over the shared prune engine; runs host-side. |

**Validated live end-to-end** (2026-08-12): push and SMS 2FA, deposit-account
roster discovery, CSV + QFX export, and statement PDFs. The card path is
built from the exploration capture and unit-tested, but has not yet been run
against the live UI. The silver loader is fully unit-tested against synthetic
exports.

The 2FA challenge contract is encoded as a standalone, tested CLI dialog in
[`auth_dialog.py`](auth_dialog.py) — the browserless half of what `download`
drives from the terminal (`python3 auth_dialog.py --demo` previews it).
The top-level `Makefile` auto-discovers this collector (`make build-chase`
/ `make test-chase`).

## Quick start

```sh
# 1. Build the image (builds on the shared base-camoufox image).
./chase build            # or, from the repo root: make build-chase

# 2. Drop credentials into the env file. chmod 0600. Single-quote any
#    value containing $, !, or backticks.
#    cat > ~/.secrets/chase.env <<'EOF'
#    CHASE_USERNAME=example-username
#    CHASE_PASSWORD='example-password'
#    EOF

# 3. Download: one-shot login + scrape. Pre-fills + submits the sign-in
#    form, then drives 2FA from the TERMINAL (no VNC) — it prompts on
#    stdin for the code (or to approve a push). Exports each account's
#    activity (CSV + QFX) and statement PDFs to a UTC-stamped bronze run
#    dir.
./chase download                 # or: wealthdb-collect chase download
./chase download --lookback 1y   # widen the statement window (default ≈ 90
                                 # days) — the activity export always fetches
                                 # Chase's full ≈ 24 months
./chase download --no-documents  # skip the statement-PDF pass

# 4. Load that bronze into the SQLite silver.
./chase load                     # or: wealthdb-collect chase load
./chase load --force             # delete + rebuild from all bronze
```

Because Chase drops the session when Firefox closes and challenges 2FA at
every sign-in ([DESIGN.md](DESIGN.md) §F), `login` and `download` are a
single browser lifetime — `login` folds into `download`, and each
`download` pays one 2FA driven from the terminal. If the CLI 2FA can't
find a challenge control (its selectors are trace-derived — DESIGN.md §5),
fall back to `./chase vnc-login`, which exposes VNC and lets you complete
Sign In + 2FA by hand in the browser. Probe a session with `./chase login
--check` (read-only; reports DEAD between runs, by design). Reclaim bronze
disk with `./chase prune --dry-run` then `./chase prune`.

### Discovery harness

```sh
./chase explore                  # drive the live UI over VNC, record traces
```

`explore` prints VNC connection instructions at startup (host port +
single-use password + the ssh tunnel line). The login form is pre-filled
from the env file — 2FA is driven by hand — and the recording stops when
the last browser window is closed, on Ctrl-C, or after `--max-duration`
(default 1 hour, sized for a human-paced session).

Artefacts land under `~/.cache/wealthdb/debug/chase/<UTC-ts>/` on the
host (override with `CHASE_DEBUG_DIR`):

| Artefact | What it holds |
| --- | --- |
| `network.har` | every request + response; flushed only on a clean close |
| `network.jsonl` | crash-safe, line-flushed request/response log (text bodies ≤ 200 KB, incl. OFX/QFX) |
| `clicks.jsonl` | one JSON object per click, plus lifecycle + login-form/OTP-field events |
| `downloads/` | files fetched in-session (statement PDFs, CSV/QFX/OFX exports) |
| `trace-chunks/`, `trace.zip` | opt-in `--trace` Playwright trace |

Useful flags: `--fresh` wipes the browser profile so the next login
presents the full 2FA challenge (the flow worth capturing);
`--no-prefill` leaves the credentials to be typed by hand; `--url`
overrides the entry point; `--trace` records a Playwright trace (off by
default — the pinned tracer crashes the current camoufox build, see
`--help`).

Override host mounts via env: `CHASE_SECRETS_DIR`, `CHASE_DATA_DIR`,
`CHASE_DEBUG_DIR`.

**The debug dir is sensitive.** Network bodies and downloads carry real
account and routing numbers, balances, and payees. It lives outside the
repo and outside `~/.secrets/`; treat it like the data tree and never
commit anything derived from it without stripping identifiers.

## Read-only

See [CLAUDE.md](CLAUDE.md). The Chase retail UI puts money movement
(transfers, Zelle, wires, bill pay), card management (payments, autopay,
limits, disputes, lock/unlock), and account settings one or two clicks
from the account overview — all permanently out of scope. *Reading* a
card is not card management and is in scope; mutating one never is. This
toolkit only ever navigates, filters, and exports: accounts overview,
statements / documents, transaction history. Account and routing
numbers, card numbers, balances, and payees are PII and never enter the
repo.
