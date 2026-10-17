"""Tests for what chase's prune configuration decides: the `--debug`
`/svc/` bodies under `raw/svc/` go from a complete dump, while the roster
bodies beside them and every load input stay. The shared engine's own
envelope is tested in collectorkit.

Synthetic files only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402

SLUG = "20260101T010000Z"


def test_debug_svc_bodies_go_and_the_load_inputs_stay(tmp_path):
    run = tmp_path / SLUG
    (run / "raw" / "svc").mkdir(parents=True)
    (run / "transactions").mkdir()
    (run / "statements" / "900001").mkdir(parents=True)
    (run / "run.json").write_text(json.dumps({"status": "complete"}))
    (run / "accounts.json").write_text("[]")
    (run / "raw" / "account-01.json").write_text("{}")
    (run / "raw" / "svc" / "001-example.json").write_text("{}")
    (run / "transactions" / "900001.csv").write_text("synthetic\n")
    (run / "statements" / "900001" / "2026-01-01.pdf").write_bytes(
        b"%PDF-1.4 synthetic")

    assert prune.main(["--bronze-dir", str(tmp_path)]) == 0

    assert not (run / "raw" / "svc" / "001-example.json").exists()
    for kept in ("run.json", "accounts.json", "raw/account-01.json",
                 "transactions/900001.csv",
                 "statements/900001/2026-01-01.pdf"):
        assert (run / kept).exists(), kept
