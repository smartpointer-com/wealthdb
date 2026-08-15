#!/usr/bin/env python3
"""Mein ELBA wire contract — the browserless core shared by login.py and
download.py.

Mein ELBA is an Angular SPA over a per-widget REST/JSON API (DESIGN.md
§3-Observed). Two origins are in play:

  * `sso.raiffeisen.at` — the OIDC login app and its `kunde-login-ui`
    REST API (region list, identify, pushTAN). login.py drives the login
    form in the browser and lets the SPA make these calls; the only piece
    this module owns on the login side is the **region → Verfüger-prefix**
    resolution (§B), which is pure data.
  * `mein.elba.raiffeisen.at` — the banking SPA and its data API
    (`produkte`, `kontoumsaetze`, `kontostaende`, the document archive).
    download.py replays these over the browser context's `request` API with
    the harvested OIDC Bearer token — no DOM scraping.

Everything here touches no browser and no network: endpoint builders,
request-body builders, the keyset-pagination cursor, and the response
projections. All pure, so it is unit-tested against synthetic payloads
(no real IBANs, Verfüger numbers, or balances).
"""
from __future__ import annotations

import re
from datetime import date
from urllib.parse import quote

# --- origins --------------------------------------------------------------

# The public entry; it redirects into the OIDC flow on sso.raiffeisen.at.
START_URL = "https://mein.elba.raiffeisen.at/"

SSO_ORIGIN = "https://sso.raiffeisen.at"
APP_ORIGIN = "https://mein.elba.raiffeisen.at"

# The login app's REST base (unauthenticated config + the identify/pushTAN
# flow). login.py mostly lets the SPA call these; only the region list is
# fetched/parsed here.
LOGIN_API = f"{SSO_ORIGIN}/api/bankingquer-kunde-login/kunde-login-ui/rest"

# The banking data API bases (per widget).
WS_API = f"{APP_ORIGIN}/api/bankingws-widgetsystem/bankingws-ui/rest"
UMSATZ_API = f"{APP_ORIGIN}/api/bankingzv-umsatz/umsatz-ui/rest"
DOCS_API = f"{APP_ORIGIN}/api/bankingquer-dokumentenablage/dokumentenablage-ui/rest"
KONTOINFO_API = f"{APP_ORIGIN}/api/bankingzv-kontoinformationen/kontoinformationen-ui/rest"

# SPA route fragments. NOTE the shared-URL trap (DESIGN.md §A/§F): the app
# origin serves the bare `/bankingws-widgetsystem/` shell *both* on the
# pre-login bounce (before it redirects to the sso login app) *and* after
# sign-in, so the bare path is NOT an auth signal. Only a real in-app SPA
# route (`/bankingws-widgetsystem/<route>`, e.g. `meine-produkte/…`) appears
# post-auth — that is what `is_authed_route` matches. The definitive auth
# signal, though, is possession of a working OIDC Bearer (login.py); the
# route only tells us the login flow reached the app so a re-harvest is
# worth a nudge.
URL_MARK_LOGIN = "/mein-login"
URL_MARK_PUSHTAN = "/sca"           # /mein-login/sca, /mein-login/sca/pushTAN
_AUTHED_ROUTE_RE = re.compile(r"/bankingws-widgetsystem/[a-z]", re.I)


def is_authed_route(url: str) -> bool:
    """True when `page.url` is on a real authenticated in-app SPA route
    (a non-empty path under the widgetsystem, off the sso login app) — e.g.
    `/bankingws-widgetsystem/meine-produkte/dashboard`. The bare
    `/bankingws-widgetsystem/` shell and the `?code=` OIDC callback do NOT
    match, so this never fires on the pre-login bounce. It marks that the
    login flow completed, not (on its own) that a usable session exists — the
    Bearer harvest is the real gate."""
    u = url or ""
    return (APP_ORIGIN in u and URL_MARK_LOGIN not in u
            and bool(_AUTHED_ROUTE_RE.search(u)))


