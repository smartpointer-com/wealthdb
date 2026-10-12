# raiffeisen_at — design notes

Design notes for the Austrian Raiffeisen (Mein ELBA) retail collector.
Sections: scope (§1), the discovery harness (§2), the discovery walk
(§3), the mapped surface (§3-Surface, with lettered parts §A–§I), the
runtime (§4), and operating notes (§5). The layout follows
[chase](../chase/DESIGN.md) and
[firstcitizens](../firstcitizens/DESIGN.md).

## 1. Scope

- **The relationship.** A retail deposit-banking relationship at an
  Austrian Raiffeisen bank, served through the group-wide **Mein ELBA**
  portal (entry `https://mein.elba.raiffeisen.at/`). **Scope is deposit
  accounts only**: checking (Girokonto) and savings (Sparkonto). Cards,
  financing, and any securities or wealth surface the same login may
  expose are out of scope (see [AGENTS.md](AGENTS.md)). The name is
  namespaced to Austria because Raiffeisen runs distinct banking systems
  in other countries.
- **The model is chase, refined by firstcitizens.** Both are US retail
  deposit collectors: browser login, REST or export data, statement PDFs
  as documents, cash accounts as conduits in gold. This collector starts
  from them and diverges only where the source differs.
- **Conduit accounts.** Cash passes through these accounts on its way to
  and from other sources. Their transactions matter for cross-source
  money-flow tracking. Their own return rows are noise, so the
  registered ReturnsPolicy (`internal/silver/raiffeisen_at/policy.go`,
  the shared deposit-bank policy) hides them at every grain. Their
  balances and flows still enter every aggregate.
- **2FA is pushTAN** (§A). The login is confirmed in the Raiffeisen app;
  nothing is typed. The completion signal is a plain REST poll. The
  browser shows a 4-character Vergleichswert, which the run announces in
  the terminal. **There is no fallback factor.**
- **pushTAN fires on every login** (§F). The persistent profile
  remembers the identity but not the second factor. So `login` cannot
  run unattended and **folds into `download`**: one browser lifetime per
  run, one pushTAN each time.
- **The data is a per-widget REST/JSON API** (§C). The app is an Angular
  SPA. Every datum arrives from `/api/<widget>/<widget>-ui/rest/…` with a
  Bearer token. The history endpoint has stable ids, booking and value
  dates, and keyset pagination. No DOM scraping is needed.
- **The CSV export is not used** (§D). It is a client-side dump of the
  history JSON with the ids and field structure stripped. There is no
  CAMT.052/053 or MT940 export.
- **Statements are a document archive** (§E): monthly Kontoauszüge with
  stable document ids, listed and downloaded like firstcitizens. The
  archive reaches only a few months past the transaction floor, so it is
  provenance, not a deep backfill (§G). The deep past comes from
  transaction listings the bank supplies on request (§I).
- **The UI is entirely German.** Selectors never key on display text
  where an id, test-id or ARIA role exists. Load-bearing German labels
  are recorded here. Repo prose stays English.
- **No bot defense is visible.** Requests go only to raiffeisen.at
  properties, with no Akamai or ThreatMetrix-class sensors, and Camoufox
  clears the login unchallenged. The collector still uses
  `shared/images/base-camoufox.Dockerfile` and a persistent profile, the
  fleet's standard browser setup.

## 2. The discovery harness (`explore`)

`explore.py` runs the shared harness
([`collectorkit.explore`](../../shared/collectorkit/collectorkit/explore.py)),
which documents its artefacts and redaction. This collector adds the
entry URL, a host gate (the pre-fill touches only raiffeisen.at frames),
the credential env names, and the field selectors. The shared in-page
detector watches those fields, plus TAN code fields. The harness never
submits a form; sign-in and pushTAN are done by hand over VNC. The
opt-in `--trace` is off by default, because the pinned
Playwright/Camoufox pair crashes on tracing.

