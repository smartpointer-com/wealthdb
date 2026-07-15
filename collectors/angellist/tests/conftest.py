"""Keep the tests off the real bronze tree.

`--documents-dir` defaults to `/data/angellist-documents` — an absolute path
that does NOT derive from `--bronze-dir`. The test container bind-mounts the
host's real bronze tree at /data, so a `load.main([...])` that scopes
`--bronze-dir` and `--silver-db` to tmp_path still reads whatever documents
are on the host, and ingests live data into the test's silver.

That is wrong on its own terms, and it also flaked: `tax_documents` rows
carry `retrieved_at` stamped `strftime('%s','now')` by the INSERT rather
than by a column default, so `_dump_silver`'s drop-the-now-defaults filter
keeps the column, and two loads of live documents disagree whenever they
land either side of a second boundary.

Redirecting the module default makes the isolation structural — a new test
cannot reintroduce the hole by forgetting a flag. Tests that exercise the
document loader pass an explicit `--documents-dir` and are unaffected.

(angellist is the only collector needing this: every other load dir either
derives from `--bronze-dir` or is passed explicitly.)
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402


@pytest.fixture(autouse=True)
def _documents_dir_never_defaults_to_real_data(tmp_path_factory, monkeypatch):
    empty = tmp_path_factory.mktemp("no-documents")
    monkeypatch.setattr(load, "DEFAULT_DOCS", empty)
