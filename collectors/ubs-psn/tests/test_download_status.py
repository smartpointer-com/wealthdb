"""
Tests for download.py's listing-driven pull, run.json lifecycle,
listing.json record, zip validation, and --recover.

connect() is mocked out; the SFTP channel is a fake that answers
listdir_attr and get() from an in-memory remote-dir -> {name: bytes}
map, so the real download_all / recover_all loops run under test:

  * an "in-progress" run.json (with a mode field) is written at run-dir
    creation, before the pull walks
  * the pull is driven by one listdir_attr per download/<OT>/ dir: the
    <OT>.zip queue file is fetched, an unexpectedly-named undotted file
    is warned about and fetched too, dot-prefixed archive copies are
    never touched by a normal pull
  * a listed name that would escape the run dir or clobber the run's
    own metadata files is refused, never fetched
  * every run records the full pre-pull listing in listing.json (host
    key + per-order-type entries), before the first fetch
  * every fetched file is validated as a zip by content, never by its
    listed size; a corrupt fetch fails the run loudly with the run dir
    kept
  * a pull that fetched nothing keeps its shell as status="empty" (the
    listing that explains it stays inspectable; prune reclaims it)
  * --recover fetches only the dated archive copies, lands them
    undotted as <OT>_<YYYYMMDD>.zip, honours --lookback, and is freely
    re-runnable
  * --dry-run creates no run dir (and never lists or fetches)
  * --debug and --lookback parse everywhere (fleet uniformity) and
    change nothing on a normal pull

Synthetic client id / zip bytes / fingerprints only — no network, no
real UBS data.
"""

from __future__ import annotations

import io
import json
import logging
import sys
import zipfile
from datetime import datetime, timedelta
from pathlib import Path
from unittest import mock

import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import download  # noqa: E402
from collectorkit import bronze  # noqa: E402


def _zip_bytes(inner_name: str = "2026-06-01_SYN_synthetic.txt",
               data: bytes = b"synthetic") -> bytes:
    """A minimal valid zip holding one synthetic entry."""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr(inner_name, data)
    return buf.getvalue()


class _FakeAttr:
    """The slice of paramiko.SFTPAttributes the listing reads."""

    def __init__(self, filename: str, st_size: int):
        self.filename = filename
        self.st_size = st_size


def _entry_bytes(v) -> bytes:
    return v[0] if isinstance(v, tuple) else v


def _entry_size(v) -> int:
    """The size the server *lists* — allowed to differ from the bytes it
    serves, which is exactly why validation must never compare sizes."""
    return v[1] if isinstance(v, tuple) else len(_entry_bytes(v))


def _fake_client(files=None, on_get=None):
    """An SSHClient stand-in: the transport presents a synthetic host key;
    the SFTP channel answers listdir_attr from `files` (a remote-dir ->
    {name: bytes | (bytes, listed_size)} map; an absent dir raises, as an
    unprovisioned order type does) and serves get() from the same map.
    `on_get(remote, local)` runs before each transfer lands."""
    files = files or {}
    client = mock.MagicMock()
    key = mock.MagicMock()
    key.asbytes.return_value = b"synthetic-host-key"
    client.get_transport.return_value.get_remote_server_key.return_value = key

    sftp = mock.MagicMock()

    def _listdir_attr(remote):
        try:
            d = files[remote]
        except KeyError:
            raise FileNotFoundError(remote) from None
        return [_FakeAttr(name, _entry_size(v)) for name, v in d.items()]

    def _get(remote, local):
        rdir, _, name = remote.rpartition("/")
        try:
            v = files[rdir][name]
        except KeyError:
            raise FileNotFoundError(remote) from None
        if on_get is not None:
            on_get(remote, Path(local))
        Path(local).write_bytes(_entry_bytes(v))

    sftp.listdir_attr.side_effect = _listdir_attr
    sftp.get.side_effect = _get
    client.open_sftp.return_value = sftp
    return client


def _run_main(tmp_path, monkeypatch, client, *extra_argv):
    """Invoke download.main() with only connect() mocked — the listing,
    fetch and validation paths all run for real."""
    monkeypatch.setattr(download, "connect", lambda args: client)
    monkeypatch.setattr(
        sys, "argv",
        ["download.py", "--client-id", "CH000000",
         "--bronze-dir", str(tmp_path), *extra_argv])
    return download.main()


def _run_dirs(root: Path):
    return [p for p in root.iterdir()
            if p.is_dir() and bronze.RUN_DIR_RE.match(p.name)]


def _meta(run_dir: Path) -> dict:
    return json.loads((run_dir / "run.json").read_text())


def _fetched_remotes(client) -> list[str]:
    return [c.args[0] for c in client.open_sftp.return_value.get.call_args_list]