def is_pushtan_url(url: str) -> bool:
    """True on the pushTAN wait screen (`/mein-login/sca…`)."""
    u = url or ""
    return URL_MARK_LOGIN in u and URL_MARK_PUSHTAN in u


# The OIDC token endpoint — its response body carries the `access_token`
# (the Bearer), an authoritative source login.py harvests alongside the
# per-request Authorization headers.
TOKEN_ENDPOINT_SUFFIX = "/as/token.oauth2"


def bearer_from_token_response(body: dict) -> str | None:
    """The `access_token` from an `/as/token.oauth2` response body, or None."""
    if isinstance(body, dict):
        tok = body.get("access_token")
        if isinstance(tok, str) and tok:
            return tok
    return None


# --- login-form selectors (RDS / Angular light DOM, DESIGN.md §A) ---------
#
# The login app is Angular with "RDS" components. Ids are auto-generated and
# unstable (`rds-input--0`), so every control is keyed on its stable
# `formcontrolname` (or ARIA role), never on display text (the UI is German).

SEL_REGION_SELECT = "rds-select[formcontrolname='mandant']"
SEL_REGION_OPTION = "rds-option"           # options mount in an overlay
SEL_VERFUEGER = "input[formcontrolname='verfuegerNr']"
SEL_PIN = "input[formcontrolname='pin']"
SEL_SUBMIT = "button[rds-button][type='submit']"

# Warm-profile path (DESIGN.md §F): a stored profile shows a saved-user card
# instead of the blank form; clicking it skips region/Verfüger/PIN entry and
# goes straight to pushTAN. The card is an `app-user-entry` component with a
# `.clickable` row (and a delete button that must NOT be clicked).
SEL_PROFILE_CARD = "app-user-entry"
SEL_PROFILE_CARD_CLICK = "app-user-entry .clickable"

# The Vergleichswert (comparison code) shown on the pushTAN screen and in the
# app — logged at sign-in so the right challenge can be confirmed against the
# app. Primary source is the pushtan POST response (`displayText`), but the pinned
# Camoufox can drop that response event (it did on the first live login), so
# the DOM read below is the reliable fallback: the pushTAN screen shows the
# code in a large display paragraph.
SEL_VERGLEICHSWERT = "p.rds-display-1"


def looks_like_vergleichswert(text: str) -> bool:
    """True when `text` looks like a Vergleichswert — a short alphanumeric
    comparison code (the real ones are 4 chars, e.g. a synthetic 'QRST'). Used
    to pick the code element out of the pushTAN screen without keying on the
    German label around it."""
    return bool(re.fullmatch(r"[A-Za-z0-9]{3,8}", (text or "").strip()))


# --- login REST (region resolution + pushTAN contract) --------------------

def mandanten_url() -> str:
    """The (unauthenticated) region list endpoint. Returns a JSON array of
    `{code, verfuegerKennung, sortOrder, disabled}` — one per Mandant
    (regional Raiffeisen banking group)."""
    return f"{LOGIN_API}/config/mandanten"


def resolve_mandant(mandanten: list, region_code: str) -> tuple[int, str]:
    """Resolve a Mandant `code` (the value of `RAIFFEISEN_AT_REGION`) to its
    (dropdown-option-index, verfuegerKennung) (DESIGN.md §B).

    The dropdown lists the Mandanten in `sortOrder` behind a single empty
    placeholder option, so the option index for the n-th entry (0-based in
    the API's sortOrder order) is n + 1. `verfuegerKennung` is the prefix
    that precedes the personal Verfüger number in the full login id.

    Raises KeyError if `region_code` is not offered (a stale env value).
    Pure — the caller fetches `mandanten` over the browser context and
    passes the parsed list here."""
    ordered = sorted(
        (m for m in mandanten if isinstance(m, dict)),
        key=lambda m: m.get("sortOrder", 0))
    for i, m in enumerate(ordered):
        if str(m.get("code", "")) == region_code:
            kennung = str(m.get("verfuegerKennung", ""))
            if not kennung:
                raise KeyError(
                    f"region {region_code!r} has no verfuegerKennung")
            return i + 1, kennung          # +1 for the placeholder option
    known = ", ".join(str(m.get("code", "")) for m in ordered)
    raise KeyError(
        f"region {region_code!r} is not an offered Mandant (known: {known})")


