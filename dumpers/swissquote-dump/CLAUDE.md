# Notes for Claude / coding agents

Four ground rules apply when working on this repo. All are
non-negotiable.

## 1. Read-only Swissquote access — never trigger write actions

The Swissquote e-banking session this toolkit drives is a fully
privileged session — the same one a human uses to place trades,
move money, and change settings. There is no read-only sub-session
or scope. This repo's contract is that it *only* reads.

Allowed UI surfaces — these are the only pages `download.py` may
navigate to or click within:
- F5 BIG-IP login form at `/my.policy` and the Mobile Level 3 MFA
  page that follows it.
- Trading Platform SPA `#transactions` route — date-range filter
  inputs and the export dropdown only.
- Trading Platform SPA `#portfoliooverview` route — the three
  export buttons (Positions, List of Assets, Export account
  overview) only. The Buy/Sell buttons inside position rows are
  present in the DOM but must never be clicked.
- eBanking SPA `#documents` route — date-range filter and Apply
  only; document PDFs are fetched via Playwright's request API
  (the `getPdfDocument` REST endpoint with the session cookie),
  not by clicking download links.
- eBanking SPA root (`/sqc-web-client-portal/`) used by
  `login.py --check` to test session liveness via URL transition.

Forbidden — do not navigate to, click, or scrape:
- Trade entry forms (`Trade`, `Buy/Sell`, `Quote`, order-book widgets).
- Payment / transfer forms (`Payments`, `Withdraw`, IBAN entry).
- Settings pages that mutate account state (notification prefs,
  trading limits, MFA factor management).
- Anything that performs a `POST` other than the login form itself.

The CLI must never accept a flag that would trigger a write action.
If a future Swissquote feature exposes structured "place order" or
"submit transfer" endpoints reachable via the logged-in session,
treat them as forbidden until the user explicitly opts in in writing.

## 2. Do not run real e-banking sessions unless the user asks

Every fresh login from this toolkit triggers a Mobile Level 3 push
to the user's phone. Repeated logins:
- Annoy the user (one biometric tap each).
- May trigger Swissquote-side fraud heuristics or temporary
  lock-out — there is no published "max sessions per day" limit, so
  err well below any plausible threshold.
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
- Trigger any non-`--check` navigation to a live `swissquote.ch`
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
messages, test fixtures, recorded Playwright traces):

- Swissquote login username / customer number.
- Account numbers, IBANs, depot numbers, sub-account identifiers.
- Personal data: names, addresses, phone numbers, email addresses,
  birth dates, banking relationship identifiers.
- Real session cookies, `storageState.json` contents, or MFA tokens.
- Any data returned by Swissquote — positions, transactions, fees,
  document IDs, instrument lists scoped to a specific account.
- Screenshots from Playwright trace capture that show logged-in UI
  with real values. If a debug screenshot is committed, it must be
  from a mocked / placeholder UI.

Test fixtures must be synthetic. Examples in docs should use
placeholders like `<USERNAME>`, `<ACCOUNT_ID>`, `<DOC_ID>`.

When in doubt, ask the user before adding a value that looks
identifier-shaped.
