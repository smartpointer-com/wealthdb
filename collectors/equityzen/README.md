# equityzen

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to EquityZen with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
EquityZen offers no read-only sub-scope, so nothing but this codebase's
own discipline restricts the session to reading. If malicious code were
ever introduced into this repository, its dependency chain, or the
container images it runs, it could act on your accounts with your full
authority and cause **irreversible financial damage, up to the total loss
of the assets reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach EquityZen's terms of
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
affiliated with, endorsed by, or sponsored by EquityZen or any other
financial institution; nothing in this repository is financial, legal, or
tax advice.

A read-only collector for [equityzen.com](https://equityzen.com), the
pre-IPO secondary marketplace. It reads a **buyer's** own holdings. Each
holding is an offering in one company, held through a **special-purpose
vehicle (SPV)** that owns the underlying pre-IPO shares. Multi-company
funds appear the same way. Per offering it captures:

- basis, share count, status and the closed-deal prices that mark it;
- the purchase and distribution cash-flow ledger;
- the offering's documents, such as capital-account statements and K-1s.

This is private, illiquid equity. It has no ticker, no quoted price, and
an SPV or partnership wrapper. The data has the same shape as the
[`angellist`](../angellist/) LP collector and is a sibling of
[`carta`](../carta/).

EquityZen publishes no API for individual investors. Its portal is a
React SPA backed by a buyer-scoped GraphQL API (`POST /api/graphql/`),
authenticated by the ordinary session cookie. The collector signs in with
Camoufox (password plus TOTP) and reads the responses the SPA's own read
queries return. [DESIGN.md](DESIGN.md) records the API investigation, the
GraphQL surface, and the bronze, silver and gold mapping.

Part of the **wealthdb** suite. See
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for the conventions
every collector shares.

## What it does

| Verb | What it does |
| --- | --- |
| `explore` | Discovery harness. Opens the portal in Camoufox over VNC and records a redacted HAR, a network log and a click log. It pre-fills the login form but never submits it. |
| `login` | Signs in with email and password, then reads the 6-digit TOTP code from stdin. The Camoufox profile keeps the session, so a later run with a live session returns without a 2FA prompt. |
| `download` | Clicks the portfolio's stage tabs (Ongoing, Closed, Exited), captures `getBuyerInvestments` per stage, then opens each offering and captures `getMyInvestmentDetails`. Fetches each offering's documents by default. |
| `load` | Parses bronze into the silver SQLite: offerings, event-sourced positions, cash flows, document metadata, parsed capital-account statements and K-1s. |
| `prune` | Reclaims bronze disk with the shared prune engine. |

`download` always reads the full history. It accepts `--lookback` and
warns that the flag has no effect.

## Examples

```sh
# Build the image (on the shared base-camoufox image).
./equityzen build

# Credentials: ~/.secrets/equityzen.env, chmod 0600.
#   EQUITYZEN_USERNAME='buyer@example.com'   # alias: EQUITYZEN_EMAIL
#   EQUITYZEN_PASSWORD='...'

# Mint the session. Prompts once for the TOTP code.
./equityzen login

# Pull a bronze dump, documents included.
./equityzen download

# Parse bronze into silver.
./equityzen load
```

Checks that send no TOTP prompt and write no data:

```sh
./equityzen login --check          # exit 0 if the session is valid, 1 if not
./equityzen download --dry-run     # read the offerings list, write nothing
./equityzen prune --dry-run        # print the deletion plan
```

Other useful flags:

- `login --totp CODE` passes the code without a prompt.
- `download --no-documents` skips the document fetch.
- `download --documents-force` re-fetches every document (see below).
- `load --force` deletes silver and rebuilds it from all bronze.
- `explore --fresh` wipes the profile, so the next `login` asks for TOTP.

Marks change only on a closed transaction or a new statement, so a
weekly or monthly run is enough.

## Documents

`download` fetches each offering's document PDFs and zip bundles
through the signed-in session. Re-runs avoid needless fetches with the
shared `collectorkit.docdedup` engine:

- Parsed documents can be restated under the same id. Statements, K-1s
  and financial reports are therefore always fetched and compared. An
  unchanged copy is hardlinked to the prior one; a changed one is kept.
- Signed legal and offering documents never change and are never parsed.
  An identical prior copy is hardlinked in and the fetch is skipped.

[DESIGN.md §4](DESIGN.md#4-login-and-download) lists the document
classes in each group.

## Output

Bronze lands in `$XDG_DATA_HOME/wealthdb/equityzen/<UTC-ts>/`:

- `investments.json`: the `getBuyerInvestments` body per stage;
- `offerings/<deal-slug>/detail.json`: `getMyInvestmentDetails` per
  offering;
- `documents/<deal-slug>/<doc-slug>.pdf`: the document blobs;
- `run.json`: the manifest, with slugs and counts only.

A deal slug is a hash of the deal id, so no company name appears in a
path. Silver is `$XDG_DATA_HOME/wealthdb/equityzen/equityzen.db`, with
the tables `offerings`, `positions`, `cash_flows`, `tax_documents`,
`capital_account_statements` and `k1_documents`.
[DESIGN.md §5](DESIGN.md#5-silver-schema) describes each table, and
[§6](DESIGN.md#6-gold-adapter) the gold adapter that reads them. A gold
run includes the source once `wealthdb.cfg` lists this silver DB under
`silver_sources`.

## Run status and pruning

`load` skips a run dir with no `run.json`, or one whose `status` is not
`complete`. A `run.json` without a `status` loads. `prune` deletes those
skipped run dirs, and strips `screenshots/` (the `download --debug`
captures) from complete dumps. The shared rules are in
[collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze).

## Read-only

See [AGENTS.md](AGENTS.md). EquityZen is a **live marketplace**. The
portal exposes order placement, Express-Deal sell, reserve and IOI,
funding, and e-sign. All of them are out of scope. The collector sends
only read GraphQL queries plus the two sign-in mutations. Company names,
SPV and fund names, share counts, basis and FMV figures, and K-1
contents are private and never enter the repo.
