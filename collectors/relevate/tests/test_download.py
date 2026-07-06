"""Regression tests for relevate's download.py dry-run contract.

Root CLAUDE.md §2: `download --dry-run` walks the read-only export
surfaces (verify the session, enumerate the overview + document index)
but must persist NOTHING under the bronze root (`--dest`). A dry-run
that left a run dir — even a run.json-only shell — could be picked up by
`load` and land as a dry-run snapshot in silver.

These tests mock the session (no real Relevate calls, no state file) and
assert the invariant: after a dry-run, the bronze dest holds no run dir
and no file at all, while the enumeration endpoints were still hit.
Synthetic ids / amounts only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import download  # noqa: E402


class _FakeResp:
    """Minimal stand-in for a requests.Response, enough for
    get_and_save_json's 200 path (status_code / content / json())."""

    def __init__(self, status_code: int, body: dict) -> None:
        self.status_code = status_code
        self._body = body
        self.content = json.dumps(body).encode("utf-8")
        self.headers: dict[str, str] = {"content-type": "application/json"}

    def json(self) -> dict:
        return self._body


class _FakeSession:
    """Routes the two master-listing GETs the dry-run walk makes, and
    records every URL so the test can prove the read-only walk ran."""

    def __init__(self) -> None:
        self.urls: list[str] = []

    def get(self, url, timeout=None, allow_redirects=None):  # noqa: ANN001
        self.urls.append(url)
        if url.endswith(download.EP_INVESTMENT_OVERVIEW):
            return _FakeResp(200, {"portfolios": [
                {"externalId": "ACC1", "id": 100}]})
        if url.endswith(download.EP_DOCUMENTS_INDEX):
            return _FakeResp(200, {"documents": [
                {"id": 1, "createDate": "2026-01-01T00:00:00"}]})
        raise AssertionError(f"unexpected dry-run GET: {url}")


def _patch_session(monkeypatch) -> _FakeSession:
    sess = _FakeSession()
    monkeypatch.setattr(
        download, "new_session_from_state", lambda state_path: (sess, None))
    monkeypatch.setattr(download, "probe_session_alive", lambda session: True)
    return sess


def test_dry_run_persists_nothing_to_bronze(tmp_path, monkeypatch):
    sess = _patch_session(monkeypatch)
    bronze = tmp_path / "bronze"
    bronze.mkdir()

    rc = download.main([
        "--dry-run",
        "--dest", str(bronze),
        "--state-path", str(tmp_path / "state.json"),
    ])

    # The walk succeeded (session verified, both listings enumerated)...
    assert rc == 0
    assert any(u.endswith(download.EP_INVESTMENT_OVERVIEW) for u in sess.urls)
    assert any(u.endswith(download.EP_DOCUMENTS_INDEX) for u in sess.urls)

    # ...but NOTHING was written under the bronze dest: no run dir, no
    # run.json shell, no endpoint dumps. This is the invariant.
    assert list(bronze.rglob("*")) == []


def test_dry_run_does_not_create_dest(tmp_path, monkeypatch):
    # Even the bronze root itself must not be materialised by a dry-run:
    # a real run's run_dir.mkdir(parents=True) would create it, a dry-run
    # must not touch --dest at all.
    _patch_session(monkeypatch)
    bronze = tmp_path / "bronze_absent"

    rc = download.main([
        "--dry-run",
        "--dest", str(bronze),
        "--state-path", str(tmp_path / "state.json"),
    ])

    assert rc == 0
    assert not bronze.exists()
