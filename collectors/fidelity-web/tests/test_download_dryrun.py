"""
Regression tests for download.walk() persistence behaviour.

The invariant under test (root AGENTS.md §2 "export nothing"):
``download --dry-run`` must persist NOTHING under the bronze dest —
no run dir, no ``run.json``. A leftover run.json, even one carrying
only master ``account_dimensions``, is a dump that ``load`` would
ingest into silver (fidelity-web's ``load._load_master`` reads
account_dimensions with no dry-run guard), so a dry-run must not
create one at all.

These tests drive ``walk()`` with a fake Playwright page (mirroring
how test_load.py mocks the load surface) so no live session or
Camoufox import is needed. The dry-run test asserts the bronze dest
stays empty; the real-run test (scrapes stubbed) asserts a run dir
IS written — proving the dry-run assertion is not vacuously true.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402

# Placeholder account ids (9-digit → in scope). No real Fidelity
# account numbers in tests.
ACCT_A = "100000001"
ACCT_B = "200000002"

_DIMS = [
    {"account_id": ACCT_A, "portfolio": "Portfolio A", "nickname": "Nick A"},
    {"account_id": ACCT_B, "portfolio": "Portfolio B", "nickname": "Nick B"},
]


class FakePage:
    """Minimal stand-in for the Playwright page walk() drives.

    ``walk()`` only touches the page via ``live_url()`` (a
    ``location.href`` evaluate) and ``enumerate_account_dimensions()``
    (the account-selector DOM probe evaluate). We route on the JS
    source: the href probe returns a post-auth URL so walk() does not
    try to navigate; anything else returns the synthetic dimensions.
    """

    url = download.URL_PORTFOLIO_SUMMARY

    def evaluate(self, js, *args):
        if "location.href" in js:
            return self.url
        return list(_DIMS)


def test_dry_run_persists_nothing_to_bronze(tmp_path):
    dest = tmp_path / "bronze"
    dest.mkdir()

    download.walk(
        context=None,
        page=FakePage(),
        config={"dest": str(dest), "mode": "all", "dry_run": "true"},
    )

    # No run dir, no run.json, no file of any kind under the dest.
    leftovers = list(dest.rglob("*"))
    assert leftovers == [], (
        "dry-run must persist nothing under bronze; found: "
        f"{[str(p) for p in leftovers]}"
    )


def test_real_run_writes_run_dir(tmp_path, monkeypatch):
    # Stub the per-phase scrapes so a non-dry-run walk finalises
    # without a live session. This makes the dry-run test above
    # non-vacuous: the same code path DOES write when not dry-run.
    for name in (
        "scrape_positions", "scrape_activity", "scrape_documents",
        "scrape_balances", "scrape_performance",
    ):
        monkeypatch.setattr(
            download, name, lambda *a, **k: {"status": "stubbed"}
        )

    dest = tmp_path / "bronze"
    dest.mkdir()

    download.walk(
        context=None,
        page=FakePage(),
        config={"dest": str(dest), "mode": "all", "dry_run": "false"},
    )

    run_dirs = [p for p in dest.iterdir() if p.is_dir()]
    assert len(run_dirs) == 1
    run_json = run_dirs[0] / "run.json"
    assert run_json.exists()
    meta = json.loads(run_json.read_text())
    assert meta["status"] == "complete"
    assert meta["account_dimensions"]  # master data present on a real run
