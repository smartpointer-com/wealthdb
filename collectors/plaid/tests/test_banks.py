"""`banks`: the linked banks, from the files on this computer alone."""
from __future__ import annotations

import banks
import load
from conftest import access_token, leave_pending, store
from test_load import write_run


def run(secrets, data, *argv):
    return banks.main(["--secrets-dir", str(secrets), "--bronze-dir",
                       str(data), *argv])


def test_nothing_linked_names_the_first_link(secrets, tmp_path, capsys):
    assert run(secrets, tmp_path / "plaid", "--sandbox") == 0
    out = capsys.readouterr().out
    assert "No bank is linked through Plaid yet (sandbox)" in out
    assert "    wealthdb-collect plaid link --item <name> --sandbox\n" in out


def test_each_bank_and_where_its_data_stands(secrets, tmp_path, capsys,
                                             plaid):
    clock = plaid.clock
    data = tmp_path / "plaid"
    store(secrets, "chase", n=1)
    store(secrets, "schwab", n=2)
    store(secrets, "other", n=3)
    store(secrets, "prod", environment="production", n=4)
    # chase: two runs, both loaded. schwab: one run, not loaded, and one
    # still in progress. other: nothing downloaded.
    write_run(data / "chase", "20260110T050000Z")
    write_run(data / "chase", "20260120T050000Z")
    assert load.main(["--bronze-dir", str(data), "--item", "chase"]) == 0
    write_run(data / "schwab", "20260115T050000Z", item_id="item-synthetic-2")
    write_run(data / "schwab", "20260121T050000Z", item_id="item-synthetic-2",
              status="in-progress")
    leave_pending(secrets, clock, "amex")

    assert run(secrets, data, "--sandbox") == 0

    out = capsys.readouterr().out
    lines = out.splitlines()
    assert "Banks linked through Plaid (sandbox):" in lines
    assert ("  chase   Synthetic Bank  linked 2026-01-02  "
            "downloaded 2026-01-20, loaded") in lines
    assert ("  schwab  Synthetic Bank  linked 2026-01-02  "
            "downloaded 2026-01-15, not loaded yet") in lines
    assert ("  other   Synthetic Bank  linked 2026-01-02  "
            "not downloaded yet") in lines
    assert "prod" not in out
    assert ("A sign-in for amex is left open, started 2026-01-02. "
            "`wealthdb-collect plaid login --sandbox` settles it.") in lines
    assert "as in --item chase." in out
    assert "    wealthdb-collect plaid login --check --sandbox" in lines
    for n in range(1, 5):
        assert access_token(n=n) not in out


def test_the_listing_writes_nothing(secrets, tmp_path):
    data = tmp_path / "plaid"
    store(secrets, "chase")
    write_run(data / "chase", "20260110T050000Z")
    before = sorted(p.relative_to(tmp_path) for p in tmp_path.rglob("*"))
    assert run(secrets, data, "--sandbox") == 0
    assert sorted(p.relative_to(tmp_path) for p in tmp_path.rglob("*")) == before
