# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The viac-specific surface below
applies on top of those shared rules.

Which verbs open a session (root [AGENTS.md](../../AGENTS.md) §2):

- `login` signs in and sends an SMS mTAN to the registered phone.
- `login --check` probes the stored cookie jar with one GET. It
  submits no credential and sends no SMS.
- `download`, `--dry-run` included, reuses the stored cookie jar and
  never signs in. A `--dry-run` walks the JSON endpoints and writes
  nothing.

## 1. Read-only VIAC access — never trigger write actions

Root [AGENTS.md](../../AGENTS.md) §1 mandates read-only access. The
concrete surface for viac:

Allowed calls — `login.py` and `download.py` (the only scripts
that touch the network) replay VIAC's REST API and drive no
browser. They may make only the following calls; treat anything
not listed as forbidden:

- The auth flow of [DESIGN.md](DESIGN.md) §2.1: `GET /`, the
  `DELETE /external-login/public/authentication/flow/` flow reset,
  the two `POST` checks (`password/check/`, `mtan/otp/check/`),
  the `GET /external-login/public/authentication/` confirm, and
  `POST /rest/web/customer/loginHook`.
- `GET /rest/web/heartbeat`, the session probe of `login --check`
  and of `download` before its walk.
- The `GET` endpoints listed in DESIGN.md §2.2 (customer profile,
  portfolio inventory, wealth summary / allocation, per-portfolio
  strategy / assets / fees, transactions, document index,
  individual PDFs).

Forbidden — do not call:

- Any other `POST` / `PUT` / `PATCH` / `DELETE`.
- Contribution / deposit endpoints ("Einzahlung", "Beitrag",
  "Vorsorge-Beitrag").
- Strategy change / fund-switch endpoints ("Strategie ändern",
  "Anlage anpassen").
- Withdrawal / payout requests ("Auszahlung", "Bezug",
  "Vorbezug").
- Beneficiary management ("Begünstigung", "Erbschaft").
- Card management or any "request card / change limits" surface
  VIAC may expose for its WIR-bank-side cash accounts.
- Payment-information endpoints.
- Profile / settings endpoints that mutate account state
  (notification prefs, MFA factor management, contact details).

## Authentication & private data

Root [AGENTS.md](../../AGENTS.md) §3 and §4 apply in full. Two viac
identifiers need the same care as an account number:

- the login id, `VIAC_LOGIN`, which is a mobile phone number;
- the portfolio number, which embeds the customer id
  ([DESIGN.md](DESIGN.md) §3.1).
