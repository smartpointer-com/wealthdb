"""Tests for `download.py --check`, the credential probe behind `fred
login --check`.

FRED mints no session — the API key is the credential — so the fleet's
"probe the stored session without minting a new one" contract becomes the
cheapest possible authenticated read. The network call is mocked out: these
assert the probe's verdict mapping, not FRED's behaviour. A key that is
merely *present* must not pass; only one FRED accepts does.
"""
from __future__ import annotations

import sys
import urllib.error
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


def _http_error(code: int) -> urllib.error.HTTPError:
    return urllib.error.HTTPError(
        url="https://api.stlouisfed.org/fred/series/observations",
        code=code, msg="synthetic", hdrs=None, fp=None,
    )


def test_check_parses_and_needs_no_bronze_dir():
    # A probe writes nothing, so --bronze-dir (required for a real download)
    # must not be required for it.
    args = download.parse_args(["--check"])
    assert args.check is True
    assert args.bronze_dir is None


def test_bare_download_still_requires_bronze_dir():
    # The probe's relaxation of --bronze-dir must not leak into the download
    # path.
    with pytest.raises(SystemExit):
        download.main(["--api-key", "SYNTHETIC"])


def test_accepted_key_returns_zero(monkeypatch):
    seen = {}

    def fake_fetch(base_url, series_id, api_key, start, end, timeout=90):
        seen.update(series=series_id, key=api_key, days=(end - start).days)
        return {"observations": [{"date": "2026-01-02", "value": "1.05"}]}

    monkeypatch.setattr(download, "fetch_observations", fake_fetch)
    assert download._check_credential("https://example.invalid", "SYNTHETIC") == 0
    # Probes one real series over a short window — never a full backfill.
    assert seen["series"] in download.FX_SERIES
    assert seen["key"] == "SYNTHETIC"
    assert seen["days"] <= 31


def test_rejected_key_returns_nonzero(monkeypatch):
    # FRED answers a bad key with 400 + an error_message body. This is the
    # case a presence-only check would miss.
    for code in (400, 403):
        monkeypatch.setattr(download, "fetch_observations",
                            lambda *a, code=code, **k: (_ for _ in ()).throw(_http_error(code)))
        assert download._check_credential("https://example.invalid", "BAD") == 1


def test_other_http_error_returns_nonzero(monkeypatch):
    monkeypatch.setattr(download, "fetch_observations",
                        lambda *a, **k: (_ for _ in ()).throw(_http_error(503)))
    assert download._check_credential("https://example.invalid", "SYNTHETIC") == 1


def test_unreachable_endpoint_returns_nonzero(monkeypatch):
    monkeypatch.setattr(
        download, "fetch_observations",
        lambda *a, **k: (_ for _ in ()).throw(urllib.error.URLError("no route")))
    assert download._check_credential("https://example.invalid", "SYNTHETIC") == 1


def test_unexpected_body_returns_nonzero(monkeypatch):
    # A 200 that isn't an observations document is not a pass.
    monkeypatch.setattr(download, "fetch_observations",
                        lambda *a, **k: {"error_message": "Bad Request."})
    assert download._check_credential("https://example.invalid", "SYNTHETIC") == 1


def test_main_check_probes_and_writes_nothing(monkeypatch, tmp_path):
    calls = []
    monkeypatch.setattr(download, "fetch_observations",
                        lambda *a, **k: calls.append(a) or {"observations": []})
    rc = download.main(["--check", "--api-key", "SYNTHETIC"])
    assert rc == 0
    assert len(calls) == 1          # exactly one probe request
    assert not list(tmp_path.iterdir())


# --- --env-file (credentials for a direct download.py run) ------------------

def test_env_file_is_sourced_before_the_credential_resolves(tmp_path,
                                                            monkeypatch):
    # The wrapper already sources <secrets>/fred.env; --env-file is what makes
    # a direct `download.py` run work, so it must land before resolution.
    monkeypatch.delenv("FRED_API_KEY", raising=False)
    monkeypatch.delenv("FRED_ENV_FILE", raising=False)
    env = tmp_path / "fred.env"
    env.write_text("FRED_API_KEY=FROM_FILE\n")
    seen = {}
    monkeypatch.setattr(download, "fetch_observations",
                        lambda base, sid, key, s, e, timeout=90:
                        seen.update(key=key) or {"observations": []})
    assert download.main(["--check", "--env-file", str(env)]) == 0
    assert seen["key"] == "FROM_FILE"


def test_env_file_env_var_is_honoured(tmp_path, monkeypatch):
    monkeypatch.delenv("FRED_API_KEY", raising=False)
    env = tmp_path / "fred.env"
    env.write_text("FRED_API_KEY=FROM_ENV_VAR_FILE\n")
    monkeypatch.setenv("FRED_ENV_FILE", str(env))
    seen = {}
    monkeypatch.setattr(download, "fetch_observations",
                        lambda base, sid, key, s, e, timeout=90:
                        seen.update(key=key) or {"observations": []})
    assert download.main(["--check"]) == 0
    assert seen["key"] == "FROM_ENV_VAR_FILE"


def test_missing_env_file_is_an_error(tmp_path, monkeypatch):
    # A file that was asked for but isn't there must fail loudly rather than
    # falling back to whatever the ambient environment happens to hold.
    monkeypatch.setenv("FRED_API_KEY", "AMBIENT")
    with pytest.raises(SystemExit):
        download.main(["--check", "--env-file", str(tmp_path / "nope.env")])
