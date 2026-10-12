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
Chase retail banking portal. It reads two products:

- **deposit accounts** (checking and savings);
- **credit-card accounts**, read-only.

For each account it fetches the transaction exports and the statement
PDFs. Card **management** is out of scope: payments, autopay, limits,
disputes, lock/unlock. So is any investment surface the same login may
expose ([DESIGN.md](DESIGN.md) §4).

Chase has no self-service API, so the collector drives the web UI in a
stealth browser (Camoufox). Chase drops the session when Firefox closes
and asks for 2FA at every sign-in. So `download` signs in, answers 2FA
from the terminal, and scrapes in one browser lifetime.

Part of the **wealthdb** suite. See
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for the conventions
every collector shares.

## Status

The pipeline runs from bronze through gold for both products. The gold
adapter carries a deposit account as cash and a card as a revolving-credit
liability ([adapter doc](../../wealthdb/docs/adapters/chase.md)).

- Deposit accounts: push and SMS 2FA, roster discovery, CSV + QFX export,
  and statement PDFs. The sign-in and export selectors are pinned from
  captures.
- Cards: the selectors for the export form's account picker, the
  statement save control and the card documents route are pinned from
  captures only. The passes fail loudly rather than save a file under the
  wrong account.

## Quick start

```sh
# 1. Build the image (on the shared base-camoufox image).
./chase build                    # or, from the repo root: make build-chase

# 2. Put the credentials in ~/.secrets/chase.env (chmod 0600):
#    CHASE_USERNAME=example-username
#    CHASE_PASSWORD='example-password'

# 3. Sign in and scrape. 2FA runs in the terminal: pick a factor, then
#    type the code or approve the push. Writes a bronze run dir.
./chase download                 # or: wealthdb-collect chase download
./chase download --lookback 1y   # widen the statement window
./chase download --no-documents  # skip the statement PDFs

# 4. Load the bronze into silver.
./chase load                     # or: wealthdb-collect chase load
./chase load --force             # delete silver and rebuild from all bronze
```

`--lookback` bounds only the statement pass. The activity export always
fetches Chase's full window of about 24 months, so older history comes from
the statement PDFs.

Other verbs:

- `./chase vnc-login` is the fallback when the terminal 2FA cannot find a
  challenge control. It exposes VNC, and Sign In + 2FA are done by hand in
  the browser. The scrape then runs as in `download`.
- `./chase login` does nothing: it folds into `download`.
  `./chase login --check` probes the stored profile without a 2FA push. It
  reports the session dead between runs, by design.
- `./chase prune --dry-run` previews the reclaim; `./chase prune` deletes
  non-complete dumps and the aged debug cache.
- `./chase help` and `./chase <verb> --help` list the flags.

The 2FA dialog is a standalone, tested module,
[`auth_dialog.py`](auth_dialog.py). Its demo replays the dialog on synthetic
data and sends nothing. From the repo root:

```sh
PYTHONPATH=shared/collectorkit python3 collectors/chase/auth_dialog.py --demo
```

## Where the output goes

Each `download` writes one bronze run dir under the collector's data dir
([data layout](../README.md#conventions-shared-across-collectors)):

| Path | Content |
| --- | --- |
| `run.json` | status manifest, plus a per-product coverage block |
| `accounts.json` | the account roster, with card detail on card records |
| `raw/*.json` | the raw roster responses, for provenance |
| `transactions/<id>.csv`, `transactions/<id>.qfx` | the two activity exports per account |
| `statements/<id>/<YYYY-MM-DD>.pdf` | statement PDFs |

`load` writes the silver SQLite `chase.db` beside the run dirs. Both
products share the tables; `accounts.product` tells them apart (`dda` or
`card`).

| Table | One row per | Main columns |
| --- | --- | --- |
| `accounts` | account and snapshot, deduplicated on content | `product`, `account_type` (`CHK`, `SAV`, `BAC`), `nickname`, `mask` (last 4), `currency`, `balance`, `pending_charges` (cards), `payload` |
| `transactions` | ledger row | `fitid` (content-derived id), `posted_at`, `account_external_id`, `amount`, `kind`, `description`, `check_number`, `balance`, `source` (`qfx`, `csv` or `statement`), card-only `txn_date` / `merchant` / `category`, `payload` |
| `documents` | statement PDF, deduplicated on its SHA-256 | `account_external_id`, `doc_date`, `doc_kind`, `filename`, `size_bytes` |
| `statement_balances` | card and statement period | `period_start`, `period_end`, `opening`, `closing`, `transactions_covered` |

Amounts and balances stay as Chase states them. A card's `balance` is the
positive amount owed, and card spend is negative. The gold adapter turns
the card sign. [DESIGN.md](DESIGN.md) §4 explains the export join, the
transaction ids, the statement backfill and the reconstructed card
balances.

## Discovery harness

```sh
./chase explore                  # drive the live UI over VNC and record it
```

`explore` prints the VNC connection details at startup. The login form is
pre-filled from the env file; 2FA is done by hand. Recording stops when the
last browser window closes, on Ctrl-C, or after `--max-duration` (default
one hour). `--fresh` wipes the browser profile first, so the session meets
the first-login challenge. That also drops the device token, so the first
sign-in after it offers the app push only ([DESIGN.md](DESIGN.md) §A).

The recording lands in the host debug dir,
`~/.cache/wealthdb/debug/chase/<UTC-ts>/` (override: `CHASE_DEBUG_DIR`).
[DESIGN.md](DESIGN.md) §2 lists the artefacts. The dir is sensitive.
Network bodies and downloads carry real account and routing numbers,
balances and payees. Never commit anything derived from it without
stripping identifiers.

## Read-only

The Chase UI puts money movement, card management and account settings one
or two clicks from the account overview. All of it is out of scope. The
collector only navigates, filters and exports: the accounts overview, the
statements area and the transaction history. Reading a card is in scope;
changing one never is. [AGENTS.md](AGENTS.md) has the exact allowed and
forbidden surfaces.
