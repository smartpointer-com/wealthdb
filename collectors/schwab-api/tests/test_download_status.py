"""Status-lifecycle tests for download.py's run.json manifest.

download.run() drops a ``{"status": "in-progress"}`` marker at run-dir
creation and atomically overwrites it with ``{"status": "complete", ...}``
at the end. These tests drive run() with the Schwab client and every
network fetch stubbed (no network), asserting:
  * a clean run leaves run.json status=complete + all load inputs
  * a mid-walk crash leaves run.json status=in-progress (so prune can
    reclaim the dump once quiescent) and no terminal manifest
  * --dry-run creates no run dir at all (nothing for prune to see)

Synthetic account hash / values only.
"""
from __future__ import annotations

import json
import sys
import types
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402

ACCT_PLAIN = "00000001"
ACCT_HASH = "HASH00000000000000000001"


def _stub_schwab(monkeypatch, *, open_orders_raises: bool = False) -> None:
    """Install a fake ``schwab`` module + stub every network boundary
    download.run() touches, so run() executes end-to-end offline."""
    fake = types.ModuleType("schwab")
    fake.auth = types.SimpleNamespace(
        client_from_token_file=lambda **_kw: object())
    monkeypatch.setitem(sys.modules, "schwab", fake)

    monkeypatch.setattr(download, "configure_timeout", lambda *_a, **_k: None)
    monkeypatch.setattr(download, "fetch_account_numbers",
                        lambda _c: [{"accountNumber": ACCT_PLAIN,
                                     "hashValue": ACCT_HASH}])
    monkeypatch.setattr(download, "fetch_user_preference",
                        lambda _c: {"accounts": []})
    monkeypatch.setattr(download, "fetch_accounts_with_positions",
                        lambda _c: [])
    monkeypatch.setattr(download, "fetch_transactions",
                        lambda _c, _h, _s, _e: [])

    def _open_orders(_c, _s, _e):
        if open_orders_raises:
            raise RuntimeError("simulated crash fetching open orders")
        return []
    monkeypatch.setattr(download, "fetch_open_orders", _open_orders)


def _argv(token: Path, dest: Path, *extra: str) -> list[str]:
    return ["--token-path", str(token), "--dest", str(dest),
            "--client-id", "synthetic-id", "--client-secret", "synthetic-secret",
            *extra]


def _run_dirs(dest: Path) -> list[Path]:
    return [p for p in dest.iterdir() if p.is_dir()]


def test_complete_run_writes_status_complete(tmp_path, monkeypatch):
    _stub_schwab(monkeypatch)
    token = tmp_path / "token.json"
    token.write_text("{}")
    dest = tmp_path / "bronze"
    dest.mkdir()

    assert download.main(_argv(token, dest)) == 0

    dirs = _run_dirs(dest)
    assert len(dirs) == 1
    run_dir = dirs[0]
    meta = json.loads((run_dir / "run.json").read_text())
    assert meta["status"] == "complete"
    assert meta["accounts"] == 1
    # All load inputs present; open_orders.json is the terminal artefact.
    for name in ("account_numbers.json", "user_preference.json",
                 "accounts_positions.json", "transactions_000.json",
                 "open_orders.json"):
        assert (run_dir / name).exists()


def test_crash_mid_walk_leaves_status_in_progress(tmp_path, monkeypatch):
    _stub_schwab(monkeypatch, open_orders_raises=True)
    token = tmp_path / "token.json"
    token.write_text("{}")
    dest = tmp_path / "bronze"
    dest.mkdir()

    with pytest.raises(RuntimeError):
        download.main(_argv(token, dest))

    dirs = _run_dirs(dest)
    assert len(dirs) == 1
    run_dir = dirs[0]
    meta = json.loads((run_dir / "run.json").read_text())
    # Terminal manifest never written ⇒ marker still in-progress; prune
    # classifies this NON_COMPLETE and reclaims it once quiescent.
    assert meta == {"status": "in-progress"}
    assert not (run_dir / "open_orders.json").exists()


def test_dry_run_creates_no_run_dir(tmp_path, monkeypatch):
    _stub_schwab(monkeypatch)
    token = tmp_path / "token.json"
    token.write_text("{}")
    dest = tmp_path / "bronze"
    dest.mkdir()

    assert download.main(_argv(token, dest, "--dry-run")) == 0
    # --dry-run returns before the run dir is minted, so there is no
    # shell and no run.json for prune to see.
    assert _run_dirs(dest) == []
