# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The relevate-specific surface below
applies on top of those shared rules.

## 1. Read-only Relevate access — never trigger write actions

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. The
concrete surface for relevate:

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

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
