# firstcitizens

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to firstcitizens.com with
your credentials and your multi-factor confirmations. The session it
holds is **fully privileged** — the same login a human uses to move
money — and First Citizens offers no read-only sub-scope, so nothing but
this codebase's own discipline restricts the session to reading. If
malicious code were ever introduced into this repository, its dependency
chain, or the container images it runs, it could act on your accounts
with your full authority and cause **irreversible financial damage, up
to the total loss of the assets reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach First Citizens'
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
affiliated with, endorsed by, or sponsored by First Citizens BancShares
or any other financial institution; nothing in this repository is
financial, legal, or tax advice.

A read-only collector for
[firstcitizens.com](https://www.firstcitizens.com), the First Citizens
Bank retail banking portal. It collects **deposit accounts** (checking +
savings): their transaction history, exports and statement PDFs. Cards,
lending, and any wealth-management, trust or brokerage surface the same
login may expose are out of scope (see [AGENTS.md](AGENTS.md)).

Part of the **wealthdb** suite. See
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for shared collector
conventions.

## How it works

The portal is a **Q2 white-label platform** with a `mobilews` REST/JSON
API. **Akamai Bot Manager** gates the sign-in, so every run signs in
inside a Camoufox browser. Once signed in, the data comes over plain
REST, with no page scraping. 2FA is a code by text or voice call. The
device trust it sets persists across runs; the session does not.

- **`login`** (interactive) signs in and registers the device. On a
  trusted device it needs no 2FA. Otherwise it drives the Secure Access
  Code **from the terminal**: pick Text or Call, then type the code.
  `vnc-login` is the by-hand fallback.
- **`download`** (unattended) signs in on the trusted device. It fetches
  the deposit roster, per-account history, exports and statement PDFs
  into bronze. If the device trust has expired, it fails and points to
  `login`.
- **`load`** parses each bronze run into silver. The history JSON is the
  ledger: each row has a stable id and a running balance. The exports
  are kept as provenance only.

[DESIGN.md](DESIGN.md) §3 maps the API and §4.2 the login and 2FA
flow. [`chase`](../chase/) is the US-retail collector this one is
modelled on.

## Setup

```
make build-firstcitizens
```

Credentials go in `~/.secrets/firstcitizens.env` (chmod `0600`):

```
FIRSTCITIZENS_USERNAME='...'
FIRSTCITIZENS_PASSWORD='...'
```

## Usage

```
./firstcitizens login                     # sign in; 2FA from the terminal if needed
./firstcitizens login --check             # does the trusted device still skip 2FA?
./firstcitizens vnc-login --fresh         # forget the device; do the full 2FA by hand over VNC
./firstcitizens download                  # REST fetch into bronze (last ~90 days)
./firstcitizens download --lookback all   # complete history
./firstcitizens download --dry-run        # sign in, log the roster and transaction totals, fetch nothing
./firstcitizens load                      # bronze → silver
./firstcitizens explore                   # discovery harness over VNC
```

`login` needs a **real TTY** for the 2FA prompt. Each real session can
fire real 2FA, so run them deliberately and never in quick succession.
`download --dry-run` signs in too.

## Output

- Bronze runs land under `$XDG_DATA_HOME/wealthdb/firstcitizens/<UTC-ts>/`.
- Silver is `$XDG_DATA_HOME/wealthdb/firstcitizens/firstcitizens.db`,
  with three tables:
  - `accounts`: the deposit roster per snapshot (type, nickname, last-4
    mask, currency, current balance);
  - `transactions`: the ledger, keyed by the source's `transactionId`
    (signed amount, description, check number, running balance);
  - `documents`: the statement PDFs, keyed by content hash.
- `login` / `download` diagnostics (with `--debug`) and `explore`
  captures land under `~/.cache/wealthdb/debug/firstcitizens/<UTC-ts>/`.

The gold adapter (`wealthdb/internal/silver/firstcitizens/`) maps the
accounts to cash accounts. It builds a closing-balance series from the
running balance and adds the current roster balance, stamped at the
latest roster snapshot. The accounts are conduits, so their return rows
are hidden.
