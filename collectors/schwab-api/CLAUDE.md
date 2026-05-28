# Notes for Claude / coding agents

Three ground rules apply when working on this repo. All are
non-negotiable.

## 1. Read-only Schwab access — never call write endpoints

The Schwab Trader API does not offer read-only OAuth scopes: any token
issued for an Accounts and Trading app can place, replace, and cancel
orders. This repo's contract is that it *only* reads. Concretely:

Allowed:
- `GET /trader/v1/accounts` and `/accounts/{hash}` (with or without
  `fields=positions`).
- `GET /trader/v1/accounts/{hash}/transactions`.
- `GET /trader/v1/accounts/accountNumbers`.
- `GET /trader/v1/userPreference`.
- `GET /trader/v1/accounts/{hash}/orders` and `GET /orders`
  (status read only — never `POST`/`PUT`/`DELETE`).
- `GET /marketdata/*`.

Forbidden in this repo — do not call, import, or wrap:
- `place_order` / any `POST /accounts/{hash}/orders`.
- `replace_order` / any `PUT /accounts/{hash}/orders/{id}`.
- `cancel_order` / any `DELETE /accounts/{hash}/orders/{id}`.
- Anything related to ACH, wire, or fund transfer (not exposed by the
  API, but still — do not write code that would call such an endpoint
  if it ever appears).

If a future Schwab endpoint would let the holder of a token affect
account state, treat it as forbidden until the user explicitly opts in
in writing.

The CLI must never accept a flag that would trigger a write operation.

## 2. Do not run real Schwab API calls unless the user asks

`download.py` consumes OAuth refresh capacity and counts against rate
limits. Schwab refresh tokens have a hard 7-day lifetime that requires
an interactive browser login to renew. Burning that window during agent
exploration is bad.

Allowed without asking:
- Read the code, configs, and docs.
- Run `download.py --dry-run` (loads tokens, validates they refresh,
  lists account hashes via `accountNumbers`, exits without fetching
  positions or transactions).
- Run unit tests and mocked-HTTP exercises.

Not allowed unless the user explicitly asks:
- Run `download.py` without `--dry-run`.
- Make any non-dry-run HTTP call against `api.schwabapi.com`, whether
  from the script, an ad-hoc REPL, or a one-off shell command.
- Trigger the initial OAuth browser login flow (interactive; consumes
  the user's attention and creates a fresh 7-day refresh window).
- Add or change scheduling (cron, launchd, systemd timer, GitHub
  Actions, etc.) that would cause downloads to fire automatically.

## 3. Do not leak private information into source

The repo is intended to be publishable. Do not write any of the
following into tracked files (source, configs, comments, commit
messages, test fixtures):

- Schwab OAuth Client ID / Client Secret. These are credentials.
- Account numbers (plain or hashed). The hash is opaque but still
  user-specific.
- Personal data: names, addresses, phone numbers, email addresses,
  brokerage relationship identifiers.
- Real OAuth tokens, refresh tokens, or token files.
- Any data returned by the Schwab API — positions, transactions,
  balances, instrument lists scoped to a specific account.

Test fixtures must be synthetic. Examples in docs should use
placeholders like `<CLIENT_ID>`, `<CLIENT_SECRET>`, and `<ACCOUNT_HASH>`.

When in doubt, ask the user before adding a value that looks
identifier-shaped.
