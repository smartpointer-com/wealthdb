# erstebank_at

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to George
(george.sparkasse.at) with your user name and your s Identity
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

## Status

The collector is at its discovery stage. `explore` records a George
session signed in by hand over VNC. `login`, `download` and `load` are
not built yet; they exit 2. [DESIGN.md](DESIGN.md) lists what the
captures must answer before they are.

## Credentials

`~/.secrets/erstebank_at.env`, chmod `0600`:

```bash
ERSTEBANK_AT_USERNAME='example-user'
```

The sign-in is confirmed in the s Identity app.

## Example

```bash
collectors/erstebank_at/erstebank_at build
collectors/erstebank_at/erstebank_at explore
```

`explore` prints a VNC address. Sign in there and walk the flows it
prints. Closing the browser ends the recording. The captures land under
`~/.cache/wealthdb/debug/erstebank_at/<UTC-ts>/` and hold real account
data. `prune` reclaims them once they are an hour old:

```bash
collectors/erstebank_at/erstebank_at prune --dry-run
```
