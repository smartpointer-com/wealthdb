# carta

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to Carta with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
Carta offers no read-only sub-scope, so nothing but this codebase's own
discipline restricts the session to reading. If malicious code were ever
introduced into this repository, its dependency chain, or the container
images it runs, it could act on your accounts with your full authority and
cause **irreversible financial damage, up to the total loss of the assets
reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach Carta's terms of
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
affiliated with, endorsed by, or sponsored by Carta or any other financial
institution; nothing in this repository is financial, legal, or tax
advice.

A read-only collector for [carta.com](https://carta.com), the
private-investment / cap-table platform. It reads a **portfolio holder's**
own holdings:

- private-company equity: share certificates, stock options (strike and
  vesting), RSUs / RSAs, warrants, SAFEs and convertible notes;
- the per-grant exercise details (date, shares, strike, 409A FMV at
  exercise);
- fund-LP interests: the capital account and capital calls;
- the document archive: K-1s, capital-account statements, capital-call and
  distribution notices, financials.

These holdings have no ticker and no quoted price. Carta's public REST API
is invite-only and not open to individual holders, so the collector signs
in through the web app with Camoufox and replays the app's internal
REST/JSON calls. [DESIGN.md](DESIGN.md) §1 has the path investigation.

Part of the **wealthdb** suite. See
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for the conventions
shared by every collector.

## Verbs

| Verb | What it does |
| --- | --- |
| `login` | Signs in through the two-step web form and reads the 2FA code from stdin. The session lives in a persistent Camoufox profile. `--check` probes it without a 2FA push. |
| `download` | Walks the internal REST/JSON API with the stored session, GET only. Captures both holding families and the document archive into one bronze run dir. |
| `load` | Parses bronze into the SQLite silver as event-dated change deltas ([DESIGN.md §5.1](DESIGN.md#51-event-driven-change-deltas-migration-0002)). Idempotent. |
| `prune` | Reclaims `screenshots/` from complete dumps, whole non-complete dumps, and the `explore` debug dir. |
| `explore` | A discovery harness. It opens Camoufox over VNC and records a human-driven session (HAR, network log, click log). Re-run it when Carta moves its UI or endpoints. |

Document fetches reuse identical copies from earlier runs where that is
safe ([DESIGN.md §3](DESIGN.md#3-discovery-via-explore)).
`--documents-force` turns that off. `--no-documents` skips the documents
pass, the run's dominant cost, and the manifest records `skipped=true`.

## Quick start

```sh
# 1. Build the image (on top of the shared base-camoufox image).
./carta build

# 2. Put the credentials in ~/.secrets/carta.env (chmod 0600):
#      CARTA_USERNAME=login-email
#      CARTA_PASSWORD=password

# 3. Mint the session. Prompts once for the 2FA code on stdin.
./carta login

# 4. Pull a bronze dump, then parse it into silver.
./carta download
./carta load
```

Cheaper runs:

```sh
./carta login --check          # exit 0 if the session is valid, 1 if not
./carta download --dry-run     # list the held entities; write nothing
./carta download --no-documents
./carta load --force           # delete silver and rebuild from all bronze
./carta prune --dry-run        # print what prune would delete
```

`download --dry-run` uses the stored session and never signs in.
`explore --fresh` wipes the profile first, so the next `login` meets the
full 2FA challenge.

Carta refreshes most data once a day, around noon US Eastern time. A
second run on the same day adds nothing.

## Output

Bronze lands in `$XDG_DATA_HOME/wealthdb/carta/<UTC-ts>/`; the layout is in
[DESIGN.md §4](DESIGN.md#download--per-surface-bronze-capture). Silver is
`carta.db` beside it. Its tables:

- `entities`, `securities`, `vesting_schedules`, `vesting_events`;
- `fund_metrics`, `cap_calls`;
- `capital_events`, the reconstructed event timeline;
- `cash_flows`, the dated money ledger;
- `documents` and `k1_capital_accounts`.

[DESIGN.md §5](DESIGN.md#5-silver-schema) describes the grain and columns
of each. Optional per-company CSVs in the bronze root override the
valuation and the exit legs (§5.1, §5.2). The gold mapping is in
[`docs/adapters/carta.md`](../../wealthdb/docs/adapters/carta.md).

`prune` follows the
[fleet rules](../README.md#debug-artefacts-run-status-and-pruning-bronze).
From a complete dump it removes only `screenshots/`, the landing-page DOM
and screenshot that `download --debug` writes.

## Read-only

The Carta holder UI can exercise options, sell or transfer shares, and
move money. A login can also reach issuer or fund-admin consoles. All of
that is out of scope. [AGENTS.md](AGENTS.md) lists the allowed and
forbidden surfaces. Company names, share counts, strikes, FMVs, vesting
schedules and tax documents are private and never enter the repo.