# ============================================================
# The normal pull: listing-driven fetch + run.json lifecycle
# ============================================================

def test_complete_pull_writes_complete_status(tmp_path, monkeypatch):
    client = _fake_client({
        "download/ZAH": {"ZAH.zip": _zip_bytes()},
        "download/Z40": {"Z40.zip": _zip_bytes()},
    })
    assert _run_main(tmp_path, monkeypatch, client) == 0
    runs = _run_dirs(tmp_path)
    assert len(runs) == 1
    d = runs[0]
    assert zipfile.ZipFile(d / "ZAH.zip").namelist()  # landed, valid
    assert zipfile.ZipFile(d / "Z40.zip").namelist()
    meta = _meta(d)
    assert meta["status"] == "complete"
    assert meta["mode"] == "download"
    assert meta["downloaded"] == 2
    assert meta["empty"] == len(download.ORDER_TYPES) - 2


def test_in_progress_marker_present_during_pull(tmp_path, monkeypatch):
    seen = {}

    def on_get(remote, local):
        seen["mid"] = json.loads((local.parent / "run.json").read_text())

    client = _fake_client({"download/ZAH": {"ZAH.zip": _zip_bytes()}},
                          on_get=on_get)
    _run_main(tmp_path, monkeypatch, client)
    assert seen["mid"]["status"] == "in-progress"
    assert seen["mid"]["mode"] == "download"


def test_zero_zip_pull_keeps_empty_shell(tmp_path, monkeypatch):
    # Nothing queued anywhere: the shell is kept as status="empty" so the
    # listing that explains the empty pull stays inspectable; prune
    # reclaims it once quiescent.
    client = _fake_client({"download/ZAH": {}})
    assert _run_main(tmp_path, monkeypatch, client) == 0
    runs = _run_dirs(tmp_path)
    assert len(runs) == 1
    meta = _meta(runs[0])
    assert meta["status"] == "empty"
    assert meta["downloaded"] == 0
    assert (runs[0] / "listing.json").exists()


def test_queue_fetch_leaves_dated_archive_untouched(tmp_path, monkeypatch):
    # The dot-prefixed dated copies are --recover's territory; a normal
    # pull fetches only the undotted queue file.
    client = _fake_client({
        "download/ZAH": {"ZAH.zip": _zip_bytes(),
                         ".ZAH_20260601.zip": _zip_bytes()},
    })
    assert _run_main(tmp_path, monkeypatch, client) == 0
    assert _fetched_remotes(client) == ["download/ZAH/ZAH.zip"]
    d = _run_dirs(tmp_path)[0]
    assert (d / "ZAH.zip").exists()
    assert not (d / ".ZAH_20260601.zip").exists()
    assert _meta(d)["downloaded"] == 1


def test_unexpected_name_is_warned_about_and_fetched(tmp_path, monkeypatch,
                                                     caplog):
    # A file UBS queued under an unexpected name is data too: bronze keeps
    # every zip, so it is fetched under its server filename — loudly.
    client = _fake_client({
        "download/ZAH": {"ZAH.zip": _zip_bytes(),
                         "ZAH-RETRY.zip": _zip_bytes()},
    })
    with caplog.at_level(logging.WARNING, logger="ubs-psn"):
        assert _run_main(tmp_path, monkeypatch, client) == 0
    assert any("ZAH-RETRY.zip" in r.message for r in caplog.records)
    d = _run_dirs(tmp_path)[0]
    assert (d / "ZAH.zip").exists()
    assert (d / "ZAH-RETRY.zip").exists()
    assert _meta(d)["downloaded"] == 2


def test_unsafe_listing_name_is_refused(tmp_path, monkeypatch, caplog):
    # A listed name that is not a plain basename would land outside the
    # run dir (pathlib: run_dir / "/abs" IS "/abs"); one that matches
    # run.json / listing.json would clobber the run's own metadata.
    # Such entries are never fetched — the queue file next to them
    # still is, and skipping a fetch consumes nothing server-side.
    # (".." falls under the dot-prefix skip, so it never reaches the
    # basename guard — but it is never fetched either.)
    client = _fake_client({
        "download/ZAH": {"ZAH.zip": _zip_bytes(),
                         "sub/escape.zip": _zip_bytes(),
                         "/abs.zip": _zip_bytes(),
                         "..": _zip_bytes(),
                         "run.json": _zip_bytes(),
                         "listing.json": _zip_bytes()},
    })
    with caplog.at_level(logging.WARNING, logger="ubs-psn"):
        assert _run_main(tmp_path, monkeypatch, client) == 0
    assert _fetched_remotes(client) == ["download/ZAH/ZAH.zip"]
    refused = [r.message for r in caplog.records
               if "not a safe run-dir basename" in r.message]
    assert len(refused) == 4
    d = _run_dirs(tmp_path)[0]
    meta = _meta(d)  # run.json is still the run's own JSON metadata
    assert meta["status"] == "complete"
    assert meta["downloaded"] == 1
    # listing.json is still the pre-pull record, not fetched zip bytes.
    listing = json.loads((d / "listing.json").read_text())
    assert listing["order_types"]["ZAH"]["status"] == "listed"


