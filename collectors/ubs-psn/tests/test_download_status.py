"""
Tests for download.py's run.json status lifecycle.

The SFTP transport (connect + the per-order-type sftp.get loop) is
mocked out; these exercise only the additive metadata wrapper the
prune convention introduced, plus the --debug listing capture:

  * an "in-progress" run.json is written at run-dir creation, before
    the pull walks
  * a finished pull (>=1 zip) overwrites it with status="complete"
  * a pull that fetched nothing removes the whole shell (rmtree, since
    the run.json makes the dir non-empty) — unless --debug captured a
    listing, which is kept as a status="empty" shell for prune
  * --dry-run creates no run dir (and never walks)
  * --debug captures the host key + per-order-type listing, BEFORE the
    pull and without ever fetching

Synthetic client id / zip bytes / fingerprints only — no network, no
real UBS data.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import download  # noqa: E402
from collectorkit import bronze  # noqa: E402


class _FakeAttr:
    """The slice of paramiko.SFTPAttributes the listing capture reads."""

    def __init__(self, filename: str, st_size: int):
        self.filename = filename
        self.st_size = st_size


def _fake_client(listing=None):
    """An SSHClient stand-in whose transport presents a synthetic host key
    and whose SFTP channel answers listdir_attr from `listing` (a
    remote-dir -> [_FakeAttr] map; anything absent raises, as an
    unprovisioned order type does)."""
    client = mock.MagicMock()
    key = mock.MagicMock()
    key.asbytes.return_value = b"synthetic-host-key"
    client.get_transport.return_value.get_remote_server_key.return_value = key

    sftp = mock.MagicMock()

    def _listdir_attr(remote):
        try:
            return (listing or {})[remote]
        except KeyError:
            raise FileNotFoundError(remote)

    sftp.listdir_attr.side_effect = _listdir_attr
    client.open_sftp.return_value = sftp
    return client


def _run_main(tmp_path, monkeypatch, download_all_impl, *extra_argv,
              client=None):
    """Invoke download.main() with connect() + download_all() mocked."""
    if client is None:
        client = mock.MagicMock()
        client.open_sftp.return_value = mock.MagicMock()
    monkeypatch.setattr(download, "connect", lambda args: client)
    monkeypatch.setattr(download, "download_all", download_all_impl)
    monkeypatch.setattr(
        sys, "argv",
        ["download.py", "--client-id", "CH000000",
         "--bronze-dir", str(tmp_path), *extra_argv])
    return download.main()


def _run_dirs(root: Path):
    return [p for p in root.iterdir()
            if p.is_dir() and bronze.RUN_DIR_RE.match(p.name)]


def test_complete_pull_writes_complete_status(tmp_path, monkeypatch):
    def dl(sftp, run_dir, verbose=False):
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        (run_dir / "Z40.zip").write_bytes(b"PK\x03\x04")
        return 2, 1

    assert _run_main(tmp_path, monkeypatch, dl) == 0
    runs = _run_dirs(tmp_path)
    assert len(runs) == 1
    d = runs[0]
    assert (d / "ZAH.zip").exists()
    meta = json.loads((d / "run.json").read_text())
    assert meta["status"] == "complete"
    assert meta["downloaded"] == 2
    assert meta["empty"] == 1


def test_in_progress_marker_present_during_walk(tmp_path, monkeypatch):
    seen = {}

    def dl(sftp, run_dir, verbose=False):
        seen["mid"] = json.loads((run_dir / "run.json").read_text())
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        return 1, 0

    _run_main(tmp_path, monkeypatch, dl)
    assert seen["mid"]["status"] == "in-progress"


def test_zero_zip_pull_removes_shell(tmp_path, monkeypatch):
    def dl(sftp, run_dir, verbose=False):
        return 0, 40

    assert _run_main(tmp_path, monkeypatch, dl) == 0
    # rmtree removed the in-progress-only shell — no run dir survives.
    assert _run_dirs(tmp_path) == []


def test_dry_run_creates_no_run_dir(tmp_path, monkeypatch):
    def dl(sftp, run_dir, verbose=False):
        raise AssertionError("download_all must not run under --dry-run")

    assert _run_main(tmp_path, monkeypatch, dl, "--dry-run") == 0
    assert _run_dirs(tmp_path) == []


def test_zero_zip_pull_with_debug_keeps_the_captured_shell(tmp_path, monkeypatch):
    # download_all stats one exact path per order type, so a file queued
    # under an unexpected name reads as "nothing queued". Discarding the
    # shell would take the listing that proves it with it — so a --debug
    # pull keeps it, marked non-complete for prune to reclaim.
    def dl(sftp, run_dir, verbose=False):
        return 0, 40

    client = _fake_client({"download/ZAH": [_FakeAttr("ZAH_2026.zip", 12)]})
    assert _run_main(tmp_path, monkeypatch, dl, "--debug", client=client) == 0
    runs = _run_dirs(tmp_path)
    assert len(runs) == 1
    meta = json.loads((runs[0] / "run.json").read_text())
    assert meta["status"] == "empty"          # non-complete ⇒ prune reclaims
    assert meta["downloaded"] == 0
    listing = (runs[0] / "screenshots" / "sftp-listing.txt").read_text()
    assert "ZAH_2026.zip (12 bytes)" in listing


# ============================================================
# --debug: the SFTP listing capture
# ============================================================

def _listing(tmp_path) -> str:
    return (_run_dirs(tmp_path)[0] / "screenshots" / "sftp-listing.txt").read_text()


def test_debug_parses_and_defaults_off(monkeypatch):
    monkeypatch.setattr(sys, "argv", ["download.py", "--client-id", "CH000000"])
    assert download.parse_args().debug is False
    monkeypatch.setattr(sys, "argv",
                        ["download.py", "--client-id", "CH000000", "--debug"])
    assert download.parse_args().debug is True


def test_without_debug_nothing_is_captured(tmp_path, monkeypatch):
    def dl(sftp, run_dir, verbose=False):
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        return 1, 0

    client = _fake_client({"download/ZAH": [_FakeAttr("ZAH.zip", 4)]})
    assert _run_main(tmp_path, monkeypatch, dl, client=client) == 0
    assert not (_run_dirs(tmp_path)[0] / "screenshots").exists()
    # A capture is opt-in, so the server is never even listed.
    client.open_sftp.return_value.listdir_attr.assert_not_called()


def test_debug_captures_host_key_and_listing(tmp_path, monkeypatch):
    def dl(sftp, run_dir, verbose=False):
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        return 1, 0

    client = _fake_client({"download/ZAH": [_FakeAttr("ZAH.zip", 4)]})
    assert _run_main(tmp_path, monkeypatch, dl, "--debug", client=client) == 0
    text = _listing(tmp_path)
    # The key the session actually accepted, in the SHA256: form UBS
    # publishes its fingerprints in.
    assert text.startswith("host-key: SHA256:")
    # What the server was offering, per order-type dir...
    assert "download/ZAH/: ZAH.zip (4 bytes)" in text
    # ...and the ones it was not: an absent dir is how an unprovisioned
    # order type looks, which is itself the diagnostic.
    assert "download/PTK/: absent (order type not provisioned)" in text
    assert len(download.ORDER_TYPES) == sum(
        1 for line in text.splitlines() if line.startswith("download/"))


def test_debug_capture_never_fetches(tmp_path, monkeypatch):
    # UBS deletes each zip server-side on a successful download and there is
    # no re-fetch: the capture must observe only. download_all owns every
    # get(); the capture may not add one.
    def dl(sftp, run_dir, verbose=False):
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        return 1, 0

    client = _fake_client({"download/ZAH": [_FakeAttr("ZAH.zip", 4)]})
    assert _run_main(tmp_path, monkeypatch, dl, "--debug", client=client) == 0
    sftp = client.open_sftp.return_value
    sftp.get.assert_not_called()
    sftp.open.assert_not_called()


def test_debug_captures_before_the_pull(tmp_path, monkeypatch):
    # The listing must show the zips while they still exist, and a capture
    # that failed must not be able to strand a downloaded-but-unfinalised
    # zip. Both mean it runs first.
    seen = {}

    def dl(sftp, run_dir, verbose=False):
        seen["captured"] = (run_dir / "screenshots" / "sftp-listing.txt").exists()
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        return 1, 0

    _run_main(tmp_path, monkeypatch, dl, "--debug", client=_fake_client())
    assert seen["captured"] is True


def test_debug_capture_failure_never_takes_down_the_pull(tmp_path, monkeypatch):
    # A diagnostic that breaks the run it is diagnosing is worse than no
    # diagnostic — the more so when that run holds an irreplaceable pull.
    def dl(sftp, run_dir, verbose=False):
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        return 1, 0

    client = _fake_client()
    client.get_transport.side_effect = RuntimeError("transport gone")
    assert _run_main(tmp_path, monkeypatch, dl, "--debug", client=client) == 0
    d = _run_dirs(tmp_path)[0]
    assert (d / "ZAH.zip").exists()          # the pull still landed...
    assert json.loads((d / "run.json").read_text())["status"] == "complete"
    assert not (d / "screenshots").exists()  # ...only the capture was lost


def test_dry_run_with_debug_creates_no_run_dir(tmp_path, monkeypatch):
    # --dry-run exports nothing, so there is no run dir for a capture to
    # land in; a listing is not worth resurrecting a bronze shell for.
    def dl(sftp, run_dir, verbose=False):
        raise AssertionError("download_all must not run under --dry-run")

    client = _fake_client()
    assert _run_main(tmp_path, monkeypatch, dl, "--dry-run", "--debug",
                     client=client) == 0
    assert _run_dirs(tmp_path) == []


def test_lookback_flag_accepted(tmp_path, monkeypatch):
    # An SFTP pull takes whatever UBS has queued; --lookback cannot narrow it,
    # so it is accepted for fleet uniformity and only logs a note —
    # the pull still runs in full.
    def dl(sftp, run_dir, verbose=False):
        (run_dir / "ZAH.zip").write_bytes(b"PK\x03\x04")
        return 1, 0

    assert _run_main(tmp_path, monkeypatch, dl, "--lookback", "4w") == 0
    d = _run_dirs(tmp_path)[0]
    assert json.loads((d / "run.json").read_text())["status"] == "complete"
