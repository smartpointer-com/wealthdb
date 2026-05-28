# Notes for Claude / coding agents

Four ground rules apply when working on this repo. All are
non-negotiable.

## 1. Read-only UBS netbanking access — never trigger write actions

The UBS netbanking session this toolkit drives is a fully privileged
session — the same one a human uses to place trades, move money,
issue payments, and change settings. There is no read-only sub-
session or scope. This repo's contract is that it *only* reads.

Allowed UI surfaces — `download.py` may only navigate to or click
within (final list TBD once the live UI is mapped, but the
allow-list pattern below is binding):

- The UBS login form and the MFA approval page that follows it.
- Read-only listing pages for: account overview, transactions,
  custody/portfolio holdings, eDocuments archive.
- Date-range / period filter inputs and "Apply" / "Search" buttons
  on the above pages.
- Export / download buttons that produce CSV, XLS, or PDF copies
  of already-displayed data.
- Document download endpoints reachable via the session cookie
  (typically REST endpoints fetched via Playwright's request API
  rather than per-row link clicks).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- Payment / transfer entry (`Payments`, `New Transfer`, `eBill`,
  IBAN entry, beneficiary management).
- Trade entry / order forms (`Trade`, `Buy/Sell`, `New Order`).
- Card management (block, unblock, request, change limits).
- Settings pages that mutate account state (notification prefs,
  trading limits, MFA factor management, contact details).
- Any "confirm" or "submit" button outside the login form itself.
- Anything that performs a `POST` other than the login form, the
  read-only filter Apply actions, and explicit export-generation
  triggers.

The CLI must never accept a flag that would trigger a write action.
If a future UBS feature exposes structured "place order" or "submit
transfer" endpoints reachable via the logged-in session, treat them
as forbidden until the user explicitly opts in in writing.

## 2. Do not run real netbanking sessions unless the user asks

Every fresh login from this toolkit triggers an MFA push (UBS
Access App / SMS / m-TAN — exact factor depends on the user's
configuration) to the user's device. Repeated logins:

- Annoy the user (one biometric tap each).
- May trigger UBS-side fraud heuristics or temporary lock-out —
  there is no published "max sessions per day" limit, so err well
  below any plausible threshold.
- Burn the persistent session cookie's lifetime if invalidated by
  parallel logins.

Allowed without asking:

- Read the code, configs, and docs.
- Run `login.py --check` (loads the stored `storageState.json`,
  hits one cheap landmark URL, reports whether the cookie is still
  valid). No new login, no MFA push.
- Run `download.py --dry-run` (uses the existing session if alive,
  walks the UI to confirm selectors still match landmarks, exits
  without exporting). Counts as one navigation, not a new login.
- Run unit tests and fixture-based parsing exercises.

Not allowed unless the user explicitly asks:

- Run `login.py` without `--check` (mints a fresh session, sends a
  push to the user's phone).
- Run `download.py` without `--dry-run`.
- Trigger any non-`--check` navigation to a live `ubs.com` URL
  from a REPL or one-off shell command.
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
  interactive prompt at `login.py` time, or from an env var that
  the user manages outside the repo.
- Cache the password "for the next login.py invocation" in process
  state or on disk.
- Add a `--password VALUE` CLI flag that puts the password in `ps`
  output or shell history. The accepted pattern is value-with-env-
  fallback for non-secret IDs, but for secrets the input is env or
  interactive prompt only.
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

- UBS login contract / agreement number, customer number, or
  e-banking user ID.
- Account numbers, IBANs, custody numbers, sub-account identifiers,
  relationship numbers, dialog user IDs.
- Personal data: names, addresses, phone numbers, email addresses,
  birth dates.
- Real session cookies, `storageState.json` contents, or MFA tokens.
- Any data returned by UBS — positions, transactions, balances,
  fees, document IDs, instrument lists scoped to a specific
  account.
- Screenshots from Playwright trace capture (or user-supplied
  reference screenshots) that show logged-in UI with real values.
  If a debug screenshot needs to be committed for documentation,
  redact identifiers first; the preferred default is "don't commit
  screenshots at all".

When the user drops a sample HTML download or screenshot into the
repo as part of bootstrapping `login.py` / `download.py`,
**strip identifiers before committing** anything derived from
it — even comments and test fixtures. When in doubt, ask the user
before adding a value that looks identifier-shaped.
