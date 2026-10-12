# swissquote

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to Swissquote with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
Swissquote offers no read-only sub-scope, so nothing but this codebase's
own discipline restricts the session to reading. If malicious code were
ever introduced into this repository, its dependency chain, or the
container images it runs, it could act on your accounts with your full
authority and cause **irreversible financial damage, up to the total loss
of the assets reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach Swissquote's terms
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
affiliated with, endorsed by, or sponsored by Swissquote or any other
financial institution; nothing in this repository is financial, legal, or
tax advice.

A read-only collector for Swissquote Bank private-client e-banking. It
drives the e-banking web UI in headless Chromium (Playwright) and exports
positions, cash balances, transactions and eDocuments. `load` parses the
exports and PDFs into a queryable SQLite silver database, which the
`wealthdb` gold engine reads.

Part of the **wealthdb** suite. See [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold model and
[collectors/README.md](../README.md) for the conventions every collector
shares.

## Why the web UI

Swissquote offers no API through which a retail client can read its own
portfolio:

- **PSD2 / Open Banking** — TPP-only (eIDAS cert required).
- **OpenWealth / `bankingapi.swissquote.ch`** — B2B-only (External
  Asset Managers, family offices). Retail requests are typically
  declined.
- **FIX API** — covers only the Forex/CFD margin sub-account, not the
  bank-account portfolio (equities, ETFs, bonds, funds).
- **PSD2 aggregators (Plaid, TrueLayer, Tink, etc.)** — no Swissquote
  coverage. Powens has it but is B2B.
- **Email feeds** — "document available" notifications carry no
  payload. The detail is only in the PDF behind the e-banking login.

That leaves the e-banking UI. It offers CSV and XLS exports for
transactions and positions, and PDF downloads for eDocuments (trade
confirmations, statements, fee notes).

## Examples

```sh
./swissquote build                       # Docker image on the shared base-playwright image

./swissquote login                       # sign in; approve the Mobile Level 3 push
./swissquote login --check               # is the stored session alive? no push
./swissquote download --dry-run          # check session and selectors, export nothing
./swissquote download                    # last ~90 days (the fleet default)
./swissquote download --lookback 2y      # wider backfill
./swissquote load                        # parse new dumps into silver
./swissquote load --force                # delete silver, rebuild from all bronze
./swissquote prune --dry-run             # preview the bronze disk reclaim
```

Credentials live in `~/.secrets/swissquote.env` as `SWISSQUOTE_USERNAME`
and `SWISSQUOTE_PASSWORD`. `login --username NAME` overrides the
username. With no `SWISSQUOTE_PASSWORD`, `login` prompts for the password
with echo off. Each script's `--help` lists every flag.

## Session and MFA

- **Sign-in.** `login` opens the eBanking root
  (`/sqc-web-client-portal/`). The F5 BIG-IP gateway redirects it to the
  login form at `/my.policy`. `login` fills the form and waits for the
  **Mobile Level 3** page.
- **Operation number.** `login` prints the on-screen Operation No., so it
  can be compared with the phone before the push is approved.
- **Approval.** `login` watches the URL for the post-auth landing page,
  up to `--mfa-timeout` seconds (default 300). Every 60 seconds it
  re-opens the eBanking root as a backstop. If approval has not landed,
  F5 re-issues the push, and `login` prints the new code.
- **Skipped push.** Swissquote sometimes skips the push for a recently
  seen device fingerprint. `login` then completes at once.
- **Session state.** The browser context (cookies and localStorage) goes
  to `~/.secrets/swissquote-state.json`, chmod `0600`. A state file named
  `swissquote_state.json` is read when the canonical one is absent.
- **Lifetime.** Swissquote sets the session lifetime by policy. Expect a
  new sign-in at least once per working session. `download` has no
  refresh logic. When F5 bounces it to `/my.policy`, it exits and asks
  for a new `login`.
- **New device.** Swissquote fingerprints devices. The first sign-in from
  a new IP may add a "verify this device" step, such as an email link or
  an SMS code. `login --screenshot-dir DIR` saves a screenshot at each
  step, which shows what the page asks for.

Every fresh sign-in needs a tap on the phone, so runs are human-triggered.
`login --check` and `download --dry-run` reuse the stored session and
never sign in.

## What `download` fetches

Each run walks a fixed sequence of pages with the stored session:

1. **Session check and account list.** Opens the eBanking root and stops
   if F5 bounces to `/my.policy`. Scrapes `#accountOverview/main`, where
   each account reads `<PRODUCT> <ID>` (for example `Trading 1234567`),
   into `accounts.json`.
2. **Portfolio overview.** On the Trading Platform `#portfoliooverview`
   route it triggers three exports: Positions (`positions.xls`), List of
   Assets (`list_of_assets.xls`, cash per currency and FX rates against
   CHF), and Export account overview (`account_overview.pdf`). It then
   scrapes each position's long name (a hover tooltip) and ISIN (the
   FullQuote link) into `position_details.json`. The Positions export
   carries only the ticker.
