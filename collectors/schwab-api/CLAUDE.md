# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The schwab-api-specific surface below
applies on top of those shared rules.

- Schwab refresh tokens have a hard 7-day lifetime, renewable only via
  an interactive browser login.

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
