"""load: bronze runs into silver, idempotent, a later run winning."""

import sqlite3
from datetime import date

import pytest

import download
import load
from collectorkit import bronze
from providers.base import Context
from tests.helpers import FakeProvider


@pytest.fixture
def fake(monkeypatch):
    p = FakeProvider()
    monkeypatch.setitem(download.REGISTRY, "fake", p)
    monkeypatch.setitem(load.REGISTRY, "fake", p)
    return p


def make_run(data, slug, reqs):
    run = data / slug
    run.mkdir(parents=True)
    entries = download.execute(reqs, run, Context(client=None))
    bronze.atomic_write_json(run / "run.json", {"status": "complete", "requests": entries})
    return run


def rows(db, q):
    con = sqlite3.connect(db)
    try:
        return con.execute(q).fetchall()
    finally:
        con.close()


def test_load_twice_is_a_no_op_the_second_time(tmp_path, fake):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    make_run(data, "20260715T010000Z", [download.Request("fund", "fake", "symbol:FUNDA", date(2026, 5, 28),
                                                         {"ref": "symbol:FUNDA"})])
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    first = rows(db, "SELECT * FROM compositions") + rows(db, "SELECT * FROM lines")
    (loaded_at,) = rows(db, "SELECT loaded_at FROM dump_runs")[0]
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    assert rows(db, "SELECT * FROM compositions") + rows(db, "SELECT * FROM lines") == first
    assert rows(db, "SELECT loaded_at FROM dump_runs") == [(loaded_at,)]
    (comp,) = rows(db, "SELECT fund_ref, as_of, provider, run_at FROM compositions")
    assert comp == ("symbol:FUNDA", 1779926400, "fake", bronze.parse_run_ts("20260715T010000Z"))


def test_a_later_run_replaces_a_re_served_day(tmp_path, fake):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    req = download.Request("fund", "fake", "symbol:FUNDA", date(2026, 5, 28), {"ref": "symbol:FUNDA"})
    make_run(data, "20260715T010000Z", [req])
    load.main(["--silver-db", str(db), "--bronze-dir", str(data)])
    run2 = make_run(data, "20260716T010000Z", [req])
    d = run2 / "funds" / "fake" / "symbol-FUNDA" / "2026-05-28"
    (d / "holdings.txt").write_text("BBB 99\n")
    load.main(["--silver-db", str(db), "--bronze-dir", str(data)])
    assert rows(db, "SELECT ticker, weight FROM lines") == [("BBB", "99")]
    first, second = [r[0] for r in rows(db, "SELECT loaded_at FROM dump_runs ORDER BY snapshot_at")]
    assert second > first


def test_a_request_that_does_not_parse_is_skipped(tmp_path, fake):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    reqs = [download.Request("fund", "fake", f"symbol:{s}", date(2026, 5, 28), {"ref": f"symbol:{s}"})
            for s in ("GOOD", "BAD")]
    run = make_run(data, "20260715T010000Z", reqs)
    (run / "funds" / "fake" / "symbol-BAD" / "2026-05-28" / "holdings.txt").write_text("garbled\n")
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    assert rows(db, "SELECT fund_ref FROM compositions") == [("symbol:GOOD",)]


def test_an_incomplete_run_is_not_loaded(tmp_path, fake):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    run = make_run(data, "20260715T010000Z", [download.Request("fund", "fake", "symbol:FUNDA", None,
                                                               {"ref": "symbol:FUNDA"})])
    bronze.atomic_write_json(run / "run.json", {"status": "in-progress", "requests": []})
    load.main(["--silver-db", str(db), "--bronze-dir", str(data)])
    assert rows(db, "SELECT COUNT(*) FROM dump_runs") == [(0,)]


def test_force_rebuilds_with_a_new_load_clock(tmp_path, fake):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    make_run(data, "20260715T010000Z", [download.Request("fund", "fake", "symbol:FUNDA", date(2026, 5, 28),
                                                         {"ref": "symbol:FUNDA"})])
    load.main(["--silver-db", str(db), "--bronze-dir", str(data)])
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data), "--force"]) == 0
    assert rows(db, "SELECT COUNT(*) FROM compositions") == [(1,)]


def test_inbox_csv_end_to_end(tmp_path):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    day = data / "inbox" / "isin_XX0000000201" / "2026-06-30"
    day.mkdir(parents=True)
    (day / "holdings.csv").write_text("isin,name,weight,country,currency,sector\n"
                                      "XX0000000202,Example Corp,70,US,USD,Technology\n"
                                      "XX0000000203,Exemplar plc,29.5,GB,GBP,Energy\n")
    reqs = [download.Request("fund", "inbox", "isin:XX0000000201", None,
                             {"ref": "isin:XX0000000201", "folder": str(data / "inbox" / "isin_XX0000000201")})]
    run = data / "20260715T010000Z"
    run.mkdir(parents=True)
    entries = download.execute(reqs, run, Context(client=None, data_dir=data))
    assert entries[0]["status"] == "ok" and entries[0]["as_of"] == "2026-06-30"
    bronze.atomic_write_json(run / "run.json", {"status": "complete", "requests": entries})
    load.main(["--silver-db", str(db), "--bronze-dir", str(data)])
    assert rows(db, "SELECT fund_ref, provider, source_kind, line_count, weight_sum FROM compositions") == \
        [("isin:XX0000000201", "inbox", "inbox", 2, "99.5")]
    assert rows(db, "SELECT isin, sector, weight FROM lines ORDER BY line_no") == \
        [("XX0000000202", "Technology", "70"), ("XX0000000203", "Energy", "29.5")]
