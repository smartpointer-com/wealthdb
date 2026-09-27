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
Bank retail banking portal: **deposit accounts** (checking + savings),
their statement PDFs, and their transaction history / exports. Cards,
lending, and any wealth-management / trust / brokerage surface the same
login may expose are out of scope (see [AGENTS.md](AGENTS.md)). Like the
other portal-only sources, it replays the browser flow via Camoufox —
the US siblings ([`chase`](../chase/), [`fidelity-web`](../fidelity-web/),
[`schwab-web`](../schwab-web/)) all measured Akamai-class bot defense
blocking vanilla browsers, so the harness builds on the shared Camoufox
base image from the start.

Part of the **wealthdb** suite — see
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for shared collector
conventions.

## Status: full pipeline through gold — `login` + `download` validated live

Discovery (2026-08-13) mapped the surface: First Citizens' digital
banking is a **Q2 white-label platform** exposing a clean `mobilews`
REST/JSON API, with SMS/voice 2FA and a **device-trust that persists**
across browser restarts (the session itself does not). **Akamai Bot
Manager** gates the logon, so — unlike a pure host-venv hybrid — the
logon runs in Camoufox every run; but once in, the data is fetched over
plain REST (no DOM scraping). `login` (trusted **and** untrusted
terminal-2FA paths) and `download` are validated live end-to-end
(2026-08-14/15): full paginated transaction history, CSV+QFX exports,
and every statement PDF. The pipeline splits into:

- **`login`** (Camoufox, interactive) — signs in and registers the
  device, leaving device-trust in the persistent profile. On a trusted
  device it runs unattended; when a Secure Access Code (2FA) is required
  it is driven **from the terminal** (pick Text/Call, enter the code),
  Chase-style — with `vnc-login` as a by-hand fallback.
- **`download`** (Camoufox, unattended) — the trusted device skips 2FA;
  fetches the deposit roster, per-account history, exports, and
  statement PDFs over REST into bronze.
- **`load`** (silver) — parses each bronze run into a source-shaped
  SQLite: the account roster, the transaction ledger, and the statement
  inventory. The `accountHistory` JSON is the authoritative ledger (a
  stable id **and** a running balance per row), so there is no
  export-join or statement-PDF backfill — the export deepens no further
  than the history, which reaches the account's full lifetime.

A **gold adapter** (`wealthdb/internal/silver/firstcitizens/`) projects
that silver into the canonical store on the chase model: cash accounts,
a closing-balance series from the per-row running balance plus a current
roster balance, and the whole deposit ledger. The cash accounts are
conduits, so the registered returns policy hides their return rows; the
coarse return grains keep the balances and count the flows.

The [`chase`](../chase/) collector is the US-retail model this one
adapts; see [DESIGN.md](DESIGN.md) §4.2 for the mapped flows and the
CLI-2FA design.

## Setup

```
make build-firstcitizens
```

Credentials go in `~/.secrets/firstcitizens.env` (chmod `0600`;
single-quote values containing `$`, `!`, or backticks):

```
FIRSTCITIZENS_USERNAME='...'
FIRSTCITIZENS_PASSWORD='...'
```

## Usage

```
./firstcitizens login              # sign in; 2FA from the terminal if needed; register the device
./firstcitizens login --fresh      # wipe device-trust first, forcing the full untrusted-device 2FA
./firstcitizens login --check      # is the device still trusted? (sends no code)
./firstcitizens vnc-login          # by-hand 2FA fallback over VNC
./firstcitizens download              # unattended: REST fetch → bronze (default window ~90 days)
./firstcitizens download --lookback all   # complete history (the initial backfill)
./firstcitizens download --dry-run    # enumerate roster + statements, export nothing
./firstcitizens load                  # bronze → silver SQLite

./firstcitizens explore            # discovery harness over VNC
```

`login` needs a **real TTY** for the 2FA prompt — run it in your own
terminal. On an already-trusted device it runs unattended (no 2FA); when
the device isn't trusted, it drives the Secure Access Code **from the
terminal** (pick Text/Call, then type the code), which registers the
device so subsequent `login` / `download` runs are unattended.
`vnc-login` is the by-hand fallback if a control ever drifts. If
device-trust has expired, `download` fails loudly telling you to run
`login`. `login` / `download` diagnostics (with `--debug`) and the
`explore` capture land under
`~/.cache/wealthdb/debug/firstcitizens/<UTC-ts>/`; bronze runs land under
`$XDG_DATA_HOME/wealthdb/firstcitizens/<UTC-ts>/`. Real sessions fire
real 2FA — run them only deliberately, never in quick succession.
