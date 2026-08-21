# raiffeisen_at — design notes

Design notes for the Austrian Raiffeisen (Mein ELBA) retail collector.
Sections: scope and what is known (§1), the harness itself (§2), what
Phase 1 set out to capture (§3), the observed answers from the first
authenticated capture (§3-Observed), the phase roadmap (§4), and the
handoff checklist (§5) — the layout [chase](../chase/DESIGN.md) and
[firstcitizens](../firstcitizens/DESIGN.md) established. Discovery is
complete (two capture sessions); the only residual probes are in
§3-Observed·§H.

## 1. Scope & what is known so far

- **The relationship.** A retail deposit-banking relationship at an
  Austrian Raiffeisen bank, served through the group-wide **Mein ELBA**
  e-banking portal (entry `https://mein.elba.raiffeisen.at/`). **Scope
  is deposit accounts only** (checking [Girokonto] + savings
  [Sparkonto]); cards, financing, and any securities / wealth surface
  the same login may expose are out of scope in code and docs (see
  CLAUDE.md). The collector is named `raiffeisen_at` because Raiffeisen
  operates distinct banking systems in other countries.
- **The playbook is chase, refined by firstcitizens.** Chase is the
  original validated retail-deposit playbook (one-shot browser login
  with terminal-driven 2FA, exports to bronze, statement-PDF backfill,
  cash accounts as conduits in gold); firstcitizens is the most recent
  run of the same playbook and carries the divergences experience
  forced (its DESIGN.md "Observed" sections). Every design choice below
  starts from those two and diverges only where a capture says this
  source differs.
- **Conduit accounts.** The cash accounts are conduits — cash passes
  through them on its way to and from other sources. Their transactions
  matter for cross-source money-flow tracking; per-account returns are
  meaningless and are blanked by the registered ReturnsPolicy
  (`internal/silver/raiffeisen_at/policy.go`, `AccountsGrainMeaningless`);
  the coarse grains keep the accounts and count their flows.
- **2FA is pushTAN (confirmed §3-Observed·§A):** the login is
  confirmed in the Raiffeisen mobile app; nothing is typed. The
  completion signal is a plain REST poll (`loggedIn` flip), the
  browser shows a 4-char Vergleichswert to announce in the terminal,
  and **no fallback factor exists** (`challenge/switch` returned
  empty).
- **Trust persistence: measured — chase's shape (§3-Observed·§F).**
  The persistent profile remembers the identity (a stored
  `profilToken` skips region/Verfüger/PIN entry), but **pushTAN fires
  on every login** — no trusted-device bypass. So `login` cannot run
  unattended and **folds into `download`**: one browser lifetime per
  run, terminal pushTAN each time.
- **Exports: CSV only (confirmed, and demoted — §3-Observed·§D):**
  CAMT.052/053 and MT940 were looked for and not found, and the CSV
  turned out to be a client-side dump of the history JSON with the
  ids and field structure stripped. The JSON is the ledger source;
  the CSV export is not driven at all.
- **Statement PDFs are a document archive (corrected §3-Observed·§E).**
  The pre-capture belief was an on-demand custom-range generator; the
  reality is a pre-generated PDF archive ("Dokumente",
  `bankingquer-dokumentenablage`) — monthly Kontoauszüge with stable
  document ids, listed and downloaded like firstcitizens. It reaches
  back only ~5 months past the transaction floor, so it feeds bronze
  as provenance, not a deep backfill (§G).
- **The UI is entirely German.** Selectors never key on display text
  where an id / test-id / ARIA role exists; load-bearing German labels
  are recorded here as captures land. Repo prose stays English.
- **The wiring is a clean per-widget REST/JSON API (measured
  §3-Observed).** The legacy-bank server-rendered-HTML worry did not
  materialise: the app is an Angular SPA, every datum arrives from
  `/api/<widget>/<widget>-ui/rest/…` with a Bearer token, and the
  history endpoint is loader-grade (stable ids, booking + value
  dates, keyset pagination). The data path needs no DOM scraping.
- **Bot defense: none observed (measured §3-Observed); Camoufox
  stays.** The capture shows only raiffeisen.at properties — no
  Akamai/ThreatMetrix-class sensors — and Camoufox cleared the login
  unchallenged. The harness keeps
  `shared/images/base-camoufox.Dockerfile` and the persistent-profile
  pattern regardless (proven fleet-wide, costs nothing).

## 2. The discovery harness (`explore`)

`explore.py` is copied and adapted from `firstcitizens`'s (itself from
chase's). The duplication across the explore harnesses is a tracked,
deliberate decision: each drifts with its source, so they are copied
and adapted, not extracted into a library.

Because the ELBA wiring is unknown (§1), **both** capture channels
matter here: the HAR / network log maps whatever XHR/REST traffic
exists, and the **DOM snapshots pin the selectors** — if the data path
turns out to be server-rendered HTML, the snapshots are the only record
selectors can be written from (network + click logs alone pin nothing).

One run records, under `/debug/<UTC-ts>/` (host:
`~/.cache/wealthdb/debug/raiffeisen_at/`):