**The pre-fill stays idle on this source, by design.** The Verfüger
input has no `name` or `autocomplete` hook (only Angular
`formcontrolname`, which the generic selectors do not key). More
fundamentally, the field's correct value is *region-prefixed* (§B), so
filling the bare `RAIFFEISEN_AT_USERNAME` would be wrong. The
prefix-aware fill is `login.py`'s job. In `explore` the credentials are
typed by hand.

## 3. The discovery walk

An `explore` session re-maps the surface by walking three flows over
VNC. Each is one continuous segment.

1. **Login + pushTAN.** The region, Verfüger and PIN form, the pushTAN
   wait screen and its completion. A second run on the same profile
   (without `--fresh`) shows what the profile remembers (§F).
2. **Statements.** The document archive (Dokumente): its listing, a PDF
   download, and its reach (§E).
3. **Transaction history.** Each account's Umsätze view, its paging and
   date filters, and the history floor (§C). The CSV export can be run
   once for comparison (§D).

## 3-Surface — the mapped surface

Endpoints below are masked (`<IBAN>` and `<id>` stand for elided
values). Account numbers, Verfüger numbers, balances and the region in
use live only in the debug dir, never here. The app is an Angular SPA
("bankingws-widgetsystem") over a per-widget REST/JSON API with a Bearer
token. Requests go only to raiffeisen.at properties, plus one TeleTrader
market-data widget.

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
   `GET rest/identify/challenge/switch` returns `[]` — **no fallback
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
   This is the completion signal. It is plain REST, so it does not
   depend on Playwright response events.
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
Camoufox profile. It skips credential entry but not the pushTAN (§F).
The form also carries a
"Verfüger speichern" checkbox (`rds-checkbox[formcontrolname=
"checked"]`), which by its label remembers the *username*, not the
device.

**UI technology:** Angular with "RDS" (Raiffeisen Design System)
components. They are light DOM (no shadow roots),
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

The list has twelve entries: the eight federal-state groups
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

**How login.py handles it:**

- In `~/.secrets/raiffeisen_at.env`, `RAIFFEISEN_AT_USERNAME` is the
  **personal (unprefixed) Verfüger number**. **`RAIFFEISEN_AT_REGION`**
  holds the **Mandant `code`** (e.g. `rbgbgld`). The code is stable
  config, where the German display label and the option index are not.
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
(plus session cookies). The token's JWT `exp` is **300 s** after issue.
How the SPA renews it is still open (§H). A
`GET …/bankingws-ui/rest/keepalive` (204) is the session heartbeat.

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
- **History floor:** `info.minBuchungstag` sits at the month boundary
  **3 years back**, matching the UI's earliest offered "ab" date. It is
  a rolling ~36-month server-side cap on history. The statement archive
  predates it by only a few months (§G).
- **Balances:** `GET /api/bankingzv-umsatz/umsatz-ui/rest/kontostaende/
  <IBAN>?von=<date>&bis=<date>` → `{"tagessalden": [{"tag", "saldo"},
  …], "kontostand", "verfuegbarerBetrag"}` — a **parameterizable
  daily closing-balance series** plus the current balance. Gold's
  closing-balance series comes from here. How far back `von` may lie is
  open (§H).
- **Account information:**
  `GET /api/bankingzv-kontoinformationen/kontoinformationen-ui/rest/konten/
  <IBAN>/details` → `{konto: {kontoart}, detailgruppen: [{ueberschrift,
  details: [{bezeichnung, inhalt}]}]}`. This is the "Kontoinformationen"
  page and carries attributes the roster does **not**: the **Kontoart**
  (account type; `konto.kontoart` is a one-letter code and the detail
  row spells it out), **Währung** (currency),
  holding **institution + BIC + Bankleitzahl**, the **Zinssatz Soll /
  Haben** (debit / credit interest rates, each with an effective date), and
  the **Kontoabschluss** cycle dates. It also returns an incidental
  **Karteninformationen** block (card type + limits) — captured as
  provenance, never acted on (deposit-only; AGENTS.md). `download`
  fetches this per account into `details/<IBAN>.json`. The values are PII
  (IBAN, balances, card number, account holder) — bronze only, never
  tracked files.
