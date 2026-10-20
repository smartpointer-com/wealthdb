"""load: bronze runs into silver, idempotent, a later run winning."""

import logging
import sqlite3
from datetime import date

import download
import load
from collectorkit import bronze
from providers.base import Context
from tests.helpers import make_run


def request(symbol, day=date(2026, 5, 28)):
    return download.Request("fund", "fake", f"symbol:{symbol}", day, {"ref": f"symbol:{symbol}"})


def rows(db, q):
    con = sqlite3.connect(db)
    try:
        return con.execute(q).fetchall()
    finally:
        con.close()


def run_load(data, db, *extra):
    return load.main(["--silver-db", str(db), "--bronze-dir", str(data), *extra])


def test_load_twice_is_a_no_op_the_second_time(tmp_path, fake):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    make_run(data, "20260715T010000Z", [request("FUNDA")])
    assert run_load(data, db) == 0
    first = rows(db, "SELECT * FROM compositions") + rows(db, "SELECT * FROM lines")
    (loaded_at,) = rows(db, "SELECT loaded_at FROM dump_runs")[0]
    assert run_load(data, db) == 0
    assert rows(db, "SELECT * FROM compositions") + rows(db, "SELECT * FROM lines") == first
    assert rows(db, "SELECT loaded_at FROM dump_runs") == [(loaded_at,)]
    (comp,) = rows(db, "SELECT fund_ref, as_of, provider, run_at FROM compositions")
    assert comp == ("symbol:FUNDA", 1779926400, "fake", bronze.parse_run_ts("20260715T010000Z"))


def test_a_later_run_replaces_a_re_served_day(tmp_path, fake):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    make_run(data, "20260715T010000Z", [request("FUNDA")])
    run_load(data, db)
    run2 = make_run(data, "20260716T010000Z", [request("FUNDA")])
    (run2 / "funds" / "fake" / "symbol_FUNDA" / "2026-05-28" / "holdings.txt").write_text("BBB 99\n")
    run_load(data, db)
    assert rows(db, "SELECT ticker, weight FROM lines") == [("BBB", "99")]
    first, second = [r[0] for r in rows(db, "SELECT loaded_at FROM dump_runs ORDER BY snapshot_at")]
    assert second > first


def test_a_request_that_does_not_parse_is_skipped(tmp_path, fake):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    run = make_run(data, "20260715T010000Z", [request("GOOD"), request("BAD")])
    (run / "funds" / "fake" / "symbol_BAD" / "2026-05-28" / "holdings.txt").write_text("garbled\n")
    assert run_load(data, db) == 0
    assert rows(db, "SELECT fund_ref FROM compositions") == [("symbol:GOOD",)]


def test_any_error_skips_its_request_and_holds_back_no_later_run(tmp_path, fake, monkeypatch):
    # A truncated PDF or .zst raises what no parser expects; the run, and
    # the runs after it, load all the same.
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    parse = fake.parse

    def explode(indir):
        if "symbol_BAD" in str(indir):
            raise RuntimeError("truncated")
        return parse(indir)
    monkeypatch.setattr(fake, "parse", explode)
    make_run(data, "20260715T010000Z", [request("BAD"), request("GOOD")])
    make_run(data, "20260716T010000Z", [request("NEXT")])
    assert run_load(data, db) == 0
    assert rows(db, "SELECT fund_ref FROM compositions ORDER BY fund_ref") == [("symbol:GOOD",), ("symbol:NEXT",)]
    assert rows(db, "SELECT COUNT(*) FROM dump_runs") == [(2,)]


def test_a_request_that_fails_to_write_leaves_the_day_as_it_was(tmp_path, fake, monkeypatch):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    make_run(data, "20260715T010000Z", [request("FUNDA")])
    run_load(data, db)
    parse = fake.parse

    def no_provider(indir):
        # The day's rows are cleared, then a row breaks the schema.
        out = parse(indir)
        out.compositions[0]["provider"] = None
        return out
    monkeypatch.setattr(fake, "parse", no_provider)
    make_run(data, "20260716T010000Z", [request("FUNDA")])
    assert run_load(data, db) == 0
    assert rows(db, "SELECT provider FROM compositions") == [("fake",)]
    assert rows(db, "SELECT COUNT(*) FROM lines") == [(1,)]


def test_an_incomplete_run_is_not_loaded(tmp_path, fake):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    run = make_run(data, "20260715T010000Z", [request("FUNDA")])
    bronze.atomic_write_json(run / "run.json", {"status": "in-progress", "requests": []})
    run_load(data, db)
    assert rows(db, "SELECT COUNT(*) FROM dump_runs") == [(0,)]


def test_a_run_whose_provider_is_gone_loads_without_its_requests(tmp_path, fake, monkeypatch, caplog):
    # A provider renamed or removed while kept bronze still names it.
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    make_run(data, "20260715T010000Z", [request("FUNDA")])
    monkeypatch.delitem(load.REGISTRY, "fake")
    caplog.set_level(logging.WARNING, logger="refdata.load")
    assert run_load(data, db) == 0
    assert rows(db, "SELECT COUNT(*) FROM dump_runs") == [(1,)]
    assert rows(db, "SELECT COUNT(*) FROM compositions") + rows(db, "SELECT COUNT(*) FROM lines") == [(0,), (0,)]
    assert "no provider fake here; skipping symbol:FUNDA" in caplog.text


def test_force_rebuilds_with_a_new_load_clock(tmp_path, fake):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    make_run(data, "20260715T010000Z", [request("FUNDA")])
    run_load(data, db)
    assert run_load(data, db, "--force") == 0
    assert rows(db, "SELECT COUNT(*) FROM compositions") == [(1,)]


def test_inbox_csv_end_to_end(tmp_path):
    data, db = tmp_path / "data", tmp_path / "refdata.db"
    day = data / "inbox" / "isin_XX0000000201" / "2026-06-30"
    day.mkdir(parents=True)
    (day / "holdings.csv").write_text("isin,name,weight,country,currency,sector\n"
                                      "XX0000000202,Example Corp,70,US,USD,Technology\n"
                                      "XX0000000203,Exemplar plc,29.5,GB,GBP,Energy\n")
    reqs = [download.Request("fund", "inbox", "isin:XX0000000201", date(2026, 6, 30),
                             {"ref": "isin:XX0000000201", "folder": str(data / "inbox" / "isin_XX0000000201")})]
    run = data / "20260715T010000Z"
    run.mkdir(parents=True)
    entries = download.execute(reqs, run, Context(client=None, data_dir=data))
    assert entries[0]["status"] == "ok" and entries[0]["as_of"] == "2026-06-30"
    bronze.atomic_write_json(run / "run.json", {"status": "complete", "requests": entries})
    run_load(data, db)
    assert rows(db, "SELECT fund_ref, provider, source_kind, line_count, weight_sum FROM compositions") == \
        [("isin:XX0000000201", "inbox", "inbox", 2, "99.5")]
    assert rows(db, "SELECT isin, sector, weight FROM lines ORDER BY line_no") == \
        [("XX0000000202", "Technology", "70"), ("XX0000000203", "Energy", "29.5")]
