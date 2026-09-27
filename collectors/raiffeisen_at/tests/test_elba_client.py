"""Unit tests for elba_client — the browserless Mein ELBA wire contract.

Synthetic values only: placeholder-letter IBANs (root AGENTS.md §4), made-up
Verfüger prefixes, round balances. No real account data.
"""
from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import elba_client as elba  # noqa: E402

# Synthetic IBAN in the placeholder-letter pattern AT<chk><BBBBB><KKKKKKKKKKK>.
IBAN_A = "ATkkBBBBBKKKKKKKKKKK"
IBAN_B = "ATppCCCCCLLLLLLLLLLL"


# ============================================================
# Auth-state URL classification
# ============================================================

def test_is_authed_route_requires_a_real_in_app_route():
    # A real post-auth SPA route matches.
    assert elba.is_authed_route(
        "https://mein.elba.raiffeisen.at/bankingws-widgetsystem/"
        "meine-produkte/dashboard")
    # The shared-URL trap (the live 2026-08-15 false positive): the bare
    # widgetsystem shell and the OIDC ?code= callback are the PRE-login
    # bounce and must NOT read as authed.
    assert not elba.is_authed_route(
        "https://mein.elba.raiffeisen.at/bankingws-widgetsystem/")
    assert not elba.is_authed_route(
        "https://mein.elba.raiffeisen.at/bankingws-widgetsystem/?code=abc&state=x")
    # The sso login app is never "authed".
    assert not elba.is_authed_route("https://sso.raiffeisen.at/mein-login/identify")
    assert not elba.is_authed_route("")


def test_bearer_from_token_response():
    assert elba.bearer_from_token_response(
        {"access_token": "TOK", "id_token": "x"}) == "TOK"
    assert elba.bearer_from_token_response({"access_token": ""}) is None
    assert elba.bearer_from_token_response({}) is None
    assert elba.bearer_from_token_response(None) is None


def test_is_pushtan_url():
    assert elba.is_pushtan_url("https://sso.raiffeisen.at/mein-login/sca/pushTAN")
    assert elba.is_pushtan_url("https://sso.raiffeisen.at/mein-login/sca")
    assert not elba.is_pushtan_url("https://sso.raiffeisen.at/mein-login/identify")
    assert not elba.is_pushtan_url(
        "https://mein.elba.raiffeisen.at/bankingws-widgetsystem/")


# ============================================================
# Region (Mandant) resolution — the login twist (§B)
# ============================================================

MANDANTEN = [
    {"code": "rbgbgld", "verfuegerKennung": "ELVIE33V", "sortOrder": 1,
     "disabled": False},
    {"code": "rbgk", "verfuegerKennung": "ELOOE03V", "sortOrder": 2},
    {"code": "rbgooe", "verfuegerKennung": "ELOOE01V", "sortOrder": 4},
    {"code": "rbgt", "verfuegerKennung": "ELOOE11V", "sortOrder": 7},
]


def test_resolve_mandant_maps_code_to_option_index_and_prefix():
    # Option index is the position in sortOrder order + 1 (a leading empty
    # placeholder option precedes the real regions in the dropdown).
    idx, kennung = elba.resolve_mandant(MANDANTEN, "rbgbgld")
    assert (idx, kennung) == (1, "ELVIE33V")
    idx, kennung = elba.resolve_mandant(MANDANTEN, "rbgooe")
    assert (idx, kennung) == (3, "ELOOE01V")     # 3rd in sortOrder → option 3
    idx, kennung = elba.resolve_mandant(MANDANTEN, "rbgt")
    assert (idx, kennung) == (4, "ELOOE11V")


def test_resolve_mandant_orders_by_sortorder_not_input_order():
    shuffled = list(reversed(MANDANTEN))
    idx, kennung = elba.resolve_mandant(shuffled, "rbgbgld")
    assert (idx, kennung) == (1, "ELVIE33V")     # still first by sortOrder


def test_resolve_mandant_unknown_region_raises():
    import pytest
    with pytest.raises(KeyError):
        elba.resolve_mandant(MANDANTEN, "rbgxx")


def test_full_verfueger_prefixes_unless_already_prefixed():
    assert elba.full_verfueger("ELOOE01V", "3V000042") == "ELOOE01V3V000042"
    # A stored full id is kept as-is (no double prefix).
    assert elba.full_verfueger("ELOOE01V", "ELOOE01V3V000042") == "ELOOE01V3V000042"


# ============================================================
# pushTAN contract (§A)
# ============================================================

