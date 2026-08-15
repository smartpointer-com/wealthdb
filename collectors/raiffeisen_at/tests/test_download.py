"""Unit tests for download.py's browserless surface: the filesystem-safe
stem, the run manifest, and a full :func:`walk` driven against stubbed REST
helpers (no browser) — covering the keyset-pagination merge, the balance
series, and the statement filter/download into a bronze run dir.

Synthetic values only: placeholder-letter IBANs, round balances.
"""
from __future__ import annotations

import json
import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402
import elba_client as elba  # noqa: E402
import login  # noqa: E402

IBAN_A = "ATkkBBBBBKKKKKKKKKKK"


# ============================================================
# safe_stem + manifest
# ============================================================

def test_safe_stem():
    assert download.safe_stem(IBAN_A) == IBAN_A
    assert download.safe_stem("EAZ/ELOOE_1") == "EAZ-ELOOE_1"
    assert download.safe_stem("../../etc") == "etc"
    assert download.safe_stem("...") == "item"


def test_build_manifest_carries_only_ibans():
    m = download.build_manifest(
        "complete", accounts=[{"iban": IBAN_A, "balance": {"amount": 9}}],
        counts={"transactions": 3}, since=date(2026, 5, 1), until=date(2026, 8, 1))
    assert m["source"] == "raiffeisen_at"
    assert m["status"] == "complete"
    assert m["account_ibans"] == [IBAN_A]
    assert m["since"] == "2026-05-01" and m["until"] == "2026-08-01"
    # No balance leaks into the manifest.
    assert "balance" not in json.dumps(m)


# ============================================================
# walk() against stubbed REST helpers
# ============================================================

class _Resp:
    def __init__(self, status, body=b""):
        self.status = status
        self._body = body

    def body(self):
        return self._body


class _RequestCtx:
    """Stands in for context.request — records statement-download POSTs and
    returns PDF bytes. `fail_status_for` maps a substring found in the URL
    (e.g. a systemId path segment) to an error status, so the KDM-422 path
    can be exercised."""

    def __init__(self, fail_status_for=None):
        self.pdf_posts = []
        self.fail_status_for = fail_status_for or {}

    def post(self, url, headers=None, data=None):
        self.pdf_posts.append(url)
        for needle, status in self.fail_status_for.items():
            if needle in url:
                return _Resp(status, b"")
        return _Resp(200, b"%PDF-1.4 synthetic")


class _Context:
    def __init__(self, request=None):
        self.request = request or _RequestCtx()


def _install_rest_stubs(monkeypatch, *, roster, history_pages, balances, docs,
                        details=None):
    """Patch login.api_get_json / api_post_json with canned responses keyed
    by URL, so walk() runs without a browser. `history_pages` is a list of
    (rows, has_more) tuples served in order per POST to kontoumsaetze."""
    hist_iter = iter(history_pages)

    def get_json(context, watch, url):
        if url == elba.produkte_url():
            return 200, roster
        if url == elba.konto_details_url(IBAN_A):
            return 200, (details if details is not None else {"konto": {}})
        if url.startswith(elba.kontostaende_url(IBAN_A, date(1, 1, 1),
                                                date(1, 1, 1)).split("?")[0]):
            return 200, balances
        return 404, None

    def post_json(context, watch, url, data):
        if url == elba.kontoumsaetze_url():
            rows, has_more = next(hist_iter)
            return 200, {"list": rows,
                         "info": {"hasMore": has_more,
                                  "minBuchungstag": "2023-08-01"}}
        if url == elba.dokumente_filter_url():
            return 200, docs
        return 404, None

    monkeypatch.setattr(login, "api_get_json", get_json)
    monkeypatch.setattr(login, "api_post_json", post_json)
    monkeypatch.setattr(login, "api_headers", lambda w: {"Accept": "*/*"})


def _roster():
    return [{"productId": IBAN_A, "type": "KONTO",
             "details": {"betragKontoWaehrung": {"amount": 100.0,
                                                 "currency": "EUR"}}}]


def _statement_doc(doc_id="a1", created="2026-08-01T01:00:00"):
    return {"systemId": "EAZ", "dokumentenId": doc_id,
            "dokumentenName": {"de": "Kontoauszug"},
            "erstellungsDatum": created, "dateiTyp": "pdf",
            "referenzIds": [{"typ": "IBAN", "iban": IBAN_A}]}