3. **Transactions.** On `#transactions` it sets the Period filter to the
   `--lookback` window and saves one CSV. Swissquote enforces no window
   cap, so there is no chunking. The date inputs ignore a plain
   `.fill()`, so the walk sets them through React's native value setter.
4. **Documents.** On the eBanking `#documents` route it sets the Period
   filter to the same window. It reads every `getPdfDocument` link and
   fetches each new PDF through Playwright's request API with the session
   cookie. A document whose id already sits in a complete earlier dump is
   skipped.
5. **Manifest.** Writes `run.json` with the customer number, the account
   entries, the window, and per-document metadata.

Both XLS files use the legacy binary `.xls` format. `load` reads them
with `xlrd==1.2.0`, the last `xlrd` line that supports it.
`account_overview.pdf` stays in bronze; nothing parses it.

## Output

Bronze lands in `$XDG_DATA_HOME/wealthdb/swissquote/`:

```
20260101T120000Z/                one dump per run (UTC timestamp)
├── transactions_000.csv         one CSV for the whole --lookback window
├── positions.xls                Positions export (securities only)
├── position_details.json        long name + ISIN per position
├── list_of_assets.xls           cash per currency + FX rates
├── account_overview.pdf         portfolio-summary PDF (not parsed)
├── accounts.json                {account_product, account_external_id} per account
├── documents/<docid>.pdf        eDocuments
└── run.json                     manifest + status
manual/                          documents obtained outside the walk
swissquote.db                    silver SQLite
```

`manual/` takes documents obtained outside the walk, such as the annual
e-tax statement PDF, which is a paid extra. `load` indexes every new file
there on each run.

## Silver

The schema is `migrations/0001` to `0007`. Each dump loads in one
transaction, and already-loaded dumps are skipped.

- **Snapshots** (`accounts`, `positions`, `currency_balances`) are
  append-only, one row per snapshot and entity.
  - `accounts` inserts a row only when an account's payload changes.
    `account_product` comes from `accounts.json`. A dump without that
    file loads as one account keyed by the customer number, with an
    empty product.
  - `positions.name` and `positions.isin` come from
    `position_details.json`, joined on `(symbol, currency)`. They are
    NULL for a dump without that file.
  - `positions.source` is `live` for rows from the Positions export, and
    `pp:<doc_id>` for rows parsed from a **Portfolio Performance** PDF.
    Those PDFs are the year-end statements, and they give silver its
    historical snapshots. Re-parsing one replaces its rows. Cash lines in
    them are skipped; cash belongs in `currency_balances`.
- **Transactions** load per account and CSV as a span replace. The span
  runs from the CSV's first row to its last, not over the requested
  window, because an export can hold fewer days than requested. A reload
  converges on Swissquote's current data.
- **Documents** are indexed in `documents` by content sha256 and
  Swissquote's document id. The PDF stays on disk. The `payload` keeps
  the file name, document type, contract number, date and target user
  from the download URL. Only Portfolio Performance PDFs are parsed;
  Account Statement PDFs are cash ledgers and are indexed only.
  `account_overview.pdf` changes every run and is not indexed.
