# Notes for Claude / coding agents

Four ground rules apply when working on this repo. All are
non-negotiable.

## 1. Read-only Fidelity web access — never trigger write actions

The fidelity.com session this toolkit drives is a fully privileged
session — the same one a human uses to place trades, move money,
issue transfers, and change settings. There is no read-only sub-
session or scope. This repo's contract is that it *only* reads.

Allowed UI surfaces — `download.py` may only navigate to or click
within (final list TBD once the live UI is mapped, but the
allow-list pattern below is binding):

- The Fidelity login form and the MFA challenge page that follows
  it (Duo push, Symantec VIP code, Google Authenticator code, SMS,
  voice — exact factor depends on the user's configuration).
- Read-only listing pages for: account summary / Portfolio
  overview, positions / holdings, activity & orders / transaction
  history, realised gains / cost basis views, statements & tax
  forms / document center.
- Date-range / period filter inputs and "Apply" / "Search" /
  "Update" buttons on the above pages.
- Export / download buttons that produce CSV, XLS, or PDF copies
  of already-displayed data (the "Download" toolbars on
  Activity & Orders, Positions, and the document center).
- Document download endpoints reachable via the session cookie
  (typically REST endpoints fetched via Playwright's request API
  rather than per-row link clicks).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- Trade entry / order forms (`Trade`, `Trade Stocks/ETFs`,
  `Trade Options`, `Trade Mutual Funds`, `Trade Bonds`, any
  quote-to-order surface).
- Transfer / movement forms (`Transfers`, `Bill Pay`, ACH, wire,
  internal account transfer, Bank Connection setup, deposit
  check, withdrawal request).
- Account opening / closing flows.
- Card management (Fidelity-branded debit / credit cards: block,
  unblock, request, change limits).
- Profile / settings pages that mutate account state (alert
  preferences, trading approvals, MFA factor management, contact
  details, beneficiary changes, third-party access grants).
- Anything related to the "Fidelity Access" third-party-app
  authorisation surface (this is the Akoya gateway; granting
  access there would expose data to an external party).
- Any "confirm" / "submit" / "place" button outside the login
  form itself.
- Anything that performs a `POST` other than the login form, the
  read-only filter Apply actions, and explicit export-generation
  triggers.

The CLI must never accept a flag that would trigger a write action.
If a future Fidelity feature exposes structured "place order" or
"submit transfer" endpoints reachable via the logged-in session,
treat them as forbidden until the user explicitly opts in in
writing.

A single Fidelity login may surface accounts of several
registrations, including accounts visible only through transitively
granted permissions. The read-only contract applies uniformly to
every account-type the login surfaces — do not use any elevated
permission the login may transitively grant for anything beyond
observation.

## 2. Do not run real Fidelity sessions unless the user asks

Every fresh login from this toolkit triggers an MFA challenge
(Duo push / Symantec VIP / Authenticator / SMS / voice — exact
factor depends on the user's configuration) to the user's device.
Repeated logins:

- Annoy the user (one code or biometric tap each).
- May trigger Fidelity-side fraud heuristics, device-trust
  reverification, or temporary lock-out — there is no published
  "max sessions per day" limit, so err well below any plausible
  threshold.
- Burn the persistent session cookie's lifetime if invalidated by
  parallel logins.

Allowed without asking:

- Read the code, configs, and docs.
- Run `download.py --check` (loads the stored profile dir, hits
  one cheap landmark URL, reports whether the session is still
  alive). No credential submit, no MFA push.
- Run `download.py --dry-run` (uses the existing session if alive,
  walks the UI to confirm selectors still match landmarks, exits
  without exporting). Counts as one navigation, not a new login.
- Run unit tests and fixture-based parsing exercises.

Not allowed unless the user explicitly asks:

- Run `download.py` without `--check` or `--dry-run` (full login
  + walk; sends an MFA challenge to the user's phone if the
  device-trust cookie has expired).
- Trigger any non-`--check` navigation to a live `fidelity.com`
  URL from a REPL or one-off shell command.
- Add or change scheduling (cron, launchd, systemd timer, GitHub
  Actions, etc.) that would cause logins or downloads to fire
  automatically. This toolkit is intentionally human-triggered;
  unattended cron does not work past the MFA gate anyway.

## 3. Do not weaken authentication

The session cookie is the keys to the kingdom (see §1). Do not:

- Write code that disables, bypasses, or downgrades MFA — even
  "temporarily for testing."
- Persist the user's password in plaintext anywhere on disk or in
  environment files committed to the repo. Passwords come from
  an env var that the user manages outside the repo
  (`FIDELITY_PASSWORD`, typically sourced from
  `~/.secrets/fidelity-web.env`).
- Cache the password "for the next download.py invocation" in
  process state or on disk.
- Add a `--password VALUE` CLI flag that puts the password in `ps`
  output or shell history. Credentials reach the script via the
  `FIDELITY_USERNAME` / `FIDELITY_PASSWORD` env vars, typically
  loaded from `~/.secrets/fidelity-web.env`.
- Reduce the `chmod` on the state file below `0600`, or store it
  in a location wider than `~/.secrets/` defaults.
- Default any debug or transient artefact (screenshot, trace
  bundle, scratch log) to a path under `~/.secrets/`. The secrets
  dir is for persistent credentials only; debug paths must be
  user-provided (`--screenshot-dir` etc.) with no fallback to the
  secrets-dir parent. `--trace` is therefore a paired flag — it
  requires `--screenshot-dir`.

## 4. Do not leak private information into source

The repo is intended to be publishable. Do not write any of the
following into tracked files (source, configs, comments, commit
messages, test fixtures, recorded Playwright traces, sample HTML
drops the user provides for landmarking):

- Fidelity login username, customer ID, advisor / relationship
  IDs, Fidelity-issued reference numbers.
- Account numbers (the standard 9-digit Fidelity account number,
  any masked variant Schwab-style with only the last N digits,
  account nicknames that embed identifying info).
- Registration identifiers — account titles, tax IDs (EIN),
  beneficiary names, agreement numbers; must not appear anywhere
  tracked.
- Third-party-manager identifiers — firm name,
  relationship numbers, advisor names, manager-side reference
  numbers — even though that data isn't directly fetched by
  this toolkit.
- Personal data: names, addresses, phone numbers, email addresses,
  birth dates, SSN fragments, beneficiary identifiers.
- Real session cookies, browser-profile contents, or MFA tokens.
- Any data returned by Fidelity — positions, transactions,
  balances, fees, cost basis, realised gains, document IDs,
  instrument lists scoped to a specific account.
- Screenshots from Playwright trace capture (or user-supplied
  reference screenshots) that show logged-in UI with real values.
  If a debug screenshot needs to be committed for documentation,
  redact identifiers first; the preferred default is "don't commit
  screenshots at all".

Bank name (Fidelity), generic widely-held example tickers
(SPY, QQQ, VTI), and IBAN-spec placeholder letters are fine.
Synthetic examples in docs only — never copy real account IDs
into examples, even comments.

**Pre-commit:** grep the staged diff for known real values BEFORE
the first `git add`, not after. When the user drops a sample HTML
download or screenshot into the repo as part of bootstrapping
`download.py`, strip identifiers before committing
anything derived from it — even comments and test fixtures. When
in doubt, ask the user before adding a value that looks
identifier-shaped.
