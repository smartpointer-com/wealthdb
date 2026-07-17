"""Tests for `download.py --check`, the credential probe behind `ubs-psn
login --check`.

PSN mints no session — the RSA key is the credential — so the fleet's "probe
the stored session without minting a new one" contract becomes a connect that
authenticates and stops. The SSH transport is mocked out; these assert the
probe's verdict mapping and, above all, that it touches no files: UBS deletes
each queue zip on a successful download, so a probe that fetched one would
destroy data.

Synthetic client id only — no network, no real UBS data.
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

import paramiko
import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


class _FakeClient:
    def __init__(self):
        self.closed = False
        self.sftp_opened = False

    def open_sftp(self):
        self.sftp_opened = True
        raise AssertionError("a probe must not open an SFTP channel")

    def close(self):
        self.closed = True


def _args(**kw) -> argparse.Namespace:
    base = dict(check=True, client_id="SYNTHETIC-ID", key=Path("/nonexistent"),
                host="sftp.example.invalid", port=26701,
                ignore_fingerprint_mismatch=False)
    base.update(kw)
    return argparse.Namespace(**base)


def test_check_flag_parses(monkeypatch):
    # parse_args() reads sys.argv directly here (no argv parameter).
    monkeypatch.setattr(sys, "argv", ["download.py", "--check"])
    assert download.parse_args().check is True
    monkeypatch.setattr(sys, "argv", ["download.py"])
    assert download.parse_args().check is False


def test_accepted_key_returns_zero_and_closes(monkeypatch):
    client = _FakeClient()
    monkeypatch.setattr(download, "connect", lambda args: client)
    assert download._check_credential(_args()) == 0
    # The session is closed rather than leaked, and no SFTP channel is opened
    # — nothing is listed, nothing is consumed.
    assert client.closed is True
    assert client.sftp_opened is False


def test_rejected_key_returns_nonzero(monkeypatch):
    def boom(args):
        raise paramiko.AuthenticationException("not authorised")
    monkeypatch.setattr(download, "connect", boom)
    assert download._check_credential(_args()) == 1


def test_unreachable_endpoint_returns_nonzero(monkeypatch):
    def boom(args):
        raise paramiko.SSHException("no route to host")
    monkeypatch.setattr(download, "connect", boom)
    assert download._check_credential(_args()) == 1

    def oserr(args):
        raise OSError("connection refused")
    monkeypatch.setattr(download, "connect", oserr)
    assert download._check_credential(_args()) == 1


def test_missing_key_file_still_exits(monkeypatch):
    # `connect` raises SystemExit for a missing/unloadable key; the probe
    # deliberately does not swallow it — the message already names the path.
    def boom(args):
        raise SystemExit("Private key not found: /nonexistent")
    monkeypatch.setattr(download, "connect", boom)
    with pytest.raises(SystemExit):
        download._check_credential(_args())


def test_probe_runs_before_the_bronze_dir_check(monkeypatch, tmp_path):
    # A probe writes nothing, so an absent/unwritable --bronze-dir must not
    # fail it.
    client = _FakeClient()
    monkeypatch.setattr(download, "connect", lambda args: client)
    monkeypatch.setattr(sys, "argv", [
        "download.py", "--check", "--client-id", "SYNTHETIC-ID",
        "--bronze-dir", str(tmp_path / "does-not-exist"),
    ])
    assert download.main() == 0
    assert client.closed is True
