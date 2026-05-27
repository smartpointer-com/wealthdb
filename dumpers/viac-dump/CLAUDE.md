# Notes for Claude / coding agents

Four ground rules apply when working on this repo. All are
non-negotiable.

## 1. Read-only VIAC access — never trigger write actions

The VIAC session this toolkit drives is a fully privileged session
— the same one a human uses to initiate Pillar-3a contributions,
change the investment strategy, change a beneficiary, or request a
withdrawal. There is no read-only sub-session or scope. This
repo's contract is that it *only* reads.

Allowed UI surfaces — the Phase 2 / Phase 3 scripts may only
navigate to or click within (final list TBD once the live SPA is
mapped in Phase 1, but the allow-list pattern below is binding):

- The VIAC login form and the MFA approval page that follows it.
- The REST endpoints listed in DESIGN.md §2.2 (customer profile,
  wealth summary / allocation, per-portfolio strategy / assets /
  fees, transactions, document index, individual PDFs).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- Contribution / deposit forms ("Einzahlung", "Beitrag",
  "Vorsorge-Beitrag").
- Strategy change / fund-switch forms ("Strategie ändern",
  "Anlage anpassen").
- Withdrawal / payout request forms ("Auszahlung", "Bezug",
  "Vorbezug").
- Beneficiary management ("Begünstigung", "Erbschaft").
- Card management or any "request card / change limits" surface
  VIAC may expose for its WIR-bank-side cash accounts.
- Profile / settings pages that mutate account state (notification
  prefs, MFA factor management, contact details).
- Any "confirm" / "submit" / "bestätigen" / "absenden" button
  outside the login form itself.
- Anything that performs a `POST` other than the login form, the
  read-only filter Apply actions, and explicit export-generation
  triggers.

The CLI must never accept a flag that would trigger a write
action. If a future VIAC feature exposes structured "place
contribution" or "submit strategy change" endpoints reachable via
the logged-in session, treat them as forbidden until the user
explicitly opts in in writing.

## 2. Do not run real VIAC sessions unless the user asks

Every fresh login from this toolkit triggers an MFA challenge
(in-app TOTP or push via VIAC's mobile app — to be confirmed in
Phase 1) to the user's device. Repeated logins:

- Annoy the user (one biometric tap each).
- May trigger VIAC-side fraud heuristics or temporary lock-out —
  there is no published "max sessions per day" limit, so err well
  below any plausible threshold.
- Burn the persistent session token's lifetime if invalidated by
  parallel logins.

Allowed without asking:

- Read the code, configs, and docs.
- Run `login.py --check` (loads the stored session, probes one
  cheap landmark / discovered JSON endpoint, reports whether the
  session is still valid). No new login, no MFA push.
- Run `download.py --dry-run` (uses the existing session if alive,
  walks the UI to confirm selectors / endpoints still match
  landmarks, exits without exporting). Counts as one navigation,
  not a new login.
- Run unit tests and fixture-based parsing exercises.

Not allowed unless the user explicitly asks:

- Run `login.py` without `--check` (mints a fresh session, sends a
  push to the user's phone).
- Run `download.py` without `--dry-run`.
- Trigger any non-`--check` navigation to a live `app.viac.ch`
  URL from a REPL or one-off shell command.
- Add or change scheduling (cron, launchd, systemd timer, GitHub
  Actions, etc.) that would cause logins or downloads to fire
  automatically. This toolkit is intentionally human-triggered;
  unattended cron does not work past the MFA gate anyway.

## 3. Do not weaken authentication

The session cookie / bearer token is the keys to the kingdom (see
§1). Do not:

- Write code that disables, bypasses, or downgrades MFA — even
  "temporarily for testing."
- Persist the user's password in plaintext anywhere on disk or in
  environment files committed to the repo. Passwords come from an
  env var that the user manages outside the repo (`VIAC_PASSWORD`,
  sourced from `~/.secrets/viac.env`).
- Cache the password "for the next login.py invocation" in process
  state or on disk.
- Add a `--password VALUE` CLI flag that puts the password in `ps`
  output or shell history. Credentials reach the script via the
  `VIAC_LOGIN` / `VIAC_PASSWORD` env vars only.
- Reduce the `chmod` on `~/.secrets/viac.env` (or any persisted
  session-state file) below `0600`, or store it in a location
  wider than `~/.secrets/` defaults.
- Default any debug or transient artefact (scratch log, trace
  bundle) to a path under `~/.secrets/`. The secrets dir is for
  persistent credentials only; debug paths must be user-provided
  with no fallback to the secrets-dir parent.

## 4. Do not leak private information into source

The repo is intended to be publishable. Do not write any of the
following into tracked files (source, configs, comments, commit
messages, test fixtures, sample JSON drops the user shares for
debugging):

- VIAC login username, customer ID, contract number, any of the
  many ID formats VIAC may use.
- Personal data: names, addresses, phone numbers, email
  addresses, birth dates, SSN / AHV fragments, beneficiary
  identifiers.
- Real session cookies / bearer tokens or MFA codes.
- Any data returned by VIAC — balances, allocations, transactions,
  contribution history, fees, document IDs, fund holdings, fund
  weights.
- Captured API response bodies, request headers, or any artefact
  from a live session that shows real values. If a debug fragment
  needs to be committed for documentation, redact identifiers
  first.

Bank name (VIAC), generic widely-held example tickers
(SPX / QQQ / VTI), and IBAN-spec placeholder letters
(`CH<chk><BBBB><RRRR><AAAAAAAA><C>` shape) are fine. Synthetic
examples in docs only — never copy real account IDs into
examples, even comments.

**Pre-commit:** grep the staged diff for known real values BEFORE
the first `git add`, not after. When the user shares a captured
response body or log fragment in chat for debugging, **strip
identifiers before committing** anything derived from it — even
comments and test fixtures. When in doubt, ask the user before
adding a value that looks identifier-shaped.