def test_pushtan_display_text():
    assert elba.pushtan_display_text({"displayText": "WXYZ"}) == "WXYZ"
    assert elba.pushtan_display_text({"displayText": ""}) is None
    assert elba.pushtan_display_text({}) is None
    assert elba.pushtan_display_text(None) is None


def test_looks_like_vergleichswert():
    # The real codes are 4-char alphanumeric; these are synthetic stand-ins.
    for good in ("QRST", "WX90", "abcd", "AB12", "12345"):
        assert elba.looks_like_vergleichswert(good), good
    assert elba.looks_like_vergleichswert("  QRST  ")     # trimmed
    # The label text around it, empty, or an over-long blob must not match —
    # so the code element is picked, not the German instruction line.
    for bad in ("", "Vergleichswert vergleichen", "approve on your phone",
                "AB", "toolongcode9"):
        assert not elba.looks_like_vergleichswert(bad), bad


# ============================================================
# Roster projection (§C)
# ============================================================

def _prod(iban, typ="KONTO", amount=1000.0):
    return {
        "productId": iban,
        "type": typ,
        "details": {
            "betragKontoWaehrung": {"amount": amount, "currency": "EUR",
                                    "currencyCode": "EUR"},
            "verfuegbarKontoWaehrung": {"amount": amount, "currency": "EUR"},
            "letzteAktualisierung": "2026-08-15T01:00:00",
        },
    }


def test_parse_produkte_keeps_only_konto():
    roster = [_prod(IBAN_A), _prod(IBAN_B, typ="DEPOT"), _prod("card", typ="KARTE")]
    rows = elba.parse_produkte(roster)
    assert [r["iban"] for r in rows] == [IBAN_A]
    assert rows[0]["balance"] == {"amount": 1000.0, "currency": "EUR"}
    assert rows[0]["available"] == {"amount": 1000.0, "currency": "EUR"}


def test_parse_produkte_deposit_only_false_keeps_all():
    roster = [_prod(IBAN_A), _prod(IBAN_B, typ="DEPOT")]
    rows = elba.parse_produkte(roster, deposit_only=False)
    assert [r["iban"] for r in rows] == [IBAN_A, IBAN_B]


def test_parse_produkte_tolerates_junk():
    assert elba.parse_produkte(None) == []
    assert elba.parse_produkte(["not-a-dict", {"type": "KONTO"}]) == [
        {"iban": "", "type": "KONTO", "balance": None, "available": None,
         "last_updated": None}]


# ============================================================
# History request body + pagination cursor (§C)
# ============================================================

def test_kontoumsaetze_body_windows_and_targets_iban():
    body = elba.kontoumsaetze_body(IBAN_A, buchung_von=date(2026, 5, 1))
    assert body["limit"] == elba.HISTORY_PAGE_LIMIT
    p = body["predicate"]
    assert p["ibans"] == [IBAN_A]
    assert p["kontotyp"] == "KONTO"
    assert p["buchungVon"] == "2026-05-01T00:00:00.000"
    assert p["buchungBis"] is None
    assert p["pending"] is True


def test_kontoumsaetze_body_applies_cursor():
    cursor = {"buchungBis": "2026-02-10T00:00:00.000", "idBis": "999",
              "neuanlageBis": "20260210x"}
    p = elba.kontoumsaetze_body(IBAN_A, cursor=cursor)["predicate"]
    assert p["buchungBis"] == "2026-02-10T00:00:00.000"
    assert p["idBis"] == "999"
    assert p["neuanlageBis"] == "20260210x"


def test_umsaetze_list_and_paging_flags():
    body = {"list": [{"id": 1}], "info": {"hasMore": True,
                                          "minBuchungstag": "2023-08-01"}}
    assert elba.umsaetze_list(body) == [{"id": 1}]
    assert elba.umsaetze_has_more(body)
    assert elba.umsaetze_min_buchungstag(body) == "2023-08-01"
    assert elba.umsaetze_list({}) == []
    assert not elba.umsaetze_has_more({"info": {"hasMore": False}})


def test_next_cursor_from_last_row():
    row = {"id": 12345, "buchungstag": "2025-02-10",
           "neuanlage": "20250210x", "betrag": {"amount": -1}}
    assert elba.next_cursor(row) == {
        "buchungBis": "2025-02-10T00:00:00.000",
        "idBis": "12345", "neuanlageBis": "20250210x"}


def test_next_cursor_none_when_keys_missing():
    assert elba.next_cursor({"id": 1}) is None          # no buchungstag
    assert elba.next_cursor({"buchungstag": "2025-02-10"}) is None  # no id
    assert elba.next_cursor("nope") is None


def test_next_cursor_passes_through_timestamped_buchungstag():
    row = {"id": 1, "buchungstag": "2025-02-10T09:00:00"}
    assert elba.next_cursor(row)["buchungBis"] == "2025-02-10T09:00:00"


