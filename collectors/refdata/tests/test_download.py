"""download: the plan a wanted list gives, and the bronze a run writes."""

import json
import logging
from datetime import date

import pytest

import config
import download
import fetcher
import wanted as wantedmod
from providers.base import Context, Provider, ProviderError, Unavailable
from providers.inbox import Inbox
from tests import helpers
from tests.helpers import TODAY, FakeHttps, FakeLookup, FakeProvider, fund, wanted_doc, write_wanted


@pytest.fixture
def failing(monkeypatch):
    p = _Failing()
    monkeypatch.setitem(download.REGISTRY, "failing", p)
    return p


class _Failing(FakeProvider):
    """Claims what FakeProvider claims, and fails every fetch."""
    name = "failing"

    def fetch(self, claim, as_of, outdir, ctx):
        raise ProviderError("the issuer's file is gone")


def ctx():
    return Context(client=None)


def plan(want, providers, **kw):
    """The requests and the unclaimed funds of a plan."""
    return helpers.plan(want, providers, **kw)[:2]


def configured(monkeypatch, **settings):
    monkeypatch.setattr(download.config, "load", lambda known: config.Config(**settings))


def test_plan_fetches_a_stale_current_composition_and_skips_a_fresh_one(fake):
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-05-29"]),
                                       fund("symbol:FRESH", "FUNDA", compositions=["2026-07-10"]),
                                       fund("symbol:NOPE", "NOPE")]))
    reqs, unclaimed = plan(want, [fake])
    assert [(r.ref, r.as_of) for r in reqs] == [("symbol:FUNDA", None)]
    assert unclaimed == ["symbol:NOPE"]


def test_a_fund_that_wants_no_composition_asks_no_provider(fake):
    # Fresh, or sold with no backfill: no claim, so no request to find it.
    want = wantedmod.parse(wanted_doc([fund("symbol:FRESH", "NOPE", compositions=["2026-07-10"]),
                                       fund("symbol:SOLD", "NOPE", held_to="2026-03-31")]))
    reqs, unclaimed = plan(want, [fake])
    assert (reqs, unclaimed, fake.claimed) == ([], [], [])
    # A backfill wants the past of both.
    plan(want, [fake], backfill=True)
    assert fake.claimed == ["symbol:FRESH", "symbol:SOLD"]


def test_plan_does_not_fetch_again_what_a_provider_served_within_max_age(fake):
    # A report states a day long past: the composition stays old however
    # recently it was fetched.
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-01-31"])]))
    served = {(fake.name, "symbol:FUNDA", date(2026, 1, 31)): date(2026, 7, 1)}
    assert plan(want, [fake], earlier=download.Fetched.of(funds=served))[0] == []
    reqs, _ = plan(want, [fake], today=date(2026, 8, 15), earlier=download.Fetched.of(funds=served))
    assert [(r.ref, r.as_of) for r in reqs] == [("symbol:FUNDA", None)]


def test_plan_backfills_the_held_window_around_what_gold_has(fake):
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", held_from="2026-02-01",
                                            held_to="2026-05-31", compositions=["2026-03-31"])]))
    reqs, _ = plan(want, [fake], backfill=True)
    # Sold in May: no current composition. January is the composition in
    # force on the first day held; February, April and May are gaps, March
    # is covered by the composition gold has.
    assert [r.as_of for r in reqs] == [date(2026, 1, 28), date(2026, 2, 28), date(2026, 4, 28), date(2026, 5, 28)]


@pytest.mark.parametrize("role", ["proxy", "reference", "underlying"])
def test_plan_wants_the_current_composition_of_a_fund_wanted_for_another_one(fake, role):
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", held_to="2026-01-31", roles=[role])]))
    assert [r.as_of for r in plan(want, [fake])[0]] == [None]


def test_plan_skips_a_stale_fund_whose_newest_composition_is_had(fake):
    # A quarterly filer: gold's newest is old, but it is the newest there is.
    fake.latest_ = date(2026, 3, 31)
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-03-31"])]))
    assert plan(want, [fake])[0] == []
    fake.latest_ = date(2026, 6, 30)
    assert [r.as_of for r in plan(want, [fake])[0]] == [None]


@pytest.mark.parametrize("error", [ValueError("garbled"), RuntimeError("surprise")])
def test_a_provider_that_cannot_name_its_newest_date_is_asked_for_its_current_one(fake, monkeypatch, error):
    def latest(claim, c):
        raise error
    monkeypatch.setattr(fake, "latest", latest)
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-03-31"])]))
    assert [r.as_of for r in plan(want, [fake])[0]] == [None]