def test_walk_paginates_history_and_writes_bronze(monkeypatch, tmp_path):
    # Two history pages: page 1 (hasMore=True), page 2 (hasMore=False).
    page1 = ([{"id": 2, "buchungstag": "2026-07-01", "neuanlage": "n2"},
              {"id": 1, "buchungstag": "2026-06-01", "neuanlage": "n1"}], True)
    page2 = ([{"id": 0, "buchungstag": "2026-05-01", "neuanlage": "n0"}], False)
    _install_rest_stubs(
        monkeypatch, roster=_roster(), history_pages=[page1, page2],
        balances={"tagessalden": [{"tag": "2026-05-01", "saldo": 100.0}],
                  "kontostand": 100.0},
        docs=[_statement_doc()])

    ctx = _Context()
    summary = download.walk(ctx, watch=login._Watch(),
                            bronze_dir=tmp_path, since=date(2026, 5, 1),
                            until=date(2026, 8, 15))

    assert summary["accounts"] == 1
    assert summary["transactions"] == 3          # 2 + 1 across pages
    assert summary["details"] == 1
    assert summary["balances"] == 1
    assert summary["statements"] == 1

    run_dir = Path(summary["run_dir"])
    manifest = json.loads((run_dir / "run.json").read_text())
    assert manifest["status"] == "complete"
    hist = json.loads((run_dir / "history" / f"{IBAN_A}.json").read_text())
    assert [t["id"] for t in hist["transactions"]] == [2, 1, 0]
    assert hist["minBuchungstag"] == "2023-08-01"
    assert (run_dir / "balances" / f"{IBAN_A}.json").is_file()
    assert (run_dir / "details" / f"{IBAN_A}.json").is_file()
    pdfs = list((run_dir / "statements" / IBAN_A).glob("*.pdf"))
    assert len(pdfs) == 1
    # The PDF name embeds the stable (created, systemId, docId) key.
    assert "EAZ" in pdfs[0].name and "a1" in pdfs[0].name


def test_walk_downloads_kdm_via_versioned_url(monkeypatch, tmp_path):
    # The older KDM document carries a versionsId; its download URL must
    # include it (…/KDM/RBGO9/1/download) or the server 422s. The stub 422s
    # any KDM download that OMITS the version, so this passes only when the
    # versioned URL is used.
    docs = [_statement_doc(doc_id="eaz1", created="2026-08-01T01:00:00"),
            {"systemId": "KDM", "dokumentenId": "RBGO9", "versionsId": 1,
             "dokumentenName": {"de": "Kontoauszug"},
             "erstellungsDatum": "2024-06-01T01:00:00", "dateiTyp": "PDF",
             "referenzIds": [{"typ": "IBAN", "iban": IBAN_A}]}]
    _install_rest_stubs(
        monkeypatch, roster=_roster(),
        history_pages=[([{"id": 1, "buchungstag": "2026-06-01"}], False)],
        balances={"tagessalden": []}, docs=docs)
    # A KDM download missing the version segment 422s; the versioned path 200s.
    ctx = _Context(_RequestCtx(fail_status_for={"/KDM/RBGO9/download": 422}))
    summary = download.walk(ctx, watch=login._Watch(),
                            bronze_dir=tmp_path, since=date(2024, 1, 1),
                            until=date(2026, 8, 15))
    assert summary["statements"] == 2            # both EAZ + KDM written
    run_dir = Path(summary["run_dir"])
    names = sorted(p.name for p in (run_dir / "statements" / IBAN_A).glob("*.pdf"))
    assert any("EAZ" in n for n in names)
    # The KDM PDF name embeds the version in its stable key.
    assert any("KDM" in n and "v1" in n for n in names)
    assert json.loads((run_dir / "run.json").read_text())["status"] == "complete"


def test_walk_dry_run_writes_no_data(monkeypatch, tmp_path):
    _install_rest_stubs(
        monkeypatch, roster=_roster(),
        history_pages=[([{"id": 1, "buchungstag": "2026-06-01"}], False)],
        balances={}, docs=[])
    ctx = _Context()
    summary = download.walk(ctx, watch=login._Watch(),
                            bronze_dir=tmp_path, since=date(2026, 5, 1),
                            until=date(2026, 8, 15), dry_run=True)
    run_dir = Path(summary["run_dir"])
    manifest = json.loads((run_dir / "run.json").read_text())
    assert manifest["status"] == "dry-run"
    assert summary["transactions"] == 0
    assert not (run_dir / "history" / f"{IBAN_A}.json").exists()


def test_walk_no_documents_skips_statements(monkeypatch, tmp_path):
    _install_rest_stubs(
        monkeypatch, roster=_roster(),
        history_pages=[([{"id": 1, "buchungstag": "2026-06-01"}], False)],
        balances={"tagessalden": []}, docs=[_statement_doc()])
    ctx = _Context()
    summary = download.walk(ctx, watch=login._Watch(),
                            bronze_dir=tmp_path, since=date(2026, 5, 1),
                            until=date(2026, 8, 15), documents=False)
    assert summary["statements"] == 0
    assert ctx.request.pdf_posts == []           # no PDF download attempted
