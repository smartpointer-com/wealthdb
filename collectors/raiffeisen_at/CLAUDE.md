# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The raiffeisen_at-specific surface below
applies on top of those shared rules.

This collector targets **Mein ELBA**, the Austrian Raiffeisen retail
e-banking portal (`mein.elba.raiffeisen.at`). The name is namespaced to
Austria because Raiffeisen operates distinct banking systems in other
countries. Two discovery captures (2026-08-15, DESIGN.md §3-Observed)
mapped the surface: an OIDC login on `sso.raiffeisen.at` driving a
`kunde-login-ui` REST API (pushTAN every login — no trusted-device
bypass), an Angular SPA over per-widget REST endpoints under `/api/…`
on `mein.elba.raiffeisen.at`, and a document archive
(`bankingquer-dokumentenablage`) holding the statement PDFs (there is
no on-demand statement generator). The allow list below is stated
against those observed endpoints and still errs wide on the forbid
side. Tighten it as more traces land; never widen it without the user
opting in in writing. Mapping a new endpoint does not widen scope.

**The UI is entirely German.** Never key a selector on display text —
English *or* German — where an id / test-id / ARIA role exists; where a
German label is load-bearing (no stable id), record it in DESIGN.md so
drift is diagnosable. Repo prose stays English.

## 0. Never run a real session unprompted

Root [CLAUDE.md](../../CLAUDE.md) §2 applies with extra force here:
`explore` is not a probe — it mints a real Mein ELBA session and fires a
real **pushTAN** confirmation at a real phone, and repeated automated
logins can trip fraud heuristics into a lock-out (the Chase sibling
soft-blocked after ~6 rapid logins in one day). Allowed without asking:
reading code/config/docs, `make build-raiffeisen_at` /
`make test-raiffeisen_at`, `--help`, and unit tests. Not allowed unless
explicitly asked: anything that touches the live site — a real
`explore` (or later `login` / `download`), or "just checking the
selectors" against the live login page. Every live run is explicitly
requested with the owner present; waits on a pushTAN approval use long
timeouts (1h+); never fire logins in quick succession.

## 1. Read-only Mein ELBA retail access — never trigger writes

Root [CLAUDE.md](../../CLAUDE.md) §1 mandates read-only access. The
ELBA retail UI is especially dangerous: payments (Überweisungen) sit one
click from the account overview, and a single submit + pushTAN approval
can move real money irreversibly. Navigation stays allow-list-tight.

Allowed — only these surfaces / endpoints may be driven (all read-only
except the login sequence itself):

- **The sign-in + pushTAN sequence** on `sso.raiffeisen.at`: the
  `/mein-login/*` screens (region/Mandant select, Verfüger + PIN form,
  the pushTAN wait screen with its resend control) and the
  `kunde-login-ui` REST calls behind them —
  `rest/config/*`, `rest/identify`, `rest/login/verfueger/<id>`,
  `rest/identify/<id>/pin`, `rest/identify/challenge/switch`,
  `rest/login/pushtan` (+ the `rest/login/pushtan/<signaturId>` poll),
  `rest/login`, `rest/notification/*`, `rest/wallpaper/` — plus the
  OIDC plumbing (`/as/authorization.oauth2`, `/start/`,
  `/as/token.oauth2`, `/pf/JWKS`, the resume URL).
- **Deposit-account data** (checking [Girokonto] + savings
  [Sparkonto]) on `mein.elba.raiffeisen.at` — the SPA's overview /
  Umsätze routes and their REST endpoints:
  - roster: `GET /api/bankingws-widgetsystem/bankingws-ui/rest/produkte`
    (kept to `type == "KONTO"` rows);
  - history: `POST /api/bankingzv-umsatz/umsatz-ui/rest/kontoumsaetze`
    (a read-only search `POST`), plus its config/lookup side calls
    (`rest/config`, `rest/kategorienByCode`, `rest/kontohashtags`,
    `rest/gesendeteAuftraege`, `rest/kontomitteilungen/<IBAN>`,
    `rest/umsaetzeZuletztGesehenAm`);
  - balances: `GET …umsatz-ui/rest/kontostaende/<IBAN>?von=&bis=`;
  - account detail: `GET …umsatz-ui/rest/konten/<IBAN>`,
    `GET /api/bankingzv-konto/kontozentrale-ui/rest/konten/<IBAN>`, and the
    account-information page
    `GET /api/bankingzv-kontoinformationen/kontoinformationen-ui/rest/konten/<IBAN>/details`
    (account type, currency, institution/BIC, interest rates). Its response
    carries an **incidental card-limits block** — captured as provenance
    only, never acted on (deposit-only scope);
  - the session keepalive
    (`GET …/bankingws-ui/rest/keepalive`).
- The **document archive** (Dokumente, widget
  `bankingquer-dokumentenablage`) — read-only listing + PDF fetch of
  account statements (Kontoauszüge):
  `POST …/dokumentenablage-ui/rest/dokumente/filter` (list; a
  read-only search `POST`),
  `POST …/rest/dokumente/<systemId>/<dokumentenId>/download` and
  `GET …/rest/dokumente/metadata/<systemId>/<dokumentenId>`. This is a
  pre-generated archive, not an on-demand generator; the download
  `POST` returns a copy and mutates nothing. Keep to the statement
  documents for the scoped deposit IBANs — do not fetch documents for
  any out-of-scope product.
