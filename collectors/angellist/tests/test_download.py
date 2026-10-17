"""Unit tests for download.py pure helpers.

Covers the tax-document completeness decision (which years' copies are
replaced each run until they settle), the document fetch loop over a
stubbed `urllib.request.urlopen`, and relative→absolute URL resolution. The
browser paths are exercised live, not here. Synthetic urls, names and bytes
only.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


def test_is_incomplete():
    # not yet marked complete -> re-download
    assert download._is_incomplete({"documentType": "estimate_provided"}) is True
    # complete label but fewer K-1s than expected -> still incomplete
    assert download._is_incomplete(
        {"documentType": "complete", "k1Count": 3, "totalK1Count": 5}) is True
    # all expected K-1s in and complete -> done, skip
    assert download._is_incomplete(
        {"documentType": "complete", "k1Count": 4, "totalK1Count": 4}) is False
    # complete with no count info (older years) -> done
    assert download._is_incomplete({"documentType": "complete"}) is False
    assert download._is_incomplete(
        {"documentType": "complete", "k1Count": None, "totalK1Count": None}) is False


def test_check_exits_3_when_browser_cannot_launch(tmp_path, monkeypatch):
    # A --check probe whose browser cannot even start must exit 3 — distinct
    # from 1 ("stale") — so the entrypoint surfaces the environment failure
    # instead of forcing a needless by-hand 2FA re-login.
    import json
    import types

    jar = tmp_path / "cookies.json"
    jar.write_text(json.dumps([{
        "name": "_angellist_v2", "value": "s", "domain": ".angellist.com",
        "path": "/", "expires": 4102444800, "httpOnly": True,
        "secure": True, "sameSite": "Lax"}]))

    class BoomFox:
        def __init__(self, **kwargs):
            raise PermissionError(13, "Permission denied", "GeoLite2-City.mmdb")

    stub = types.ModuleType("camoufox.sync_api")
    stub.Camoufox = BoomFox
    monkeypatch.setitem(sys.modules, "camoufox", types.ModuleType("camoufox"))
    monkeypatch.setitem(sys.modules, "camoufox.sync_api", stub)

    assert download.main(["--cookies", str(jar), "--check"]) == 3


def test_documents_dir_derives_from_bronze_dir():
    # parse_args leaves the default None; main() resolves it through
    # _resolve_documents_dir as <bronze-dir>/angellist-documents — the
    # bronze-root sibling `load` parses (its side is pinned end-to-end by
    # test_load.test_default_paths_derive_from_bronze_dir against the same
    # literal) — so scoping --bronze-dir scopes the document drop too.
    assert download.parse_args([]).documents_dir is None
    assert download._resolve_documents_dir(None, Path("/b")) == \
        Path("/b/angellist-documents")
    # an explicit flag wins over the derivation
    assert download.parse_args(["--documents-dir", "/x"]).documents_dir == Path("/x")
    assert download._resolve_documents_dir(Path("/x"), Path("/b")) == Path("/x")


def test_abs_url():
    assert download._abs_url("/k1_packets/1/csv") == \
        "https://venture.angellist.com/k1_packets/1/csv"
    assert download._abs_url("https://cdn.example/x.pdf") == "https://cdn.example/x.pdf"
    assert download._abs_url(None) is None
    assert download._abs_url("") == ""


def test_check_flag():
    # `login` uses `download --check` as the authoritative server
    # probe (a cookie can be unexpired yet server-rejected).
    assert download.parse_args(["--check"]).check is True
    assert download.parse_args([]).check is False


def test_debug_flag():
    # Gates the per-route DOM + screenshot captures under
    # <run>/screenshots/. Off by default: a normal run writes none, and
    # `prune` reclaims them from the dumps that do.
    assert download.parse_args(["--debug"]).debug is True
    assert download.parse_args([]).debug is False


def test_debug_help_promises_bronze_captures(capsys):
    # Guards against the flag regressing to a warn-only stub.
    with pytest.raises(SystemExit):
        download.parse_args(["--help"])
    out = capsys.readouterr().out
    assert "--debug" in out
    assert "gates nothing" not in out
    assert "NOT YET IMPLEMENTED" not in out


def test_lookback_flag():
    # Accepted for fleet uniformity: angellist always captures the
    # full portfolio snapshot (no server-side date filter), so --lookback only
    # drives a warning; the value is still validated against the
    # shared presets so a typo fails loudly.
    assert download.parse_args(["--lookback", "4w"]).lookback == "4w"
    assert download.parse_args([]).lookback is None
    with pytest.raises(SystemExit):
        download.parse_args(["--lookback", "1m"])  # not a preset


def test_no_documents_flag():
    # The fleet-wide document opt-out, a real skip: main() calls
    # download_documents only when it is absent. Default off, so a bare
    # run still fetches.
    assert download.parse_args(["--no-documents"]).no_documents is True
    assert download.parse_args([]).no_documents is False


def test_no_documents_help_promises_a_skip(capsys):
    # Guards against the flag regressing to a warn-only stub: the help must
    # still declare it and must not advertise it as unimplemented.
    with pytest.raises(SystemExit):
        download.parse_args(["--help"])
    out = capsys.readouterr().out
    assert "--no-documents" in out
    assert "NOT YET IMPLEMENTED" not in out


def test_op_name_drops_the_portals_optional_suffix():
    from graphql_ops import PORTFOLIO_OPS, op_name
    assert op_name("PositionsTableQuery") == "PositionsTable"
    assert op_name("PositionsTable") == "PositionsTable"
    assert op_name(None) == ""
    # The op sets are spelled without the suffix, so a wait on them matches
    # a capture under either spelling.
    assert "PositionsTable" in PORTFOLIO_OPS and not any(o.endswith("Query") for o in PORTFOLIO_OPS)


def test_exchanges_single_request():
    body = {"operationName": "ViewerQuery", "variables": {"a": 1}}
    payload = {"data": {"currentUser": {"slug": "u"}}, "errors": None}
    assert download._exchanges(body, payload) == [{
        "op": "ViewerQuery", "variables": {"a": 1},
        "data": {"currentUser": {"slug": "u"}}, "errors": None}]


def test_exchanges_batched_request_pairs_each_operation_with_its_result():
    # A batched POST is answered with one result per operation, in order;
    # an item without an operation name is skipped, and a missing or
    # non-object result captures no data.
    body = [{"operationName": "Activity"}, {"query": "{x}"},
            {"operationName": "OpenInvestments", "variables": {"v": 2}}]
    payload = [{"data": {"n": 1}}, {"data": {"n": 2}},
               {"errors": [{"message": "denied"}]}]
    assert download._exchanges(body, payload) == [
        {"op": "Activity", "variables": {}, "data": {"n": 1}, "errors": None},
        {"op": "OpenInvestments", "variables": {"v": 2}, "data": None,
         "errors": [{"message": "denied"}]},
    ]
    assert download._exchanges({"operationName": "Viewer"}, None) == [
        {"op": "Viewer", "variables": {}, "data": None, "errors": None}]


# ============================================================
# download_documents over a stubbed urlopen
# ============================================================

class _Resp:
    """The parts of an http.client response download_documents reads."""

    def __init__(self, body: bytes, headers: dict):
        self._body = body
        self.headers = headers

    def read(self) -> bytes:
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def _file(name: str, body: bytes, ctype: str = "text/csv") -> _Resp:
    return _Resp(body, {"content-type": ctype,
                        "content-disposition": f'attachment; filename="{name}"'})


def _stub_urlopen(monkeypatch, answers: dict) -> list[str]:
    """Answer each GET from `answers` (absolute url -> _Resp) and return the
    list of urls requested, in order."""
    import urllib.request
    calls: list[str] = []

    def urlopen(req, timeout=None):
        calls.append(req.full_url)
        return answers[req.full_url]

    monkeypatch.setattr(urllib.request, "urlopen", urlopen)
    return calls


def _documents_capture(tax: list[dict], financial: list[dict] = ()) -> list:
    return [{"op": "AccountDocumentsQuery", "data": {"invest": {
        "accountDocuments": {"taxDocuments": tax,
                             "financialDocuments": list(financial)}}}}]


_VENTURE = "https://venture.angellist.com"
_COOKIES = [{"name": "_angellist_v2", "value": "synthetic"}]


def test_documents_keep_a_complete_copy_and_replace_an_incomplete_one(
        tmp_path, monkeypatch, caplog):
    # Every listed file is fetched, since only the response names it. A
    # complete year's copy on disk is kept as it is; an incomplete year's
    # copy and a file not yet on disk are written.
    docs = tmp_path / "angellist-documents"
    docs.mkdir()
    (docs / "2097 K-1 Package.csv").write_bytes(b"complete, as first fetched\n")
    (docs / "2098 K-1 Package.csv").write_bytes(b"estimate\n")
    calls = _stub_urlopen(monkeypatch, {
        f"{_VENTURE}/k1_packets/1/csv": _file("2097 K-1 Package.csv",
                                              b"complete, refetched\n"),
        f"{_VENTURE}/k1_packets/2/csv": _file("2098 K-1 Package.csv",
                                              b"revised estimate\n"),
        f"{_VENTURE}/financial_reports/3/download": _file(
            "Fund Report.pdf", b"%PDF-1.7 synthetic", "application/pdf"),
    })
    captures = _documents_capture(
        [{"documentType": "complete", "csvUrl": "/k1_packets/1/csv"},
         {"documentType": "estimate_provided", "csvUrl": "/k1_packets/2/csv"}],
        [{"pdfUrl": "/financial_reports/3/download"}])

    with caplog.at_level("INFO"):
        download.download_documents(_COOKIES, captures, docs, dry_run=False)

    assert len(calls) == 3
    assert (docs / "2097 K-1 Package.csv").read_bytes() == \
        b"complete, as first fetched\n"
    assert (docs / "2098 K-1 Package.csv").read_bytes() == b"revised estimate\n"
    assert (docs / "Fund Report.pdf").read_bytes() == b"%PDF-1.7 synthetic"
    assert sorted(p.name for p in docs.iterdir()) == [
        "2097 K-1 Package.csv", "2098 K-1 Package.csv", "Fund Report.pdf"]
    assert any("2 fetched and written, 1 complete copies kept" in m
               for m in caplog.messages)


def test_documents_stop_at_the_login_wall(tmp_path, monkeypatch, caplog):
    # A stale cookie gets the login page in place of a file: the loop stops
    # there, writes nothing for it or after it, and the run goes on.
    docs = tmp_path / "angellist-documents"
    calls = _stub_urlopen(monkeypatch, {
        f"{_VENTURE}/k1_packets/1/csv": _file("2097 K-1 Package.csv", b"k1\n"),
        f"{_VENTURE}/k1_packets/1/download": _Resp(
            b"<html>sign in</html>", {"content-type": "text/html"}),
        f"{_VENTURE}/k1_packets/2/csv": _file("2098 K-1 Package.csv", b"k1\n"),
    })
    captures = _documents_capture(
        [{"documentType": "complete", "csvUrl": "/k1_packets/1/csv",
          "pdfUrl": "/k1_packets/1/download"},
         {"documentType": "complete", "csvUrl": "/k1_packets/2/csv"}])

    with caplog.at_level("WARNING"):
        download.download_documents(_COOKIES, captures, docs, dry_run=False)

    assert calls == [f"{_VENTURE}/k1_packets/1/csv",
                     f"{_VENTURE}/k1_packets/1/download"]
    assert [p.name for p in docs.iterdir()] == ["2097 K-1 Package.csv"]
    assert any("login wall" in m and "(1 written before this.)" in m
               for m in caplog.messages)


def test_documents_dry_run_fetches_nothing(tmp_path, monkeypatch):
    calls = _stub_urlopen(monkeypatch, {})
    captures = _documents_capture(
        [{"documentType": "complete", "csvUrl": "/k1_packets/1/csv"}])
    download.download_documents(_COOKIES, captures, tmp_path / "docs",
                                dry_run=True)
    assert calls == []
    assert not (tmp_path / "docs").exists()
