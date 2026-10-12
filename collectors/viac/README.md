# viac

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates the provider's web client and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates VIAC's own web client**: it authenticates
with your credentials and your multi-factor confirmations and replays the
portal's private REST API. The session it holds is **fully privileged** —
the same login a human uses to manage the account — and VIAC offers no
read-only sub-scope, so nothing but this codebase's own discipline
restricts the session to reading. If malicious code were ever introduced
into this repository, its dependency chain, or the container images it
runs, it could act on your accounts with your full authority and cause
**irreversible financial damage, up to the total loss of the assets
reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach VIAC's terms of
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
affiliated with, endorsed by, or sponsored by VIAC or any other financial
institution; nothing in this repository is financial, legal, or tax
advice.

A read-only scraper for [VIAC](https://viac.ch)'s Pillar-3a customer
portal at `app.viac.ch`. It replays the portal's auth flow and its
`/rest/web/` REST API from Python with `httpx`. There is no browser, no
Xvfb and no VNC. `login` prompts for the SMS mTAN on stdin.

`download` lands per-portfolio JSON and the document PDFs in a bronze
tree. `load` parses them into a SQLite silver database. The period-end
statement PDFs give a position history older than the first download
([DESIGN.md §5.1](DESIGN.md#51-historical-positions-from-the-reporting-pdfs)).

VIAC offers no retail read API, so the portal's own backend is the only
channel ([DESIGN.md §1.1](DESIGN.md#11-why-a-scraper)). Every fresh login
needs an SMS mTAN, so runs are human-triggered.

Part of the **wealthdb** suite. See [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold model and
[collectors/README.md](../README.md) for the conventions shared by every
collector. The closest sibling is [relevate](../relevate/) (Swiss vested
benefits, a similar Airlock-shaped auth stack).

## Verbs

| Verb | What it does |
| --- | --- |
| `login` | Replays the auth flow, prompts for the SMS mTAN, and saves cookies + CSRF metadata to `viac-state.json` (chmod 0600). `--check` probes the session; it sends no SMS. |
| `download` | Walks the REST API with the stored session and writes one bronze run dir. It never signs in. |
| `load` | Parses bronze into silver. Idempotent on `dump_runs`. |
| `prune` | Reclaims non-complete dumps and `download --debug` HTTP traces. |

## Examples

```sh
./viac build                               # build the image
./viac login --check                       # session alive? exit 0 or 1; no SMS
./viac login                               # mint a session; sends an SMS mTAN
./viac download --dry-run                  # walk the JSON endpoints; write nothing
./viac download                            # bronze dump; documents from the last ~90 days
./viac download --lookback all             # every document in the archive
./viac download --no-transaction-documents # skip the per-event receipt PDFs
./viac load                                # parse bronze into silver
./viac prune --dry-run                     # print what prune would delete
```

`--lookback` bounds the documents and the transactions. The documents walk
fetches only the PDFs whose `timestamp` falls in the window. The REST
transactions call always returns the full history. Bronze keeps all of it,
and `load` applies the window recorded in `run.json`. A dump whose
`run.json` has no window loads every transaction. The historical positions
of DESIGN.md §5.1 need the old statements, so a first run uses
`--lookback all`.

The document centre always downloads inside the window.
`--no-transaction-documents` skips only the per-event receipt PDFs
(TRADE_REPORT, DIVIDEND, FEE_CHARGE, INTEREST, DIVIDEND_CANCELLATION).
`SECURITY_FUSION` PDFs always download, because they hold the only old→new
ISIN map. Identical PDFs from earlier runs are reused where that is safe;
`--documents-force` turns that off
([DESIGN.md §5](DESIGN.md#5-bronze-layout)).

### Credentials

`login` reads two variables from `~/.secrets/viac.env`
([env-file rules](../README.md#conventions-shared-across-collectors)):

- `VIAC_LOGIN` — the mobile number in **E.164 form, with the country
  code** (e.g. `+41XXXXXXXXX`). The web UI hides the country code behind a
  drop-down; the API does not.
- `VIAC_PASSWORD` — the VIAC login password.

## Output

```
$XDG_DATA_HOME/wealthdb/viac/
├── 20260101T120000Z/                    one bronze dump per run
│   ├── run.json                         manifest: status, timestamp, flags, windows, document counts
│   ├── customer.json                    /rest/web/customer/current/<N-N>
│   ├── wealth/
│   │   ├── portfolio-inventory.json     master list (p3a + pvb + inv)
│   │   ├── summary.json                 daily NAV time series across all wealth
│   │   └── allocation.json              cross-portfolio allocation breakdown
│   ├── positions/<portfolio-num>/       p3a portfolios only
│   │   ├── strategy.json                strategy / risk level / custody bank
│   │   ├── assets.json                  current holdings per fund
│   │   └── fees.json                    fee config
│   ├── transactions/all.json            every transaction, keyed by portfolio
│   └── documents/
│       ├── index.json                   document catalogue (one entry per document)
│       └── <docid>.pdf                  PDF binaries
└── viac.db                              silver SQLite
```

The silver tables are `accounts`, `positions`, `cash_balances`,
`instruments`, `transactions`, `wealth_history` and `documents`.
[DESIGN.md §6](DESIGN.md#6-silver-schema) lists their keys and columns;
§7 lists the columns the gold adapter reads.

`prune` follows the
[fleet rules](../README.md#debug-artefacts-run-status-and-pruning-bronze);
the viac specifics are in
[DESIGN.md §5.2](DESIGN.md#52-run-status--reclaiming-disk).

## Read-only

VIAC's portal can start a contribution, change a strategy, change a
beneficiary and request a withdrawal. This toolkit does none of that.
Beyond the auth flow it only reads. [AGENTS.md](AGENTS.md) §1 lists the
allowed surface.