| Artefact | Purpose |
| --- | --- |
| `network.har` | The primary endpoint map — every request + response. Flushed only on a clean context close. |
| `network.jsonl` | Crash-safe line-flushed twin of the HAR; text bodies ≤ 200 KB captured inline, OFX/QFX content types included as defence. |
| `clicks.jsonl` | Click log via an injected `document.addEventListener` (VNC clicks bypass the Playwright API), plus lifecycle, login-form and code-field events. |
| `dom/<NNN>/` | **Every distinct screen's full DOM** (all raiffeisen.at frames) + a screenshot, deduped by DOM structure — the record selectors are pinned from. |
| `downloads/` | Every file the session fetches (statement PDFs, CSV exports), sequence-prefixed against reused filenames. |
| `trace-chunks/`, `trace.zip` | Opt-in `--trace` Playwright trace — off by default because the pinned Playwright 1.49 tracer crashes the camoufox 152.0.4 build (matched-set drift; see base-camoufox). |

Mechanics carried over from chase/firstcitizens (see chase's DESIGN.md
§2 for the full rationale): frame-aware login pre-fill gated to
raiffeisen.at frames, both-fields-in-one-frame before either is
touched, fill-once with read-back verification, `signon.*` prefs off so
a profile-saved credential can never autofill on top of the
programmatic fill, a one-time-code-field detector that logs the field's
static descriptor (never its value; pushTAN likely means no code field
— the detector records any fallback factor), and username/password
redaction across every logged header and body. Sign-in and 2FA are
submitted by hand over VNC; the harness never clicks a button.

ELBA-specific caveat, updated after the first capture: the pre-fill
**stayed idle by design and stays that way**. The Verfüger input
carries no `name`/`autocomplete` hooks (Angular `formcontrolname`
only, which the deliberately-generic selectors don't key), and more
fundamentally the field's correct value is *region-prefixed*
(§3-Observed·§B) — filling the bare `RAIFFEISEN_AT_USERNAME` would be
wrong. The prefix-aware fill is login.py's job; in `explore` the
credentials are typed by hand.

## 3. What Phase 1 must capture — the three flows

Each flow is one continuous VNC-driven session segment. This section is
the original capture brief; every item is now answered in §3-Observed
(flows 1 + 3 in the first session, flow 2 + trust in the second).

### Flow 1 — Login + pushTAN

- **Web technology & bot defense.** What the app is (server-rendered?
  SPA? which framework? web components / shadow DOM?) and what fronts
  it (fingerprinting / bot-manager scripts, sensor headers). Whether
  Camoufox clears the login without a challenge.
- **The login form's shape.** Which identifier it asks for (Verfüger
  number? username? account-scoped ID?) — this fixes what
  `RAIFFEISEN_AT_USERNAME` maps onto — field ids/names, one-step or
  two-step, and where the form lives (main frame? iframe? separate
  host?).
- **The pushTAN challenge.** Its DOM + network shape: what the browser
  shows while waiting, what traffic polls (or pushes) while the app
  waits for the phone, and — decisive for Phase 3 — **the completion
  signal observable from the browser** (a route change? a polled
  endpoint flipping state?). Any resend / retry affordance, timeout
  behaviour, and any fallback factor offered (a code entry? SMS-TAN?).