@pytest.mark.parametrize("error", [ValueError("Expecting value: line 1 column 1"), AttributeError("no .values")])
def test_a_product_list_that_does_not_parse_is_no_claim(fake, error):
    class Garbled(FakeProvider):
        name = "garbled"

        def claim(self, f, c):
            raise error
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA")]))
    reqs, unclaimed = plan(want, [Garbled(), fake])
    assert [r.provider for r in reqs] == ["fake"] and unclaimed == []


def test_plan_asks_only_the_refs_given(fake):
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA"), fund("symbol:OTHER", "FUNDA")],
                                      [{"key": "isin:XX0000000081", "scope": "direct", "lookups": {}}]))
    reqs, _ = plan(want, [fake, FakeLookup()], refs={"symbol:OTHER"})
    assert [r.ref for r in reqs] == ["symbol:OTHER"]


def test_the_inbox_serves_beside_the_provider_that_claims(tmp_path, fake):
    (tmp_path / "inbox" / "symbol_FUNDA" / "2026-03-15").mkdir(parents=True)
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-07-10"])]))
    c = Context(client=None, data_dir=tmp_path)
    reqs, unclaimed, _ = download.plan(want, [Inbox(), fake], c, today=TODAY, max_age=30, backfill=False)
    assert [(r.provider, r.as_of) for r in reqs] == [("inbox", date(2026, 3, 15))]
    assert unclaimed == []


def test_an_inbox_composition_counts_as_had_whatever_the_order(tmp_path, fake):
    (tmp_path / "inbox" / "symbol_FUNDA" / "2026-07-01").mkdir(parents=True)
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA", compositions=["2026-03-31"])]))
    c = Context(client=None, data_dir=tmp_path)
    reqs, _, _ = download.plan(want, [fake, Inbox()], c, today=TODAY, max_age=30, backfill=False)
    assert [(r.provider, r.as_of) for r in reqs] == [("inbox", date(2026, 7, 1))]


def test_a_failed_current_fetch_falls_through_to_the_next_claimant(tmp_path, fake, failing):
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA")]))
    (r,) = plan(want, [failing, fake])[0]
    assert (r.provider, r.fallbacks) == ("failing", ("fake",))
    # The one standing by claims the fund only once the first failed.
    assert fake.claimed == []
    (entry,) = download.execute([r], tmp_path, ctx())
    assert (entry["status"], entry["provider"]) == ("ok", "fake")
    assert entry["tried"][0]["provider"] == "failing" and fake.claimed == ["symbol:FUNDA"]


class _Listed(_Failing):
    """Claims every fund and states the ISIN and ticker an issuer's list
    would give it; fails every fetch."""
    name = "listed"

    def claim(self, f, c):
        self.claimed.append(f.ref)
        return {"ref": f.ref, "ids": {"isin": "XX0000000001", "cusip": "", "symbol": "FUNDA"}}


def test_a_standby_claims_by_an_identifier_the_first_claim_states(tmp_path, fake, monkeypatch):
    # The fund is named by its ISIN, and the one standing by finds a fund
    # by ticker only: the one the first provider's list gives. The
    # composition keeps the fund's reference.
    listed = _Listed()
    monkeypatch.setitem(download.REGISTRY, "listed", listed)
    want = wantedmod.parse(wanted_doc([fund("isin:XX0000000001")]))
    (r,) = plan(want, [listed, fake])[0]
    assert (r.fund.isin, r.fund.cusip, r.fund.symbol) == ("XX0000000001", None, "FUNDA")
    (entry,) = download.execute([r], tmp_path, ctx())
    assert (entry["status"], entry["provider"], entry["ref"]) == ("ok", "fake", "isin:XX0000000001")
    assert fake.fetched == [("isin:XX0000000001", date(2026, 7, 14))]


def test_a_claim_fills_only_the_identifiers_a_fund_lacks():
    f = wantedmod.Fund(ref="symbol:FUNDA", symbol="FUNDA")
    d = download.described(f, {"ids": {"isin": "XX0000000001", "symbol": "OTHER"}})
    assert (d.ref, d.isin, d.symbol) == ("symbol:FUNDA", "XX0000000001", "FUNDA")
    assert download.described(f, {"ref": "symbol:FUNDA"}) is f


