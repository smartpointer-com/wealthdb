# firstcitizens — design notes

Discovery-phase notes for the First Citizens Bank retail collector.
Everything here predates real captures — the `explore` harness exists
precisely to replace these assumptions with traces. Sections: scope and
what is known (§1), the harness itself (§2), what Phase 1 must capture
(§3), the phase roadmap (§4), and the handoff checklist (§5). As
captures land, each §3 question gets an "Observed" section recording
the measured answer, the way [chase](../chase/DESIGN.md) did.

## 1. Scope & what is known so far

- **The relationship.** A retail deposit-banking relationship at First
  Citizens Bank (firstcitizens.com). **Scope is deposit accounts only**
  (checking + savings); cards, lending, and any wealth-management /
  trust / brokerage surface the same login may expose are out of scope
  in code and docs (see CLAUDE.md).
- **The playbook is chase.** The Chase collector is the validated twin
  for a US retail deposit bank — one-shot browser login with
  terminal-driven 2FA, CSV+OFX-class exports joined in silver,
  statement-PDF backfill behind the export cap, balance-chained
  attribution, cash accounts as conduits in gold. Every design choice
  below starts from chase and diverges only where a capture says the
  source differs.
- **Conduit accounts.** The cash accounts are conduits — cash passes
  through them on its way to and from other sources. Their transactions
  matter for cross-source money-flow tracking; per-account returns are
  meaningless and are blanked by the registered ReturnsPolicy
  (`internal/silver/firstcitizens/policy.go`), reusing chase's
  conduit reasoning as-is.
- **The data surface is a clean Q2 REST API (measured §3).** The digital
  banking runs on a **Q2 white-label platform** at
  `digitalbanking.firstcitizens.com/FCBTCOnline/`, and every datum —
  roster, history, exports, statements — arrives over a `mobilews`
  REST/JSON API. So the data path needs no browser; only the interactive
  login does (§4). This is what makes a lightweight `schwab-api`-style
  hybrid the right runtime, rather than chase's full browser scrape.
- **Bot defense: Dynatrace RUM + Akamai mPulse (measured §3).** Camoufox
  cleared the login end-to-end with no visible challenge; the harness
  builds on `shared/images/base-camoufox.Dockerfile` for the login image.
  Whether a *headless* client can complete the password re-logon is the
  one open Phase-2 risk (§4).