def test_dry_run_creates_no_run_dir(tmp_path, monkeypatch):
    client = _fake_client({"download/ZAH": {"ZAH.zip": _zip_bytes()}})
    assert _run_main(tmp_path, monkeypatch, client, "--dry-run") == 0
    assert _run_dirs(tmp_path) == []
    sftp = client.open_sftp.return_value
    sftp.listdir_attr.assert_not_called()
    sftp.get.assert_not_called()


def test_check_and_recover_are_mutually_exclusive(monkeypatch):
    monkeypatch.setattr(sys, "argv",
                        ["download.py", "--check", "--recover"])
    with pytest.raises(SystemExit) as e:
        download.parse_args()
    assert e.value.code == 2


# ============================================================
# listing.json: the always-on pre-pull record
# ============================================================

def test_listing_json_written_on_every_run(tmp_path, monkeypatch):
    zbytes = _zip_bytes()
    client = _fake_client({
        "download/ZAH": {"ZAH.zip": zbytes, ".ZAH_20260601.zip": zbytes},
    })
    assert _run_main(tmp_path, monkeypatch, client) == 0
    listing = json.loads((_run_dirs(tmp_path)[0] / "listing.json").read_text())
    assert set(listing) == {"captured_at", "host_key", "order_types"}
    # The key the session actually accepted, in the SHA256: form UBS
    # publishes its fingerprints in.
    assert listing["host_key"].startswith("SHA256:")
    # One record per known order type: what each dir offered...
    assert set(listing["order_types"]) == set(download.ORDER_TYPES)
    zah = listing["order_types"]["ZAH"]
    assert zah["status"] == "listed"
    assert {"name": ".ZAH_20260601.zip", "size": len(zbytes)} in zah["entries"]
    assert {"name": "ZAH.zip", "size": len(zbytes)} in zah["entries"]
    # ...and an absent dir is how an unprovisioned order type looks,
    # which is itself part of the record.
    assert listing["order_types"]["PTK"] == {"status": "absent"}


def test_listing_recorded_before_the_pull(tmp_path, monkeypatch):
    # The listing must show the zips while they still exist server-side
    # (the queue copy is deleted by its own fetch), so it lands first.
    seen = {}

    def on_get(remote, local):
        seen["captured"] = (local.parent / "listing.json").exists()

    client = _fake_client({"download/ZAH": {"ZAH.zip": _zip_bytes()}},
                          on_get=on_get)
    _run_main(tmp_path, monkeypatch, client)
    assert seen["captured"] is True


# ============================================================
# Zip validation: content, never size
# ============================================================

def test_corrupt_fetch_fails_the_run_loudly(tmp_path, monkeypatch):
    client = _fake_client({"download/ZAH": {"ZAH.zip": b"not a zip"}})
    with pytest.raises(SystemExit, match="Corrupt zip"):
        _run_main(tmp_path, monkeypatch, client)
    # The run dir is kept as-is: the queue copy is already consumed
    # server-side, so discarding the fetch would destroy the only local
    # trace; run.json stays non-terminal.
    d = _run_dirs(tmp_path)[0]
    assert (d / "ZAH.zip").exists()
    assert _meta(d)["status"] == "in-progress"


def test_listed_size_is_never_trusted(tmp_path, monkeypatch):
    # The served stream can undercut the listed st_size while still being
    # a complete, valid zip; validation is content-based, so the run
    # completes.
    zbytes = _zip_bytes()
    client = _fake_client({
        "download/ZAH": {"ZAH.zip": (zbytes, len(zbytes) + 512)},
    })
    assert _run_main(tmp_path, monkeypatch, client) == 0
    assert _meta(_run_dirs(tmp_path)[0])["status"] == "complete"


# ============================================================
# --recover: replaying the dated archive trail
# ============================================================

