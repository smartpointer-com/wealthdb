# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The erstebank_at-specific surface below
applies on top of those shared rules.

This collector targets **George**, the retail e-banking portal of Erste
Bank und Sparkassen in Austria (`george.sparkasse.at`). The name is
namespaced to Austria because Erste runs a distinct George system in each
country. Scope is two account kinds: **checking accounts (Girokonto)** and
**credit cards**, as far as George itself serves them.

The surface is **not mapped yet**. DESIGN.md §H lists what the first
explore must answer. Until it is mapped, the allow list below is stated
against UI surfaces rather than endpoints, and it errs wide on the forbid
side. Each explore narrows it to the endpoints it records. Never widen it
without the user opting in in writing. Mapping a new endpoint does not
widen scope.

**The UI is entirely German.** Never key a selector on display text —
English *or* German — where an id / test-id / ARIA role exists. Where a
German label is load-bearing (no stable id), record it in DESIGN.md so
drift is diagnosable. Repo prose stays English.

## 0. Never run a real session unprompted

Root [AGENTS.md](../../AGENTS.md) §2 applies with extra force here.
`explore` mints a real George session and fires a real **s Identity
push** at a real phone. Repeated sign-ins can trip fraud heuristics into
a lock-out.

Allowed without asking:

- reading code, config and docs, and reading an existing capture under
  the debug dir;
- `make build-erstebank_at` / `make test-erstebank_at`, unit tests,
  `--help` on any verb;
- `prune` (host-side file walk; `--dry-run` first).

Not allowed unless explicitly asked: anything that touches the live site.
That covers a real `explore` and "just checking the selectors" against the
live login page. The future `login`, `download` and `download --dry-run`
count as signing in until this file says otherwise: whether a session
survives the browser is still unmeasured (DESIGN.md §F). Every live run is
explicitly requested with the user present. Never fire sign-ins in quick
succession.

## 1. Read-only George access — never trigger writes

Root [AGENTS.md](../../AGENTS.md) §1 mandates read-only access. George is
especially dangerous: a new payment sits one click from every account
view, and a single submit plus an s Identity approval can move real money
irreversibly. The s Identity app signs payments with the same push that
signs the login, so **any push other than the one the sign-in itself
fired must never be approved**. Navigation stays allow-list-tight.

Allowed — only these surfaces may be driven (all read-only except the
sign-in itself):

- **The sign-in and its s Identity challenge**: the user-name form, the
  push wait screen with any comparison code it shows, its resend or
  cancel control, any factor-choice screen the challenge offers (read
  only — choosing a factor is part of signing in, enrolling one is not),
  and the OIDC / token plumbing behind them.
- **Checking accounts (Girokonto)**, read-only: the account overview and
  roster, each account's detail, its transaction list (search, filter and
  paging are reads), one transaction's detail view, and the balance as
  displayed.
- **Credit cards**, read-only, as far as George serves them: the card
  roster, each card account's balance / limit / due date as displayed,
  its transaction list and detail, and its statements. A card portal on
  another site is out of scope: record that the link exists, never follow
  it.
- **Transaction exports** of an in-scope account (CSV, JSON or whatever
  formats George offers): triggering an export is a read.
- **Statements (Kontoauszüge / Kreditkartenabrechnungen)** of in-scope
  accounts: the listing and the PDF download, including a read-only
  "generate statement" trigger. Fetch documents of in-scope accounts
  only.
- Logout (optional; not required between runs, but harmless).

The dashboard fires calls outside this list **on its own** when its
routes load (product tiles, Postfach unread count, plugin and marketing
widgets, spending statistics). That passive traffic is not scope
widening. The collector's own code must never call those endpoints or
navigate their surfaces.

Forbidden — do not navigate to, click, or scrape:

- **Anything that moves money, in all its forms** — payments and
  transfers (domestic / SEPA / international / instant), standing orders
  (Daueraufträge), direct-debit mandates (Lastschrift / SEPA-Mandat),
  templates (Vorlagen), scheduled or batch payments, payment requests,
  QR / photo payments, transfers between own accounts, card repayment.
  Any control amounting to "Neue Zahlung", "Überweisung", "Auftrag",
  "Senden", "Unterschreiben", "Zeichnen", "Freigeben", "Bestätigen" —
  and any s Identity approval other than the sign-in's own.