def full_verfueger(kennung: str, username: str) -> str:
    """The full Verfüger login id: the region's `verfuegerKennung` prefix
    followed by the personal (unprefixed) Verfüger number. If `username`
    already carries the prefix (a stored full id), it is returned unchanged
    so the prefix is never doubled."""
    if username.startswith(kennung):
        return username
    return f"{kennung}{username}"


def pushtan_display_text(body: dict) -> str | None:
    """The Vergleichswert from a `login/pushtan` response (`displayText`) —
    the code shown on screen and in the app, to be compared against each
    other. None if absent/misshaped."""
    if isinstance(body, dict):
        txt = body.get("displayText")
        if isinstance(txt, str) and txt:
            return txt
    return None


# (The SPA polls `login/pushtan/<signaturId>` for the `loggedIn` flip and
# completes the OIDC hand-off itself; this collector detects completion via
# the Bearer/`produkte` probe instead, so it does not read that poll — see
# login.authenticate and DESIGN.md §A/§F.)


# --- data API: roster -----------------------------------------------------

def produkte_url() -> str:
    return f"{WS_API}/produkte"

# The deposit-account marker in the product roster: `type == "KONTO"`.
# Cards, securities, loans carry other types and are out of scope
# (CLAUDE.md). Kept as the one place the deposit rule lives.
DEPOSIT_PRODUCT_TYPE = "KONTO"


def is_deposit_product(prod: dict) -> bool:
    """True for a deposit account (checking / savings) — `type == 'KONTO'`."""
    return isinstance(prod, dict) and str(prod.get("type", "")) == DEPOSIT_PRODUCT_TYPE


def _amount(obj) -> dict | None:
    """Project a `{amount, currency, currencyCode}` money object to
    `{amount, currency}`, or None if misshaped."""
    if not isinstance(obj, dict):
        return None
    return {"amount": obj.get("amount"),
            "currency": obj.get("currency") or obj.get("currencyCode")}


def parse_produkte(body, *, deposit_only: bool = True) -> list[dict]:
    """Project the `produkte` roster to the fields download.py needs, keeping
    only deposit accounts by default. Each row: `iban` (the account key, used
    in every other endpoint), `type`, the current `balance` and `available`
    money objects, and `last_updated`. Pure — tested on synthetic payloads.

    `body` is the top-level array the endpoint returns."""
    rows = []
    for prod in body if isinstance(body, list) else []:
        if not isinstance(prod, dict):
            continue
        if deposit_only and not is_deposit_product(prod):
            continue
        det = prod.get("details") or {}
        rows.append({
            "iban": str(prod.get("productId", "")),
            "type": str(prod.get("type", "")),
            "balance": _amount(det.get("betragKontoWaehrung")),
            "available": _amount(det.get("verfuegbarKontoWaehrung")),
            "last_updated": det.get("letzteAktualisierung"),
        })
    return rows


# --- data API: transaction history (kontoumsaetze) ------------------------

def kontoumsaetze_url() -> str:
    return f"{UMSATZ_API}/kontoumsaetze"

# The SPA's own page size for the Umsätze view; its CSV export uses 3001.
# A large page keeps the paginated full-history walk short.
HISTORY_PAGE_LIMIT = 500

# Safety cap on pagination (500 rows/page → 250k rows). A real deposit
# account is far under this; the cap only bounds a runaway loop.
MAX_HISTORY_PAGES = 500


def _iso_midnight(d: date) -> str:
    """A date as the API's `YYYY-MM-DDT00:00:00.000` timestamp."""
    return f"{d.isoformat()}T00:00:00.000"


