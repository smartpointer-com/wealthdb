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
Express card portal. It covers **credit and charge card accounts**:

- the card roster and balances;
- the transaction history and its exports;
- the statement PDFs.

Everything else the same login can reach is out of scope. That includes
money movement, Membership Rewards redemption, offers, Plan It,
travel, account changes, settings and the message center. The exact
allow and forbid surface is in [AGENTS.md](AGENTS.md).

Part of the **wealthdb** suite. See
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for the shared
collector conventions.

## How it works

The signed-in app is a React SPA. It talks to a
**`functions.americanexpress.com` BFF** and a **`/api/servicing/…` REST
API**. Akamai Bot Manager guards the site, as at the US siblings
([`chase`](../chase/), [`fidelity-web`](../fidelity-web/),
[`schwab-web`](../schwab-web/)). Its defense lives in cookies, and no data
endpoint carries a sensor header. So a Camoufox browser signs in, and the
data comes over plain REST with the browser's cookie jar. The second factor
is a **one-time passcode** (no push), typed into a six-box control.
[DESIGN.md](DESIGN.md) §A–§H maps each endpoint.

**`download` is the one verb that signs in.** Amex allows few sign-ins:
a handful inside a short window draw a captcha. A separate `login` would
spend two sign-ins per run, so `login` folds into `download`, as at
[`chase`](../chase/) and [`schwab-web`](../schwab-web/)
([DESIGN.md](DESIGN.md) §L).

- **Device trust persists.** The `device-id` cookie lives about 396 days.
  On a registered device `download` signs in with no passcode.
- **The session does not.** The session cookies die with the browser, so
  every run signs in again.
- **`vnc-login`** runs the same walk with the sign-in done by hand over
  VNC. It is the only way past a captcha.
- **`login`** is a host-side no-op. `login --check` reports whether this
  device is registered. It reads the profile's cookie and signs in to
  nothing.
- **`download --dry-run` still signs in.** It skips the exports and
  documents, not the sign-in.

**`load`** parses each bronze run into SQLite. The activity JSON is the
ledger of record. It carries one stable id per row across every channel,
both dates, the merchant category and pending rows, so no export join is
needed. Three rules are specific to this source:

- it **negates every amount**, because Amex states spend as positive and
  the fleet's card convention is negative;
- it **replaces** pending rows on every load that ingests a run, because
  their ids are provisional;
- it **rebuilds** the statement-sourced rows from all of bronze on every
  load that ingests a run, and after a parser change or an unfinished
  rebuild, because the seam they are gated on moves as wider windows land.

The **statement backfill** reaches past the 24 months where the activity
and the exports stop, into the provider's ~7-year PDF archive. A statement
is imported only when its printed summary adds up and each section's rows
sum to the figure printed for it. Only periods that end before the
activity's oldest row are imported ([DESIGN.md](DESIGN.md) §4.3).

The **gold adapter** (`wealthdb/internal/silver/amex/`) projects each card
as a `card` liability. Its value is net worth and spending: cards stay out
of returns by construction. Modern-era rows carry the provider's own spend
category. A card's payment legs let the internal-transfer matcher pair the
bills paid from a collected cash account. See
[the adapter doc](../../wealthdb/docs/adapters/amex.md).

## Setup

```
make build-amex
```

Credentials go in `~/.secrets/amex.env`, quoted as described in
[collectors/README.md](../README.md#conventions-shared-across-collectors):

```
AMEX_USERNAME='...'
AMEX_PASSWORD='...'
```

## Usage

```
./amex download                    # the whole run: sign in + REST fetch → bronze (~90 days)
./amex download --lookback all     # everything the source offers (the initial backfill)
./amex download --dry-run          # roster + activity counts; STILL SIGNS IN, exports nothing
./amex download --no-documents     # skip the statement PDFs
./amex download --fresh            # move device trust aside, forcing the untrusted flow
./amex download --format xls       # csv, xls, qfx, qbo (repeatable; default csv+qfx)

./amex vnc-login              # the same walk, sign-in by hand over VNC (answers a captcha)
./amex login --check          # is this device registered? (profile cookie, no sign-in)

./amex load                   # bronze → silver SQLite
./amex load --force           # delete the silver DB and rebuild from all bronze

./amex prune --dry-run        # preview what prune would reclaim
./amex explore                # discovery harness over VNC
```

When a passcode does fire, `download` asks for it **in the terminal** and
registers the device while there, so later runs skip it. The wrapper
forwards stdin only when stdin and stdout are both TTYs. With no terminal,
a challenge fails loudly instead of blocking, so an unattended run fails
cleanly. `--cli-mfa` and `--no-cli-mfa` force either behaviour.

A **captcha** cannot be answered from a terminal. `download` recognises one
and stops at once, naming `vnc-login`. Captcha reputation builds per
account and decays with idle time. The remedy is to wait, not to retry, so
real runs should never follow each other quickly.

`--lookback` narrows the fetch at the source, on two windows:

- The activity and the exports stop at 24 months, Amex's own limit. Their
  window is clamped there with a warning.
- The statement PDFs use the window **as requested**, unclamped. A wide
  `--lookback` therefore reaches the provider's full retention, which is
  what feeds the backfill.

## Output

Each `download` writes a UTC-stamped run dir under
`$XDG_DATA_HOME/wealthdb/amex/`:

```
<run>/
  run.json                        status manifest, windows and per-account coverage
  accounts.json                   the card roster
  activity/<key>.json             the activity ledger, category map and cycle balances
  transactions/<key>.<ext>        the exports, one per --format
  statements/<key>/<date>.pdf     statement PDFs (skipped by --no-documents)
  statements/<key>/yes-<year>.pdf year-end summary PDFs
  raw/*.json                      raw roster and statement-archive bodies
```

`<key>` is the account's opaque servicing key, never a card number. A dry
run's only files are `run.json`, `accounts.json` and the raw roster.

`load` writes `$XDG_DATA_HOME/wealthdb/amex/amex.db`:

| table | one row per | notable columns |
| --- | --- | --- |
| `accounts` | card per run, when its payload changed | `balance` (positive amount owed), `pending_charges`, `payment_due_at`, `mask`, `account_token` |
| `transactions` | ledger row | `amount` (spend negative), `posted_at`, `txn_date`, `category`, `category_code`, `is_pending`, `source` (`activity` or `statement`) |
| `statement_balances` | card and billing period | `opening`, `closing`, `new_charges`, `payments_and_credits`, `fees`, `interest`, `transactions_covered`, `source` |
| `documents` | distinct PDF | `sha256`, `doc_kind` (`statement` or `year_end_summary`), `doc_date` |

[`migrations/0001_initial.sql`](migrations/0001_initial.sql) documents
every column and the sign convention.

Under `--debug`, the sign-in diagnostics of `download` and `vnc-login`
land in `~/.cache/wealthdb/debug/amex/screenshots/`. Each `explore` run
writes to `~/.cache/wealthdb/debug/amex/<UTC-ts>/`. On `login --check`,
`--debug` only raises the log level. Both trees hold real account data,
so treat them as sensitive. Captures are redacted as described in
[collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze).
An `explore --trace` bundle is never redacted and holds the credential, so
delete it once read.

## Known limits

Paths covered by unit tests only:

- the untrusted-device flow (`--fresh`);
- the `--no-cli-mfa` failure branch;
- statement layouts the parser has not met. Layouts vary by card product,
  and a new one may need its section headings added.
