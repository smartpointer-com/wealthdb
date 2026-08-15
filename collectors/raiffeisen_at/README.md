# raiffeisen_at

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to Mein ELBA
(mein.elba.raiffeisen.at) with your credentials and your pushTAN
confirmations. The session it holds is **fully privileged** — the same
login a human uses to move money — and Mein ELBA offers no read-only
sub-scope, so nothing but this codebase's own discipline restricts the
session to reading. If malicious code were ever introduced into this
repository, its dependency chain, or the container images it runs, it
could act on your accounts with your full authority and cause
**irreversible financial damage, up to the total loss of the assets
reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach the bank's terms
of service; verifying that your use is permitted is likewise your
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
affiliated with, endorsed by, or sponsored by any bank of the Austrian
Raiffeisen Banking Group or any other financial institution; nothing in
this repository is financial, legal, or tax advice.

A read-only collector for [Mein ELBA](https://mein.elba.raiffeisen.at/),
the Austrian Raiffeisen retail e-banking portal: **deposit accounts**
(checking + savings), their transaction history / CSV exports, and
on-demand statement PDFs. Cards, financing, and any securities / wealth
surface the same login may expose are out of scope (see
[CLAUDE.md](CLAUDE.md)). The collector is named `raiffeisen_at` because
Raiffeisen operates distinct banking systems in other countries. Like
the other portal-only sources it replays the browser flow via Camoufox
from day one — the fleet's browser-scraped banks all measured
Akamai-class bot defense blocking vanilla engines, so the harness builds
on the shared Camoufox base image rather than re-measuring.

Part of the **wealthdb** suite — see
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for shared collector
conventions.

## Status: `login` + `download` built — awaiting live validation

Two `explore` captures (2026-08-15) mapped the whole surface: an OIDC
login on `sso.raiffeisen.at` with a **region (Mandant) dropdown that
prefixes the Verfüger number**, **pushTAN** confirmed in the Raiffeisen
app (announce-then-poll, a Vergleichswert shown on both screens, no
fallback factor, **and it fires on every login** — the profile
remembers the identity but not the second factor), and — despite the
legacy-bank expectation — a clean Angular-SPA REST API for the data: a
stable-id transaction ledger with booking + value dates, a
parameterizable daily-balance series, and a ~3-year server-side history
floor. The CSV export turned out to be a client-side dump of that same
JSON (so the JSON is the ledger source), and statements are a
**document archive** (Dokumente) of monthly Kontoauszüge, not an
on-demand generator — reaching only ~5 months past the transaction
floor, so there is no deep backfill. Because 2FA fires every run,
`login` folds into `download` (chase's shape). See
[DESIGN.md](DESIGN.md) §3-Observed. The [`chase`](../chase/) collector
is the retail-deposit playbook this one adapts, with
[`firstcitizens`](../firstcitizens/) the most recent run of the same
playbook.

The `login` + `download` verbs are **built and unit-tested** (the
browserless halves) but **not yet live-validated** — no real session
has driven them. The first live `download` (owner present, pushTAN) is
the acceptance test. `load` (silver) and the gold adapter are the
remaining phases.

## Setup

```
make build-raiffeisen_at
```

Credentials go in `~/.secrets/raiffeisen_at.env` (chmod `0600`;
single-quote values containing `$`, `!`, or backticks):

```
RAIFFEISEN_AT_USERNAME='...'   # the personal Verfüger number, WITHOUT the region prefix
RAIFFEISEN_AT_PASSWORD='...'   # the PIN
RAIFFEISEN_AT_REGION='...'     # the Mandant code selecting the regional bank (DESIGN.md §3-Observed·§B)
```

## Usage

```
./raiffeisen_at download                 # one-shot login + REST fetch → bronze (default window ~90 days)
./raiffeisen_at download --lookback all   # full history (the initial backfill)
./raiffeisen_at download --dry-run        # log in, enumerate the roster, fetch nothing
./raiffeisen_at download --fresh          # force the cold region/Verfüger/PIN form
./raiffeisen_at login --check             # is a session still alive? (dead between runs is expected)
./raiffeisen_at vnc-login                 # by-hand login fallback over VNC

./raiffeisen_at explore                   # discovery harness over VNC
```

`download` fills the region/Verfüger/PIN form (or clicks the saved-user
card) from the env credentials and then waits for you to **approve the
pushTAN in the Raiffeisen app** — there is nothing to type, so no TTY is
needed. It compares a 4-char Vergleichswert shown on screen and in the
app. Real sessions fire real pushTAN prompts — run them only
deliberately, never in quick succession.

`explore` opens Mein ELBA in Camoufox and records the session (HAR +
network log + click log + DOM snapshots + downloads) under
`~/.cache/wealthdb/debug/raiffeisen_at/<UTC-ts>/`; connect a VNC viewer
to the forwarded port and walk the three flows in
[DESIGN.md](DESIGN.md) §3. The login is confirmed via **pushTAN** in the
Raiffeisen mobile app. Real sessions fire real pushTAN prompts — run
them only deliberately, never in quick succession.