def kontoumsaetze_body(iban: str, *, buchung_von: date | None = None,
                       cursor: dict | None = None,
                       limit: int = HISTORY_PAGE_LIMIT) -> dict:
    """Build the `kontoumsaetze` POST body for one account (DESIGN.md §C).

    `buchung_von` floors the window (the `--lookback` start); the results
    come back newest-first with no upper bound needed. `cursor` (from
    :func:`next_cursor` on the previous page's last row) advances keyset
    pagination — it carries `buchungBis` / `idBis` / `neuanlageBis`. The
    predicate mirrors the captured request exactly, differing only in the
    windowing fields."""
    predicate = {
        "kontotyp": "KONTO",
        "buchungVon": _iso_midnight(buchung_von) if buchung_von else None,
        "buchungBis": None,
        "neuanlageBis": None,
        "idBis": None,
        "betragVon": None,
        "betragBis": None,
        "betragsrichtung": "BEIDE",
        "kategorieCodes": None,
        "kategorieCodesNotIn": False,
        "hashtags": None,
        "ibans": [iban],
        "pending": True,
        "folgenummernKarteByIban": None,
    }
    if cursor:
        predicate.update(cursor)
    return {"predicate": predicate, "limit": limit}


def umsaetze_list(body: dict) -> list:
    """The `list` array of a `kontoumsaetze` response (the transactions)."""
    if isinstance(body, dict) and isinstance(body.get("list"), list):
        return body["list"]
    return []


def umsaetze_has_more(body: dict) -> bool:
    """`info.hasMore` — whether another page exists (DESIGN.md §C)."""
    info = body.get("info") if isinstance(body, dict) else None
    return bool(isinstance(info, dict) and info.get("hasMore"))


def umsaetze_min_buchungstag(body: dict) -> str | None:
    """`info.minBuchungstag` — the account's history floor (≈ 3 years back)."""
    info = body.get("info") if isinstance(body, dict) else None
    if isinstance(info, dict):
        v = info.get("minBuchungstag")
        return v if isinstance(v, str) else None
    return None


def next_cursor(last_row: dict) -> dict | None:
    """Build the keyset-pagination cursor from a page's last (oldest) row
    (DESIGN.md §C): the next page repeats the query with `buchungBis` /
    `idBis` / `neuanlageBis` set from that row, so results continue strictly
    older. Returns None if the row lacks the keys (stop paginating)."""
    if not isinstance(last_row, dict):
        return None
    buchungstag = last_row.get("buchungstag")
    row_id = last_row.get("id")
    neuanlage = last_row.get("neuanlage")
    if buchungstag is None or row_id is None:
        return None
    # buchungstag is a date (YYYY-MM-DD); the cursor wants the same
    # timestamped form the captured pagination used.
    bis = (f"{buchungstag}T00:00:00.000"
           if isinstance(buchungstag, str) and "T" not in buchungstag
           else buchungstag)
    cursor = {"buchungBis": bis, "idBis": str(row_id)}
    if neuanlage is not None:
        cursor["neuanlageBis"] = str(neuanlage)
    return cursor


# --- data API: account information (kontoinformationen) -------------------

def konto_details_url(iban: str) -> str:
    """The account-information detail endpoint (the "Kontoinformationen"
    page). Returns `{konto: {kontoart}, detailgruppen: [{ueberschrift,
    details: [{bezeichnung, inhalt}]}]}` — grouped attribute rows the roster
    (`produkte`) doesn't carry: the account type (Kontoart, e.g. a salary
    account), currency, holding institution + BIC, and the debit/credit
    interest rates (Zinssatz Soll/Haben) and statement-cycle dates. Read-only
    (DESIGN.md §C). The response also carries an incidental card-limits block;
    it is captured as provenance but never acted on (deposit-only scope,
    CLAUDE.md)."""
    return f"{KONTOINFO_API}/konten/{quote(iban)}/details"

# The Statement-of-Fees PDF (Entgeltaufstellung) lives at
# `{KONTOINFO_API}/konten/<IBAN>/entgeltnachweise?datumVon=&datumBis=` — the
# EU Payment-Accounts-Directive fee document, generated on demand over a date
# range. It is mapped but not fetched (DESIGN.md §E): it is informative-only
# (no transaction/balance data) and needs a range convention (the regulatory
# statement is annual), so it is a documented future opt-in, not a default.