- **Trust persistence.** Any "trust this browser / device" control and
  its wording. After a successful login, close the browser and run
  `explore` again *without* `--fresh`: whether the second run lands
  authenticated, skips the pushTAN, or repeats the full challenge is
  the single most important data point for Phase 3 — it decides whether
  `login` and `download` split (firstcitizens' shape) or fold into one
  verb (chase's shape). Observe only; no automation is designed around
  the answer now.
- **Session keepalive**, if visible (a heartbeat endpoint matters for
  the session-holder iteration pattern, §5).

### Flow 2 — On-demand statement PDFs (Kontoauszüge)

- Where the statement generator lives and how it is parameterised: the
  date-range controls, per-account selection, any format/layout
  options.
- **The maximum range one request accepts** (a month? a year?
  arbitrary?) and **how far back the generator reaches** — together
  with flow 3's export cap this decides whether a Phase 5 backfill
  exists at all.
- **Idempotency**: generate the same range twice — identical document,
  or a new sequence-numbered artefact each time? (This shapes the
  deterministic range convention Phase 3 imposes.)
- The fetch mechanism the generated PDF arrives by (signed URL?
  session-cookie GET? POST-then-download?), and whether generation is
  synchronous or queued.
- Whether any *delivered* periodic statements exist besides the
  generator (e.g. in a mailbox surface — out of scope to read for now;
  note their existence only).

### Flow 3 — Transaction history + CSV export

- The accounts overview and per-account history (Umsätze): the listing
  UI, its backing traffic (server-rendered HTML vs XHR JSON — and if
  JSON, its shape and whether it carries a stable transaction id /
  running balance), pagination, date-range filters, and **any
  server-side history cap** (how far back the UI can reach at all).
- The CSV export: its trigger and parameters, its **date-range
  limits**, whether it is per-account or cross-account, and its
  **columns** — stable transaction id? running balance? booking date
  vs value date (Buchungsdatum vs Valuta)? amount signing? encoding
  and delimiter (Austrian CSVs are often `;`-separated, `dd.mm.yyyy`,
  comma decimals — verify, don't assume).
- One export per account with an explicit date range so the request
  parameters land in the HAR and the payload in `downloads/`.
- Whether the history view's backing data (if XHR) is richer than the
  CSV — this decides the Phase 4 ledger source.

## 3-Observed — authenticated capture (2026-08-15)

One VNC-driven `explore` session: a full login (region pick → pushTAN →
app) plus a transaction-history walk with two CSV exports. Endpoints
below are masked (`<IBAN>` / `<id>` = elided values); no account
numbers, Verfüger numbers, balances, or the observed region selection
are recorded here — they live only in the debug dir. The statement
generator was **not** walked (§E). Two headline surprises:

1. **The wiring is clean after all.** The app is an Angular SPA
   ("bankingws-widgetsystem") over a per-widget REST/JSON API — the
   legacy-bank server-rendered-HTML worry did not materialise. Data
   arrives from `/api/<widget>/<widget>-ui/rest/…` endpoints with a
   Bearer token; no DOM scraping is needed for the ledger.
2. **No bot defense observed.** Every request in the capture went to
   raiffeisen.at properties (plus one TeleTrader market-data widget);
   no Akamai/ThreatMetrix-class sensor hosts or headers appeared, and
   Camoufox cleared the login with no challenge. Camoufox stays (costs
   nothing, proven fleet-wide), but no stealth escalation is expected.

### §A — Login: OIDC + the `kunde-login-ui` REST flow (flow 1)

Entry `https://mein.elba.raiffeisen.at/` redirects into an **OAuth2 /
OIDC authorization-code + PKCE flow** on `sso.raiffeisen.at`
(PingFederate-style: `/as/authorization.oauth2`, client
`DRB-PFP-RBG-WEB`), which serves its own Angular login app at
`sso.raiffeisen.at/mein-login/identify`. The login app drives a REST
API under `/api/bankingquer-kunde-login/kunde-login-ui/rest/`, in
order:

1. `GET rest/config/context` — session config (30-minute sso session,
   `resumePath` for the OIDC hand-back).
2. `GET rest/config/mandanten` — the **region list** (§B).
3. `GET rest/login/verfueger/<typed>` — fired (debounced) as the
   Verfüger field is typed; returns the **canonical zero-padded id**,
   normalising short forms.
4. `POST rest/identify/<fullVerfueger>/pin` — submits the PIN (the
   password; the form field caps it at `maxlength="5"`). Response
   carries `challengeType: "PUSH"` → pushTAN required.
   `GET rest/identify/challenge/switch` returned `[]` — **no fallback
   factor exists** on this flow; pushTAN is the only path.
5. `POST rest/login/pushtan?language=de` — sends the push. The response
   is the whole polling contract:
   `{signaturId, timeout: 300, firstPollDelay: 3, pollDelay: 3,
   displayText: "<4 chars>", …}`. `displayText` is the
   **Vergleichswert** — a comparison code shown on the browser screen
   *and* in the app; the login screen shows it with a 5-minute
   countdown.
6. `GET rest/login/pushtan/<signaturId>` — **polled every 3 s; the
   response's `loggedIn` flips `false` → `true` on in-app approval.**
   This is the completion signal, and it is plain REST — exactly the
   announce-then-poll shape Phase 3 wanted, with no dependence on
   Playwright response events.
7. `POST rest/login` `{"updateSession": true, "profilToken": null}` →
   `{resumeUrl, loginMethod: "PUSH", profileIdentifier: {profilToken,
   imageToken, deviceId}}`. The SPA then navigates `resumeUrl`
   (`/as/…/resume/as/authorization.ping`) → OIDC code →
   `POST /as/token.oauth2` (PKCE) → Bearer `access_token` (+
   `id_token`), and lands authenticated on
   `mein.elba.raiffeisen.at/bankingws-widgetsystem/…/dashboard`.

The returned **`profilToken` / `deviceId`** is the stored-profile
identity: the login POST sends `profilToken: null` on an unknown
profile and the response mints one, which the SPA persists in the
Camoufox profile. Its effect is now measured — **§F: it skips
credential entry but not the pushTAN.** The form also carries a
"Verfüger speichern" checkbox (`rds-checkbox[formcontrolname=
"checked"]`), which by its label remembers the *username*, not the
device.

**UI technology:** Angular with "RDS" (Raiffeisen Design System)
components — light DOM (no shadow-root walls anywhere in the capture),
auto-generated unstable ids (`rds-input--0`, `rds-option-3`), but
clean **`formcontrolname` anchors** on every form control. Pinned
selectors:

- Region select: `rds-select[formcontrolname="mandant"]`; its options
  mount in an overlay as `rds-option` (`#rds-option-<n>`,
  `span.rds-option-text` labels; option 0 is the empty placeholder).
- Verfüger: `input[formcontrolname="verfuegerNr"]`.
- PIN: `input[formcontrolname="pin"]` (`type="password"`,
  `maxlength="5"`).
- Submit: `button[rds-button][type="submit"]` (label "Weiter").
- Remember-username: `rds-checkbox[formcontrolname="checked"]`.

Load-bearing German labels (orientation only, never selector keys):
"Weiter" (submit), "Verfüger speichern" (remember username), the
region names ("Burgenland", "Kärnten", "Niederösterreich/Wien", …),
and the pushTAN screen's instruction to check the Vergleichswert on
the pushTAN-enabled device.

### §B — The region dropdown & username prefixing (the login twist)

`GET rest/config/mandanten` (unauthenticated, part of the sign-in
surface) returns one entry per **Mandant** — the regional Raiffeisen
banking group — e.g.:

```json
[{"code": "rbgbgld", "verfuegerKennung": "ELVIE33V", "sortOrder": 1,
  "disabled": false}, …]
```

Twelve entries in the capture: the eight federal-state groups
(`rbgbgld`, `rbgk`, `rbgnoew`, `rbgooe`, `rbgsbg`, `rbgstmk`, `rbgt`,
`rbgvlbg`) plus special entities (`rbgooebd`, `rbgooepb`, `rbgtjh`,
`rbgalpenbank`) that share a state group's `verfuegerKennung`. The
dropdown lists them **in `sortOrder`**, so option `#rds-option-<n>`
corresponds to the n-th config entry (option 0 = placeholder).

Picking a region **prefixes the Verfüger field with that Mandant's
`verfuegerKennung`**; the full login id is
`<verfuegerKennung><personal number>` (synthetic example: Burgenland's
prefix + a personal part → `ELVIE33V0V000042`). The backend
normalises short forms — the debounce `GET rest/login/verfueger/…`
zero-pads the personal part to its canonical width — and the
`identify/<fullVerfueger>/pin` POST takes the canonical full id.

**Decision — how login.py handles it:**

- `~/.secrets/raiffeisen_at.env` keeps `RAIFFEISEN_AT_USERNAME` as the
  **personal (unprefixed) Verfüger number**, and gains
  **`RAIFFEISEN_AT_REGION`** holding the **Mandant `code`** (e.g.
  `rbgbgld`) — the code is config-stable where the German display
  label and the option index are not.
- login.py fetches `rest/config/mandanten` live, resolves the code to
  its list position + `verfuegerKennung`, picks
  `#rds-option-<position>` in the `mandant` select, and then fills
  `verfuegerNr` with `verfuegerKennung + RAIFFEISEN_AT_USERNAME` —
  **verifying by read-back that the field's value starts with the
  expected prefix**, so a drifted option order can never submit under
  the wrong Mandant. No German text is ever keyed on.

### §C — The data API (flow 3: roster, history, balances)

All app data rides `/api/<widget>/<widget>-ui/rest/…` on
`mein.elba.raiffeisen.at`, authenticated by the **OIDC Bearer token**
(plus session cookies). The captured token's JWT `exp` was **300 s**
after issue — download.py must handle token refresh (mechanics
unobserved, §E). A `GET …/bankingws-ui/rest/keepalive` (204) exists —
the session-holder heartbeat.

- **Roster:** `GET /api/bankingws-widgetsystem/bankingws-ui/rest/produkte`
  → product cards: `productId` (**the IBAN** — it is also the account
  key in every other endpoint and in the SPA routes),
  `type: "KONTO"` for deposit accounts (the deposit filter), balance
  + available amount under `details`, and the SPA route in
  `clickProduktKarte.destination`
  (`/meine-produkte/konten/<IBAN>/kontozentrale`).
- **History:** `POST /api/bankingzv-umsatz/umsatz-ui/rest/kontoumsaetze`
  with `{"predicate": {"kontotyp": "KONTO", "buchungVon": <ISO>,
  "buchungBis": <ISO|null>, "ibans": ["<IBAN>"], "pending": true, …},
  "limit": N}` → `{"list": […], "info": {"hasMore",
  "minBuchungstag", …}}`. **Keyset pagination**: the next page repeats
  the query with `idBis` / `neuanlageBis` / `buchungBis` taken from
  the last row. The SPA pages at `limit: 200`; its own CSV export
  fetches with `limit: 3001`.
- **Each row is loader-grade**: stable `id` (plus `idDetail`),
  `betrag: {amount, currency}` (signed), **`buchungstag` (booking
  date) and `valuta` (value date)**, description + counterparty lines
  (`verwendungszweckZeile1`, `transaktionsteilnehmerZeile1`,
  `auftraggeberIban`/`Bic`), SEPA mandate fields, a category code,
  and `transaktionsherkunft`. **No per-row running balance** — the
  balance series comes from `kontostaende` instead.
- **History floor:** `info.minBuchungstag` sat exactly at the month
  boundary **3 years back**, matching the UI's earliest offered "ab"
  date — a rolling ~36-month server-side cap on history. The document
  archive (§G) predates it by only ~5 months, so there is no deep tail
  to reconstruct (§G → Phase 5 verdict).
- **Balances:** `GET /api/bankingzv-umsatz/umsatz-ui/rest/kontostaende/
  <IBAN>?von=<date>&bis=<date>` → `{"tagessalden": [{"tag", "saldo"},
  …], "kontostand", "verfuegbarerBetrag"}` — a **parameterizable
  daily closing-balance series** plus the current balance. Reach
  unmeasured (§H); if it spans the history window, gold's
  closing-balance series comes straight from here.
- **Account information (added 2026-08-15, live capture):**
  `GET /api/bankingzv-kontoinformationen/kontoinformationen-ui/rest/konten/
  <IBAN>/details` → `{konto: {kontoart}, detailgruppen: [{ueberschrift,
  details: [{bezeichnung, inhalt}]}]}`. This is the "Kontoinformationen"
  page and carries attributes the roster does **not**: the **Kontoart**
  (account type, e.g. a salary/checking account — `konto.kontoart` is a
  one-letter code, the detail row spells it out), **Währung** (currency),
  holding **institution + BIC + Bankleitzahl**, the **Zinssatz Soll /
  Haben** (debit / credit interest rates, each with an effective date), and
  the **Kontoabschluss** cycle dates. It also returns an incidental
  **Karteninformationen** block (card type + limits) — captured as
  provenance, never acted on (deposit-only; CLAUDE.md). `download` now
  fetches this per account into `details/<IBAN>.json`. The values are PII
  (IBAN, balances, card number, account holder) — bronze only, never
  tracked files.
- Also mapped (allowed, secondary): `GET …umsatz-ui/rest/konten/<IBAN>`
  and `GET /api/bankingzv-konto/kontozentrale-ui/rest/konten/<IBAN>`
  (account detail), `GET …umsatz-ui/rest/gesendeteAuftraege` (sent
  orders overlay the Umsätze view loads — read-only GET).
- The dashboard SPA fires **widget calls outside our scope on its
  own** (card overview, mailbox unread count, spending statistics,
  marketing tiles). Loading the dashboard inevitably triggers them;
  download.py must simply never call them itself.

### §D — CSV export (flow 3): client-side, strictly poorer than the JSON

The Umsätze view's export ("Als CSV speichern") is **generated
client-side**: the SPA fetches the same `kontoumsaetze` JSON
(`limit: 3001`, paged) and assembles a `blob:` download —
`meinElba_umsaetze_<IBAN>_suche.csv`. Shape: `;`-separated, **no
header row**, BOM-prefixed, columns ≈ booking date; one big
concatenated detail string (payee, Verwendungszweck, counterparty
IBAN/BIC, mandate refs all flattened into one quoted field); value
date; amount (comma decimal); currency; a timestamp. **No stable id,
no running balance.**

Decision (mirrors firstcitizens): **the `kontoumsaetze` JSON is the
authoritative ledger** — it is a strict superset of the CSV, carries
the stable id and the clean field split, and the export date-range UI
is bounded by the same 3-year floor anyway. `download` saves the raw
JSON pages to bronze (`raw/` + a merged `history/<IBAN>.json` on the
firstcitizens layout); driving the export UI per run adds nothing and
is dropped — the two captured CSVs stay in the debug dir as
provenance.

### §E — Statements: a document archive, not an on-demand generator (flow 2)

The pre-capture belief (and the harness scaffold) framed statements as
generated on demand over custom ranges. **The second capture
(2026-08-15) corrected this: statements are pre-generated PDFs in a
document archive** — the "Dokumente" surface, widget
`bankingquer-dokumentenablage`. There is no custom-range generator;
the owner's "could not find a way to generate statements older than
~2023" is exactly this — the archive simply doesn't retain older ones.
So the shape is firstcitizens' (list + download by id), not chase's,
and the deterministic-range-convention worry evaporates (documents
carry stable ids). Endpoints:

- **List:** `POST …/dokumentenablage-ui/rest/dokumente/filter`
  `{"von": <ISO|null>, "bis": <ISO|null>, "skip": 0, "limit": 50}` →
  an array of `{systemId, dokumentenId, dokumentenName: {de, en},
  erstellungsDatum, dateiTyp, dateiName, dateiGroesse,
  referenzIds: [{typ: "IBAN", iban, altBezeichnung}]}`. `von`/`bis`
  null returns the **whole archive** (no server date cap on the
  listing itself); `skip`/`limit` paginate. The list is **not**
  per-account in the request — every account's docs come back and are
  filtered client-side by `referenzIds[].iban` (the Umsätze-view
  "Dokumente" shortcut just adds `?type=KONTO&iban=<IBAN>` to the SPA
  route).
- **Download:** `POST …/rest/dokumente/<systemId>/<dokumentenId>/download`
  `{}` → `application/pdf` (Content-Length matches the list's
  `dateiGroesse`). `GET …/rest/dokumente/metadata/<systemId>/
  <dokumentenId>` returns the same metadata for one doc.
- **The `(systemId, dokumentenId)` pair is a stable document key** —
  idempotent dedup in bronze/silver is trivial (no content-hashing,
  no range convention).
- **The download URL carries the `versionsId` when present (resolved
  2026-08-15).** The first full `download` 422'd on all 22 older
  **KDM**-system Kontoauszüge (`versionsId: 1`) while the 19 newer
  **EAZ** ones (`versionsId: null`) downloaded fine. A follow-up capture
  settled it: the working KDM call puts the version in the path —
  `POST …/dokumente/KDM/<dokumentenId>/<versionsId>/download {}` — while
  the versionless EAZ call is `POST …/dokumente/EAZ/<dokumentenId>/download
  {}`. `dokument_download_url` now appends the `versionsId` segment iff the
  document has one, so both lineages fetch. The `(systemId, dokumentenId,
  versionsId)` triple is the stable dedup key (in the bronze PDF name and
  for silver). The `GET …/dokumente/metadata/<systemId>/<dokumentenId>[/
  <versionsId>]` the SPA also fires is not needed for the download.

**Document taxonomy observed** (kinds, not this login's roster):
`dokumentenName.de == "Kontoauszug"` are the account **statements**,
roughly monthly (a statement-cycle cadence), in two `systemId`
lineages — an older one (filename `Kontoauszug.PDF`) and a newer one
(filename `<IBAN>_EUR_<seq>`, the cutover mid-window). Alongside them
sit annual fee notices (`Entgeltmitteilung` / `Entgeltanpassung`) that
are **not** transaction documents. Phase 3 keeps `dokumentenName.de ==
"Kontoauszug"` for the scoped IBANs and files the rest as unparsed
provenance (or skips them).

**Also mapped — the on-demand Statement of Fees (Entgeltaufstellung).**
The account-details page (§C) exposes a **separate** fee-document export:
`GET …/kontoinformationen-ui/rest/konten/<IBAN>/entgeltnachweise?datumVon=
&datumBis=` → `application/pdf` (the EU Payment-Accounts-Directive
Statement of Fees, generated on demand over a date range; a live capture
pulled one month). It is **not fetched by default**: it carries no
transaction or balance data (purely informative), and doing it well needs
a range convention (the regulatory statement is annual). Recorded as a
future opt-in (a `--fees` flag pulling whole calendar years) if the fee
history is ever wanted — the decision is deferred, not the mapping.

### §F — Trust persistence: identity persists, pushTAN does not (the verb-split answer)

The second session was a **close-and-relaunch on the same profile (no
`--fresh`)**. The stored `profilToken` (§A) survived: the login app
did `GET rest/profile/<profilToken>`, showed a **profile card** on the
identify screen, and one click on it went straight to
`POST rest/identify/<verfueger>` **with the token as the body — no
region pick, no Verfüger typing, no PIN** — and then
`POST rest/login/pushtan`. **The pushTAN fired again.** So:

- the persistent Camoufox profile **remembers the identity** (skips
  the whole region/Verfüger/PIN entry), but
- **2FA fires on every login** — there is no trusted-device bypass.

This is **chase's shape, not firstcitizens'**: `login` cannot run
unattended, so it **folds into `download`** — one browser lifetime per
run, the pushTAN driven from the terminal each time. login.py must
still implement the full credential path (region + Verfüger + PIN, §B)
because a cold/`--fresh`/expired profile shows no card; the warm-profile
happy path is just "click the card → approve pushTAN". Detect which by
whether the profile card is present on the identify screen.

### §G — History floor vs archive floor: no deep backfill (Phase 5 verdict)

- Transaction-history JSON floor (§C): **~2023-08** (rolling 36
  months).
- Statement archive floor (§E): the earliest **Kontoauszug** in the
  capture was **~2023-04**; older archive entries are only annual fee
  notices (from 2021), which carry no transactions.

The statement PDFs therefore predate the JSON ledger by only ~4–5
months (≈ 2023-03…2023-07), and it is a **fixed, non-growing** window
(both floors roll forward together). Unlike chase (7-year statements
behind a 2-year export), there is **no deep tail worth a
reconcile-gated backfill.** Phase 5 verdict: **download statements to
bronze as documents** (provenance, on the firstcitizens model), and
**defer/skip** parsing them for the ~5 months of extra transactions
unless that slice is explicitly wanted — a low-value PDF-parser build.

### §H — Still open (next explore session / probes)

1. **Access-token refresh** — the Bearer's JWT `exp` is ~300 s; the live
   `download --lookback all` finished well inside that (~18 s of fetching),
   so renewal is still unobserved. It matters once a multi-account walk
   crosses 300 s. How the SPA renews (silent OIDC iframe? refresh grant?)
   and what `download.py` should do (re-harvest vs reload) is the probe.
   The 30-minute sso-session bound is the outer limit.
2. **`kontostaende` reach** — how far back `von` may lie (the live run
   fetched only the `--lookback` window; does an `all` window return the
   full daily series, so gold's balance series comes straight from it?).
3. Whether `pending: true` rows need special handling in the loader (a
   pending row's `id` stability across settlement).

## 4. Phase roadmap

1. **Scaffold** (done, committed 2026-08-15) — explore harness with
   DOM-snapshot-per-distinct-screen capture, Dockerfile on
   `wealthdb/base-camoufox`, wrapper via
   `shared/wrappers/wrapper-lib.sh`, collectorkit, this document,
   CLAUDE.md. `make build-raiffeisen_at` / `make test-raiffeisen_at`
   run via the root Makefile's collector auto-discovery.

2. **Explore** (live, owner-triggered) — **done (two sessions,
   2026-08-15)**, §3-Observed. Session 1: login + pushTAN + history +
   exports. Session 2: the statement archive (§E) and the trust probe
   (§F). Only the §H minor probes remain (token refresh, `kontostaende`
   reach) and they fold into Phase 3's live-validation. Credentials from
   `~/.secrets/raiffeisen_at.env` (`RAIFFEISEN_AT_USERNAME` /
   `RAIFFEISEN_AT_PASSWORD` / `RAIFFEISEN_AT_REGION` — §B), env only,
   never argv.

3. **login + download — one folded verb (chase's shape, §F). Built;
   awaiting live validation.** pushTAN fires every login, so login and
   the fetch run in one browser lifetime; `login` on its own is a no-op
   (the wrapper's `login` folds into `download`, chase-style), and
   `login --check` is a read-only session probe (DEAD between runs by
   design). As built:
   - **`elba_client.py`** — the pure wire contract (endpoints, request
     bodies, projections, the region→prefix resolution, the
     keyset-pagination cursor, the statement filter). Unit-tested on
     synthetic payloads.
   - **Login drive** (`login.py`). On a warm profile the identify
     screen shows a **profile card** (`app-user-entry .clickable`) —
     click it, then pushTAN. On a cold/`--fresh` profile: fetch
     `config/mandanten`, resolve `RAIFFEISEN_AT_REGION` → (dropdown
     option index, Verfüger prefix) via the config-index method (§B),
     select the Mandant, fill `verfuegerNr` (prefixed;
     `RAIFFEISEN_AT_USERNAME` is the unprefixed personal number) and
     `pin` by `formcontrolname`, submit. Either way announce the
     pushTAN **Vergleichswert** (`displayText`, captured from the
     `login/pushtan` response; the DOM also shows it) and wait — **the
     SPA polls `login/pushtan/<signaturId>` and completes the OIDC
     hand-off on its own**, so there is nothing to type and no stdin is
     needed. Completion is polled **event-free**: `page.url` reaching
     the authenticated app origin, backed by a `GET produkte` REST
     probe — never a lone Playwright response event (the pinned
     Camoufox drops them). Every wait pumps the event loop
     (`wait_for_timeout`, never `time.sleep`). The UI is light-DOM
     Angular — no shadow-root click mechanics. `vnc-login`
     (`--no-cli-mfa`) is the by-hand fallback.
   - **Data fetch over `page.request`** (`download.py`) against the §C
     endpoints, authenticated by the **OIDC Bearer harvested from the
     SPA's own `/api/` requests** (token-refresh handling deferred to
     §H — the ~300 s expiry only bites a long multi-account walk):
     `produkte` (keep `type == "KONTO"`), then per IBAN the
     **keyset-paginated `kontoumsaetze` walk** (windowed by `buchungVon`
     for `--lookback`, cursor from each page's last row) and the
     `kontostaende` daily-balance series for the same window. No DOM
     scraping, no export-UI driving (§D).
   - **Statements over the document archive** (§E), firstcitizens-style:
     `POST dokumente/filter` once (full archive; paginate `skip`/`limit`),
     keep `dokumentenName.de == "Kontoauszug"` rows for the scoped
     IBANs (match on `referenzIds[].iban`), and `POST
     dokumente/<systemId>/<dokumentenId>/download` each to bronze. The
     `(systemId, dokumentenId)` pair is the stable dedup key — no range
     convention needed.
   - **Account information** (`details/<IBAN>.json`, added after the first
     live run): `GET …/kontoinformationen-ui/rest/konten/<IBAN>/details`
     per account (§C) — account type, currency, institution/BIC, interest
     rates. Read-only; the incidental card block is captured, never acted
     on.
   - Bronze per run dir on the firstcitizens layout: `run.json`,
     `accounts.json`, `details/<IBAN>.json`, `history/<IBAN>.json` (merged
     ledger), `balances/<IBAN>.json`, `statements/<IBAN>/*.pdf`, `raw/`.
   - **First live attempt (2026-08-15) — the shared-URL false positive,
     fixed.** The first `download` reported "authenticated" instantly,
     fired no pushTAN, and then 401'd on `produkte`. Root cause: the app
     origin serves the bare `/bankingws-widgetsystem/` shell **both** on
     the pre-login bounce (before redirecting to the sso login app) and
     after sign-in, so the URL-based auth check matched the very first
     navigation and skipped the whole login — the chase/firstcitizens
     shared-URL trap, in a new dress. Fix: **the auth gate is now
     possession of a working OIDC Bearer** — a `GET produkte` that
     returns 200, which requires the Bearer (produkte 401s on cookies
     alone). The Bearer is harvested from the SPA's `/api/` request
     Authorization headers **and** the `/as/token.oauth2` response body
     (two sources against the pinned-Camoufox event drop), and if the
     SPA reaches a real in-app route without a harvested Bearer the
     driver reloads once to re-trigger the authenticated calls. The URL
     is used only to *announce* the pushTAN and to decide the reload
     nudge, never as the auth signal. Fleet lesson (again): on a
     bounce-through-SSO SPA, authenticate on a token/data probe, never
     on the app URL.
   - **Still not fully live-validated**: the corrected flow has not yet
     completed a real login end-to-end. The next live `download` is the
     acceptance test — the RDS form drive, the region-prefix fill, the
     pushTAN wait, and the Bearer harvest are proven only once it lands
     (§H probes fold into it).

4. **load — built (`load.py` + `migrations/0001`), validated on the real
   bronze.** bronze → SQLite silver, idempotent (snapshot gate +
   `INSERT OR IGNORE`/content-dedup), unit-tested on synthetic fixtures
   only. `account_external_id` is the **IBAN** (the ubs-web / ubs-psn
   convention, so an Austrian cash account joins across sources on the
   IBAN); the silver DB lives in the private data tree, never committed.
   The tables:
   - **transactions** — the `kontoumsaetze` JSON is the source of record
     (§C/§D): stable `id` → `txn_id`, signed `betrag.amount`,
     `buchungstag` → `posted_at` **and** `valuta` → `value_at` (both
     kept), `kategorieCode` → `category`, Verwendungszweck →
     `description`, Transaktionsteilnehmer → `counterparty`. No CSV
     parsing, no synthetic-id scheme needed (a missing id is tolerated
     with a content hash). **No per-row balance** (the source carries
     none).
   - **daily_balances** — the `kontostaende` `tagessalden` series
     (account, day → closing saldo). This is the cash time series (the
     history has no per-row balance), keyed (account, day) with
     `INSERT OR REPLACE`.
   - **accounts** — the roster row enriched with the curated
     `details/<IBAN>.json` attributes (Kontoart → `account_type`,
     currency, institution + BIC, and the Zinssatz Soll/Haben interest
     rates in the payload). The card block **and the account-holder
     name** are dropped (deposit-only; the name is PII a cash account
     doesn't need). `mask` is the IBAN's last-4.
   - **documents** — the statement PDFs, deduped by content sha256.

   Validated on the 2026-08-15 bronze: 1 account, **394 transactions**
   (25 credit / 369 debit), **238 daily balances** (2023-07-31 …
   2026-08-10), **41 statements** (19 EAZ + 22 KDM), and a re-load is a
   clean no-op.

5. **Statement transaction-backfill — not planned (§G).** The document
   archive predates the transaction-history floor by only ~5 months
   (and both floors roll forward together), so there is no deep tail to
   reconstruct — the chase reconcile-gated backfill is not ported.
   Statement PDFs are still fetched to bronze as documents (Phase 3)
   and loaded as a statement inventory (Phase 4), just not parsed for
   transactions. Revisit only if that ~5-month slice is explicitly
   wanted.

6. **Gold adapter — built** (`wealthdb/internal/silver/raiffeisen_at/`,
   package `raiffeisenat`, kind `raiffeisen_at`). On the
   chase/firstcitizens model: one cash account per deposit account (from
   `accounts`; DisplayName = account type + mask, EUR, taxable_personal /
   self_directed, all overridable via `account_overrides`), a CLOSING
   cash balance per day from the **`daily_balances`** series plus a
   CURRENT balance from the roster, and every transaction (from
   `transactions`, `amount` already signed; interest / fee recognised
   from the category / description, else deposit / withdrawal by sign).
   Registered via the blank import in `cmd/wealthdb/main.go` and the
   `silver_sources` whitelist migration
   (`internal/gold/migrations/0035_silver_sources_raiffeisen_at.sql`).
   Unit-tested; the whole gold suite passes. The cash accounts are
   conduits, so the registered ReturnsPolicy (`policy.go` beside the
   adapter) blanks their accounts-grain TWR/MWR; the coarse grains keep
   the accounts and count their flows.

## 5. Handoff checklist / status

**Status: full pipeline through gold built and validated (2026-08-15).**
`login` + `download` validated live; `load` validated on the real
bronze into the SQLite silver, idempotently; and the **gold adapter**
(`internal/silver/raiffeisen_at/`, kind `raiffeisen_at`) is built,
registered, and unit-tested with the whole gold suite green. The
collector is feature-complete. Remaining is operational, not code: a
real `wealthdb load` merge into gold (the owner's step, like the
download/load), plus adding the source to `wealthdb.cfg` (a silver
source entry; conduit accounts-grain blanking ships in the registered
ReturnsPolicy, no config needed). The §H probes (token
refresh on a >300 s walk; `kontostaende` reach — the live series
bottomed at the ~2023-07 history floor) fold into future runs.

- `make build-raiffeisen_at` builds the image;
  `make test-raiffeisen_at` runs the unit tests in the container.
- Credentials: `~/.secrets/raiffeisen_at.env` at chmod 0600 with
  `RAIFFEISEN_AT_USERNAME` (the personal, unprefixed Verfüger number),
  `RAIFFEISEN_AT_PASSWORD` (the PIN), and `RAIFFEISEN_AT_REGION` (the
  Mandant code, §B) — single-quote values containing `$`, `!`, or
  backticks; the file is created by hand, never by tooling. (In place.)
- **The acceptance test is a live `download`** (owner present):
  `wealthdb-collect raiffeisen_at download --dry-run` first (logs in,
  enumerates the roster, fetches nothing), then a real
  `download --lookback all`. It needs no TTY — the pushTAN is a phone
  approval, nothing is typed. `--debug` captures the login DOM to
  `/debug` if a selector has drifted; `vnc-login` is the by-hand
  fallback; `--fresh` forces the cold form.
- Re-run discovery any time: `./raiffeisen_at explore`. `--fresh` wipes
  the profile to force the full region/Verfüger/PIN + pushTAN challenge
  (the warm profile otherwise shows just the identity card, §F).
- Live sessions only when explicitly requested with the owner present
  (CLAUDE.md §0); every wait on the owner uses long timeouts (1h+);
  never fire logins in quick succession.
- Iterate cheap after a login: hold the session (session-holder
  pattern — watch a trigger file, `importlib.reload` bind-mounted code
  against the live page) instead of paying a pushTAN per probe.
- When a control won't click or a wait hangs, capture ground truth
  (frame DOM + element/shadow skeleton dumps) before guessing — both
  prior collectors burned logins on click mechanics that one
  diagnostic dump settled.