# ============================================================
# Balances + document endpoints (§C/§E)
# ============================================================

def test_kontostaende_url():
    url = elba.kontostaende_url(IBAN_A, date(2026, 5, 1), date(2026, 8, 15))
    assert url.endswith(f"/kontostaende/{IBAN_A}?von=2026-05-01&bis=2026-08-15")


def test_konto_details_url():
    url = elba.konto_details_url(IBAN_A)
    assert url.endswith(f"/kontoinformationen-ui/rest/konten/{IBAN_A}/details")


def test_dokumente_filter_body():
    assert elba.dokumente_filter_body() == {
        "von": None, "bis": None, "skip": 0, "limit": 50}
    assert elba.dokumente_filter_body(skip=50, limit=25, von=date(2023, 1, 1)) == {
        "von": "2023-01-01", "bis": None, "skip": 50, "limit": 25}


def test_dokument_download_url():
    # No version (newer EAZ documents): plain path.
    assert elba.dokument_download_url("EAZ", "ELOOE_42").endswith(
        "/dokumente/EAZ/ELOOE_42/download")
    assert elba.dokument_download_url("EAZ", "ELOOE_42", None).endswith(
        "/dokumente/EAZ/ELOOE_42/download")
    # With a version (older KDM documents): the versionsId is in the path.
    assert elba.dokument_download_url("KDM", "RBGO9", 1).endswith(
        "/dokumente/KDM/RBGO9/1/download")
    # Empty string is treated as "no version".
    assert elba.dokument_download_url("KDM", "RBGO9", "").endswith(
        "/dokumente/KDM/RBGO9/download")


# ============================================================
# Statement filtering (§E) — Kontoauszug only, by IBAN, by date
# ============================================================

def _doc(name_de, iban, created, sys_id="EAZ", doc_id="d1"):
    return {
        "systemId": sys_id, "dokumentenId": doc_id,
        "dokumentenName": {"de": name_de, "en": None},
        "erstellungsDatum": created, "dateiTyp": "pdf",
        "referenzIds": [{"typ": "IBAN", "iban": iban}],
    }


def test_parse_statements_keeps_only_kontoauszug_for_iban():
    body = [
        _doc("Kontoauszug", IBAN_A, "2026-08-01T01:00:00", doc_id="a1"),
        _doc("Entgeltanpassung", IBAN_A, "2026-01-01T01:00:00", doc_id="fee"),
        _doc("Kontoauszug", IBAN_B, "2026-08-01T01:00:00", doc_id="other"),
    ]
    rows = elba.parse_statements(body, IBAN_A)
    assert [r["dokument_id"] for r in rows] == ["a1"]
    assert rows[0]["system_id"] == "EAZ"
    assert rows[0]["version_id"] is None          # EAZ carries no versionsId
    assert rows[0]["created"] == "2026-08-01T01:00:00"


def test_parse_statements_carries_versionsid_for_kdm():
    body = [{"systemId": "KDM", "dokumentenId": "RBGO9", "versionsId": 1,
             "dokumentenName": {"de": "Kontoauszug"},
             "erstellungsDatum": "2024-06-01T01:00:00", "dateiTyp": "PDF",
             "referenzIds": [{"typ": "IBAN", "iban": IBAN_A}]}]
    row = elba.parse_statements(body, IBAN_A)[0]
    assert row["version_id"] == 1
    # …and that version drives the download path.
    assert elba.dokument_download_url(
        row["system_id"], row["dokument_id"], row["version_id"]).endswith(
        "/dokumente/KDM/RBGO9/1/download")


def test_parse_statements_since_floor():
    body = [
        _doc("Kontoauszug", IBAN_A, "2026-08-01T01:00:00", doc_id="new"),
        _doc("Kontoauszug", IBAN_A, "2023-04-01T01:00:00", doc_id="old"),
    ]
    rows = elba.parse_statements(body, IBAN_A, since=date(2026, 1, 1))
    assert [r["dokument_id"] for r in rows] == ["new"]


def test_parse_statements_drops_docs_without_stable_key():
    body = [{"dokumentenName": {"de": "Kontoauszug"},
             "referenzIds": [{"typ": "IBAN", "iban": IBAN_A}],
             "erstellungsDatum": "2026-08-01", "systemId": "", "dokumentenId": ""}]
    assert elba.parse_statements(body, IBAN_A) == []


def test_parse_statements_tolerates_junk():
    assert elba.parse_statements(None, IBAN_A) == []
    assert elba.parse_statements(["x", {}], IBAN_A) == []