- Also mapped (allowed, secondary): `GET …umsatz-ui/rest/konten/<IBAN>`
  and `GET /api/bankingzv-konto/kontozentrale-ui/rest/konten/<IBAN>`
  (account detail), `GET …umsatz-ui/rest/gesendeteAuftraege` (sent
  orders overlay the Umsätze view loads — read-only GET).
- The dashboard SPA fires **widget calls outside the scope on its
  own** (card overview, mailbox unread count, spending statistics,
  marketing tiles). Loading the dashboard triggers them. `download.py`
  never calls them itself.

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

So, as at firstcitizens, **the `kontoumsaetze` JSON is the
authoritative ledger**. It is a strict superset of the CSV, it carries
the stable id and the clean field split, and the export's date range is
bounded by the same 3-year floor. `download` saves the merged JSON to
bronze (`history/<IBAN>.json`, the firstcitizens layout) and never
drives the export UI.

### §E — Statements: a document archive, not an on-demand generator (flow 2)

**Statements are pre-generated PDFs in a document archive**: the
"Dokumente" surface, widget `bankingquer-dokumentenablage`. There is no
custom-range generator, so a statement older than the archive's
retention cannot be produced. The shape is firstcitizens' (list +
download by id), not chase's, and documents carry stable ids, so no
range convention is needed. Endpoints:

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
- **The download URL carries the `versionsId` when present.** The older
  **KDM**-system Kontoauszüge have `versionsId: 1`, and their download
  puts the version in the path:
  `POST …/dokumente/KDM/<dokumentenId>/<versionsId>/download {}`. Without
  it the call returns 422. The newer **EAZ** ones have
  `versionsId: null` and download at
  `POST …/dokumente/EAZ/<dokumentenId>/download {}`.
  `dokument_download_url` appends the `versionsId` segment only when the
  document has one. The `(systemId, dokumentenId, versionsId)` triple is
  the stable dedup key, in the bronze PDF name and in silver. The
  `GET …/dokumente/metadata/<systemId>/<dokumentenId>[/<versionsId>]` the
  SPA also fires is not needed for the download.

**Document kinds** (a taxonomy, not a roster):
`dokumentenName.de == "Kontoauszug"` are the account **statements**,
roughly monthly (a statement-cycle cadence), in two `systemId`
lineages — an older one (filename `Kontoauszug.PDF`) and a newer one
(filename `<IBAN>_EUR_<seq>`). Alongside them sit annual fee notices
(`Entgeltmitteilung` / `Entgeltanpassung`) that are **not** transaction
documents. `download` keeps `dokumentenName.de == "Kontoauszug"` for the
scoped IBANs and skips the rest.

**Also mapped — the on-demand Statement of Fees (Entgeltaufstellung).**
The account-details page (§C) exposes a **separate** fee-document export:
`GET …/kontoinformationen-ui/rest/konten/<IBAN>/entgeltnachweise?datumVon=
&datumBis=` → `application/pdf` (the EU Payment-Accounts-Directive
Statement of Fees, generated on demand over a date range). It is **not
fetched**. It carries no transaction or balance data, and doing it well
needs a range convention, since the regulatory statement is annual. A
future opt-in could pull whole calendar years.

### §F — Trust persistence: identity persists, pushTAN does not (the verb-split answer)

On a relaunch with the same profile (no `--fresh`), the stored
`profilToken` (§A) survives. The login app calls
`GET rest/profile/<profilToken>` and shows a **profile card** on the
identify screen. One click on it goes straight to
`POST rest/identify/<verfueger>` **with the token as the body**: no
region pick, no Verfüger typing, no PIN. Then comes
`POST rest/login/pushtan`, and **the pushTAN fires again.** So:

- the persistent Camoufox profile **remembers the identity** (skips
  the whole region/Verfüger/PIN entry), but
