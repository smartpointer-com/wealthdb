"""download: the plan a wanted list gives, and the bronze a run writes."""

import json
from datetime import date

import pytest

import download
import wanted as wantedmod
from providers.base import Context
from tests.helpers import FakeProvider, fund, wanted_doc, write_wanted


@pytest.fixture
def fake(monkeypatch):
    p = FakeProvider()
    monkeypatch.setitem(download.REGISTRY, "fake", p)
    return p


def ctx():
    return Context(client=None)


def test_plan_fetches_a_stale_current_composition_and_skips_a_fresh_one(fake):
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-05-29"]),
                                       fund("symbol:FRESH", "FUNDA", compositions=["2026-07-10"]),
                                       fund("symbol:NOPE", "NOPE")]))
    reqs, unclaimed = download.plan(want, [fake], ctx(), today=date(2026, 7, 15), max_age=30, backfill=False)
    assert [(r.ref, r.as_of) for r in reqs] == [("symbol:FUNDA", None)]
    assert unclaimed == ["symbol:NOPE"]


def test_plan_backfills_the_held_window_around_what_gold_has(fake):
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", held_from="2026-02-01",
                                            held_to="2026-05-31", compositions=["2026-03-31"])]))
    reqs, _ = download.plan(want, [fake], ctx(), today=date(2026, 7, 15), max_age=30, backfill=True)
    # Sold in May: no current composition. January is the composition in
    # force on the first day held; February, April and May are gaps, March
    # is covered by the composition gold has.
    assert [r.as_of for r in reqs] == [date(2026, 1, 28), date(2026, 2, 28), date(2026, 4, 28), date(2026, 5, 28)]


def test_plan_wants_the_current_composition_of_a_proxy_or_reference_fund(fake):
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", held_to="2026-01-31", roles=["proxy"])]))
    reqs, _ = download.plan(want, [fake], ctx(), today=date(2026, 7, 15), max_age=30, backfill=False)
    assert [r.as_of for r in reqs] == [None]


def test_a_run_writes_bronze_and_a_manifest(tmp_path, fake, monkeypatch):
    monkeypatch.setattr(download.config, "load", lambda known: download.config.Config(providers=("fake",)))
    w = write_wanted(tmp_path / "wanted.json", wanted_doc([
        fund("symbol:FUNDA", "FUNDA", held_from="2026-05-01", compositions=[]),
        fund("symbol:NOPE", "NOPE")]))
    rc = download.main(["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w), "--backfill", "--limit", "2"])
    assert rc == 0
    (run,) = [p for p in (tmp_path / "data").iterdir()]
    manifest = json.loads((run / "run.json").read_text())
    assert manifest["status"] == "complete" and manifest["unclaimed"] == ["symbol:NOPE"]
    statuses = [(r["asked"], r["status"]) for r in manifest["requests"]]
    # The current one and April, in force on the first day held; May and
    # June wait for the next run.
    assert statuses == [("current", "ok"), ("2026-04-28", "ok"), ("2026-05-28", "deferred"),
                        ("2026-06-28", "deferred")]
    assert (run / manifest["requests"][0]["dir"] / "holdings.txt").is_file()


def test_dry_run_writes_nothing(tmp_path, fake, monkeypatch, capsys):
    monkeypatch.setattr(download.config, "load", lambda known: download.config.Config(providers=("fake",)))
    w = write_wanted(tmp_path / "wanted.json", wanted_doc([fund("symbol:FUNDA", "FUNDA")]))
    assert download.main(["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w), "--dry-run"]) == 0
    assert not (tmp_path / "data").exists()
    assert "symbol:FUNDA\tcurrent" in capsys.readouterr().out


def test_an_sec_provider_needs_a_contact(caplog):
    cfg = download.config.Config(providers=("inbox", "sec-nport"), contact="")
    names = [p.name for p in download.enabled_providers(cfg, None)]
    assert names == ["inbox"]
    assert "contact" in caplog.text


def test_a_failed_request_does_not_fail_the_run(tmp_path, fake, monkeypatch):
    def boom(*a, **k):
        raise RuntimeError("parser exploded")
    monkeypatch.setattr(fake, "fetch", boom)
    reqs = [download.Request("fund", "fake", "symbol:FUNDA", None, {"ref": "symbol:FUNDA"})]
    (entry,) = download.execute(reqs, tmp_path, ctx())
    assert entry["status"] == "error" and "parser exploded" in entry["error"]


def test_plan_skips_a_stale_fund_whose_newest_composition_is_had(fake):
    # A quarterly filer: gold's newest is old, but it is the newest there is.
    fake.latest_ = date(2026, 3, 31)
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-03-31"])]))
    reqs, _ = download.plan(want, [fake], ctx(), today=date(2026, 7, 15), max_age=30, backfill=False)
    assert reqs == []
    fake.latest_ = date(2026, 6, 30)
    reqs, _ = download.plan(want, [fake], ctx(), today=date(2026, 7, 15), max_age=30, backfill=False)
    assert [r.as_of for r in reqs] == [None]


def test_a_second_run_before_the_next_load_fetches_nothing(tmp_path, fake, monkeypatch):
    monkeypatch.setattr(download.config, "load", lambda known: download.config.Config(providers=("fake",)))
    w = write_wanted(tmp_path / "wanted.json", wanted_doc([fund("symbol:FUNDA", "FUNDA", held_from="2026-05-01")]))
    monkeypatch.setattr(download, "today_utc", lambda: date(2026, 7, 15))
    args = ["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w), "--backfill"]
    assert download.main(args) == 0
    first = len(fake.fetched)
    assert first > 0
    monkeypatch.setattr(download.bronze, "ts_slug", lambda: "20990101T000000Z")
    assert download.main(args) == 0
    assert len(fake.fetched) == first


def test_the_inbox_serves_beside_the_provider_that_claims(tmp_path, fake):
    from providers.inbox import Inbox
    (tmp_path / "inbox" / "symbol_FUNDA" / "2026-03-15").mkdir(parents=True)
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-07-10"])]))
    c = Context(client=None, data_dir=tmp_path)
    reqs, unclaimed = download.plan(want, [Inbox(), fake], c, today=date(2026, 7, 15), max_age=30, backfill=False)
    assert [(r.provider, r.as_of) for r in reqs] == [("inbox", date(2026, 3, 15))]
    assert unclaimed == []
