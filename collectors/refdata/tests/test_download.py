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


def test_plan_does_not_fetch_again_what_a_provider_served_within_max_age(fake):
    # A report states a day long past: the composition stays old however
    # recently it was fetched.
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-01-31"])]))
    served = {(fake.name, "symbol:FUNDA", date(2026, 1, 31)): date(2026, 7, 1)}
    reqs, _ = download.plan(want, [fake], ctx(), today=date(2026, 7, 15), max_age=30, backfill=False,
                            fetched=served)
    assert reqs == []
    reqs, _ = download.plan(want, [fake], ctx(), today=date(2026, 8, 15), max_age=30, backfill=False,
                            fetched=served)
    assert [(r.ref, r.as_of) for r in reqs] == [("symbol:FUNDA", None)]


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


class _Failing(FakeProvider):
    """Claims what FakeProvider claims, and fails every fetch."""
    name = "failing"

    def fetch(self, claim, as_of, outdir, ctx):
        from providers.base import ProviderError
        raise ProviderError("the issuer's file is gone")


def test_a_failed_current_fetch_falls_through_to_the_next_claimant(tmp_path, fake, monkeypatch):
    failing = _Failing()
    monkeypatch.setitem(download.REGISTRY, "failing", failing)
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA")]))
    reqs, _ = download.plan(want, [failing, fake], ctx(), today=date(2026, 7, 15), max_age=30, backfill=False)
    (r,) = reqs
    assert r.provider == "failing" and [name for name, _ in r.fallbacks] == ["fake"]
    (entry,) = download.execute(reqs, tmp_path, ctx())
    assert (entry["status"], entry["provider"]) == ("ok", "fake")
    assert entry["tried"][0]["provider"] == "failing"


def test_a_product_list_that_does_not_parse_is_no_claim(fake):
    class Garbled(FakeProvider):
        name = "garbled"

        def claim(self, f, c):
            raise ValueError("Expecting value: line 1 column 1")
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA")]))
    reqs, unclaimed = download.plan(want, [Garbled(), fake], ctx(), today=date(2026, 7, 15), max_age=30,
                                    backfill=False)
    assert [r.provider for r in reqs] == ["fake"] and unclaimed == []


def test_securities_held_directly_come_first_then_constituents_by_share():
    doc = wanted_doc(securities=[
        {"key": "isin:XX0000000071", "scope": "constituent", "share_pct": 0.5, "missing": ["sector"], "lookups": {}},
        {"key": "isin:XX0000000072", "scope": "direct", "share_pct": None, "missing": ["sector"], "lookups": {}},
        {"key": "isin:XX0000000073", "scope": "constituent", "share_pct": 2.5, "missing": ["country"], "lookups": {}},
        {"key": "isin:XX0000000074", "scope": "unheard-of", "missing": [], "lookups": {}}])
    want = wantedmod.parse(doc)
    assert [s.key for s in download.security_order(want.securities, True)] == [
        "isin:XX0000000072", "isin:XX0000000073", "isin:XX0000000071"]
    assert [s.key for s in download.security_order(want.securities, False)] == ["isin:XX0000000072"]
    assert want.securities[2].share_pct == 2.5



class _FakeLookup:
    """A security provider that answers a country and claims everything."""
    name = "lookup"
    serves_funds = False
    serves_securities = True
    additive = False
    answers = frozenset({"country"})

    def claim_security(self, sec, ctx):
        return {"key": sec.key}

    def wants_constituent(self, sec):
        return bool(self.answers & set(sec.missing))


def test_plan_asks_constituents_only_when_allowed_and_what_a_provider_answers():
    doc = wanted_doc(securities=[
        {"key": "isin:XX0000000091", "scope": "direct", "missing": ["sector"], "lookups": {}},
        {"key": "isin:XX0000000092", "scope": "constituent", "share_pct": 1.0, "missing": ["country"],
         "lookups": {}},
        {"key": "isin:XX0000000093", "scope": "constituent", "share_pct": 2.0, "missing": ["sector"],
         "lookups": {}},
        {"key": "isin:XX0000000094", "scope": "constituent", "share_pct": 3.0, "missing": ["country"],
         "lookups": {"lookup": "2026-07-10"}}])
    want = wantedmod.parse(doc)
    lk = _FakeLookup()
    keys = lambda reqs: [r.ref for r in reqs]  # noqa: E731
    off, _ = download.plan(want, [lk], ctx(), today=date(2026, 7, 15), max_age=30, backfill=False)
    assert keys(off) == ["isin:XX0000000091"]
    on, _ = download.plan(want, [lk], ctx(), today=date(2026, 7, 15), max_age=30, backfill=False,
                          constituents=True)
    # The direct one always; the constituents lacking a country, but not
    # one looked up recently, nor one lacking only what it cannot answer.
    assert keys(on) == ["isin:XX0000000091", "isin:XX0000000092"]
    # An earlier run's answer counts until gold holds it.
    again, _ = download.plan(want, [lk], ctx(), today=date(2026, 7, 15), max_age=30, backfill=False,
                             constituents=True, looked_up={("lookup", "isin:XX0000000092"): date(2026, 7, 15)})
    assert keys(again) == ["isin:XX0000000091"]


def test_earlier_runs_count_their_lookups_and_compositions(tmp_path):
    run = tmp_path / "20260714T120000Z"
    run.mkdir()
    (run / "run.json").write_text(json.dumps({"status": "complete", "requests": [
        {"kind": "security", "provider": "gleif", "ref": "isin:XX0000000095", "status": "ok"},
        {"kind": "security", "provider": "gleif", "ref": "isin:XX0000000096", "status": "error"},
        {"kind": "fund", "provider": "fake", "ref": "symbol:FUNDA", "as_of": "2026-01-31", "status": "ok"}]}))
    funds, lookups = download.fetched_before(tmp_path)
    assert lookups == {("gleif", "isin:XX0000000095"): date(2026, 7, 14)}
    assert funds == {("fake", "symbol:FUNDA", date(2026, 1, 31)): date(2026, 7, 14)}
