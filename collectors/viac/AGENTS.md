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

Allowed surfaces — `login.py` and `download.py` (the only scripts
that touch the network) may reach only the following; treat
anything not listed as forbidden:

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

## Authentication & private data

Root [AGENTS.md](../../AGENTS.md) §3 and §4 apply in full. Two viac
identifiers need the same care as an account number:

- the login id, `VIAC_LOGIN`, which is a mobile phone number;
- the portfolio number, which embeds the customer id
  ([DESIGN.md](DESIGN.md) §3.1).
