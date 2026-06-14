# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The schwab-api-specific surface below
applies on top of those shared rules.

- Schwab refresh tokens have a hard 7-day lifetime, renewable only via
  an interactive browser login.
- `login` now drives that re-auth through a headed Camoufox browser
  (VNC), reusing the `schwab-web` Schwab login credentials
  (`SCHWAB_LOGIN_ID` / `SCHWAB_PASSWORD` from `schwab-web.env`).
  `download` / `load` remain host-venv. Never weaken auth (root
  [CLAUDE.md](../../CLAUDE.md) §3): no `--password` flag, no bypassing
  2FA, no persisting the password.

## 0. The login browser surface — auth only, never act on the account

The OAuth `login` flow may navigate only: the Schwab login form, the 2FA
challenge, the account-selection step, and the final **"Allow" / consent
button** that grants the API app read access. That consent click is the
*only* permitted mutation. Do NOT navigate to, click, or script any
account surface (positions, transfers, trade, settings) reachable from
the logged-in session — the browser exists solely to complete the OAuth
grant and capture the `?code=…` redirect.

## 1. Read-only Schwab access — never call write endpoints

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. The
concrete surface for schwab-api:

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

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
