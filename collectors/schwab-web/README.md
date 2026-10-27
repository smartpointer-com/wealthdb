# schwab-web

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to Charles Schwab with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
Charles Schwab offers no read-only sub-scope, so nothing but this
codebase's own discipline restricts the session to reading. If malicious
code were ever introduced into this repository, its dependency chain, or
the container images it runs, it could act on your accounts with your full
authority and cause **irreversible financial damage, up to the total loss
of the assets reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach Charles Schwab's
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
affiliated with, endorsed by, or sponsored by Charles Schwab or any other
financial institution; nothing in this repository is financial, legal, or
tax advice.

schwab-web reads the Charles Schwab data that the Trader API does not
cover. It drives the Schwab client site (`client.schwab.com`) under
Camoufox, a stealth-patched Firefox, and downloads:

- monthly and quarterly account statements (PDF);
- tax forms, plus the XML and CSV twins Schwab offers for the 1099
  Composite;
- letters, and reports & plans;
- Transaction History exports (CSV, JSON and XML), with each row's
  "More" detail.

`load` parses the downloads into a SQLite silver database. The wealthdb
Schwab adapter merges it with the [schwab-api](../schwab-api/) silver.
[DESIGN.md](DESIGN.md) is the merge contract, and
[INTEROP.md](INTEROP.md) its short form.

