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
the Austrian Raiffeisen retail e-banking portal. It collects **deposit
accounts** (checking + savings): their transaction history, daily
balances and archived statement PDFs. Cards, financing, and any
securities or wealth surface the same login may expose are out of scope
(see [AGENTS.md](AGENTS.md)). The name is namespaced to Austria because
Raiffeisen runs distinct banking systems in other countries.

Part of the **wealthdb** suite. See
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for shared collector
conventions.

## How it works

- **Sign-in** is an OIDC login on `sso.raiffeisen.at` in a Camoufox
  browser. A region (Mandant) dropdown prefixes the Verfüger number.
- **pushTAN** is confirmed in the Raiffeisen app. It fires on every
  login, so `login` folds into `download`: one browser lifetime and one
  pushTAN per run.
- **Data** comes from the portal's REST API: a transaction ledger with
  stable ids and booking + value dates, a daily balance series, and the
  statement archive. The server keeps about three years of history.
- **Deeper history** comes from transaction listings a branch prints on
  request ([below](#supplied-transaction-listings)).
- **`load`** writes a SQLite silver: the account roster, transactions,
  daily closing balances, and the statement and listing inventory.
- **Gold** (`wealthdb/internal/silver/raiffeisen_at/`) maps these to
  cash accounts with a closing-balance series and the deposit ledger.

[DESIGN.md](DESIGN.md) §3-Surface maps the surface. The
[`chase`](../chase/) and [`firstcitizens`](../firstcitizens/) collectors
are the retail-deposit models this one follows.

## Setup

```
make build-raiffeisen_at
```

Credentials go in `~/.secrets/raiffeisen_at.env` (chmod `0600`):

```
RAIFFEISEN_AT_USERNAME='...'   # the personal Verfüger number, WITHOUT the region prefix
RAIFFEISEN_AT_PASSWORD='...'   # the PIN
RAIFFEISEN_AT_REGION='...'     # the Mandant code selecting the regional bank (DESIGN.md §3-Surface·§B)
```

## Usage

```
./raiffeisen_at download                 # one-shot login + REST fetch → bronze (default window ~90 days)
./raiffeisen_at download --lookback all   # full history (the initial backfill)
./raiffeisen_at download --dry-run        # sign in, record the roster, fetch nothing
./raiffeisen_at download --fresh          # force the cold region/Verfüger/PIN form
./raiffeisen_at login --check             # is a session still alive? (dead between runs is expected)
./raiffeisen_at vnc-login                 # by-hand login fallback over VNC
./raiffeisen_at load                      # bronze + supplied listings → silver SQLite
./raiffeisen_at load --force              # rebuild the silver from all bronze

./raiffeisen_at explore                   # discovery harness over VNC
```

`download` fills the region/Verfüger/PIN form (or clicks the saved-user
card) from the env credentials. It then logs a 4-character
Vergleichswert and waits for the **pushTAN approval in the Raiffeisen
app**. The same value shows in the app. Nothing is typed, so no TTY is
needed. Every real session, `--dry-run` included, fires a real pushTAN,
so run them deliberately and never in quick succession.

`explore` opens Mein ELBA in Camoufox for a VNC viewer on the forwarded
port and records the session under
`~/.cache/wealthdb/debug/raiffeisen_at/<UTC-ts>/`. [DESIGN.md](DESIGN.md)
§3 lists the flows to walk.

## Output

- Bronze runs land under `$XDG_DATA_HOME/wealthdb/raiffeisen_at/<UTC-ts>/`.
- Silver is `$XDG_DATA_HOME/wealthdb/raiffeisen_at/raiffeisen_at.db`,
  with the tables `accounts`, `transactions`, `daily_balances`,
  `documents` and `run_windows` ([DESIGN.md](DESIGN.md) §4.2).

## Supplied transaction listings

A branch can print a core-banking transaction listing (a "SMARTBank Abfrage"
PDF) for an account from any start date. Such listings go in `supplied/`
under the data dir, beside the run dirs:

```
<data-dir>/
  <UTC-ts>/          download runs
  supplied/          transaction listings, any *.pdf / *.PDF, any names
  raiffeisen_at.db   the silver
```

Every `load` reads them with no flag, so `wealthdb-collect raiffeisen_at
load` picks them up too. For each listing, `load`:

- checks it against its own arithmetic (every day's closing balance, the
  stated totals);
- binds it to its account;
- stitches it next to the live history. A day a download fully covered
  stays live. Other days come from a listing that covers them, preferring
  one that prints booking text, then the newest. A listing also tops up
  a day a download saw only partly.

A listing must agree with everything it overlaps and join it without a
balance jump, or it is left out whole. Overlapping and repeated listings
are fine. Each listing's outcome is logged: accepted with the days it
speaks for, or rejected with the reason. A listing that binds to an
account gets a row in the silver `documents` table
(`doc_kind = 'transaction_listing'`).

The stitch is recomputed on every load. Adding, replacing or removing a
listing takes effect on the next `load`, and so does a later, deeper
download. An empty `supplied/` removes the stitched rows; a missing one
leaves them alone. Listings are account data like everything else in the
data dir and are never committed. [DESIGN.md](DESIGN.md) §I has the
mechanics.