def test_recover_fetches_dated_copies_only(tmp_path, monkeypatch):
    client = _fake_client({
        "download/ZAH": {"ZAH.zip": _zip_bytes(),
                         ".ZAH_20260601.zip": _zip_bytes(),
                         ".ZAH_20260415.zip": _zip_bytes(),
                         ".hidden": b"noise",
                         ".ZAH_2026.zip": b"noise"},
    })
    assert _run_main(tmp_path, monkeypatch, client, "--recover") == 0
    # The undotted queue file is consumed by a fetch, so recovery must
    # never touch it; malformed dot names are skipped.
    assert sorted(_fetched_remotes(client)) == [
        "download/ZAH/.ZAH_20260415.zip",
        "download/ZAH/.ZAH_20260601.zip",
    ]
    d = _run_dirs(tmp_path)[0]
    assert zipfile.ZipFile(d / "ZAH_20260415.zip").namelist()
    assert zipfile.ZipFile(d / "ZAH_20260601.zip").namelist()
    assert not (d / "ZAH.zip").exists()
    meta = _meta(d)
    assert meta["status"] == "complete"
    assert meta["mode"] == "recover"
    assert meta["downloaded"] == 2


def test_recover_lookback_bounds_the_replay(tmp_path, monkeypatch):
    client = _fake_client({
        "download/ZAH": {".ZAH_20260601.zip": _zip_bytes(),
                         ".ZAH_20260415.zip": _zip_bytes()},
    })
    assert _run_main(tmp_path, monkeypatch, client,
                     "--recover", "--lookback", "2026-05-01") == 0
    assert _fetched_remotes(client) == ["download/ZAH/.ZAH_20260601.zip"]
    d = _run_dirs(tmp_path)[0]
    assert (d / "ZAH_20260601.zip").exists()
    assert not (d / "ZAH_20260415.zip").exists()
    assert _meta(d)["downloaded"] == 1


def test_recover_is_freely_rerunnable(tmp_path, monkeypatch):
    # Dated copies survive their own fetch, so a second recovery run
    # finds and lands the same files again, in its own run dir.
    class _TickingDT(datetime):
        _tick = 0

        @classmethod
        def now(cls, tz=None):
            _TickingDT._tick += 1
            return (datetime(2026, 7, 1, 12, 0, 0, tzinfo=tz)
                    + timedelta(seconds=10 * _TickingDT._tick))

    monkeypatch.setattr(download, "datetime", _TickingDT)
    files = {"download/ZAH": {".ZAH_20260601.zip": _zip_bytes()}}
    for _ in range(2):
        client = _fake_client(files)
        assert _run_main(tmp_path, monkeypatch, client, "--recover") == 0
    runs = _run_dirs(tmp_path)
    assert len(runs) == 2
    for d in runs:
        assert (d / "ZAH_20260601.zip").exists()
        assert _meta(d)["mode"] == "recover"


def test_recover_with_nothing_in_the_archive_keeps_empty_shell(
        tmp_path, monkeypatch):
    client = _fake_client({"download/ZAH": {"ZAH.zip": _zip_bytes()}})
    assert _run_main(tmp_path, monkeypatch, client, "--recover") == 0
    assert _fetched_remotes(client) == []
    d = _run_dirs(tmp_path)[0]
    meta = _meta(d)
    assert meta["status"] == "empty"
    assert meta["mode"] == "recover"


# ============================================================
# Fleet-uniform flags: parsed everywhere, informative here
# ============================================================

def test_debug_parses_and_defaults_off(monkeypatch):
    monkeypatch.setattr(sys, "argv", ["download.py", "--client-id", "CH000000"])
    assert download.parse_args().debug is False
    monkeypatch.setattr(sys, "argv",
                        ["download.py", "--client-id", "CH000000", "--debug"])
    assert download.parse_args().debug is True


def test_debug_changes_nothing(tmp_path, monkeypatch):
    # Every run records its listing unconditionally, so --debug has
    # nothing left to gate: same artefacts with and without it, and no
    # screenshots/ capture dir.
    files = {"download/ZAH": {"ZAH.zip": _zip_bytes()}}
    assert _run_main(tmp_path, monkeypatch, _fake_client(files),
                     "--debug") == 0
    d = _run_dirs(tmp_path)[0]
    assert not (d / "screenshots").exists()
    assert (d / "listing.json").exists()
    assert _meta(d)["status"] == "complete"


def test_lookback_flag_accepted_on_normal_pull(tmp_path, monkeypatch):
    # A normal SFTP pull takes whatever UBS has queued; --lookback cannot
    # narrow it, so it is accepted for fleet uniformity and only logs a
    # note — the pull still runs in full.
    client = _fake_client({"download/ZAH": {"ZAH.zip": _zip_bytes()}})
    assert _run_main(tmp_path, monkeypatch, client, "--lookback", "4w") == 0
    d = _run_dirs(tmp_path)[0]
    assert _meta(d)["status"] == "complete"
    assert _fetched_remotes(client) == ["download/ZAH/ZAH.zip"]
