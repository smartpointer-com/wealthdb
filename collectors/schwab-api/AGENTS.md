# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The schwab-api-specific surface below
applies on top of those shared rules.

- Schwab refresh tokens have a hard 7-day lifetime, renewable only via
  an interactive browser login.
- `login` drives that re-auth through a headed Camoufox browser in a
  container (`vnc-login` over VNC). It reuses the `schwab-web` login
  credentials (`SCHWAB_LOGIN_ID` / `SCHWAB_PASSWORD` from
  `schwab-web.env`); the OAuth app credentials are `SCHWAB_CLIENT_ID` /
  `SCHWAB_CLIENT_SECRET` in `schwab-api.env`. The other verbs run on the
  host venv.
- The factor is a 2FA code read from stdin. `login` and `vnc-login` sign
  in and fire it, so they are real sessions (root
  [AGENTS.md](../../AGENTS.md) §2).
- `login --check` reads only the token file: no browser, no network.
  `download --dry-run` uses the stored token and signs in to nothing: it
  refreshes the access token, lists account hashes, and writes nothing.

## 0. The login browser surface — auth only, never act on the account

The OAuth `login` flow may navigate only: the Schwab login form, the 2FA
challenge, the account-selection step, and the final **"Allow" / consent
button** that grants the API app read access. That consent click is the
*only* permitted mutation. Do NOT navigate to, click, or script any
account surface (positions, transfers, trade, settings) reachable from
the logged-in session — the browser exists solely to complete the OAuth
grant and capture the `?code=…` redirect.

## 1. Read-only Schwab access — never call write endpoints

Root [AGENTS.md](../../AGENTS.md) §1 mandates read-only access. The
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
