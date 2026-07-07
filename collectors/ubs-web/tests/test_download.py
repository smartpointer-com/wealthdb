"""Tests for download.py's bronze-persistence discipline.

The load-bearing guarantee: ``download --dry-run`` is the read-only
walk (root CLAUDE.md §2) and must persist NOTHING under the bronze
``--dest`` — not even a ``run.json`` shell, because ``load``'s
``scan_bronze`` has no status guard and would ingest such a shell as a
dump run. These tests pin that invariant plus the real-run counterpart.
"""
from __future__ import annotations

import sys
import types
from pathlib import Path

import download


# --------------------------------------------------------------------
# _prepare_run_dir: the run-dir-target decision in isolation
# --------------------------------------------------------------------

def test_prepare_run_dir_dry_run_creates_nothing(tmp_path):
    dest = tmp_path / "bronze"
    run_dir = download._prepare_run_dir(dest, dry_run=True)
    assert run_dir is None
    # --dest must be left entirely untouched (not even created).
    assert not dest.exists()


def test_prepare_run_dir_real_run_writes_in_progress_marker(tmp_path):
    dest = tmp_path / "bronze"
    run_dir = download._prepare_run_dir(dest, dry_run=False)
    assert run_dir is not None
    assert run_dir.parent == dest
    run_json = run_dir / "run.json"
    assert run_json.is_file()
    import json
    assert json.loads(run_json.read_text())["status"] == "in-progress"


# --------------------------------------------------------------------
# End-to-end dry-run: main() with the Playwright layer mocked
# --------------------------------------------------------------------

class _FakePage:
    def set_default_navigation_timeout(self, _ms):
        pass


class _FakeContext:
    def new_page(self):
        return _FakePage()

    def close(self):
        pass


class _FakeBrowser:
    def close(self):
        pass


class _FakeSyncPlaywright:
    """Stand-in for playwright.sync_api.sync_playwright()."""

    def __enter__(self):
        return object()  # the `pw` handle; _new_context is mocked away

    def __exit__(self, *_a):
        return False


def _mock_playwright_layer(monkeypatch):
    """Neutralise the browser layer so main() exercises pure control
    flow: no real Chromium, no network, no live UBS session."""
    fake_mod = types.ModuleType("playwright.sync_api")
    fake_mod.sync_playwright = lambda: _FakeSyncPlaywright()
    monkeypatch.setitem(sys.modules, "playwright", types.ModuleType("playwright"))
    monkeypatch.setitem(sys.modules, "playwright.sync_api", fake_mod)
    monkeypatch.setattr(
        download, "_new_context", lambda _pw, _sp: (_FakeBrowser(), _FakeContext())
    )
    monkeypatch.setattr(download, "_verify_session", lambda _page: None)
    monkeypatch.setattr(download, "enumerate_accounts", lambda _page, _sd: [])


def test_dry_run_download_persists_nothing_to_bronze(tmp_path, monkeypatch):
    _mock_playwright_layer(monkeypatch)
    dest = tmp_path / "bronze"
    state = tmp_path / "state.json"
    state.write_text("{}")

    rc = download.main(
        ["--state-path", str(state), "--dest", str(dest), "--dry-run"]
    )

    assert rc == 0
    # The core regression: no run dir — and nothing at all — under --dest.
    if dest.exists():
        assert list(dest.iterdir()) == []
    # Belt and suspenders: no UTC-timestamped dump dir anywhere below it.
    assert list(Path(dest).glob("*/run.json")) == []


# --------------------------------------------------------------------
# _fetch_document: content-addressed naming
# --------------------------------------------------------------------

class _FakeResp:
    def __init__(self, body, ok=True, status=200):
        self._body, self.ok, self.status = body, ok, status

    def body(self):
        return self._body


class _FakeReqContext:
    """A Playwright context whose request.get returns a fixed body."""

    def __init__(self, body):
        self._body = body

        class _Req:
            def get(_self, _href, timeout=None):
                return _FakeResp(self._body)

        self.request = _Req()


def test_fetch_document_named_by_content_hash(tmp_path):
    import hashlib
    docs = tmp_path / "documents"
    docs.mkdir()
    body = b"%PDF-1.4 synthetic statement bytes\n"
    sha = hashlib.sha256(body).hexdigest()

    meta = download._fetch_document(
        _FakeReqContext(body),
        "https://ubs.example/doc?apikey=TENANTSECRET&Accept=application/pdf",
        "persessiontoken0000", "Account statement 01.02.2026", docs)

    # Named by content, not by the per-session token.
    assert meta["filename"] == f"{sha}.pdf"
    assert "persessiontoken" not in meta["filename"]
    assert (docs / f"{sha}.pdf").read_bytes() == body
    assert meta["content_sha256"] == sha
    assert meta["label"] == "Account statement 01.02.2026"
    # The tenant apikey secret is never persisted in the recorded url.
    assert "apikey" not in meta["url"] and "TENANTSECRET" not in meta["url"]


def test_fetch_document_identical_bytes_collapse_to_one_file(tmp_path):
    # Two different session tokens, identical bytes → one content-addressed
    # file (the old token naming would have written two copies).
    docs = tmp_path / "documents"
    docs.mkdir()
    body = b"%PDF-1.4 same statement\n"
    ctx = _FakeReqContext(body)
    m1 = download._fetch_document(ctx, "https://u?apikey=K", "tokenAAAA", "L", docs)
    m2 = download._fetch_document(ctx, "https://u?apikey=K", "tokenBBBB", "L", docs)
    assert m1["filename"] == m2["filename"]
    assert len(list(docs.glob("*.pdf"))) == 1


def test_fetch_document_non_pdf_returns_none(tmp_path):
    docs = tmp_path / "documents"
    docs.mkdir()
    meta = download._fetch_document(
        _FakeReqContext(b"<html>login</html>"),
        "https://u?apikey=K", "tok", "L", docs)
    assert meta is None
    assert list(docs.glob("*.pdf")) == []
