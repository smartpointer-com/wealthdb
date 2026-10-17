# relevate

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates the provider's web client and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates Relevate / Pensexpert's own web client**: it
authenticates with your credentials and your multi-factor confirmations
and replays the portal's private REST API. The session it holds is **fully
privileged** — the same login a human uses to manage the account — and
Relevate / Pensexpert offers no read-only sub-scope, so nothing but this
codebase's own discipline restricts the session to reading. If malicious
code were ever introduced into this repository, its dependency chain, or
the container images it runs, it could act on your accounts with your full
authority and cause **irreversible financial damage, up to the total loss
of the assets reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach Relevate /
Pensexpert's terms of service; verifying that your use is permitted is
likewise your responsibility.

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
affiliated with, endorsed by, or sponsored by Relevate / Pensexpert or any
other financial institution; nothing in this repository is financial,
legal, or tax advice.

A read-only scraper for Relevate's vested-benefits (Pillar 2) customer
portal at `portal.pens-expert.ch`. It replays the portal's Airlock IAM
sign-in and its `/middlelayer/v2/` REST API from Python with `requests`.
There is no browser, no Xvfb and no VNC. `login` prompts for the SMS mTAN
on stdin.

`download` lands per-portfolio JSON and the document PDFs in a bronze
tree. `load` parses them into a SQLite silver database. The quarterly
reports give a position history older than the first download, and the
credit notes give the contribution events.

Part of the **wealthdb** suite. See [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold model and
[collectors/README.md](../README.md) for the conventions shared by every
collector. The closest sibling is [viac](../viac/) (Swiss Pillar 3a, a
similar REST + mTAN shape).

## Verbs

| Verb | What it does |
| --- | --- |
| `login` | Replays the Airlock sign-in, prompts for the SMS mTAN, and saves the cookie jar to `relevate-state.json` (chmod 0600). `--check` probes the session and prints `ALIVE` / `DEAD` / `MISSING`; it sends no mTAN. |
| `download` | Walks the REST API with the stored session and writes one bronze run dir. It never signs in; with no live session it exits and asks for `login`. |
| `load` | Applies the migrations and parses each new dump into silver. It skips a dump whose `run.json` status is set to anything but `complete`. Idempotent on `dump_runs`. |
| `prune` | Reclaims non-complete dumps and `download --debug` HTTP traces. |

`./relevate help` prints the full list, including `build` and `sh`.

## Examples

```sh
./relevate build                                   # build the image
./relevate login --check                           # session alive? no mTAN
./relevate login                                   # mint a session; sends an SMS mTAN
./relevate download --dry-run                      # read the two listings, log the plan, write nothing
./relevate download                                # bronze dump; documents from the last ~90 days
./relevate download --lookback all                 # every document in the archive
./relevate download --lookback 2020-01-01          # an ISO start date works too
./relevate download --no-documents                 # everything except the document index and PDFs
./relevate download --mode portfolios --limit-portfolios 1
./relevate load                                    # parse bronze into silver
./relevate prune --dry-run                         # print what prune would delete
```

`--mode {all,accounts,portfolios,documents}` and the `--limit-*` flags
re-run one phase cheaply while iterating.

`--lookback` sets the window start. The `/deposits` endpoint takes a
whole year only, so `download` asks for every year from the window's
start year to the current year. The document index always comes back
whole. `download` writes the full index to bronze, then fetches only the
PDFs whose `createDate` falls in the window. A PDF identical to one in an
earlier complete run is reused where that is safe; `--documents-force`
turns that off ([DESIGN.md §4.1](DESIGN.md#41-flow), step 7).

### Credentials

`login` reads two variables from `~/.secrets/relevate.env`
([env-file rules](../README.md#conventions-shared-across-collectors)):

- `RELEVATE_LOGIN` — the portal login (email or OASI number).
- `RELEVATE_PASSWORD` — the portal password.

### Configuration

The wrapper sources `${XDG_CONFIG_HOME:-$HOME/.config}/relevate.cfg` (or
`$RELEVATE_CFG`) when it exists. It is plain bash and can set:

```sh
RELEVATE_SECRETS_DIR=/path/to/secrets
RELEVATE_DATA_DIR=/path/to/wealthdb/relevate
RELEVATE_DEBUG_DIR=/path/to/cache/relevate-debug
RELEVATE_IMAGE=wealthdb/relevate:<tag>   # default: the checkout's image tag
RELEVATE_CONTAINER=relevate
```

### Docker on macOS

The collector runs in Docker. Under Colima, a `mounts:` list in
`colima.yaml` replaces the default `$HOME` share, so list your home
directory there explicitly.

## Output

```
$XDG_DATA_HOME/wealthdb/relevate/
├── 20260101T120000Z/              one bronze dump per run
│   ├── run.json                   manifest: status, windows, accounts, document counts, errors
│   ├── accounts/                  investment overview + small ancillary endpoints
│   ├── portfolios/<slug>/         per-portfolio JSON (slug = sha256 of the account number)
│   └── documents/                 index.json + <docId>.pdf
└── relevate.db                    silver SQLite
```

[DESIGN.md §6](DESIGN.md#6-bronze-layout) lists every bronze file.

The silver tables are `accounts`, `cash_balances`, `positions`,
`instruments`, `performance_points`, `transactions`, `documents`,
`historical_position_snapshots` and `historical_cash_balances`.
`positions` holds the model portfolio's target allocation. The
`historical_*` tables hold the units and values printed in the quarterly
reports. `transactions` holds the contributions read from the credit
notes. [DESIGN.md §7](DESIGN.md#7-silver-schema) lists their keys and
columns.

`prune` follows the
[fleet rules](../README.md#debug-artefacts-run-status-and-pruning-bronze);
the relevate specifics are in [DESIGN.md §6](DESIGN.md#6-bronze-layout).

## Read-only

Relevate's portal can change an investment strategy, change a
beneficiary and request a payout. This toolkit does none of that. Beyond
the sign-in it only reads. [AGENTS.md](AGENTS.md) §1 lists the allowed
surface.