Part of the **wealthdb** suite. See [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold model and
[collectors/README.md](../README.md) for the shared collector
conventions. [AGENTS.md](AGENTS.md) lists the read-only surface.

## Why a web scraper

[`schwab-api`](../schwab-api/) reads the Trader API. It returns current
positions and balances, and up to a year of transactions per request.
It has no history of past snapshots: no as-of date, no statements.

The client site holds that history:

- statement PDFs for about ten years, with positions, market values
  and cash activity at each period end;
- annual tax forms, including the 1099 Composite and year-end
  realized gain and loss reports;
- Transaction History exports that reach further back than the Trader
  API.

No retail read API serves that history. The
[plaid](../plaid/README.md) collector reads balances, holdings and
transactions where Plaid covers Schwab. It reads no statements or tax
forms. Gold cannot tell two copies of one account apart, so a Schwab
login read here stays unlinked in Plaid
([adapters/plaid.md](../../wealthdb/docs/adapters/plaid.md)).
Schwab's "new document" emails carry no payload.

Gold reads this silver beside the `schwab-api` one, or on its own. On
its own, gold keys each account by its full account number. It holds
the statement-date positions and every statement and history
transaction, but no daily snapshots between statements
([adapters/schwab.md](../../wealthdb/docs/adapters/schwab.md) §7.4).

Each statement's cash interest rate lands beside its closing cash
balance. Gold reads it as `interest_rate`
([DESIGN.md §3.1](DESIGN.md#31-the-cashs-interest-rate)).

Trade confirmations are skipped on purpose: low signal, high volume.

## Sign-in and 2FA

Schwab runs Akamai Bot Manager and its own anti-bot rules. Stock
Playwright browsers are rejected at credential submit with "Invalid
login ID or password", even with valid credentials. That holds for
Chromium (headless or headed), patchright, upstream Playwright Firefox,
and cookies imported from a real Firefox. Camoufox in macOS mode
presents one consistent fingerprint at every layer and gets through.

Every fresh session meets a second factor: an SMS code, a voice call,
a push, or a security question, depending on the account's settings.
`download` prompts for a code on stdin, so the toolkit cannot run
unattended.

- A run submits **at most one code**. A rejected code shows Schwab's
  error verbatim and exits 8; a re-run mints a fresh challenge.
  Repeated submissions are the defect class that can lock an account
  (see [schwab-api's DESIGN.md §3.1](../schwab-api/DESIGN.md#31-loginpy--authentication-lifecycle)).
- A challenge left idle is fatal for that sign-in attempt. Schwab locks
  it behind an identity-verification banner and rejects every later
  code. The lock covers the attempt, not the account; a re-run
  recovers.
- A gateway notice page (`#/information/<code>`), such as an account
  lockout, aborts the run at once. The run shows the page's own
  message and exits 9. An account lockout must be cleared with Schwab
  directly; nothing here retries into it.

## Session lifecycle

Schwab kills the session as soon as the Firefox process that minted it
exits. Re-opening the same profile dir gets `SessionTimeOut=y`, however
fresh the cookies and `_abck` on disk are.

So sign-in and scrape share one continuous Firefox session. `download`
signs in, walks Statements & Tax Forms and Transaction History, and
exits. Each `download` pays one 2FA challenge. `login` is a no-op that
says so. `download --check` reports whether the stored profile is
alive; after Firefox has closed, it reports DEAD.

On a new IP, Schwab may add a device-trust prompt on top of 2FA the
first time. The profile dir under `/secrets/schwab-web-profile/` then
carries enough state for later sign-ins.

## Build

```sh
make build-schwab-web    # or: ./schwab-web build
make test-schwab-web
```

The image builds on the shared `wealthdb/base-camoufox`
([`shared/images/base-camoufox.Dockerfile`](../../shared/images/base-camoufox.Dockerfile)),
which pins Camoufox and the Playwright release that matches it.
`requirements.txt` adds `pypdfium2` for statement text extraction.

## Credentials

`login.py` reads two env vars inside the container:

- `SCHWAB_LOGIN_ID` — the Schwab Login ID. It identifies the customer,
  so treat it as sensitive.
- `SCHWAB_PASSWORD` — the Schwab login password.

They do not share a prefix with `schwab-api`'s `SCHWAB_CLIENT_ID` /
`SCHWAB_CLIENT_SECRET`, so the two credential sets never collide in one
shell.

Credentials go in `~/.secrets/schwab-web.env`; see
[collectors/README.md](../README.md#conventions-shared-across-collectors)
for the shared env-file rules. The login script reads
`/secrets/schwab-web.env`, and a value in the file wins over one
already set in the environment. When either variable is unset, the
form is not pre-filled, and Log In is driven by hand over `vnc-login`.
Nothing prompts for a password.

```sh
# ~/.secrets/schwab-web.env
SCHWAB_LOGIN_ID='your-login-id'
SCHWAB_PASSWORD='your-password-with-$pecial-chars'
```

## Run

```sh
./schwab-web download                         # statements + transactions, last 3 months
./schwab-web download --lookback all          # full backfill, capped at ~10 years
./schwab-web download --lookback 2016-01-01   # from a date
./schwab-web download --mode statements       # documents only
./schwab-web vnc-login                        # sign in by hand over VNC, then scrape
./schwab-web load                             # bronze → silver
./schwab-web prune --dry-run                  # preview the disk reclaim
```

`./schwab-web help` lists the verbs, and `download --help` the flags. A
few behaviours worth knowing:

- `--mode` is `statements`, `transactions` or `all` (the default).
- `--lookback` sets where the window starts; the default is 90 days
  back. A named preset (`1w` … `5y`, `all`) maps to the narrowest
  Schwab preset that covers it. An ISO date fills Schwab's custom
  date range on both pages, and falls back to the covering preset if
  the fill fails. Schwab's widest preset is ten years; a wider window
  is capped, with a warning.
- The Transaction History pass also opens each row's "More" detail by
  default. That costs about one click per transaction;
  `--no-more-detail` skips it ([DESIGN.md §7](DESIGN.md#7-known-gaps-not-blockers-for-the-gold-merge-but-worth-noting)).
- `--dry-run` signs in and walks the account and result pages without
  downloading a statement document. Its tx-history exports go to a
  temporary directory. Nothing is written under the bronze dir.
- A walk that finds no accounts exits 1. An interrupt (Ctrl-C)
  exits 130.
- `--debug` saves the tx-history landing page HTML under
  `<run>/screenshots/`. `load` never reads it.
- `--screenshot-dir /debug/<name>` saves login and landmark captures,
  and a DEBUG-level `run.log`, outside bronze. `--trace` and
  `--capture-bodies` need it and write there too. These files are
  unredacted: never commit them. The pinned Playwright tracer crashes
  the pinned Camoufox build, so `--trace` ends the login.

### VNC sign-in

`vnc-login` is the fallback when the 2FA selectors drift or a
challenge needs more than a code. Log In and 2FA are done by hand over
VNC, and the script then takes over the same page and scrapes. The
wrapper picks a free port in 5900-6000 and prints it:

```sh
./schwab-web vnc-login --screenshot-dir /debug/login-$(date +%Y%m%dT%H%M%SZ) -v
#   vnc-login: VNC ready on 127.0.0.1:5900
#   vnc-login: password (single-use):  <16 hex chars>
ssh -L 5900:127.0.0.1:5900 <host>    # from another machine
open vnc://localhost:5900            # macOS Screen Sharing
```

The wait for the human sign-in defaults to five minutes (`--mfa-timeout`).

### Mounts

The wrapper mounts `~/.secrets` as `/secrets` and
`$XDG_DATA_HOME/wealthdb/schwab-web` as `/data`
([collectors/README.md](../README.md#conventions-shared-across-collectors)).
It adds `/debug` (default `~/.cache/wealthdb/debug/schwab-web`) for
opt-in diagnostics. `SCHWAB_WEB_SECRETS_DIR`, `SCHWAB_WEB_DATA_DIR` and
`SCHWAB_WEB_DEBUG_DIR` override the host paths.

## Output

```
$XDG_DATA_HOME/wealthdb/schwab-web/
├── 20260520T120000Z/          one dump per download run
│   ├── run.json               manifest: the document inventory and the run status
│   ├── statements/<suffix>/   statements, tax forms, letters, reports
│   ├── transactions/<suffix>/ tx-history CSV / JSON / XML, more-details.json
│   └── screenshots/           only with --debug
└── schwab-web.db              silver SQLite
```

`<suffix>` is the account-number suffix Schwab shows beside the account
name. [DESIGN.md §6](DESIGN.md#6-bronze-artefact-layout-for-reference)
has the full layout and what `load` parses from each file. Inside the
container (`./schwab-web sh`), `python3 pdf_parsers.py <pdf>...` prints
a statement's parse as JSON.

## Reclaiming disk (`prune`)

```sh
./schwab-web prune --dry-run     # preview; deletes nothing
./schwab-web prune
```

`prune` follows the fleet rules in
[collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze).
It removes `<run>/screenshots/` from complete dumps, whole unfinished
dumps, and old entries of the `/debug` cache. For schwab-web it also
removes `transactions/*/page-*.html`: landing-page HTML that some run
dirs hold beside the tx-history exports. `load` never reads those
files, and the glob matches no `load` input. A run dir without a
status counts as complete unless its manifest says it was a dry run.
`prune` runs on the host, so it works while a `download` container
runs.

## Reclaiming disk (`collapse-statements`)

Schwab re-renders a statement PDF on **every** download. The same
statement comes back with new bytes each run, so the `statements/` tree
grows one full copy per run
([DESIGN.md §4.4](DESIGN.md#44-schwab-regenerates-pdfs-per-download--sha256-dedup-is-byte-level-only)).
The fleet's `wealthdb-collect dedup` sweep cannot share these, because
the bytes differ.

`collapse-statements` parses each statement PDF exactly as `load` does.
Within one logical statement, it hardlinks every copy that parses
identically onto the oldest copy. The newer copies' bytes are lost, but
silver is not: `load` keys `documents` off the manifest sha256 and
re-parses whatever bytes the file holds. `run.json` is never touched,
so `load --force` reproduces identical silver. Copies that do not parse
alike are reported as **DIVERGENT** and left alone.

```sh
./schwab-web collapse-statements --dry-run   # evidence report; collapses nothing
./schwab-web collapse-statements
```

It needs the PDF parser, so it runs in the container like `load`. It
is a manual one-off: run it once a backlog of re-downloaded statements
has built up. `--min-age-hours` (default 1) skips a dump still being
written. `--sample-text-diff K` shows, for K groups, that the copies'
text differs only in render metadata.
