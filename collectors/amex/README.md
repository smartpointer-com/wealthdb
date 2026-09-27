# amex

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to americanexpress.com with
your credentials and your multi-factor confirmations. The session it holds
is **fully privileged** — the same login a human uses to pay a bill,
redeem rewards, or open a plan — and American Express offers no read-only
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
credentials. Automated access may additionally breach American Express'
terms of service; verifying that your use is permitted is likewise your
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
affiliated with, endorsed by, or sponsored by American Express Company or
any other financial institution; nothing in this repository is financial,
legal, or tax advice.

A read-only collector for
[americanexpress.com](https://www.americanexpress.com), the American
Express card portal: **credit and charge card accounts** — the card
roster and balances, transaction history and exports, and statement PDFs.
Everything else the same login may expose is out of scope: money movement
in every form, Membership Rewards redemption, offers and enrolment,
Plan It / pay-over-time, travel and booking, account lifecycle, profile
and settings, and the message center (see [AGENTS.md](AGENTS.md)). Like
the other portal-only sources it replays the browser flow via Camoufox —
the US siblings ([`chase`](../chase/), [`fidelity-web`](../fidelity-web/),
[`schwab-web`](../schwab-web/)) all measured Akamai-class bot defense
blocking vanilla browsers, so the harness builds on the shared Camoufox
base image from the start.

Part of the **wealthdb** suite — see
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for shared collector
conventions.

## Status: full pipeline through gold, validated live

The signed-in app is a React SPA over a **`functions.americanexpress.com`
BFF** plus a **`/api/servicing/…` REST API**, behind **Akamai Bot Manager**
that is **cookie-borne** — no data endpoint carries a sensor header, so once
the browser holds the session jar the data is fetched over plain REST. 2FA
is a **one-time passcode** (no push) entered in a six-box control, driven
from the terminal. Endpoint by endpoint: [DESIGN.md](DESIGN.md) §A–§H.

**`download` is the one verb that signs in.** Device trust persists
(`device-id`, ~396 days) while the session cookies die with the browser, so
after the first run the sign-in needs no passcode — but that durable trust
did *not* earn a separate `login` verb. Amex's sign-in budget is small
enough that about seven in twenty minutes provoked a captcha, and a
`login` → `download` pair spent two of them for nothing, so `login` folds
into `download` (§L) as it does at [`chase`](../chase/) and
[`schwab-web`](../schwab-web/). `vnc-login` runs the same walk behind a
hand-driven sign-in — the only way past a captcha; `login` is a host-side
no-op; `login --check` reports device registration from the profile without
signing in at all.

**`load`** parses each bronze run into a source-shaped SQLite: the card
roster, the transaction ledger, the per-period statement balances and the
document inventory. The activity JSON is the ledger of record — one stable
id across every channel, both dates, the merchant category and pending rows
— so there is no export join. Three things are specific to this source: it
**negates every amount**, because Amex states spend as positive while the
fleet's card convention is negative; it **replaces** pending rows rather
than accumulating them, because their ids are provisional; and it
**rebuilds** the statement-sourced rows from all of bronze on every load,
because the seam they are gated on moves as wider windows land.

The **statement backfill** is what reaches past the 24 months the activity
and exports stop at, into the provider's ~7-year archive. Each statement is
gated on its own printed summary adding up *and* on each section's rows
summing to the figure printed for it; only periods ending before the
activity's oldest row are imported.

A **gold adapter** (`wealthdb/internal/silver/amex/`) projects that silver
as **card liabilities**: one `card` account per card, the owed balance
negated into canonical negative cash at the roster mark and at every
statement period, and the whole ledger. Cards contribute nothing to returns
by construction — the engine drops them, because a card's balance swings are
purchases and payments — so this source's value is net worth and
**spending**: every modern-era row carries the provider's own spend
category, and the card's payment legs let the internal-transfer matcher net
out the bills paid from a collected cash account, replacing the `card_spend`
placeholder with the purchases the card itemises. See
[the adapter doc](../../wealthdb/docs/adapters/amex.md).

Validated live on 2026-09-06, bronze through gold. What that leaves
untested is named in [DESIGN.md](DESIGN.md) §5: the untrusted-device path,
the `--no-cli-mfa` failure branch, and the statement layouts the parser has
not met — they vary by card product, and a new one may need its section
headings added.

## Setup

```
make build-amex
```

Credentials go in `~/.secrets/amex.env` (chmod `0600`; single-quote values
containing `$`, `!`, or backticks):

```
AMEX_USERNAME='...'
AMEX_PASSWORD='...'
```

## Usage

```
./amex download                    # the whole run: sign in + REST fetch → bronze (~90 days)
./amex download --lookback all     # everything the source offers (the initial backfill)
./amex download --dry-run          # roster + activity — STILL SIGNS IN, exports nothing
./amex download --no-documents     # skip the statement PDFs
./amex download --fresh            # move device trust aside, forcing the untrusted flow
./amex download --format xls       # csv, xls, qfx, qbo (repeatable; default csv+qfx)

./amex vnc-login              # the same walk, sign-in by hand over VNC (answers a captcha)
./amex login --check          # is this device registered? (profile cookie, no sign-in)

./amex load                   # bronze → silver SQLite
./amex load --force           # delete the silver DB and rebuild from all bronze

./amex explore                # discovery harness over VNC
```

On an already-registered device `download` signs in with no passcode. When
one does fire, it is answered **from your terminal** — the wrapper forwards
stdin only when stdin and stdout are both TTYs, so run it in a real one —
and the device is registered while it is there, so later runs skip it. With
no terminal a challenge is a loud failure rather than a blocked prompt, so a
cron run fails cleanly; `--cli-mfa` / `--no-cli-mfa` force either half.

A **captcha** cannot be answered from a terminal. `download` recognises one
and stops immediately, naming `vnc-login` — which runs the same walk with
the sign-in done by hand in the browser, so answering it costs one sign-in
rather than two.

`--lookback` narrows the fetch at the source, on two windows rather than
one. Past 24 months the activity and exports stop — Amex's own limit — so
their window is clamped there with a warning. The statement PDFs are
fetched over the window **as requested**, unclamped, so a wide `--lookback`
reaches the provider's full retention; that is what makes the backfill
above reachable at all.

The sign-in diagnostics `download` / `vnc-login` capture under `--debug`
land in `~/.cache/wealthdb/debug/amex/screenshots/`, each `explore` capture
in `~/.cache/wealthdb/debug/amex/<UTC-ts>/`, and bronze runs under
`$XDG_DATA_HOME/wealthdb/amex/<UTC-ts>/`. On `login`, `--debug` only raises
the log level — it captures nothing. Both trees carry real account data —
treat those directories as sensitive. Credentials are masked out of every
capture the harness writes itself, including the HAR, which is rewritten
after the browser closes; `explore --trace` is the exception — Playwright
writes that bundle in its own format and nothing scrubs it, and a trace
cannot be redacted after the fact because its DOM snapshots store every
input's value, so a trace holds the credential and is worth deleting once
it has been read.

Real sessions can fire a real passcode at your device — run them only
deliberately, **never in quick succession**. Amex's budget is small: about
seven sign-ins inside twenty minutes provoked a **captcha**, which no
terminal can answer (`vnc-login` and a human can). Captcha reputation
accrues per account and decays with idle time, so the remedy is to wait, not
to retry. `login` is a host-side no-op here, so a `login` → `download` pair is one
sign-in, and once the device is trusted `download` alone is enough.