- **`manual/` files** are indexed with `source = 'manual'` when their
  sha256 is new.

Every position and transaction carries its own `currency`. Transaction
times are Europe/Zurich wall-clock in the CSV and stored as UTC epoch;
the original string stays in `payload`.

### `accounts.account_product`

The product label from `#accountOverview/main`. The scraper reads a
label that starts with a capital letter, including German diacritics,
followed by a 6- to 8-digit number. A label it cannot parse, such as one
that starts with a lowercase letter, is logged as a warning and left out
of `accounts`.

### Cost-basis columns

`positions` and `transactions` carry the figures a cost-basis reader
needs as columns (migrations 0006 and 0007). Each column holds the
source's figure as printed. Nothing is converted or rescaled. NULL means
the source states no figure. The `payload` keeps the full row.

`positions`:

- `average_cost` is the cost per unit, in the row's currency. A live
  row takes the export's "Unit cost". A statement row takes the
  Portfolio Performance "Average price".
- `price_quote` says how the row's prices are quoted: `unit`, or
  `percent` of nominal. The statement prints bond prices in percent.
  The live export prints every price per unit; a bond's unit is one
  unit of nominal.
- `market_value_chf` and `unrealized_gain_loss_chf` are the export's
  "Total value CHF" and "P&L Nominal CHF". Their difference is the
  position's CHF cost at the FX rates of its purchases.
- Statement rows leave the two CHF columns NULL. The statement states
  no P&L. Its CHF valuation stays in `payload`.
- `accrued_interest_chf` is a statement bond's accrued interest, the
  CHF amount printed on its own line under the bond's row, on top of
  the row's valuation. NULL on every other row.

`transactions` takes these from the CSV export, on every row type:

- `quantity`, `unit_price` and `accrued_interest`, as printed.
- `price_quote`: `percent` when the export marks the unit price with
  "%" (a bond trade), `unit` otherwise, NULL with no unit price.
- `fees`: the export's "Costs" cell.

A blank cell, or a placeholder with no digit, is NULL. A printed zero
stays 0.

## Run status and pruning

The fleet rules are in
[collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze).
The swissquote specifics:

- `load` skips a run dir with no `run.json`, or one whose `status` is
  not `complete`. A `run.json` without a `status` loads.
- A failed `download` deletes its own partial run dir. With `--debug` it
  keeps the dir, so the captures survive the failure they explain.
- `download --debug` saves DOM and screenshot captures in
  `<run>/screenshots/` at three landmarks: the portfolio overview, the
  transactions page, and the widened documents list.
- `prune` deletes run dirs that `load` skips, such as one a hard kill
  left behind or one `--debug` kept. From a complete dump it strips only
  `screenshots/`. It never touches `manual/`.
- `--screenshot-dir` and `--trace` write outside bronze, to the `/debug`
  mount. `--trace` needs `--screenshot-dir`.

## Caveats

- **UI churn.** A Swissquote redesign breaks selectors. The walk then
  fails with an error that names the broken landmark; it never writes an
  empty CSV. A `--trace` bundle helps with a report.
- **Pendo overlays.** In-app guide overlays can block the export
  buttons. `download` removes their DOM nodes first (see
  [AGENTS.md](AGENTS.md) §1).
- **CSV header drift.** Swissquote sometimes renames CSV columns while
  the UI stays the same. `load` fails on an unknown header rather than
  skipping it.
- **XLS trailing blank cell.** The XLS export sometimes emits a trailing
  blank header cell and sometimes not. The header checks ignore trailing
  blank cells but still fail on any change to a labelled column.
- **Several accounts per login.** The schema and code support them. No
  test fixture covers more than one account.

## Gold

The wealthdb Swissquote adapter reads this silver. It derives, for
example, `tax_wrapper` from `accounts.account_product` and joins
instruments on `positions.isin`. See
[the adapter doc](../../wealthdb/docs/adapters/swissquote.md).

## Read-only

See [AGENTS.md](AGENTS.md) for the pages the collector may open and the
ones it must never touch.
