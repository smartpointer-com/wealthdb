# firstcitizens — design notes

Design notes for the First Citizens Bank retail collector. Sections:
scope (§1), the discovery harness (§2), the source's REST API (§3), and
the runtime (§4).

## 1. Scope

- **The relationship.** A retail deposit-banking relationship at First
  Citizens Bank (firstcitizens.com). **Scope is deposit accounts only**
  (checking + savings). Cards, lending, and any wealth-management,
  trust or brokerage surface the same login may expose are out of scope
  (see [AGENTS.md](AGENTS.md)).
- **The model is chase.** The [Chase](../chase/DESIGN.md) collector is
  the twin for a US retail deposit bank. This collector starts from
  chase and diverges only where the source differs.
- **Conduit accounts.** Cash passes through these accounts on its way to
  and from other sources. Their transactions matter for cross-source
  money-flow tracking. Their own return rows are noise, so the
  registered ReturnsPolicy (`internal/silver/firstcitizens/policy.go`)
  hides them, with chase's conduit reasoning.
- **The data surface is a Q2 REST API.** The digital banking runs on a
  **Q2 white-label platform** at
  `digitalbanking.firstcitizens.com/FCBTCOnline/`. Every datum arrives
  over a `mobilews` REST/JSON API: roster, history, exports, statements.
- **2FA is SMS or voice OTP.** There is no push, email or TOTP factor.
  **Device trust persists** across browser restarts and skips 2FA on
  later logons. The session cookie does not persist, so each `download`
  signs in again with the password only.

## 2. The discovery harness (`explore`)

`explore.py` runs the shared harness
([`collectorkit.explore`](../../shared/collectorkit/collectorkit/explore.py)),
which documents its artefacts and redaction. This collector adds only
what is particular to the source:

- the entry URL and a host gate, so the login pre-fill touches only
  firstcitizens.com frames;
- the credential env names and the login-form fill, which `login.py`
  reuses;
- the in-page detector script for clicks, the login form and the OTP
  field.

For this source the **HAR and network log are the decisive artefacts**.
The data is a clean REST API, mapped straight from the traffic. The
harness never submits a form; sign-in and 2FA are done by hand over VNC.
The opt-in `--trace` is off by default, because the pinned
Playwright/Camoufox pair crashes on tracing.

## 3. The Q2 `mobilews` API

The app is a hash-routed Q2 SPA (`uux.aspx#/…`). All data arrives over
`mobilews` REST/JSON endpoints. Account keys, document ids, phone digits
and balances live only in the debug dir, never here.

### Login + 2FA (flow 1)

The sign-in form sits in a homepage **modal** on `www.firstcitizens.com`,
opened by a "Log In" button. Its fields are classic light DOM
(`#login-form-0-id-textfield`, `#login-form-0-password-textfield`,
submit `button[name='db-login-button']`). Submitting it navigates into
`digitalbanking.firstcitizens.com`, a firstcitizens.com subdomain, so
the pre-fill host gate covers it. The SPA then drives a REST sequence:

1. `POST …/mobilews/preLogonUser` `{"userId": <user>}` is a pre-check.
2. `POST …/mobilews/logonUser` submits the password (in the request, not
   the JSON body). The response **status is the auth signal**:
   - **HTTP 203** with `accessCodeTargets` means **2FA required**. The
     targets are a list of `{notificationType, display, value}`, where
     **3 = SMS text** and **2 = voice call**. `pushAccessCodeTargets` is
     null.
   - **HTTP 200** with `accessCodeTargets: null` and a populated
     `userProfileData` means **authenticated, no 2FA** (trusted device).
3. `POST …/mobilews/accessCode` `{"accessCode": <target value>}` sends
   the code to the chosen target.
4. `POST …/mobilews/accessCode/validate` `{"accessCode": <typed OTP>}`
   submits the entered code.
5. `GET …/mobilews/registerDevice` is the "remember this device" step.
   It plants the device-trust cookie.

**Akamai Bot Manager gates only the logon.** `preLogonUser` and
`logonUser` carry Akamai sensor headers (`x-qjzesqau-*`, randomized
names). The `-f` payload is browser-generated telemetry. They also carry
`x-dtpc` (Dynatrace RUM). Camoufox produces these headers and clears the
login with no visible challenge. **Every other endpoint** carries no
sensor, only the session cookie and a `q2token`. The `q2token` is both a
cookie and a request header (the CSRF token). So:

- a plain host HTTP client cannot forge the sensor, and the logon must
  run in the browser;
- once signed in, the data comes over plain REST.

The **Secure Access Code (2FA) screens are Q2/Stencil web components**
with shadow DOM (`q2-input#tacEntry`, `q2-btn[test-id="btnSubmit"]`).
§4.2 describes how `login` drives them.

**Device trust persists; the session cookie does not.** A second logon
on the same profile re-runs `preLogonUser` + `logonUser` with the
password but fires no `accessCode` call. So:

- the **device registration** survives a browser restart, for a
  lifetime the source does not state;
- the **authenticated session** does not, and every run performs a fresh
  password logon;