- **2FA: SMS + voice OTP, no push (measured §3).** `logonUser` returns a
  data-driven factor menu (`accessCodeTargets`: SMS / voice); Phase 2
  drives it from the terminal via an auth_dialog-style tested CLI core
  (chase's `auth_dialog.py` is the reference). **Device-trust persists**
  across browser restarts and skips 2FA on later logons — but the
  session cookie does not, so each `download` still logs in
  password-only (§3, §4).

## 2. The discovery harness (`explore`)

`explore.py` is copied and adapted from `chase`'s — the most complete
of the explore harnesses. For First Citizens the **HAR / network log was
the decisive artefact** (the data is a clean `mobilews` REST API mapped
straight from the traffic); the DOM snapshots mattered less here than at
chase (whose login/2FA hid in a cross-origin iframe), but both are cheap
to keep and the harness stays a faithful copy. The duplication across
the explore harnesses is a tracked, deliberate decision: each drifts
with its source, so they are copied and adapted, not extracted into a
library.

One run records, under `/debug/<UTC-ts>/` (host:
`~/.cache/wealthdb/debug/firstcitizens/`):

| Artefact | Purpose |
| --- | --- |
| `network.har` | The primary endpoint map — every request + response. Flushed only on a clean context close. |
| `network.jsonl` | Crash-safe line-flushed twin of the HAR; text bodies ≤ 200 KB captured inline, OFX/QFX content types included. |
| `clicks.jsonl` | Click log via an injected `document.addEventListener` (VNC clicks bypass the Playwright API), plus lifecycle, login-form and OTP-field events. |
| `dom/<NNN>/` | **Every distinct screen's full DOM** (all firstcitizens.com frames) + a screenshot, deduped by DOM structure — the record selectors are pinned from. |
| `downloads/` | Every file the session fetches (statement PDFs, exports), sequence-prefixed against reused filenames. |
| `trace-chunks/`, `trace.zip` | Opt-in `--trace` Playwright trace — off by default because the pinned Playwright 1.49 tracer crashes the camoufox 152.0.4 build (matched-set drift; see base-camoufox). |

Mechanics carried over from chase (see that harness's DESIGN.md §2 for
the full rationale): frame-aware login pre-fill gated to
firstcitizens.com frames, both-fields-in-one-frame before either is
touched, fill-once with read-back verification, `signon.*` prefs off so
a profile-saved credential can never autofill on top of the
programmatic fill, an OTP-field detector that logs the field's static
descriptor (never its value), and username/password redaction across
every logged header and body. Sign-in and 2FA are submitted by hand
over VNC; the harness never clicks a button.

## 3. Observed — authenticated capture (2026-08-13)

Two VNC-driven `explore` sessions: one full login + data walk, and one
re-login probe for session persistence. The digital-banking app is a
**Q2 white-label platform** ("uux") served at
`digitalbanking.firstcitizens.com/FCBTCOnline/`, and — unlike chase's
opaque SPA — all data arrives over a **clean `mobilews` REST/JSON API**.
Endpoints below are masked (account keys, doc ids, phone digits, and
balances live only in the debug dir, never here). This section replaces
the §3 open questions the scaffold shipped with.

### Login + 2FA (flow 1)

Entry is `www.firstcitizens.com` → a redirect into
`digitalbanking.firstcitizens.com` (a firstcitizens.com subdomain, so
the pre-fill host gate covers it). The login form is classic light DOM
(`#login-form-0-id-textfield`, `#login-form-0-password-textfield`,
submit `#login-form-0-login-other-return`); submitting it drives a
**`mobilews` REST sequence**:

1. `POST …/mobilews/preLogonUser` `{"userId": <user>}` — pre-check.
2. `POST …/mobilews/logonUser` — submits the password (carried in the
   request, not the JSON body). The response **status is the auth
   signal**:
   - **HTTP 203** + `accessCodeTargets` (a list of
     `{notificationType, display, value}`, **3 = SMS text**, **2 = voice
     call**; `pushAccessCodeTargets` null, no email/TOTP) → **2FA
     required** (untrusted / registering device).
   - **HTTP 200** + `accessCodeTargets: null` + populated
     `userProfileData` → **authenticated, no 2FA** (trusted device).
3. `POST …/mobilews/accessCode` `{"accessCode": <target value>}` — sends
   the code to the chosen target.
4. `POST …/mobilews/accessCode/validate` `{"accessCode": <typed OTP>}` —
   submits the entered code.
5. `GET …/mobilews/registerDevice` — the "remember this device" step;
   plants the device-trust cookie.

**Bot defense — Akamai Bot Manager gates only the logon (the decisive
runtime constraint).** `preLogonUser` and `logonUser` carry
**Akamai Bot Manager sensor headers** (`x-qjzesqau-*`, randomized names;
the `-f` payload is browser-generated telemetry) plus `x-dtpc`
(**Dynatrace RUM**); Camoufox produces them and cleared the login with
no visible challenge. **Every other endpoint** — `accessCode`,
`accessCode/validate`, `registerDevice`, and all data calls — carries
**no sensor**, only the session cookie + a `q2token` (which is both a
cookie and a request header, the CSRF token). So a plain host HTTP
client cannot forge the sensor and **the logon must run in the browser**;
but once logged in, the data can be fetched over plain REST. The
sign-in form is classic light DOM in a homepage **modal** (opened by a
"Log In" button; fields `#login-form-0-id-textfield` /
`-password-textfield`, submit `button[name='db-login-button']`), which
navigates into a hash-routed Q2 SPA (`uux.aspx#/…`). The **Secure Access
Code (2FA) screens are Q2/Stencil web components with shadow DOM**
(`q2-input#tacEntry`, `q2-btn[test-id="btnSubmit"]`), so the untrusted
path needs shadow-DOM driving — deferred to a live session (§4.2); the
trusted path (the common case) touches none of it.

**Session persistence — device-trust persists, session cookie does not
(the decisive Phase-2 finding).** The second `explore` run, on the same
profile with no `--fresh`, **re-ran `preLogonUser` + `logonUser`
(password) but fired no `accessCode` calls** — the registered device
skipped 2FA entirely. So:

- the **device registration** survives a browser restart (for some
  undetermined lifetime), but the **authenticated session does not** —
  every run still performs a fresh password logon;
- that is neither chase's shape (2FA every time → login folds into
  download) nor a fully durable session (schwab-api's refresh token).
  It is the **schwab-api hybrid split**: one interactive `login` mints
  the durable trust, and each unattended `download` logs in
  password-only against it.

### Account statements (flow 2)

- **List:** `GET …/mobilews/accountStatement/<acctId>` → `{"data": [
  {"period": "MM/DD/YYYY", "value": <docId>}, … ]}` — per account,
  **monthly, ~2 years deep**.
- **PDF:** `POST …/mobilews/accountStatement/<acctId>/<docId>/pdf` →
  `application/pdf`. The POST **requires the `q2token` as a multipart form
  field** (like the export); a header-only POST 400s (verified
  2026-08-14). One document per account per month (no combined-product
  statements — so chase's per-segment balance-chained attribution is
  **not** needed).
- A `GET …/mobilews/accountStatement/form` returns the picker config;
  tax documents hang off `…/mobilews/form/TaxDocument` (out of scope).

### Transaction history + exports (flow 3)

- **History:** `GET …/mobilews/accountHistory/<acctId>` returns
  `{"data": {"transactions": […], "transactionCount": N,
  "oldestTransactionDate": …}}` — **paginated**: `page[number]` (1-based)
  + `page[size]` (SPA default 100), `sort=postedDate%1Fd` (descending;
  the field/direction separator is a literal `US`/0x1F). Without a filter
  it carries the **full history** back to the account's
  `oldestTransactionDate` (the SVB-migration date). It also **narrows
  server-side to a `postedDate` window**:
  `postedDate=<from>\x1f<to>` in `M/D/YYYY`, the `to` end carrying
  `23:59:59.999` — exactly what the account-detail "Time Period" /
  "Custom Date" picker sends (verified 2026-08-15; `transactionCount`
  reflects the window). `--lookback` rides that param, so `download`
  narrows at the source. (An earlier note called the picker client-side —
  that was a bad test where the Stencil date-input fill hadn't stuck.)
  Each row carries a stable `transactionId`, `runningBalance`, signed
  `amount` + `isDebit`, `description`, `checkNumber`, `postedDate`.
  `GET …/mobilews/account/<acctId>` gives account detail; the roster is
  `GET …/mobilews/accounts` (plus a PFM overlay `GET …/v2/pfm/accounts`).
- **Export:** `POST …/mobilews/accountExport/<acctId>/<Format>` where
  **`Format` ∈ {`Csv`, `Xls`, `Ofx`, `QFX_1_0_2`, `Qbo`}** (all five
  captured). The request is `multipart/form-data` carrying **only a
  `q2token`** (the CSRF token download.py must harvest). It honours the
  **same `postedDate` window as a URL query param** (verified 2026-08-15),
  so a windowed download narrows the exports too; without it each export
  returns the account's full available history (~3 years, the
  post-SVB-migration lifetime).
- **Format comparison (resolved in Phase 3):** the export reaches
  ~3 years but statements only ~2, so **the export is the deeper source
  and there is no statement-backfill tail to fill** — the opposite of
  chase. And the `accountHistory` JSON turned out richer still: it
  carries a stable `transactionId` **and** a per-row `runningBalance` in
  one payload, so silver loads the **JSON as the authoritative ledger**
  and needs no export join at all. The CSV/QFX exports are captured as
  provenance only (a flat export can omit a pending/edge row the JSON
  keeps), never the source of record.
- Statements are still downloaded as **documents** (PDFs belong in
  bronze), but they are **not** a transaction source here.

## 4. Phase roadmap

1. **Scaffold** (done, committed 2026-08-13) — explore harness,
   Dockerfile on base-camoufox, wrapper via
   `shared/wrappers/wrapper-lib.sh`, this document, CLAUDE.md.

2. **Runtime: Camoufox `login` + `download`, split verbs, REST data
   (Phase-2 scaffold built 2026-08-13).** The intent was a lightweight
   `schwab-api`-style hybrid (host-venv REST download, no browser). The
   §3 Akamai finding **rules that out**: the sensor headers on
   `preLogonUser`/`logonUser` are browser-generated telemetry a plain
   host client cannot forge, and the session cookie does not survive a
   browser restart — so **every run's logon must happen in Camoufox**.
   What the clean REST API *does* buy is that, once logged in, the data
   is fetched over plain REST (`page.request`) with the harvested
   `q2token` — **no DOM scraping** of transactions, unlike chase. So the
   runtime is a full Docker (Camoufox) collector, not a host-venv hybrid,
   but the login/download split the persistence finding motivates still
   holds:

   - **`login` (Camoufox, interactive).** The login form lives in a
     **modal** on `www.firstcitizens.com` (opened by a "Log In" button,
     `SEL_LOGIN_TRIGGER`); `login` opens it, pre-fills, and submits
     (`button[name='db-login-button']`), which navigates into the Q2 SPA
     (`uux.aspx#/login`) where the SPA + Akamai JS make the sensor-bearing
     `preLogonUser`/`logonUser` calls. **Auth is detected by polling the
     live SPA route + a REST probe, never a lone response event** (the
     pinned Camoufox drops `.on()` events across navigations — the
     chase/schwab-web lesson that first surfaced here as a hang): a
     trusted device routes to `#/landingPage` (and `GET accounts` returns
     200), an untrusted device routes to `#/login/mfa/*`.
     - **Trusted device** → signed in, nothing more to do.
     - **Untrusted device** → the Secure Access Code (2FA) is **driven
       from the terminal** ([`auth_dialog.py`](auth_dialog.py)): the
       delivery methods are read from the on-screen `q2-btn[test-id=
       "btnTacTarget"]` buttons (not the `logonUser` response, which the
       pinned Camoufox may drop), the human picks Text/Call, the code is
       read from stdin, typed into `q2-input#tacEntry`, submitted via
       `q2-btn[test-id="btnSubmit"]`, and the device registered via
       `btnRegister`. These are Q2/Stencil shadow-DOM controls, so every
       click targets the **inner `<button>`** inside the `q2-btn` — a
       click on the `q2-btn` host reports actionable but the Stencil
       handler never fires (the chase shadow-DOM lesson). `vnc-login`
       (`--no-cli-mfa`) remains the by-hand fallback; `--fresh` wipes the
       profile to force this untrusted path for testing.

     The **persistent Camoufox profile** holds the device-trust for later
     unattended runs; a small `~/.secrets/firstcitizens-state.json`
     records the last successful login. `login --check` reports whether
     the trusted-device logon still skips 2FA (submits the form, reads the
     route, sends no code).
   - **`download` (Camoufox, unattended — no VNC, no human).** Pre-fill +
     submit the same form; the trusted device makes `logonUser` return
     **200** with no 2FA. If it instead returns **203**, the device-trust
     has expired: `download` **fails loudly** ("run `login`") rather than
     blocking on a challenge it has no terminal for. Once authenticated
     it harvests the `q2token` and fetches over `page.request`:
     `GET accounts` (kept to `hydraProductTypeCode == "D"` deposit rows),
     then per account the **full paginated `accountHistory`** (the richest,
     authoritative ledger — stable `transactionId` **and** `runningBalance`
     per row; walked to `transactionCount`, §3), each requested
     `accountExport/<acct>/<Format>`, and the statement list + each PDF
     (`multipart` `q2token`, §3). Bronze per run dir: `run.json`,
     `accounts.json`, `history/<acct>.json` (the merged full ledger),
     `transactions/<acct>.{csv,qfx}`, `statements/<acct>/*.pdf`,
     `raw/*.json`, on chase's layout. **Bounded collector:** `--lookback`
     narrows the fetch server-side — the `postedDate` window rides the
     history + export URLs (§3) and statements are filtered by period — so
     the default (~90 days) suits routine refreshes and `--lookback all`
     pulls the complete history for the initial backfill.

   **Live-validation status.** `login` (both **trusted** and
   **untrusted/terminal-2FA** paths), `login --check`, and `download` are
   **validated live end-to-end** (2026-08-14/15). Trusted `login` reaches
   `#/landingPage` in ~3s; untrusted `login` drives the full Secure
   Access Code flow from the terminal (pick delivery → enter code →
   register device → authenticated) and leaves the device trusted for
   later unattended runs. `download` fetches over `page.request` with the
   harvested `q2token` and writes complete bronze: the deposit roster,
   the **full paginated history** per account (every row back to the
   SVB-migration date, counts matching `transactionCount`), CSV+QFX
   exports, and every statement PDF, and `--lookback` narrows the fetch
   server-side via the `postedDate` window (windowed and full runs both
   verified live 2026-08-15).

   **The CLI-2FA no-navigation bug — root cause (diagnosed 2026-08-15).**
   The terminal-2FA click first appeared to fire without advancing: the
   `accessCode` POST succeeded (the SMS arrived) but the SPA never routed
   to `#/login/mfa/entertarget`. After ruling out the click mechanics,
   stdin/TTY, and timing (all reproduced as working in isolation), the
   cause was **`authenticate`'s own REST `GET /accounts` probe**: fired
   every 3s to back up the route detection, it hit the server **during
   the login→MFA transition** and poisoned the challenge session, so the
   later delivery-button click could not advance. The fix gates that
   probe to fire **only once off a `/login*` route** (where it is a
   harmless authed-state fallback); the event-safe SPA route markers
   carry the pre-auth detection. Lesson for the fleet: an
   authentication-progress probe must never touch an
   authenticated-only endpoint mid-challenge.

3. **load (built).** bronze → SQLite silver, idempotent, migrations
   (`migrations/0001_initial.sql`), unit tests on synthetic fixtures
   only (`tests/test_load.py`). The `accountHistory` JSON already carries
   a stable id + running balance in one payload, so the chase CSV↔QFX
   join is unnecessary — the loader parses the **JSON `history/<acct>.json`
   as the authoritative ledger** (its `transactions` list): a stable
   `transactionId`, the signed `amount` (magnitude × `isDebit`), the
   per-row `runningBalance`, description and check number. The flat CSV
   export can carry slightly fewer rows than the JSON (a pending/edge
   item it omits), so it is provenance only, never the source of record.
   The silver schema is column-compatible with chase's, so the two gold
   adapters run in parallel. Statements are ingested as documents only
   (no transaction backfill).

4. **Gold adapter (built).** `wealthdb/internal/silver/firstcitizens/`
   on chase's model: cash accounts (`AccountKind` cash, `taxable_personal`
   / `self_directed`), a closing-balance series from the per-row running
   balance plus a current roster balance, and every transaction (interest
   / fee recognised from the description, else deposit/withdrawal by
   sign). Registered via a blank import in `cmd/wealthdb/main.go` and the
   `silver_sources` whitelist migration
   (`internal/gold/migrations/0034_silver_sources_firstcitizens.sql`).
   Conduit returns-exclusion per §1.