# --- data API: daily balances (kontostaende) ------------------------------

def kontostaende_url(iban: str, von: date, bis: date) -> str:
    """Daily closing-balance series for `[von, bis]` (DESIGN.md §C):
    `{tagessalden: [{tag, saldo}], kontostand, verfuegbarerBetrag}`."""
    return (f"{UMSATZ_API}/kontostaende/{quote(iban)}"
            f"?von={von.isoformat()}&bis={bis.isoformat()}")


# --- data API: document archive (statements) ------------------------------

def dokumente_filter_url() -> str:
    return f"{DOCS_API}/dokumente/filter"


def dokumente_filter_body(*, skip: int = 0, limit: int = 50,
                          von: date | None = None,
                          bis: date | None = None) -> dict:
    """The `dokumente/filter` POST body. `von`/`bis` null returns the whole
    archive (not per-account — it is filtered client-side by IBAN);
    `skip`/`limit` paginate (DESIGN.md §E)."""
    return {
        "von": von.isoformat() if von else None,
        "bis": bis.isoformat() if bis else None,
        "skip": skip,
        "limit": limit,
    }


def dokument_download_url(system_id: str, dokument_id: str,
                          version_id=None) -> str:
    """POST endpoint returning the document PDF bytes (empty `{}` body).

    Documents that carry a `versionsId` (the older KDM-system Kontoauszüge)
    put it in the path — `…/<systemId>/<dokumentenId>/<versionsId>/download`
    — and 422 without it; documents with no version (the newer EAZ ones) use
    the plain `…/<systemId>/<dokumentenId>/download` (measured 2026-08-15,
    DESIGN.md §E)."""
    base = f"{DOCS_API}/dokumente/{quote(system_id)}/{quote(dokument_id)}"
    if version_id not in (None, ""):
        base += f"/{quote(str(version_id))}"
    return f"{base}/download"


# The statement document name (German) — the only document kind that is an
# account statement; the archive also holds annual fee notices
# (Entgeltmitteilung / Entgeltanpassung) that are not transaction documents
# and are out of scope (DESIGN.md §E).
STATEMENT_DOC_NAME = "Kontoauszug"


def _doc_name_de(doc: dict) -> str:
    name = doc.get("dokumentenName")
    if isinstance(name, dict):
        return str(name.get("de", "") or "")
    return str(name or "")


def _doc_ibans(doc: dict) -> set[str]:
    out = set()
    for ref in doc.get("referenzIds") or []:
        if isinstance(ref, dict) and ref.get("iban"):
            out.add(str(ref["iban"]))
    return out


def parse_statements(body, iban: str, *, since: date | None = None) -> list[dict]:
    """Project a `dokumente/filter` response to this account's statement
    rows (DESIGN.md §E), keeping only `Kontoauszug` documents whose
    `referenzIds` include `iban`, on or after `since`. Each row:
    `system_id`, `dokument_id`, `version_id` (the `versionsId`, or None —
    the triple is the stable dedup key and drives the download URL, §E),
    `created` (erstellungsDatum), `file_type`. Newest-first is preserved as
    the source returns it. Pure — tested on synthetic payloads."""
    rows = []
    for doc in body if isinstance(body, list) else []:
        if not isinstance(doc, dict):
            continue
        if _doc_name_de(doc) != STATEMENT_DOC_NAME:
            continue
        if iban not in _doc_ibans(doc):
            continue
        created = str(doc.get("erstellungsDatum", "") or "")
        if since is not None and created:
            # erstellungsDatum is an ISO datetime; compare the date part.
            if created[:10] < since.isoformat():
                continue
        sys_id = str(doc.get("systemId", "") or "")
        doc_id = str(doc.get("dokumentenId", "") or "")
        if not (sys_id and doc_id):
            continue
        version_id = doc.get("versionsId")
        rows.append({
            "system_id": sys_id,
            "dokument_id": doc_id,
            "version_id": version_id,
            "created": created,
            "file_type": str(doc.get("dateiTyp", "") or ""),
        })
    return rows