- one interactive `login` mints the durable trust, and each unattended
  `download` signs in with the password only against it.

### Account statements (flow 2)

- **List:** `GET …/mobilews/accountStatement/<acctId>` returns
  `{"data": [{"period": "MM/DD/YYYY", "value": <docId>}, … ]}`, one
  monthly document per account. The archive is shallower than the
  transaction history.
- **PDF:** `POST …/mobilews/accountStatement/<acctId>/<docId>/pdf`
  returns `application/pdf`. The POST **requires the `q2token` as a
  multipart form field**, like the export. A header-only POST returns
  400.
- There are no combined-product statements, so chase's per-segment
  balance-chained attribution is not needed.
- `GET …/mobilews/accountStatement/form` returns the picker config. Tax
  documents hang off `…/mobilews/form/TaxDocument` (out of scope).

### Transaction history + exports (flow 3)

- **History:** `GET …/mobilews/accountHistory/<acctId>` returns
  `{"data": {"transactions": […], "transactionCount": N,
  "oldestTransactionDate": …}}`.
  - It is **paginated**: `page[number]` (1-based) and `page[size]` (SPA
    default 100), with `sort=postedDate%1Fd` (descending). The
    field/direction separator is a literal `US` (0x1F).
  - Without a filter it returns the **full history** back to
    `oldestTransactionDate`. For a migrated account that is its
    migration date.
  - It **narrows server-side to a `postedDate` window**:
    `postedDate=<from>\x1f<to>` in `M/D/YYYY`, with `23:59:59.999` on
    the `to` end. The account-detail "Custom Date" picker sends the same
    parameter, and `transactionCount` reflects the window. `--lookback`
    rides this parameter.
  - Each row carries a stable `transactionId`, `runningBalance`, signed
    `amount` + `isDebit`, `description`, `checkNumber` and `postedDate`.
- **Roster and detail:** `GET …/mobilews/accounts` is the roster, with a
  PFM overlay at `GET …/v2/pfm/accounts`. `GET …/mobilews/account/<acctId>`
  gives account detail.
- **Export:** `POST …/mobilews/accountExport/<acctId>/<Format>` where
  **`Format` ∈ {`Csv`, `Xls`, `Ofx`, `QFX_1_0_2`, `Qbo`}**. The request
  is `multipart/form-data` carrying **only a `q2token`**. It honours the
  same `postedDate` window as a URL query parameter. Without it, an
  export returns the full history back to `oldestTransactionDate`.
- **The JSON history is the ledger.** It carries a stable
  `transactionId` and a per-row `runningBalance` in one payload, so
  silver needs no export join. The CSV/QFX exports are provenance only.
  A flat export can omit a pending or edge row that the JSON keeps.
- **A memo-posted item is not loaded.** The JSON also returns items the
  bank memo-posted that day (`transactionType` `Memo`, no running
  balance). Once one clears, it comes back as a `History` row under a
  new `transactionId`. Silver keyed on that id would hold both copies,
  so only posted history is the ledger. A memo-posted item joins it on
  the run after it clears.
- **The `transactionId` is not always stable.** A movement in the open
  statement cycle can come back under a fresh id on every run. The core
  banking system's `hostTranNumber` does not move. So a row whose host
  number, posting date and amount match a stored row under another id is
  the same movement, and the id first seen keeps it.
- **Statements give no transactions.** The history reaches further back
  than the statement archive, so there is no older tail to rebuild from
  PDFs. Chase's statement transactions and balance-chained attribution
  are not ported. A statement is read for its interest rate alone
  (§4.3).

## 4. Runtime

### 4.1 Docker (Camoufox), with REST data

A host-venv REST download is ruled out by §3. The Akamai sensor on the
logon is browser telemetry a plain client cannot forge, and the session
cookie does not survive a browser restart. So **every run signs in
inside Camoufox**. Once signed in, the data comes over the browser
context's request API (`context.request`) with the harvested `q2token`.
There is **no DOM scraping** of transactions, unlike chase.

The verbs split along the persistence in §3: `login` mints the durable
device trust, and `download` signs in with the password against it.

### 4.2 `login` and `download`

**`login` (Camoufox, interactive).** It opens the modal, pre-fills and
submits the form. The SPA and Akamai JS then make the sensor-bearing
`preLogonUser`/`logonUser` calls. A form that did not pre-fill is never
submitted, since a blank submit counts as a failed sign-in. `vnc-login`
then leaves Sign In to the person at the VNC display.

- **Auth is detected by polling the SPA route.** The pinned Camoufox
  drops `.on()` response events across navigations, so no lone response
  event decides the outcome. A trusted device routes to `#/landingPage`.
  An untrusted device routes to `#/login/mfa/*`.
- **The REST probe waits until the SPA leaves `/login*`.** A
  `GET accounts` probe backs up the route detection. Fired during the
  login→MFA transition, it poisons the challenge session, and the
  delivery-button click no longer advances. So the probe fires only off
  a `/login*` route, where it is a harmless authed-state fallback. An
  authentication-progress probe must never touch an authenticated-only
  endpoint mid-challenge.
