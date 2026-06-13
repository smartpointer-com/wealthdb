# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The angellist-specific surface below
applies on top of those shared rules.

This collector is **implemented and working end-to-end** (2026-06-09):
`login` (real-Firefox cookie lift) → `download` (Camoufox + injected
cookie, passive GraphQL capture + tax-doc fetch) → `load` (event-sourced
silver) → gold adapter (`wealthdb/internal/silver/angellist/`). The
allow/forbid surface below is binding.

## 1. Read-only AngelList Investor Portal access — never trigger writes

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. The
concrete surface for angellist:

Allowed — once mapped, only these surfaces may be navigated or clicked:

- The Investor Portal login form and the 2FA challenge page that
  follows it (handled by the human in `login`; the tooling types no
  credentials — see §3).
- Read-only LP reporting surfaces: portfolio / holdings summary,
  per-investment (SPV / fund) detail and capital-account statement,
  activity / transaction history (capital calls, distributions, fee
  draws), and the documents / tax centre (K-1s, capital-account
  statements, quarterly reports).
- Date-range / period filter inputs and "Apply" / "Search" buttons on
  the above pages.
- Export / download buttons that produce CSV / XLS / PDF copies of
  already-displayed data, and document-download endpoints reachable via
  the session cookie (fetched through Playwright's request API).
- The venture LP read routes
  `venture.angellist.com/v/<user>/i/<investAccount>/{portfolio,commitments,
  taxes-and-documents,funding-accounts}` (the last is the dated cash ledger +
  balance, read-only) and the investor portal `portal.angellist.com`,
  plus the read-only GraphQL the SPA fetches from
  `venture.angellist.com/venture/graphql` (captured passively by
  `download` — we only navigate; the SPA issues the queries).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- **Investing / subscription flows** — committing capital to a new SPV
  or fund, increasing an existing commitment, reserving an allocation,
  "Invest" / "Commit" / "Back this deal", or e-signing any subscription
  or side-letter document.
- **Funding / banking mutations** — adding or editing linked bank
  accounts or wire instructions, initiating or confirming a capital-call
  payment, ACH / wire setup, distribution-election changes.
- **Account / profile mutations** — email, password, 2FA factor,
  accreditation / KYC details, tax forms (W-9/W-8) submission, notification
  or beneficiary settings.
- **The syndicate-lead / fund-manager / fund-admin surface.** An
  AngelList login can carry more than the LP role (the same person may
  also be a syndicate lead or fund GP). Any "Manage fund", "Lead",
  "Raise", "Investor management", or fund-admin / Data-Room console is
  **out of scope** — this collector observes the LP's own positions
  only. Treat manager surfaces as forbidden even if the login surfaces
  them; never use an elevated role to read or change anything.
- **Messaging / community** — contacting a fund manager, deal comments,
  document-request forms.
- Any "confirm" / "submit" / "sign" / "save" / "send" button outside the
  login + 2FA forms themselves.
- Anything that performs a `POST` / `PUT` / `DELETE` other than the
  login + 2FA forms, the read-only filter Apply actions, and explicit
  export / document-download triggers.

When in doubt during `explore`, treat a surface as forbidden until it
appears in the allow-list above.

## 2. Protect the session

The session cookie (`_angellist_v2`, domain-wide `.angellist.com`,
~27-day lifetime) is the keys to the kingdom — it can move money via the
investing/funding flows above. `login` lifts it from a real Firefox
into these `~/.secrets/` artefacts; treat each as a credential (0600/0700):

- `angellist-cookies.json` — the extracted session cookie jar that
  `download` injects.
- `angellist-fxprofile/` — the stock-Firefox profile `login` logs into
  (holds the live plaintext `cookies.sqlite`).
- `angellist-profile/` — the Camoufox profile used by `explore`.

Don't invalidate without cause: re-run `login` only when the cookie
genuinely expires (each one is a real human login + 2FA on the user's
device). Don't log out programmatically; don't add a routine "fresh login
per N runs".

## 3. Authentication is a by-hand login — never weaken it

The auth path is `login` (a bring-your-own-cookie bootstrap): the user logs
into a genuine Firefox **by hand** (that is what clears AngelList's invisible
anti-bot challenge). The
tooling **types no credentials and runs no automated login** — it only
lifts the resulting cookie. Per root [CLAUDE.md](../../CLAUDE.md) §3: never
bypass, downgrade, or "temporarily disable" 2FA; never add a `--password`
flag; never persist a password to disk. Do **not** try to automate the
venture SPA login (bot-walled by design) or to forge/replay the
`x-al-gql` GraphQL signature — drive the real browser instead.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and §4
(no private information in source). They apply in full here.

This collector's PII surface is unusually sensitive: K-1s and
capital-account statements carry the LP's legal name, SSN/EIN fragments,
and exact per-vehicle dollar figures, and SPV / fund names are
themselves identifying. **Never** copy a real SPV / fund / manager name,
LP name, commitment amount, or document into a tracked file (source,
fixtures, comments, commit messages). Synthetic placeholders only — e.g.
"SPV Alpha", "Fund I", round example figures. The raw artefacts live
only under `~/wealthdb/angellist/` and `~/.secrets/`, never in the repo.