- **Card lifecycle and card settings** — limits, blocks / unblocks,
  geo-blocking, PIN, activation, replacement, online-payment (3-D Secure)
  settings, digital-wallet enrolment, instalment or "pay later" options
  on a card charge. Reading a card's data is allowed; changing anything
  about the card is not.
- **Savings and securities** — any savings account (Sparen,
  Sparkonto, Kapitalsparbuch), Depot / Wertpapiere / Fonds / brokerage,
  pension or insurance products, Bausparen.
- **Loans and financing** — Kredite, overdraft (Überziehungsrahmen)
  changes, leasing, any credit offer.
- **Account lifecycle and offers** — opening or closing any account or
  product, product offers and upgrades, account switching services.
- **George plugins and the George store** — activating, buying or
  configuring any plugin.
- **Profile, settings and s Identity** — contact details, user name or
  alias, password, signature limits, notifications, statement-delivery
  settings, s Identity device pairing or method changes, and any privacy,
  data-sharing or third-party-access (open-banking) consent surface —
  granting it would expose data to an external party.
- **The Postfach (message center)** beyond reading statements — reading,
  composing or sending messages, and the advisor-contact surfaces. If
  statements are delivered there, only their listing and PDF fetch are
  allowed.
- Any "confirm" / "submit" / "send" / "save" control outside the sign-in
  flow itself.
- Anything that performs a `POST` / `PUT` / `PATCH` / `DELETE` other than
  the sign-in and its challenge, a read-only search or export trigger,
  and a statement download.

When in doubt during `explore`, treat a surface as forbidden until it
appears in the allow-list above.

## 2. Protect the session and any device trust

The George session is the keys to the kingdom: it can move money through
the flows above, subject to an s Identity approval.

- `/secrets/erstebank_at-profile/` is the persistent Camoufox profile
  dir. It holds the session cookie and any browser or device-trust
  state. Keep it at 0700; treat the whole dir as a credential.
- `--fresh` wipes the profile deliberately, to force the unknown-device
  path. Never add a routine fresh-login pattern.
- Don't log out programmatically at the end of any verb.

## 3. Never weaken authentication

Per root [AGENTS.md](../../AGENTS.md) §3: never bypass, downgrade, or
"temporarily disable" 2FA. Credentials arrive via env only, from
`~/.secrets/erstebank_at.env`: `ERSTEBANK_AT_USERNAME` (the George user
name). The form is expected to ask for nothing else. Should explore show
a password field, `ERSTEBANK_AT_PASSWORD` joins it — never a
`--password` flag, never persisted. The s Identity approval happens on
the phone; nothing code-like is ever accepted on argv. If the site
challenges the browser, the answer is a better stealth profile, never an
auth bypass.

## 4. Private data

Root [AGENTS.md](../../AGENTS.md) §4 applies in full. George PII covers:

- IBANs, account numbers, card numbers (even masked), the user name;
- balances, limits, transaction counterparties and amounts;
- statement PDFs and export files;
- names and addresses;
- **the account roster itself**: which accounts or cards the login
  holds, their number or type, or that a product is absent.

None of it enters tracked files (source, fixtures, comments, commit
messages). Use synthetic placeholders and round figures only. Synthetic
IBANs use the placeholder-letter pattern `AT<chk><BBBBB><RRRRRRRRRRR>`
with obviously fake digits. Describe scope as the account *kinds* handled
("checking accounts and credit cards"), never what this login was seen to
contain. German UI *labels* are not PII; recording load-bearing ones in
DESIGN.md is expected.

The explore debug dir (`~/.cache/wealthdb/debug/erstebank_at/`) carries
full response bodies and downloaded documents. The harness masks the user
name and session tokens
([collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze)),
but everything else there is real account data. Treat the dir as
sensitive. Never commit anything derived from it without stripping
identifiers first.