- **Trusted device:** signed in, nothing more to do.
- **Untrusted device:** the Secure Access Code is **driven from the
  terminal** ([`auth_dialog.py`](auth_dialog.py)).
  - The delivery methods come from the on-screen
    `q2-btn[test-id="btnTacTarget"]` buttons. The `logonUser` response
    is not used, because the pinned Camoufox may drop it.
  - The human picks Text or Call. The code is read from stdin, typed into
    `q2-input#tacEntry` and submitted via `q2-btn[test-id="btnSubmit"]`.
    `btnRegister` then registers the device.
  - These are Stencil shadow-DOM controls. Every click targets the
    **inner `<button>`** of the `q2-btn`. A click on the `q2-btn` host
    reports actionable, but the Stencil handler never fires.
  - `vnc-login` (`--no-cli-mfa`) is the by-hand fallback. `--fresh`
    wipes the profile to force this untrusted path.

The **persistent Camoufox profile** holds the device trust for later
unattended runs. A small `firstcitizens-state.json` in the secrets dir
records the last successful login. `login --check` submits the form on
the trusted device and reads the route. It sends no code, so it fires no
2FA. Exit `0` means the device is still trusted.

**`download` (Camoufox, unattended).** It pre-fills and submits the same
form. On a trusted device `logonUser` returns **200** with no 2FA. A
**203** means the device trust has expired. `download` then **fails
loudly** with a pointer to `login`, since it has no terminal to answer a
challenge.

Once signed in, it harvests the `q2token` and fetches:

- `GET accounts`, kept to `hydraProductTypeCode == "D"` deposit rows;
- per account, the **paginated `accountHistory`**, walked to
  `transactionCount` (§3);
- each requested `accountExport/<acct>/<Format>` (default `csv` + `qfx`;
  `--format` picks others);
- the statement list and each PDF (multipart `q2token`, §3), unless
  `--no-documents`.

Bronze per run dir follows chase's layout:

```
<run>/
  run.json                      status manifest
  accounts.json                 deposit roster
  history/<acct>.json           merged accountHistory pages (the ledger)
  transactions/<acct>.<fmt>     activity exports (provenance)
  statements/<acct>/<period>.pdf
  raw/*.json                    raw roster and listing bodies
```

**Bounded collector.** `--lookback` narrows the fetch server-side. The
`postedDate` window rides the history and export URLs (§3), and
statements are filtered by period. The default window (≈ 90 days) suits
routine refreshes. `--lookback all` pulls the complete history.

**`--dry-run` signs in.** It records the roster and logs each account's
transaction total from one history page. It fetches no history, exports
or statements, and stamps the manifest `dry-run`.

### 4.3 `load`

`load` parses each bronze run into SQLite silver (`migrations/`). It
reads the **JSON `history/<acct>.json` as the authoritative ledger**:

- the stable `transactionId` as the row id;
- the signed `amount` (magnitude × `isDebit`);
- the per-row `runningBalance`, so every row has a balance;
- the description and check number.

The CSV/QFX exports are not read. Statements give no transactions. The
silver schema is column-compatible with chase's, so the two gold
adapters run in parallel.

`statement_parser.py` reads each statement's `pdftotext -layout` text
for its cycle and its rate lines, into `statement_rates`:

- "Interest Rate on Statement Day" is the rate. An interest checking
  statement prints it as the sentence "YOUR INTEREST RATE WAS ...%"
  instead;
- "Annual Percentage Yield Earned" checks it. The yield must lie
  between the rate and the rate compounded daily, each widened by
  0.01 points of rounding. Outside that band the loader warns and keeps
  the rate. The rate moved during the cycle, or a line was misread;
- a statement that prints neither has no row.

The bank renders a statement anew on every download. The first copy
read writes the cycle's row. A copy with other figures is kept out with
a warning. `parsed_statements` marks each document read, so a load
reads only new documents. A moved parser generation reads them all
again.

The roster keeps a row only when an account changed. The
`run_accounts` table records the accounts each run listed, so a run
that still listed an unchanged account is visible.

### 4.4 Gold adapter

`wealthdb/internal/silver/firstcitizens/` follows chase's model:

- cash accounts (`AccountKind` cash, `taxable_personal` /
  `self_directed`);
- a closing-balance series from the per-row running balance, each
  stamped at its day's last second;
- a current balance from the roster at each run that listed the account
  (`run_accounts`). An account the bank stops listing has no balance
  after its last listing run, so gold can end it;
- every transaction: interest and fee recognised from the description,
  else deposit or withdrawal by sign;
- each statement's rate, as `interest_rate`, `interest_side` `credit`
  and `interest_valid_from` the statement's closing day, on a closing
  balance of its cycle: the one on the closing day, else the latest
  before it. The rate holds from the closing day even where its balance
  is an earlier one. A cycle in which the balance never moved has no
  closing balance, and its rate is not used.

The adapter registers via a blank import in `cmd/wealthdb/main.go` and
the `silver_sources` migration
(`internal/gold/migrations/0034_silver_sources_firstcitizens.sql`). Its
return rows are hidden per §1.