- **2FA fires on every login** — there is no trusted-device bypass.

This is **chase's shape, not firstcitizens'**. `login` cannot run
unattended, so it **folds into `download`**: one browser lifetime per
run, one pushTAN each time. `login.py` still implements the full
credential path (region + Verfüger + PIN, §B), because a cold, `--fresh`
or expired profile shows no card. The warm path is "click the card,
approve the pushTAN". The presence of the profile card on the identify
screen tells the two apart.

### §G — History floor vs archive floor: no deep backfill from the live surfaces

- The transaction-history JSON floor (§C) is a rolling 36-month window.
  It is the platform's retention, not an account property.
- The statement archive (§E) reaches a few months further back than the
  JSON window. Older entries are only annual fee notices, which carry
  no transactions.

So the statement PDFs predate the JSON ledger by only a few months, and
that window does not grow, because both floors roll forward together.
Unlike chase (seven years of statements behind a two-year export), there
is **no deep tail worth a reconcile-gated statement backfill.**
Statements go to bronze as documents only (the firstcitizens model). The
deep tail comes from transaction listings the bank supplies on request,
which also cover the statements' span (§I).

### §H — Still open

1. **Access-token refresh.** The Bearer's JWT `exp` is ~300 s. A full
   `download --lookback all` finishes well inside that, so renewal has
   not been needed. It matters once a multi-account walk crosses 300 s.
   The probe is how the SPA renews (silent OIDC iframe? refresh grant?)
   and what `download.py` should do (re-harvest or reload). The
   30-minute sso session is the outer limit.
2. **`kontostaende` reach.** How far back `von` may lie, and whether an
   `all` window returns the full daily series.
3. **Pending rows.** Whether a `pending: true` row keeps its `id` through
   settlement, and so whether the loader needs to treat it specially.

### §I — Supplied transaction listings: the deep backfill

