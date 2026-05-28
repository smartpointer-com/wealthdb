# Notes for Claude / coding agents

Four ground rules apply when working on this repo. All are
non-negotiable.

## 1. Read-only Relevate access — never trigger write actions

The Relevate (Pensexpert / pens-expert.ch) customer-portal session
this toolkit drives is a fully privileged session — the same one
a human uses to initiate withdrawals, change beneficiaries, update
contact details, and direct contributions. There is no read-only
sub-session or scope. This repo's contract is that it *only*
reads.

Allowed surfaces — these are the only Relevate endpoints the
toolkit may call:

- `POST /auth/rest/public/authentication/applications/b2c/access`
  (flow-init probe; expected 401), `POST .../password/check`
  (credentials), `POST .../mtan/otp/check` (OTP verification),
  `GET /auth/rest/protected/self-service/ui/configuration/portal`
  (session-alive landmark probe).
- `GET /middlelayer/v2/portfolio/investment-overview` and the
  per-portfolio read-only endpoints (`/Portfolio/{id}/deposits`,
  `/portfolio/{id}/performance`, `/portfolio/{id}/fees`,
  `/portfolio/{id}/investment/allocation`,
  `/Portfolio/proposal/{id}/modelportfolio`).
- `GET /middlelayer/v2/documents` (index) and
  `GET /middlelayer/v2/document/{id}` (PDF binary).
- `GET /middlelayer/v2/{compliance/products, contact/messages,
  contact/notification, contact/risk-protection, sso/claims}`
  (small ancillary read-only endpoints).

Forbidden — do not call, navigate to, or replay:

- `/dashboard/depot/{id}/investment/allocation/change` and any
  other `*/change` URL.
- Withdrawal / payout request endpoints (`Bezug`, `Auszahlung`,
  withdrawal-to-Pillar-3a, retirement payout, hardship withdrawal).
- Beneficiary management (`Begünstigte`).
- Contribution direction / investment-strategy mutations
  (`Anlagestrategie`, fund-switch forms).
- Contact-detail / profile mutations (address, phone, email,
  IBAN changes).
- Account opening / closing flows.
- Any `POST` / `PUT` / `DELETE` other than the three auth POSTs
  above and Airlock's logout `DELETE /auth/rest/public/authentication`.

The CLI must never accept a flag that would trigger a write
action. If a future Relevate feature exposes structured "submit
withdrawal" or "change beneficiary" endpoints reachable via the
logged-in session, treat them as forbidden until the user
explicitly opts in in writing.

## 2. Do not run real Relevate sessions unless the user asks

Every fresh login from this toolkit triggers an mTAN to the user's
phone. Repeated logins:

- Annoy the user (one code each).
- May trigger Relevate-side fraud heuristics, device-trust
  reverification, or temporary lock-out — there is no published
  "max sessions per day" limit, so err well below any plausible
  threshold.
- Burn the persistent session cookie's lifetime if invalidated
  by parallel logins.

Allowed without asking:

- Read the code, configs, and docs.
- Run `./relevate-dump login --check` (loads the persisted cookie
  jar, hits the landmark probe, reports ALIVE / DEAD / MISSING).
  No credential submit, no mTAN push.
- Run `./relevate-dump download --dry-run` (uses the existing
  session if alive; hits only `/investment-overview` and
  `/documents` to count work; exits without per-portfolio or
  per-document fetches).
- Run unit tests / fixture-based parsing exercises.

Not allowed unless the user explicitly asks:

- Run `./relevate-dump login` without `--check` (mints a fresh
  session, sends an mTAN to the user's phone).
- Run `./relevate-dump download` without `--dry-run`.
- Trigger any non-`--check` request to a live `pens-expert.ch`
  URL from a REPL or one-off shell command.
- Add or change scheduling (cron, launchd, systemd timer, GitHub
  Actions, etc.) that would cause logins or downloads to fire
  automatically. This toolkit is intentionally human-triggered;
  unattended cron does not work past the mTAN gate anyway.

## 3. Do not weaken authentication

The Airlock session cookies (`AL_SESS-S`, `CSRFT759-S`,
`AL_LoginFromNewDevice`) are the keys to the kingdom (see §1).
Do not:

- Write code that disables, bypasses, or downgrades mTAN — even
  "temporarily for testing."
- Persist the user's password in plaintext anywhere on disk or in
  environment files committed to the repo. Credentials reach the
  scripts via the `RELEVATE_LOGIN` / `RELEVATE_PASSWORD` env vars,
  sourced from `~/.secrets/relevate.env` (`chmod 0600`), which
  the toolkit reads and never writes.
- Cache the password "for the next `login.py` invocation" in
  process state or on disk.
- Add a `--password VALUE` CLI flag that puts the password in
  `ps` output or shell history. The accepted pattern is value-
  with-env-fallback for non-secret IDs, but for secrets the
  input is env or interactive prompt only.
- Reduce the `chmod` on the state file below `0600`, or store
  state in a location wider than `~/.secrets/` defaults.
- Default any debug or transient artefact (scratch log, trace
  bundle) to a path under `~/.secrets/`. The secrets dir is for
  persistent credentials and session state only; debug paths
  must be user-provided (`--screenshot-dir`, `--trace-dir`,
  `--dest`, etc.) with no fallback to the secrets-dir parent.

## 4. Do not leak private information into source

The repo is intended to be publishable. Do not write any of the
following into tracked files (source, configs, comments, commit
messages, test fixtures):

- Relevate login username / customer ID / foundation member
  number.
- Account numbers (any of the many ID formats Relevate uses —
  AHV-derived, foundation-internal, the `NNNN.NNNNNN.N`
  `externalId` form, IBAN-shaped).
- Personal data: names, addresses, phone numbers (especially
  Swiss `+41...` mobile numbers), email addresses, birth dates,
  AHV / social-security fragments, beneficiary identifiers,
  employer names.
- Real session cookies, cookie-jar contents, or mTAN codes.
- Any data returned by Relevate — balances, positions,
  transactions, fees, contribution history, document IDs, fund
  identifiers scoped to a specific account.
- Specific portfolio / proposal / document IDs the user has
  shared during debugging.

Bank name (Relevate / Pensexpert), generic widely-held example
tickers (SPX / QQQ / VTI), and IBAN-spec placeholder letters
(`CHKKBBBBxxxxxxxxxxxxxxC`-style) are fine. Synthetic examples in
docs only — never copy real account IDs into examples, even
comments.

**Pre-commit:** grep the staged diff for known real values BEFORE
the first `git add`, not after. When the user shares a captured
response body or log fragment in chat for debugging, **strip
identifiers before committing** anything derived from it — even
comments and test fixtures. When in doubt, ask the user before
adding a value that looks identifier-shaped.