- The **Statement-of-Fees PDF** (Entgeltaufstellung / Entgeltnachweis)
  over a date range —
  `GET …/kontoinformationen-ui/rest/konten/<IBAN>/entgeltnachweise?datumVon=&datumBis=`
  — an allowed read-only export trigger. Mapped but not fetched by
  default (informative-only; DESIGN.md §E).
- Logout (optional; not required between runs, but harmless).

The dashboard SPA fires widget calls outside this list **on its own**
(card overview, mailbox unread count, spending statistics, marketing
tiles) when its routes load; that passive traffic is not scope
widening. The collector's own code must never call those endpoints or
navigate their surfaces.

Forbidden — do not navigate to, click, or scrape:

- **Anything that moves money, in all its forms** — Überweisungen
  (transfers, domestic / SEPA / international), Daueraufträge (standing
  orders: create, edit, pause, delete), Lastschrift / SEPA-Mandat
  management (direct debits), payment templates (Vorlagen), scheduled /
  batch payments, payment requests. Any control amounting to "Neue
  Überweisung", "Zahlung", "Auftrag", "Senden", "Freigeben",
  "Unterschreiben", "Zeichnen" — and any pushTAN approval for anything
  other than the login itself.
- **Card surfaces (Karten) — read *and* write.** Scope is deposit-only,
  so any debit-/credit-card products the login exposes are out of scope
  entirely; card management (lock, replacement, PIN, limits) is
  forbidden on top of that.
- **Any securities / wealth surface** the login may expose — Depot,
  Wertpapiere, Fonds, brokerage, Vorsorge / insurance, Bausparen,
  Kredite / financing. This collector observes the retail deposit
  relationship only.
- **Account lifecycle & offers** — opening or closing accounts or
  products, product offers / upgrades, overdraft (Überziehungsrahmen)
  changes, linked-account setup.
- **Profile / settings / TAN-method mutations** — contact details,
  password / PIN, Verfüger management, pushTAN / TAN-method enrolment or
  device management, signature limits (Zeichnungslimits), alerts,
  statement-delivery settings, privacy or data-sharing grants (any
  third-party-access / open-banking consent surface — granting it would
  expose data to an external party).
- **The message center / mailbox (Postfach, Mitteilungen; widget
  `bankingvt-mailbox`)** — reading, composing, or sending messages, the
  Berater/Bank contact surfaces. This is distinct from the document
  archive (`bankingquer-dokumentenablage`, allowed above): the archive
  holds statement PDFs and is read-only; the mailbox is correspondence
  and stays out of scope.
- Any "confirm" / "submit" / "send" / "save" control outside the
  sign-in + pushTAN flow itself.
- Anything that performs a `POST` / `PUT` / `DELETE` other than the
  sign-in + 2FA flow and the explicit read-only export /
  document-download triggers above.

When in doubt during `explore`, treat a surface as forbidden until it
appears in the allow-list above.

## 2. Protect the session & any device trust

The Mein ELBA session is the keys to the kingdom (it can move money via
the flows above, subject to pushTAN).

- `/secrets/raiffeisen_at-profile/` is the persistent Camoufox profile
  dir. It holds the session cookie and any browser/device-trust state.
  Keep it at 0700; treat the whole dir as a credential.
- Trust persistence is measured (DESIGN.md §3-Observed·§F): the
  profile persists a `profilToken` that remembers the *identity* (skips
  region/Verfüger/PIN entry) but **not** the second factor — pushTAN
  fires on every login. So the profile still holds sign-in state worth
  protecting, but it is never a route past 2FA. Don't invalidate it
  without cause: `--fresh` wipes the profile deliberately (to capture
  the full challenge); never add a routine fresh-login pattern.
- Don't log out programmatically at the end of any verb.

## 3. Never weaken authentication

Per root [CLAUDE.md](../../CLAUDE.md) §3: never bypass, downgrade, or
"temporarily disable" 2FA; credentials arrive via env only, from
`~/.secrets/raiffeisen_at.env`: `RAIFFEISEN_AT_USERNAME` (the
personal, unprefixed Verfüger number), `RAIFFEISEN_AT_PASSWORD` (the
PIN), and `RAIFFEISEN_AT_REGION` (the Mandant code that selects the
regional bank and determines the Verfüger prefix — DESIGN.md
§3-Observed·§B) — never a `--password` flag, never persisted. The
pushTAN approval happens on the phone (no fallback factor exists on
this flow); nothing code-like is ever accepted on argv. No bot
defense has been observed; if the site ever challenges the browser,
the answer is a better stealth profile in `explore`, never an auth
bypass.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.

Raiffeisen PII: IBANs and account numbers, the Verfüger number /
username, balances, transaction counterparties and amounts, statement
PDFs, names and addresses — **and the account roster itself** (which
accounts/products the login holds, their number or type, or that a
product is absent; see root [CLAUDE.md](../../CLAUDE.md) §4). None of
it enters tracked files (source, fixtures, comments, commit messages) —
synthetic placeholders and round figures only. Synthetic IBANs use the
placeholder-letter pattern `AT<chk><BBBBB><KKKKKKKKKKK>`. Describe scope
as the account *kinds* handled ("deposit accounts: checking + savings;
cards out of scope"), never what this login was seen to contain. German
UI *labels* are not PII — recording load-bearing ones in DESIGN.md is
expected. The explore debug dir
(`~/.cache/wealthdb/debug/raiffeisen_at/`) carries full response bodies
and downloaded documents; the harness redacts the username + password
from `network.jsonl`, but everything else in there is real account data
— treat the dir as sensitive and never commit anything derived from it
without stripping identifiers first.