A branch can print, on request, a
**core-banking transaction listing** (a "SMARTBank Abfrage" PDF) for one
account from any start date up to the print day — years past the live
floor. Listings dropped into `<data-dir>/supplied/` are stitched into silver
on every `load` (listing_parser.py reads them, stitch.py places them,
load.py's supplied pass writes them).

**Layout** (a fixed-width table; `pdftotext -layout` keeps the geometry):

- Every page repeats a header block: a banner with the print time and page
  number (`…/<dd.mm.yyyy>/<hh:mm>/… Seite <n>`), then
  `Kontonummer: … Ausgabe Datum ab: <dd.mm.yy> Auswahl Umsätze: <x>`, two
  `Schlüsseltext:` lines, `Saldo anzeigen J/N: …  Klartext anz. J/N: …`, a
  rule, the column header
  `BUTAG AN PRNR HERK TXT VAL. UMSATZ SALDO DRUCKDATUM ART`, a rule, the
  account line (`<account no>/<ccy>  <holder>  REF: …`) and a
  `KTO-WHG/<ccy>` units banner. The last page carries only `Summe Soll` /
  `Summe Haben`.
- **Load-bearing labels** (German, no ids exist): the column header above;
  `Kontonummer`, `Ausgabe Datum ab` (the coverage start), `Auswahl Umsätze`
  (`A` = all postings), `Schlüsseltext` (`alle Umsätze`), `Saldo anzeigen`,
  `Klartext anz.`; `Summe Soll` / `Summe Haben`; the continuation labels
  `Zahlungsempfänger`, `Empfänger`, `Auftraggeber`, `Verwendungszweck`,
  `Zahlungsreferenz`, `Kundendaten`, `Mandat`, `Empfänger-Kennung`,
  `Auftraggeberinformation`, `Entgelte`.
- The columns **drift a few characters page to page**, and the header
  labels are not flush with their data: each page's own column header
  tells the two right-aligned money columns apart (UMSATZ and SALDO end
  ~17 columns apart); the left-hand codes are read by shape.
- **BUTAG** (booking day) prints on a day's first row only, even across a
  page break; **SALDO** (the day's closing balance) on its last row only.
  A debit carries a trailing minus. **VAL.** has no year — it takes the
  booking day's, or the neighbouring year's when the months are more than
  six apart.
- **DRUCKDATUM** is the day the posting was printed on a statement, blank
  until it has been — so two extracts of one account differ there.
- **Continuation lines** belong to the row above, run on across page breaks
  and past a mid-page units banner, and the bank's fixed-width text record
  can split a label across two lines (`… EUR 1,00Auftrag` / `geber: …`),
  which the parser re-joins.
- The operation code **HERK** tells booking families apart (`SELE` direct
  debit, `SEUA`/`SEUE` outgoing/incoming transfer, `ABS` account-closing
  entries, `SBGA` cash/card, `DT` standing order, …); it is kept in the
  payload, not mapped to a category.

**Per-listing checks** (`listing_parser.problems`; a failure excludes that
listing only): the selection is complete (`A`, `alle Umsätze`, balances
shown); booking days are in order inside `[Ausgabe Datum ab, print day]`;
every day's printed balance follows from the previous one plus that day's
postings (the chain); and the postings net exactly to the stated totals —
the bank leaves reversal pairs out of `Summe Soll`/`Summe Haben`, so each
gross side may exceed its total by the same amount, no more than same-day
equal-and-opposite pairs explain. The listing binds to the one silver
account whose IBAN ends in its zero-padded `Kontonummer`.

**Coverage** (recorded per loaded run in silver's `run_windows`). A
download certifies its transaction window `[max(since, minBuchungstag),
until]` (from `run.json` and the history file) up to the day before its own
— the run's day is partial. `download.py` records whether the history walk
reached its last page (`complete` in the history file): a walk cut short by
a failed page, a missing cursor or the page cap misses the *oldest* days.
It certifies only from the day after the oldest day it returned, since a
page boundary can cut that day too. A history file without the flag is
treated the same way.
Balances are certified over the `kontostaende` `[von, bis − 1]`. A listing
certifies `[Ausgabe Datum ab, print day − 1]`.

**Ownership** (stitch.py). Every booking day has one source of truth:

1. **live**, when a download certified the day;
2. else the **highest-precedence accepted listing** certifying it. On a
   day live saw only partly (a download's own day, the oldest day of a
   cut-short walk), the listing tops live up: it contributes only the
   postings live lacks, and its closing balance replaces live's intraday
   one;
3. else nobody.

So the seam is not `MIN(posted_at)`: a hole between two downloads is
filled, and a later deeper download takes days back. A live balance on a
day live certifies always stands.

**The stitch.** Listings are tried in precedence order — with booking text
(`Klartext J`) first, then the newest print, then the file hash — in
repeated passes until a pass accepts nothing. A listing is **rejected** if,
on any day it shares with the live history or an accepted listing, the
postings (booking day, amount) differ — on a day only one side certifies,
the certifying side must hold the other's — or the closing balance
differs; it is **accepted** when it agrees and its balance is continuous at
every join with another source; otherwise it waits, and one still waiting
at the end is rejected with the uncovered gap named. Which accepted listing
speaks for a day is then settled by precedence alone (they agree on every
shared day), so the result depends only on the set of files. With no
certified live history for the account, the first listing that agrees with
what live did see seeds it.

**Rows.** A supplied posting's `txn_id` is `doc_` + a hash of its
structural columns and an occurrence count — never the parsed text, never
DRUCKDATUM — so every extract gives it the same id. `category` stays NULL
(a listing has no `kategorieCode`); `description` is the purpose / customer
data, else the payment reference, else the booking text; `counterparty`
the first payee/payer line. The payload keeps the listing's own vocabulary
and the owning file's hash; a top-up row also records what live saw of
its day (`live_seen`). Each listing that binds to an account gets a
`documents` row (`doc_kind = 'transaction_listing'`) recording its
coverage, status, reason and the day ranges it speaks for; a file that
does not read as a listing or binds to no account is only logged. The gold
classifier recognises the closing entries from their text (`Entgelt …`,
`Buchungsentgelt`, `Kontoführung`) — the operation code alone cannot tell
a fee from interest or tax.

**Applying it** (load.py). The supplied layer is re-derived on every load
and written only when it differs from the stored one:

- first, in a transaction of its own, supplied rows the live history has
  overtaken are dropped — postings on a day live now certifies or now sees
  differently than when they were stitched, balances on a day live now
  certifies — so no failure later in the load can leave both sources on
  one day;
- an unchanged stitch writes nothing;
- a stitch that would lose days while every file that contributed before
  is still present is not applied (a parser or poppler regression, not a
  decision — removing the file or `load --force` applies it);
- a missing `pdftotext`, or a contributing file that no longer extracts,
  keeps the stored layer; any other unreadable file is skipped alone;
- the write asserts that supplied postings sit only on days live does not
  certify and, with what live saw, add up to the listing's exactly.

An absent `supplied/` keeps the stored layer; an empty one clears it.
Whenever silver changes without a download in the same load, a `dump_runs`
row (`run_dir` = the supplied dir, `snapshot_at` one past the latest)
moves gold's load clock — never the wall clock, which could outrun the slug
of a download still in flight.

A listing that reaches past the live floor moves an account's history
earlier in gold. Its returns inception, flows before inception,
net-worth history and spending categorisation (supplied rows carry no
category) then change, so they deserve a look when such a listing first
lands.

## 4. Runtime

### 4.1 `login` + `download`: one folded verb

pushTAN fires on every login (§F), so sign-in and fetch run in one
browser lifetime. The wrapper's `download` runs `login.py`, which signs
in and then calls `download.walk()`. A bare `login` is a no-op that says
so. `login --check` is a read-only session probe; it reports DEAD
between runs by design.

- **`elba_client.py`** is the pure wire contract: endpoints, request
  bodies, projections, the region→prefix resolution, the
  keyset-pagination cursor and the statement filter.
- **The login drive** (`login.py`).
  - On a warm profile the identify screen shows a **profile card**
    (`app-user-entry .clickable`). The drive clicks it.
  - On a cold or `--fresh` profile it fetches `config/mandanten` and
    resolves `RAIFFEISEN_AT_REGION` to the dropdown option and Verfüger
    prefix (§B). It selects the Mandant and fills `verfuegerNr`
    (prefixed) and `pin` by `formcontrolname`, then submits.
  - Either way it announces the pushTAN **Vergleichswert**
    (`displayText` from the `login/pushtan` response; the DOM shows it
    too) and waits. The SPA polls `login/pushtan/<signaturId>` and
    completes the OIDC hand-off itself. Nothing is typed and no stdin is
    needed.
  - The UI is light-DOM Angular, so no shadow-root click mechanics are
    needed. Every wait pumps the event loop (`wait_for_timeout`, never
    `time.sleep`).
  - `vnc-login` (`--no-cli-mfa`) is the by-hand fallback.
- **The auth gate is a working Bearer, never the URL.** The app origin
  serves the bare `/bankingws-widgetsystem/` shell both on the pre-login
  bounce and after sign-in. A URL check would match the first
  navigation and skip the login. So the gate is a `GET produkte` that
  returns 200, which needs the Bearer (it returns 401 on cookies alone).
  - The Bearer is harvested from the SPA's `/api/` request headers and
    from the `/as/token.oauth2` response body. Two sources guard against
    the pinned Camoufox dropping events.
  - If the SPA reaches an in-app route with no Bearer harvested, the
    driver reloads once to re-trigger the authenticated calls.
  - The URL only announces the pushTAN and decides that reload.
  - On a bounce-through-SSO SPA, authenticate on a token or data probe,
    never on the app URL.
- **The data fetch** (`download.py`) runs over `context.request` with
  the harvested Bearer, against the §C endpoints:
  - `produkte`, kept to `type == "KONTO"`;
  - per IBAN, the account information (`details/<IBAN>.json`), the
    keyset-paginated `kontoumsaetze` walk (windowed by `buchungVon` for
    `--lookback`) and the `kontostaende` series for the same window;
  - the document archive once (`dokumente/filter`), then each
    Kontoauszug of a scoped IBAN (§E), unless `--no-documents`.
- **`--dry-run` signs in**, writes the roster, and fetches nothing else.

Bronze per run dir follows the firstcitizens layout:

```
<run>/
  run.json                  status manifest
  accounts.json             deposit roster
  details/<IBAN>.json       account information
  history/<IBAN>.json       merged kontoumsaetze pages (the ledger)
  balances/<IBAN>.json      kontostaende daily series
  statements/<IBAN>/*.pdf   Kontoauszüge
  raw/                      raw roster and archive listings
```

### 4.2 `load`

`load` parses bronze into SQLite silver (`migrations/0001_initial.sql`,
`0002_supplied_listings.sql`). It is idempotent: a snapshot gate plus
`INSERT OR IGNORE` and content dedup. `account_external_id` is the
**IBAN**, the ubs-web / ubs-psn convention, so an Austrian cash account
joins across sources on it.

- **`transactions`**: the `kontoumsaetze` JSON is the source of record
  (§C/§D). The stable `id` becomes `txn_id` (a missing id falls back to
  a content hash). The signed `betrag.amount` is the amount.
  `buchungstag` becomes `posted_at` and `valuta` becomes `value_at`.
  `kategorieCode` becomes `category`, Verwendungszweck the
  `description`, Transaktionsteilnehmer the `counterparty`. There is
  **no per-row balance**.
- **`daily_balances`**: the `kontostaende` `tagessalden` series
  (account, day → closing saldo), the cash time series. Its `source`
  column tells live rows from supplied ones.
- **`accounts`**: the roster row enriched with the `details/<IBAN>.json`
  attributes: Kontoart → `account_type`, currency, institution + BIC,
  and the interest rates in the payload. The card block **and the
  account-holder name** are dropped. `mask` is the IBAN's last 4.
- **`documents`**: the statement PDFs, deduped by content sha256, and one
  row per supplied listing that binds to an account (§I).
- **`run_windows`**: the days each loaded run certifies (§I).

### 4.3 Deep backfill

Statements stay a document inventory (§G). The deep past comes from
supplied listings (§I): `listing_parser.py` and `stitch.py` (both pure)
plus `load.py`'s supplied pass. `pdftotext` comes from the image's
`poppler-utils`.

### 4.4 Gold adapter

`wealthdb/internal/silver/raiffeisen_at/` (package `raiffeisenat`, kind
`raiffeisen_at`) follows the chase/firstcitizens model:

- one cash account per deposit account (DisplayName = account type +
  mask, EUR, `taxable_personal` / `self_directed`, all overridable via
  `account_overrides`);
- a closing cash balance per day from `daily_balances`;
- a current balance from the roster, stamped at the source's latest
  roster snapshot;
- every transaction, with `amount` already signed: interest and fee
  recognised from the category or description, else deposit or
  withdrawal by sign.

It registers via the blank import in `cmd/wealthdb/main.go` and the
`silver_sources` migration
(`internal/gold/migrations/0035_silver_sources_raiffeisen_at.sql`). Its
return rows are hidden per §1.

## 5. Operating notes

- A first live run starts with `download --dry-run` (it signs in,
  records the roster, fetches nothing), then `download --lookback all`.
  `--debug` captures the login DOM to `/debug` when a selector has
  drifted; `vnc-login` is the by-hand fallback; `--fresh` forces the
  cold form.
- Iterate cheaply after a login. Hold the session (watch a trigger file,
  `importlib.reload` bind-mounted code against the live page) instead of
  paying a pushTAN per probe.
- When a control will not click or a wait hangs, capture ground truth
  first (frame DOM plus element and shadow skeleton dumps). A diagnostic
  dump settles click mechanics faster than guessing.