def test_a_standby_is_not_claimed_when_the_first_serves(tmp_path, fake, failing):
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA")]))
    (entry,) = download.execute(plan(want, [fake, failing])[0], tmp_path, ctx())
    assert (entry["status"], entry["provider"], failing.claimed) == ("ok", "fake", [])
    assert "tried" not in entry


def test_every_claimant_failing_records_the_first_ones_error(tmp_path, fake, failing, monkeypatch):
    def unavailable(claim, as_of, outdir, c):
        raise Unavailable("no file today")
    monkeypatch.setattr(fake, "fetch", unavailable)
    want = wantedmod.parse(wanted_doc([fund("symbol:FUNDA", "FUNDA")]))
    (entry,) = download.execute(plan(want, [fake, failing])[0], tmp_path, ctx())
    assert (entry["status"], entry["error"]) == ("unavailable", "no file today")
    assert [(t["provider"], t["status"]) for t in entry["tried"]] == [("fake", "unavailable"), ("failing", "error")]


def test_limit_counts_attempts_a_standby_included(tmp_path, fake, failing):
    want = wantedmod.parse(wanted_doc([fund(f"symbol:FUND{c}", "FUNDA") for c in "ABC"]))
    reqs = plan(want, [failing, fake])[0]
    entries = download.execute(reqs, tmp_path, ctx(), limit=3)
    # Two attempts serve the first fund; the third fails the second, and
    # its standby waits for the next run, with the third fund.
    assert [(e["status"], e["provider"]) for e in entries] == [("ok", "fake"), ("error", "failing"),
                                                               ("deferred", "failing")]
    assert len(fake.fetched) == 1 and "error" not in entries[2]


def test_a_failed_request_does_not_fail_the_run(tmp_path, fake, monkeypatch):
    def boom(*a, **k):
        raise RuntimeError("parser exploded")
    monkeypatch.setattr(fake, "fetch", boom)
    reqs = [download.Request("fund", "fake", "symbol:FUNDA", None, {"ref": "symbol:FUNDA"})]
    (entry,) = download.execute(reqs, tmp_path, ctx())
    assert entry["status"] == "error" and "parser exploded" in entry["error"]


def test_a_fetch_that_states_no_day_is_an_error(tmp_path, fake, monkeypatch):
    monkeypatch.setattr(fake, "fetch", lambda claim, as_of, outdir, c: None)
    reqs = [download.Request("fund", "fake", "symbol:FUNDA", None, {"ref": "symbol:FUNDA"})]
    (entry,) = download.execute(reqs, tmp_path, ctx())
    assert (entry["status"], entry["error"]) == ("error", "the files fetched state no day")


def test_two_refs_never_share_a_folder(tmp_path):
    def folder(ref):
        return download.request_dir(tmp_path, download.Request("fund", "p", ref)).parent.name
    assert folder("isin:XX0000000001") == "isin_XX0000000001"
    assert folder("symbol:XYZ-B") == "symbol_XYZ-B"
    assert folder("symbol:XYZ/B").startswith("symbol_XYZ-B-") and folder("symbol:XYZ/B") != folder("symbol:XYZ B")


def test_a_run_writes_bronze_and_a_manifest(tmp_path, fake, monkeypatch, caplog):
    caplog.set_level(logging.INFO, logger="refdata.download")
    configured(monkeypatch, providers=("fake",))
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
    # The run ends with each provider's line on what it read.
    assert caplog.records[-1].getMessage() == "fake: 2 fetched"


