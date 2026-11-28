# erstebank_at

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to George
(george.sparkasse.at) with your user name and your George app
confirmations. The session it holds is **fully privileged** — the same
login a human uses to move money — and George offers no read-only
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
affiliated with, endorsed by, or sponsored by Erste Bank, any Austrian
Sparkasse, or any other financial institution; nothing in this repository
is financial, legal, or tax advice.

A read-only collector for [George](https://george.sparkasse.at/), the
retail e-banking portal of Erste Bank und Sparkassen in Austria. It
covers **checking accounts** (Girokonto) and **credit cards**: their
transaction history and statement PDFs. Savings, securities, loans and
any other product the same login may expose are out of scope (see
[AGENTS.md](AGENTS.md)). The name is namespaced to Austria because Erste
runs a distinct George system in each country.

Part of the **wealthdb** suite. See
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for shared collector
conventions.

## How it works

- **Sign-in** happens in a Camoufox browser on `login.sparkasse.at`. The
  form asks for the user name only.
- **The push** is approved in the George app or in George ID. `download`
  prints the 4-character comparison code the page shows. A session can
  outlive the browser (see [DESIGN.md](DESIGN.md) §F). A run that finds
  one needs no push. `login` folds into `download`.
- **Data** comes from George's REST API: the account and card rosters,
  each transaction history, and the statement PDFs of accounts and cards.
  A history row has a stable id, booking and value dates, and a running
  balance.
- **`load`** writes a SQLite silver: the accounts and cards, the
  transactions, a daily closing balance per account, and the statement
  listings and PDFs. A card's balance is stored as the positive amount
  owed.
- **Gold** (`wealthdb/internal/silver/erstebank_at/`) maps these to cash
  and card accounts with a closing-balance series and both ledgers.

[DESIGN.md](DESIGN.md) maps the source. The
[`raiffeisen_at`](../raiffeisen_at/) collector is the model for the
checking half, and [`amex`](../amex/) for the card half.

## Setup

```
make build-erstebank_at
```

Credentials go in `~/.secrets/erstebank_at.env` (chmod `0600`):

```
ERSTEBANK_AT_USERNAME='...'   # the George user name (Verfügernummer or alias)
```

## Usage

```
wealthdb-collect erstebank_at download                  # sign in if needed + REST fetch → bronze (default window ~90 days)
wealthdb-collect erstebank_at download --lookback all   # full history (the initial backfill)
wealthdb-collect erstebank_at download --dry-run        # read the rosters only
wealthdb-collect erstebank_at login --check             # is a session alive? (no push)
wealthdb-collect erstebank_at vnc-login                 # by-hand sign-in fallback over VNC
wealthdb-collect erstebank_at load                      # bronze → silver SQLite
wealthdb-collect erstebank_at load --force              # rebuild the silver from all bronze

wealthdb-collect erstebank_at explore                   # discovery harness over VNC
```

`download` types the user name from the env file. It then prints the
comparison code and waits for the push approval. Nothing is typed in the
terminal, so no TTY is needed. A run that finds no live session fires a
real push, `--dry-run` included. Run it deliberately and never in quick
succession.

`download` exits 0 when the run is whole, 4 when it came back short
(a history read only in part, a statement listing or PDF that failed),
and 1 when it could not finish. A short run is still loadable. The next
run asks again.

`explore` prints a VNC address and records the session under
`~/.cache/wealthdb/debug/erstebank_at/<UTC-ts>/`. That dir holds real
account data. `prune` reclaims it once it is an hour old:

```
wealthdb-collect erstebank_at prune --dry-run
```

## Output

- Bronze runs land under `$XDG_DATA_HOME/wealthdb/erstebank_at/<UTC-ts>/`.
  Each run holds the rosters, one history file per account or card, and
  the statement PDFs with their listings.
- Silver is `$XDG_DATA_HOME/wealthdb/erstebank_at/erstebank_at.db`, with
  the tables `accounts`, `transactions`, `daily_balances`, `statements`,
  `documents`, `run_accounts` and `dump_runs` ([DESIGN.md](DESIGN.md) §5).