**No statement-backfill phase.** Chase needed one because its export was
capped at ~24 months while statements reached ~7 years; here the export
is the *deeper* source (~3y vs ~2y statements) and covers the full
account lifetime, so there is no older tail to reconstruct and
`statement_parser.py` / balance-chained attribution are not ported.

## 5. Status & operation

**Phase 2 built and validated live (2026-08-14/15).** `login` (trusted
and untrusted terminal-2FA paths), `login --check`, and `download` all
run end-to-end: trusted sign-in reaches `#/landingPage` in seconds;
untrusted sign-in drives the Secure Access Code from the terminal
(pick delivery → enter code → register device) and leaves the device
trusted; `download` writes complete bronze — the deposit roster, the
full paginated transaction history per account (every row to the
SVB-migration date), CSV+QFX exports, and every statement PDF — with a
`complete` run manifest. **`load` (silver) and the gold adapter are
built** (§4.3–4.4): `load` parses the history JSON into the SQLite
silver, and `wealthdb/internal/silver/firstcitizens/` projects it into
gold as cash accounts + a closing-balance series + the deposit ledger.
Both ship with unit tests on synthetic fixtures.

- `make build-firstcitizens` builds the image;
  `make test-firstcitizens` runs the unit tests in the container.
- Credentials: `~/.secrets/firstcitizens.env` at chmod 0600 with
  `FIRSTCITIZENS_USERNAME` / `FIRSTCITIZENS_PASSWORD` (single-quote
  values containing `$`, `!`, or backticks) — in place.
- Re-run discovery any time: `./firstcitizens explore` (or
  `wealthdb-collect firstcitizens explore`), connect a VNC viewer to
  the forwarded port, walk the flows. `--fresh` wipes the profile to
  force the full 2FA challenge again; `--no-prefill` types the
  credentials by hand.
- Live sessions only when explicitly requested with the owner present
  (CLAUDE.md §0); waits on the owner use long timeouts (1h+); never
  fire logins in quick succession.