def test_dry_run_writes_nothing(tmp_path, fake, monkeypatch, capsys):
    configured(monkeypatch, providers=("fake",))
    w = write_wanted(tmp_path / "wanted.json", wanted_doc([fund("symbol:FUNDA", "FUNDA")]))
    assert download.main(["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w), "--dry-run"]) == 0
    assert not (tmp_path / "data").exists()
    assert "symbol:FUNDA\tcurrent" in capsys.readouterr().out


def test_a_second_run_before_the_next_load_fetches_nothing(tmp_path, fake, monkeypatch):
    configured(monkeypatch, providers=("fake",))
    w = write_wanted(tmp_path / "wanted.json", wanted_doc([fund("symbol:FUNDA", "FUNDA", held_from="2026-05-01")]))
    monkeypatch.setattr(download, "today_utc", lambda: TODAY)
    args = ["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w), "--backfill"]
    assert download.main(args) == 0
    first = len(fake.fetched)
    assert first > 0
    monkeypatch.setattr(download.bronze, "ts_slug", lambda: "20990101T000000Z")
    assert download.main(args) == 0
    assert len(fake.fetched) == first


def test_debug_traces_the_requests_the_plan_makes(tmp_path, monkeypatch):
    class Lister(FakeProvider):
        """Finds its funds in a product list, as an issuer's provider does."""
        name = "lister"

        def claim(self, f, c):
            c.client.get("https://www.example.com/products.json")
            return super().claim(f, c)
    monkeypatch.setitem(download.REGISTRY, "lister", Lister())
    configured(monkeypatch, providers=("lister",))
    https, client = FakeHttps(default=(200, b"[]", {})), fetcher.Client
    monkeypatch.setattr(download.fetcher, "Client", lambda contact: client(contact, https=https))
    w = write_wanted(tmp_path / "wanted.json", wanted_doc([fund("symbol:FUNDA", "FUNDA")]))
    assert download.main(["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w), "--debug"]) == 0
    (trace,) = (tmp_path / "data").glob("*/screenshots/http-trace.jsonl")
    assert [json.loads(ln)["url"] for ln in trace.read_text().splitlines()] == ["https://www.example.com/products.json"]


def test_a_run_narrowed_to_one_provider_asks_no_other_source(tmp_path, monkeypatch):
    # sec-nport finds a fund named by a US ISIN alone through OpenFIGI,
    # but only in a run that asks openfigi too.
    configured(monkeypatch, providers=("sec-nport", "openfigi"), contact="a@example.org")
    https, client = FakeHttps(), fetcher.Client
    monkeypatch.setattr(download.fetcher, "Client", lambda contact: client(contact, https=https, sleep=lambda s: None))
    w = write_wanted(tmp_path / "wanted.json", wanted_doc([fund("isin:US0000000001")]))
    args = ["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w), "--dry-run"]
    assert download.main([*args, "--provider", "sec-nport"]) == 0
    assert https.seen == []
    assert download.main(args) == 0
    assert "https://api.openfigi.com/v3/mapping" in [r.full_url for r in https.seen]


def test_main_exit_codes(tmp_path, monkeypatch):
    def broken(known):
        raise config.ConfigError("refdata.cfg is not valid JSON")
    monkeypatch.setattr(download.config, "load", broken)
    assert download.main(["--bronze-dir", str(tmp_path)]) == 2
    configured(monkeypatch, providers=("inbox",))
    assert download.main(["--bronze-dir", str(tmp_path), "--wanted", str(tmp_path / "none.json")]) == 3
    with pytest.raises(SystemExit, match="--bronze-dir is required"):
        download.main([])


def test_provider_names_a_subset_and_refuses_a_name_not_enabled():
    cfg = config.Config(providers=("inbox", "gleif", "openfigi"))
    assert [p.name for p in download.enabled_providers(cfg, ["openfigi", "inbox"])] == ["inbox", "openfigi"]
    with pytest.raises(SystemExit, match="--provider ishares, nope: not enabled .* Enabled: inbox, gleif, openfigi"):
        download.enabled_providers(cfg, ["ishares", "nope", "gleif"])


def test_a_provider_that_reads_a_setting_waits_for_it(caplog):
    cfg = config.Config(providers=("inbox", "fidelity-529"), contact="a@example.org")
    assert [p.name for p in download.enabled_providers(cfg, None)] == ["inbox"]
    assert "fidelity_529_plans" in caplog.text
    cfg = config.Config(providers=("inbox", "fidelity-529"), contact="a@example.org",
                        fidelity_529_plans=(("XX", "https://example.org/x.pdf"),))
    assert [p.name for p in download.enabled_providers(cfg, None)] == ["inbox", "fidelity-529"]


def test_an_sec_provider_needs_a_contact(caplog):
    cfg = config.Config(providers=("inbox", "sec-nport"), contact="")
    assert [p.name for p in download.enabled_providers(cfg, None)] == ["inbox"]
    assert "contact" in caplog.text


class _Probed(Provider):
    def __init__(self, name, answer):
        self.name, self.answer = name, answer

    def probe(self, ctx):
        if isinstance(self.answer, Exception):
            raise self.answer
        return self.answer


def test_probe_asks_every_provider_and_fails_where_one_does(caplog):
    caplog.set_level(logging.INFO, logger="refdata.download")
    good = _Probed("good", "answered 200")
    assert download.probe([good], ctx()) == 0
    for error in (ProviderError("no plan"), ValueError("garbled"), fetcher.FetchError("https://x", 503),
                  RuntimeError("surprise")):
        assert download.probe([_Probed("bad", error), good], ctx()) == 1
    assert caplog.text.count("good: answered 200") == 5


def test_check_probes_and_writes_nothing(tmp_path, monkeypatch):
    monkeypatch.setitem(download.REGISTRY, "bad", _Probed("bad", ProviderError("no plan")))
    configured(monkeypatch, providers=("inbox", "bad"))
    assert download.main(["--check", "--bronze-dir", str(tmp_path / "data")]) == 1
    assert not (tmp_path / "data").exists()


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
    # Below the floor a constituent is not asked; one held directly is,
    # and one at the floor is.
    assert [s.key for s in download.security_order(want.securities, True, 1.0)] == [
        "isin:XX0000000072", "isin:XX0000000073"]
    assert [s.key for s in download.security_order(want.securities, True, 0.5)] == [
        "isin:XX0000000072", "isin:XX0000000073", "isin:XX0000000071"]


def test_the_floor_counts_a_share_by_its_size():
    # Under a negative net worth every share is negative; an unknown one
    # passes.
    doc = wanted_doc(securities=[
        {"key": "isin:XX0000000075", "scope": "constituent", "share_pct": -0.5, "missing": ["sector"], "lookups": {}},
        {"key": "isin:XX0000000076", "scope": "constituent", "share_pct": None, "missing": ["sector"], "lookups": {}},
        {"key": "isin:XX0000000077", "scope": "constituent", "share_pct": -0.01, "missing": ["sector"], "lookups": {}}])
    want = wantedmod.parse(doc)
    assert [s.key for s in download.security_order(want.securities, True, 0.1)] == [
        "isin:XX0000000075", "isin:XX0000000076"]


def test_plan_asks_no_constituent_below_the_floor():
    doc = wanted_doc(securities=[
        {"key": "isin:XX0000000078", "scope": "constituent", "share_pct": 0.05, "missing": ["country"], "lookups": {}},
        {"key": "isin:XX0000000079", "scope": "constituent", "share_pct": 0.5, "missing": ["country"], "lookups": {}}])
    reqs, _ = plan(wantedmod.parse(doc), [FakeLookup()], constituents=True, floor_pct=0.1)
    assert [r.ref for r in reqs] == ["isin:XX0000000079"]


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
    lk = FakeLookup()
    keys = lambda reqs: [r.ref for r in reqs]  # noqa: E731
    assert keys(plan(want, [lk])[0]) == ["isin:XX0000000091"]
    # The direct one always; the constituents lacking a country, but not
    # one looked up recently, nor one lacking only what it cannot answer.
    assert keys(plan(want, [lk], constituents=True)[0]) == ["isin:XX0000000091", "isin:XX0000000092"]
    # An earlier run's answer counts until gold holds it.
    again, _ = plan(want, [lk], constituents=True,
                    earlier=download.Fetched.of(lookups={("lookup", "isin:XX0000000092"): TODAY}))
    assert keys(again) == ["isin:XX0000000091"]
    # Without securities, none is asked.
    assert plan(want, [lk], constituents=True, securities=False)[0] == []


def test_earlier_runs_count_their_lookups_and_compositions(tmp_path):
    run = tmp_path / "20260714T120000Z"
    run.mkdir()
    (run / "run.json").write_text(json.dumps({"status": "complete", "requests": [
        {"kind": "security", "provider": "gleif", "ref": "isin:XX0000000095", "status": "ok"},
        {"kind": "security", "provider": "gleif", "ref": "isin:XX0000000096", "status": "error"},
        {"kind": "fund", "provider": "fake", "ref": "symbol:FUNDA", "as_of": "2026-01-31", "status": "ok"}]}))
    # A run that did not finish counts for nothing.
    crashed = tmp_path / "20260715T120000Z"
    crashed.mkdir()
    (crashed / "run.json").write_text(json.dumps({"status": "in-progress", "requests": [
        {"kind": "security", "provider": "gleif", "ref": "isin:XX0000000096", "status": "ok"}]}))
    earlier = download.fetched_before(tmp_path)
    assert earlier.lookups == {("gleif", "isin:XX0000000095"): date(2026, 7, 14)}
    assert earlier.funds == {("fake", "symbol:FUNDA", date(2026, 1, 31)): date(2026, 7, 14)}
    # A request sent counts as asked whether or not it served.
    assert earlier.asked == {("gleif", "isin:XX0000000095"): date(2026, 7, 14),
                             ("gleif", "isin:XX0000000096"): date(2026, 7, 14)}
    assert earlier == download.Fetched.of(funds=earlier.funds, lookups=earlier.lookups, asked=earlier.asked)
