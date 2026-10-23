"""download: the facts a version 2 wanted list names beyond compositions,
the bulk requests, and what a run says of every fact wanted."""

import json
import logging
import sqlite3
from datetime import date

import pytest

import config
import download
import load
from download import Fetched
from providers.base import Context, Parsed, Provider, ProviderError, read_meta, row, write_meta
from tests.helpers import (TODAY, FakeLookup, FakeProvider, fund, held, make_run, plan, run_entries, wanted_doc,
                           write_wanted)

REF = "symbol:FUNDA"


class _Facts(FakeProvider):
    """Serves the compositions of the funds it knows, as FakeProvider, and
    states the facts it names beside them, for past days too unless
    `dated` is false, and the `stated` ones with every composition; with
    `fails`, fails every fetch; with `missing`, names those facts missing
    from every fetch that succeeds."""

    def __init__(self, name, facts=(), known=("FUNDA",), fails=False, compositions=True, dated=True, missing=(),
                 stated=(), **kw):
        super().__init__(known, **kw)
        self.name, self.facts, self.fails, self.serves_compositions = name, frozenset(facts), fails, compositions
        self.dated_facts, self.missing = (self.facts if dated else frozenset()), list(missing)
        self.composition_facts = frozenset(stated)

    def fetch(self, claim, as_of, outdir, ctx):
        if self.fails:
            raise ProviderError("the issuer's page is gone")
        day = super().fetch(claim, as_of, outdir, ctx)
        if self.missing:
            write_meta(outdir, **read_meta(outdir), facts_missing=self.missing)
        return day


class _Frames(Provider):
    """A bulk provider: one request per period answers the earnings of
    every security it names, those with a US ISIN, by an invented CIK.
    Its claim names `families` (None: no such entry); the answer states
    no figure for the keys in `unanswered`."""
    name = "frames"
    serves_bulk = True

    def __init__(self, periods=("CY2026Q1",), answers=("earnings",), families=("earnings",), unanswered=()):
        self.periods, self.answers, self.families = periods, frozenset(answers), families
        self.unanswered = set(unanswered)

    def plan_bulk(self, securities, ctx, today, fetched, max_age):
        keys = [s.key for s in securities if "earnings" in s.missing and s.key.startswith("isin:US")]
        claims = [{"period": p, "keys": {f"{i:010d}": [k] for i, k in enumerate(keys, 1)}} for p in self.periods]
        if self.families is not None:
            claims = [{**c, "families": list(self.families)} for c in claims]
        return [(f"frame:eps:{c['period']}", c) for c in claims if keys and f"frame:eps:{c['period']}" not in fetched]

    def fetch_bulk(self, claim, outdir, ctx):
        outdir.mkdir(parents=True, exist_ok=True)
        write_meta(outdir, **claim, answered=sorted(download.bulk_keys(claim) - self.unanswered))

    def parse(self, indir):
        return Parsed(period_facts=[
            row("period_facts", security_key=k, field="eps_diluted", as_of=date(2026, 3, 31),
                period_start=date(2026, 1, 1), provider=self.name, value="1.25", unit="USD/shares")
            for k in read_meta(indir)["answered"]])


class _Company(Provider):
    """A company lookup: it answers a sector and, for a security held
    directly, the families of figures its claim names."""
    name = "company"
    serves_securities = True
    answers = frozenset({"sector", "earnings", "dividends", "financials"})

    def claim_security(self, sec, ctx):
        return {"key": sec.key, "families": ["earnings", "dividends", "financials"] if sec.scope == "direct" else []}

    def fetch_security(self, claim, outdir, ctx):
        outdir.mkdir(parents=True, exist_ok=True)
        write_meta(outdir, key=claim["key"], families=claim["families"])


@pytest.fixture
def registry(monkeypatch):
    """The providers download knows, these only: a reason names no real
    provider."""
    reg = {}
    monkeypatch.setattr(download, "REGISTRY", reg)

    def add(*providers):
        reg.update((p.name, p) for p in providers)
        return providers
    return add


def security(key, missing, scope="direct", lookups=None):
    return {"key": key, "scope": scope, "share_pct": None if scope == "direct" else 1.0, "missing": list(missing),
            "lookups": lookups or {}, "periods": {}}


def v2(funds=(), securities=()):
    return wanted_doc(funds, securities, version=2)


def asked(reqs):
    return [(r.provider, r.as_of, r.facts, r.fallbacks) for r in reqs]


def test_a_fact_is_asked_of_its_provider_beside_the_one_serving_compositions(registry):
    comp, issuer = registry(_Facts("comp"), _Facts("issuer", {"characteristics"}))
    doc = v2([held(compositions=["2026-05-29"], wants=["characteristics"])])
    reqs, _, cover = plan(doc, [comp, issuer])
    assert asked(reqs) == [("comp", None, (), ("issuer",)), ("issuer", None, ("characteristics",), ())]
    assert cover["characteristics"][REF].requests == [reqs[1]]
    # The provider that serves the composition asks for the fact in the
    # same request.
    reqs, _, _ = plan(doc, [issuer, comp])
    assert asked(reqs) == [("issuer", None, ("characteristics",), ("comp",))]


def test_a_request_for_facts_alone_loads_no_composition(registry, tmp_path, monkeypatch):
    """The issuer is asked for a fact alone: its entry says so, the
    composition its document states stays out of silver, and the next run
    does not count it as a composition had."""
    comp, issuer = registry(_Facts("comp"), _Facts("issuer", {"characteristics"}))
    for p in (comp, issuer):
        monkeypatch.setitem(load.REGISTRY, p.name, p)
    reqs, _, _ = plan(v2([held(compositions=["2026-05-29"], wants=["characteristics"])]), [comp, issuer])
    data = tmp_path / "data"
    entries = run_entries(make_run(data, "20260715T010000Z", reqs))
    assert [(e["provider"], e.get("facts_only")) for e in entries] == [("comp", None), ("issuer", True)]
    db = tmp_path / "refdata.db"
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    con = sqlite3.connect(db)
    try:
        assert con.execute("SELECT provider FROM compositions").fetchall() == [("comp",)]
    finally:
        con.close()
    assert set(download.fetched_before(data).funds) == {("comp", REF, date(2026, 7, 14))}


def test_the_next_provider_that_answers_a_fact_stands_by_for_it(registry, tmp_path):
    a, b = registry(_Facts("a", {"risk_metrics"}, fails=True), _Facts("b", {"risk_metrics"}))
    doc = v2([held(compositions=["2026-07-10"], wants=["risk_metrics"], facts={"risk_metrics": ["2026-03-31"]})])
    (r,), _, _ = plan(doc, [a, b])
    assert (r.provider, r.facts, r.fallbacks, b.claimed) == ("a", ("risk_metrics",), ("b",), [])
    (entry,) = download.execute([r], tmp_path, Context(client=None))
    assert (entry["status"], entry["provider"], entry["facts"]) == ("ok", "b", ["risk_metrics"])
    assert [t["provider"] for t in entry["tried"]] == ["a"]


def test_a_standby_that_serves_states_only_the_facts_it_answers(registry, tmp_path):
    issuer, comp = registry(_Facts("issuer", {"characteristics"}, fails=True), _Facts("comp"))
    (r,), _, _ = plan(v2([held(wants=["characteristics"])]), [issuer, comp])
    (entry,) = download.execute([r], tmp_path, Context(client=None))
    assert (entry["provider"], entry["facts"]) == ("comp", [])


def test_a_fact_had_within_max_age_is_not_asked(registry):
    (issuer,) = registry(_Facts("issuer", {"characteristics"}))
    fresh = v2([held(compositions=["2026-07-10"], wants=["characteristics"],
                     facts={"characteristics": ["2026-07-01"]})])
    reqs, _, cover = plan(fresh, [issuer])
    assert (reqs, issuer.claimed, cover["characteristics"][REF].had) == ([], [], True)
    # A report states a day long past: an earlier run's fetch from the
    # provider counts until max_age has passed since.
    stale = v2([held(compositions=["2026-07-10"], wants=["characteristics"],
                     facts={"characteristics": ["2026-01-31"]})])
    earlier = {("issuer", REF, "characteristics"): date(2026, 7, 10)}
    reqs, _, cover = plan(stale, [issuer], earlier=Fetched.of(facts=earlier))
    assert reqs == [] and cover["characteristics"][REF].had
    reqs, _, _ = plan(stale, [issuer], earlier=Fetched.of(facts=earlier), today=date(2026, 8, 15))
    assert asked(reqs) == [("issuer", None, ("characteristics",), ())]


def test_a_fact_whose_newest_day_is_had_is_not_asked(registry):
    (filing,) = registry(_Facts("filing", {"risk_metrics"}, latest_=date(2026, 3, 31)))
    doc = v2([held(compositions=["2026-07-10"], wants=["risk_metrics"], facts={"risk_metrics": ["2026-03-31"]})])
    assert plan(doc, [filing])[0] == []
    # Nor where an earlier run fetched that day from the provider; a newer
    # one is.
    filing.latest_ = date(2026, 6, 30)
    fetched = Fetched.of(funds={("filing", REF, date(2026, 6, 30)): date(2026, 7, 1)})
    assert plan(doc, [filing], earlier=fetched)[0] == []
    assert asked(plan(doc, [filing])[0]) == [("filing", None, ("risk_metrics",), ())]


def test_a_sold_fund_wants_its_facts_only_in_a_backfill(registry):
    (issuer,) = registry(_Facts("issuer", {"characteristics"}, dates_=[date(2026, 2, 28)]))
    doc = v2([held(wants=["characteristics"], held_to="2026-03-31", compositions=["2026-02-28"])])
    reqs, _, cover = plan(doc, [issuer])
    assert (reqs, cover) == ([], {})
    reqs, _, cover = plan(doc, [issuer], backfill=True)
    assert asked(reqs) == [("issuer", date(2026, 2, 28), ("characteristics",), ())] and cover == {}


def test_a_backfill_asks_the_past_days_a_fact_lacks(registry):
    (filing,) = registry(_Facts("filing", {"risk_metrics"}))
    doc = v2([held(held_from="2026-02-01", compositions=["2026-01-28", "2026-03-28", "2026-04-28", "2026-05-28",
                                                          "2026-06-28"],
                   wants=["risk_metrics"], facts={"risk_metrics": ["2026-03-28"]})])
    reqs, _, _ = plan(doc, [filing], backfill=True,
                      earlier=Fetched.of(funds={("filing", REF, date(2026, 5, 28)): date(2026, 6, 1)}))
    # The composition of February is a gap, and asks for the fact too.
    # January is the day in force when the holding began; March is had in
    # gold, May fetched by an earlier run.
    f = ("risk_metrics",)
    assert [(r.as_of, r.facts) for r in reqs] == [(date(2026, 2, 28), f), (None, f), (date(2026, 1, 28), f),
                                                  (date(2026, 4, 28), f), (date(2026, 6, 28), f)]


def test_a_backfill_does_not_ask_again_for_the_newest_day_the_current_request_fetches(registry):
    (filing,) = registry(_Facts("filing", {"risk_metrics"}, latest_=date(2026, 6, 28)))
    every = ["2026-01-28", "2026-02-28", "2026-03-28", "2026-04-28", "2026-05-28", "2026-06-28", "2026-07-10"]
    f = ("risk_metrics",)
    # The fact alone is asked for: its current answer is the newest day's.
    doc = v2([held(held_from="2026-02-01", compositions=every, wants=["risk_metrics"],
                   facts={"risk_metrics": ["2026-03-28"]})])
    reqs, _, _ = plan(doc, [filing], backfill=True)
    assert [(r.as_of, r.facts) for r in reqs] == [(None, f), (date(2026, 1, 28), f), (date(2026, 2, 28), f),
                                                  (date(2026, 4, 28), f), (date(2026, 5, 28), f)]
    # The fact joins the current composition request.
    doc = v2([held(held_from="2026-02-01", compositions=every[:3], wants=["risk_metrics"],
                   facts={"risk_metrics": ["2026-03-28"]})])
    reqs, _, _ = plan(doc, [filing], backfill=True)
    assert date(2026, 6, 28) not in [r.as_of for r in reqs] and (None, f) in [(r.as_of, r.facts) for r in reqs]


def test_a_backfilled_fact_skips_only_the_days_it_is_held_for(registry):
    # A monthly filing states every business day of its month: the
    # February filing is wanted though March's first days are held.
    month_ends = [date(2026, 1, 31), date(2026, 2, 28), date(2026, 3, 31)]
    (mmf,) = registry(_Facts("mmf", {"money_market"}, compositions=False, dates_=month_ends))
    doc = v2([held(held_from="2026-02-01", compositions=["2026-07-10"], wants=["money_market"],
                   facts={"money_market": ["2026-03-02", "2026-03-16", "2026-03-31"]})])
    reqs, _, _ = plan(doc, [mmf], backfill=True)
    assert [r.as_of for r in reqs] == [None, date(2026, 1, 31), date(2026, 2, 28)]


def test_the_days_a_request_for_facts_alone_fetched_count_as_had(registry, tmp_path):
    run = tmp_path / "20260714T120000Z"
    run.mkdir()
    (run / "run.json").write_text(json.dumps({"status": "complete", "requests": [
        {"kind": "fund", "provider": "mmf", "ref": REF, "as_of": day, "facts": ["money_market"], "facts_only": True,
         "status": "ok"} for day in ("2026-02-28", "2026-03-31")]}))
    earlier = download.fetched_before(tmp_path)
    assert earlier.funds == {}
    assert earlier.fact_days == {("mmf", REF, "money_market"): {date(2026, 2, 28), date(2026, 3, 31)}}
    month_ends = [date(2026, 1, 31), date(2026, 2, 28), date(2026, 3, 31)]
    (mmf,) = registry(_Facts("mmf", {"money_market"}, compositions=False, dates_=month_ends,
                             latest_=date(2026, 3, 31)))
    doc = v2([held(held_from="2026-02-01", compositions=["2026-07-10"], wants=["money_market"])])
    # The newest day is had, so no current request; of the past, January.
    reqs, _, cover = plan(doc, [mmf], backfill=True, earlier=earlier, today=date(2026, 9, 1))
    assert [r.as_of for r in reqs] == [date(2026, 1, 31)] and cover["money_market"][REF].had


def test_a_provider_of_facts_only_stands_by_on_a_request_for_facts_alone_only(registry):
    x, mmf = registry(_Facts("x", {"money_market"}), _Facts("mmf", {"money_market"}, compositions=False))
    # x serves the composition, and the fact joins it: mmf cannot stand by.
    reqs, _, _ = plan(v2([held(wants=["money_market"])]), [x, mmf])
    assert asked(reqs) == [("x", None, ("money_market",), ())]
    # x is asked for the fact alone: mmf stands by.
    reqs, _, _ = plan(v2([held(wants=["money_market"], compositions=["2026-07-10"])]), [x, mmf])
    assert asked(reqs) == [("x", None, ("money_market",), ("mmf",))]


def test_a_fact_stated_for_today_only_is_not_backfilled(registry):
    (page,) = registry(_Facts("page", {"characteristics"}, dated=False))
    doc = v2([held(held_from="2026-02-01", compositions=["2026-07-10"], wants=["characteristics"])])
    reqs, _, _ = plan(doc, [page], backfill=True)
    # The page's past compositions are backfilled; the fact rides only on
    # today's request.
    assert [(r.as_of, r.facts) for r in reqs if r.facts] == [(None, ("characteristics",))]


def test_a_fact_the_fetch_names_missing_stays_due(registry, tmp_path):
    (page,) = registry(_Facts("page", {"characteristics"}, missing=["characteristics"]))
    (r,), _, _ = plan(v2([held(wants=["characteristics"])]), [page])
    (entry,) = download.execute([r], tmp_path, Context(client=None))
    assert (entry["status"], entry["facts"]) == ("ok", [])


def test_a_provider_of_facts_only_is_offered_no_composition(registry):
    mmf, comp = registry(_Facts("mmf", {"money_market"}, compositions=False), _Facts("comp"))
    reqs, unclaimed, _ = plan(v2([held(wants=["money_market"])]), [mmf, comp])
    assert asked(reqs) == [("comp", None, (), ()), ("mmf", None, ("money_market",), ())]
    assert unclaimed == []


def test_a_fact_a_composition_states_comes_with_the_composition_request(registry):
    (holdings,) = registry(_Facts("holdings", stated={"line_durations"}))
    doc = v2([held(compositions=["2026-05-29"], wants=["line_durations"])])
    reqs, _, cover = plan(doc, [holdings])
    # The current composition is asked for, and no request for the fact
    # alone; past --limit it waits for the next run.
    assert asked(reqs) == [("holdings", None, (), ())] and reqs[0].compositions
    assert cover["line_durations"][REF].requests == reqs
    assert download.tally(cover)[0][1:] == (1, 1, 0, [])
    assert download.tally(cover, {id(r) for r in reqs})[0][4] == [(REF, "deferred by --limit")]
    # A backfill's past compositions ask for it too.
    doc = v2([held(held_from="2026-02-01", compositions=["2026-07-10"], wants=["line_durations"])])
    reqs, _, cover = plan(doc, [holdings], backfill=True)
    assert [r.as_of for r in reqs] == [date(2026, m, 28) for m in range(1, 6)]
    assert cover["line_durations"][REF].requests == reqs


def test_a_fact_a_composition_states_is_never_asked_for_alone(registry):
    comp, holdings = registry(_Facts("comp"), _Facts("holdings", {"characteristics"}, stated={"line_durations"}))
    doc = v2([held(compositions=["2026-05-29"], wants=["line_durations"])])
    reason = "comes only with a composition from: holdings"
    # Another provider serves the composition; holdings only stands by.
    reqs, _, cover = plan(doc, [comp, holdings])
    assert asked(reqs) == [("comp", None, (), ("holdings",))]
    c = cover["line_durations"][REF]
    assert (c.requests, c.had, c.reason) == ([], False, reason)
    # A request to it for another fact alone loads no composition.
    doc = v2([held(compositions=["2026-05-29"], wants=["characteristics", "line_durations"])])
    reqs, _, cover = plan(doc, [comp, holdings])
    assert [(r.provider, r.compositions) for r in reqs] == [("comp", True), ("holdings", False)]
    assert (cover["line_durations"][REF].requests, cover["line_durations"][REF].reason) == ([], reason)
    # Nor where the run leaves it off.
    _, _, cover = plan(doc, [comp])
    assert cover["line_durations"][REF].reason == reason


def test_a_composition_an_earlier_run_fetched_states_the_fact_for_max_age(registry):
    (holdings,) = registry(_Facts("holdings", stated={"line_durations"}))
    doc = v2([held(compositions=["2026-05-29"], wants=["line_durations"])])
    earlier = Fetched.of(funds={("holdings", REF, date(2026, 7, 10)): date(2026, 7, 10)})
    reqs, _, cover = plan(doc, [holdings], earlier=earlier)
    assert reqs == [] and download.tally(cover)[0][1:] == (1, 0, 1, [])
    reqs, _, cover = plan(doc, [holdings], earlier=earlier, today=date(2026, 8, 15))
    assert asked(reqs) == [("holdings", None, (), ())] and cover["line_durations"][REF].requests == reqs


def test_a_standby_that_does_not_claim_the_fund_takes_no_attempt(registry, tmp_path):
    a, b, c = registry(_Facts("a", {"risk_metrics"}, fails=True), _Facts("b", {"risk_metrics"}, known=()),
                       _Facts("c", {"risk_metrics"}))
    (r,), _, _ = plan(v2([held(compositions=["2026-07-10"], wants=["risk_metrics"])]), [a, b, c])
    assert r.fallbacks == ("b", "c")
    (entry,) = download.execute([r], tmp_path, Context(client=None), limit=2)
    assert (entry["status"], entry["provider"], [t["provider"] for t in entry["tried"]]) == ("ok", "c", ["a"])
    assert b.claimed == [REF]


def test_a_fetch_that_writes_no_meta_json_names_no_fact_missing(registry, tmp_path):
    class Bare(_Facts):
        def fetch(self, claim, as_of, outdir, ctx):
            day = super().fetch(claim, as_of, outdir, ctx)
            (outdir / "meta.json").unlink()
            return day

    (page,) = registry(Bare("page", {"characteristics"}))
    (r,), _, _ = plan(v2([held(wants=["characteristics"])]), [page])
    (entry,) = download.execute([r], tmp_path, Context(client=None))
    assert (entry["status"], entry["facts"]) == ("ok", ["characteristics"])


class _Unplanned(_Frames):
    """A bulk provider whose plan fails."""
    name = "unplanned"

    def plan_bulk(self, securities, ctx, today, fetched, max_age):
        raise ProviderError("the frame index is gone")


def test_a_bulk_plan_that_fails_leaves_the_rest_of_the_plan(registry, caplog):
    comp, unplanned, divs = registry(_Facts("comp"), _Unplanned(), FakeLookup("divs", ("dividends",)))
    caplog.set_level(logging.WARNING, logger="refdata.download")
    reqs, _, _ = plan(v2([held()], [security("isin:US0000000004", ["earnings", "dividends"])]),
                      [comp, unplanned, divs])
    assert [(r.kind, r.provider) for r in reqs] == [("fund", "comp"), ("security", "divs")]
    assert "bulk: unplanned could not plan its requests: the frame index is gone" in caplog.text


def test_a_bulk_request_round_trips_through_execute_and_load(registry, tmp_path, monkeypatch):
    (frames,) = registry(_Frames(unanswered={"isin:US0000000002"}))
    monkeypatch.setitem(load.REGISTRY, "frames", frames)
    doc = v2(securities=[security("isin:US0000000001", ["earnings"], "constituent"),
                         security("isin:US0000000002", ["earnings"], "constituent")])
    reqs, _, _ = plan(doc, [frames], constituents=True)
    assert [(r.kind, r.ref) for r in reqs] == [("bulk", "frame:eps:CY2026Q1")]
    data = tmp_path / "data"
    entries = run_entries(make_run(data, "20260715T010000Z", reqs))
    assert entries[0]["status"] == "ok" and entries[0]["dir"].startswith("bulk/frames/frame_eps-CY2026Q1-")
    db = tmp_path / "refdata.db"
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    con = sqlite3.connect(db)
    try:
        got = con.execute("SELECT security_key, field, value FROM period_facts ORDER BY 1").fetchall()
    finally:
        con.close()
    assert got == [("isin:US0000000001", "eps_diluted", "1.25")]
    # The next run knows the frame fetched: the provider plans it no more.
    # A security its answer states a figure for counts as had, the other
    # one it named as left.
    earlier = download.fetched_before(data)
    assert earlier.bulk == {("frames", "frame:eps:CY2026Q1"): date(2026, 7, 15)}
    assert earlier.answered == {("frames", "isin:US0000000001"): date(2026, 7, 15)}
    reqs, _, cover = plan(doc, [frames], constituents=True, earlier=earlier)
    assert reqs == [] and download.tally(cover)[0][1:] == (2, 0, 1, [("isin:US0000000002", "no provider claims it")])
    # Past max_age the answer counts no more.
    _, _, cover = plan(doc, [frames], constituents=True, earlier=earlier, today=date(2026, 8, 15))
    assert not cover["earnings"]["isin:US0000000001"].had


def test_a_bulk_answer_without_answered_counts_none(tmp_path):
    run = tmp_path / "20260714T120000Z"
    (run / "bulk" / "frames" / "f").mkdir(parents=True)
    write_meta(run / "bulk" / "frames" / "f", keys=["isin:US0000000001"])
    (run / "run.json").write_text(json.dumps({"status": "complete", "requests": [
        {"kind": "bulk", "provider": "frames", "ref": "frame:eps:CY2026Q1", "status": "ok", "dir": "bulk/frames/f"},
        {"kind": "bulk", "provider": "frames", "ref": "frame:eps:CY2026Q2", "status": "ok", "dir": "bulk/frames/g"}]}))
    earlier = download.fetched_before(tmp_path)
    assert len(earlier.bulk) == 2 and earlier.answered == {}


def test_a_bulk_request_is_planned_only_for_the_families_its_claim_names(registry):
    doc = v2(securities=[security("isin:US0000000009", ["earnings", "dividends"])])
    (both,) = registry(_Frames(answers=("earnings", "dividends")))
    _, _, cover = plan(doc, [both])
    assert [(f, c.reason) for f in ("earnings", "dividends") for c in cover[f].values()] == [
        ("earnings", ""), ("dividends", "no provider claims it")]
    (unnamed,) = registry(_Frames(families=None))
    reqs, _, cover = plan(doc, [unnamed])
    assert [r.kind for r in reqs] == ["bulk"] and cover["earnings"]["isin:US0000000009"].requests == []


def test_bulk_requests_come_last_and_are_narrowed_like_the_others(registry, tmp_path):
    frames, divs = registry(_Frames(periods=("CY2025Q4", "CY2026Q1")), FakeLookup("divs", ("dividends",)))
    doc = v2(securities=[security("isin:US0000000003", ["earnings", "dividends"])])
    reqs, _, _ = plan(doc, [frames, divs])
    assert [(r.kind, r.ref) for r in reqs] == [("security", "isin:US0000000003"), ("bulk", "frame:eps:CY2025Q4"),
                                               ("bulk", "frame:eps:CY2026Q1")]
    # --ref keeps a bulk ref it names, and none where it names a security
    # or a fund only; --no-securities skips them.
    assert [r.ref for r in plan(doc, [frames, divs], refs={"frame:eps:CY2026Q1"})[0]] == ["frame:eps:CY2026Q1"]
    assert [r.kind for r in plan(doc, [frames, divs], refs={"isin:US0000000003"})[0]] == ["security"]
    assert plan(doc, [frames, divs], securities=False)[0] == []
    # --limit counts a bulk fetch as any other.
    entries = download.execute(reqs[1:], tmp_path, Context(client=None), limit=1)
    assert [e["status"] for e in entries] == ["ok", "deferred"]


def test_bulk_keys_reads_a_list_or_lists_by_an_identifier():
    assert download.bulk_keys({"keys": ["isin:US0000000001"]}) == {"isin:US0000000001"}
    assert download.bulk_keys({"keys": {"1": ["isin:US0000000001", "symbol:EXMP"], "2": "isin:US0000000002"}}) == {
        "isin:US0000000001", "symbol:EXMP", "isin:US0000000002"}
    assert download.bulk_keys({"frame": "CY2026Q1"}) == download.bulk_keys(None) == set()


def test_a_lookup_that_asked_for_no_figures_does_not_hold_back_the_families(registry):
    (company,) = registry(_Company())
    key = "isin:US0000000005"
    doc = v2(securities=[security(key, ["earnings"], lookups={"company": "2026-07-10"})])
    looked = {("company", key): date(2026, 7, 12)}
    reqs, _, cover = plan(doc, [company], earlier=Fetched.of(lookups=looked))
    assert [(r.kind, r.ref) for r in reqs] == [("security", key)] and cover["earnings"][key].requests == reqs
    # A run that asked for the families holds them back for max_age, for a
    # company that files none too; past max_age they are asked again.
    asked = {("company", key): date(2026, 7, 12)}
    reqs, _, cover = plan(doc, [company], earlier=Fetched.of(lookups=looked, families=asked))
    assert reqs == [] and cover["earnings"][key].had
    assert [r.ref for r in plan(doc, [company], earlier=Fetched.of(families=asked), today=date(2026, 8, 15))[0]] == [
        key]
    # A field other than the families keeps the lookup's own rule.
    assert plan(v2(securities=[security(key, ["sector"], lookups={"company": "2026-07-10"})]), [company])[0] == []


def test_a_constituent_counts_its_families_from_no_lookup(registry):
    (company,) = registry(_Company())
    key = "isin:US0000000006"
    # A recent lookup is no family had; a request whose claim asks for no
    # family is none planned.
    recent = v2(securities=[security(key, ["earnings"], "constituent", lookups={"company": "2026-07-10"})])
    reqs, _, cover = plan(recent, [company], constituents=True)
    assert reqs == [] and cover["earnings"][key].reason == "no provider claims it"
    reqs, _, cover = plan(v2(securities=[security(key, ["earnings"], "constituent")]), [company], constituents=True)
    assert [r.ref for r in reqs] == [key]
    assert (cover["earnings"][key].requests, cover["earnings"][key].reason) == ([], "no provider claims it")


def test_a_security_request_records_the_families_it_asks_for(registry, tmp_path):
    (company,) = registry(_Company())
    doc = v2(securities=[security("isin:US0000000007", ["earnings"]),
                         security("isin:US0000000008", ["sector"], "constituent")])
    reqs, _, _ = plan(doc, [company], constituents=True)
    data = tmp_path / "data"
    entries = run_entries(make_run(data, "20260715T010000Z", reqs))
    assert [e.get("families") for e in entries] == [["earnings", "dividends", "financials"], None]
    earlier = download.fetched_before(data)
    assert earlier.families == {("company", "isin:US0000000007"): date(2026, 7, 15)}
    assert set(earlier.lookups) == {("company", "isin:US0000000007"), ("company", "isin:US0000000008")}


def test_earlier_runs_count_their_fund_facts_and_bulk_requests(tmp_path):
    run = tmp_path / "20260714T120000Z"
    run.mkdir()
    (run / "run.json").write_text(json.dumps({"status": "complete", "requests": [
        {"kind": "fund", "provider": "filing", "ref": REF, "as_of": "2026-03-31", "facts": ["risk_metrics"],
         "status": "ok"},
        {"kind": "fund", "provider": "issuer", "ref": REF, "facts": ["characteristics"], "status": "error"},
        {"kind": "bulk", "provider": "frames", "ref": "frame:eps:CY2026Q1", "status": "ok"},
        {"kind": "bulk", "provider": "frames", "ref": "frame:eps:CY2026Q2", "status": "unavailable"}]}))
    earlier = download.fetched_before(tmp_path)
    assert earlier.funds == {("filing", REF, date(2026, 3, 31)): date(2026, 7, 14)}
    assert earlier.facts == {("filing", REF, "risk_metrics"): date(2026, 7, 14)}
    assert earlier.bulk == {("frames", "frame:eps:CY2026Q1"): date(2026, 7, 14)}


def _coverage_case(tmp_path, monkeypatch, registry):
    """A wanted list whose facts end every way the coverage tells, and the
    args of a run of it: refdata.cfg enables comp, issuer and frames;
    filing and divs are registered but off."""
    registry(_Facts("comp"), _Facts("issuer", {"characteristics"}), _Facts("filing", {"risk_metrics"}), _Frames(),
             FakeLookup("divs", ("dividends",)))
    monkeypatch.setattr(download.config, "load", lambda known: config.Config(providers=("comp", "issuer", "frames")))
    monkeypatch.setattr(download, "today_utc", lambda: TODAY)
    w = write_wanted(tmp_path / "wanted.json", v2(
        [held(wants=["risk_metrics", "characteristics", "line_durations"]),
         held("symbol:FUNDB", "EXMB", wants=["characteristics"], compositions=["2026-07-10"]),
         held("symbol:FUNDC", "EXMC", wants=["characteristics"], compositions=["2026-07-10"],
              facts={"characteristics": ["2026-07-10"]})],
        [security("isin:US0000000001", ["earnings"]), security("isin:XX0000000002", ["earnings", "dividends"])]))
    return ["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w)]


def test_dry_run_says_what_becomes_of_every_fact_wanted(tmp_path, monkeypatch, registry, capsys):
    args = _coverage_case(tmp_path, monkeypatch, registry)
    assert download.main([*args, "--dry-run"]) == 0
    out = capsys.readouterr().out.splitlines()
    assert out[:3] == ["fund\tcomp\tsymbol:FUNDA\tcurrent", "fund\tissuer\tsymbol:FUNDA\tcurrent",
                       "bulk\tframes\tframe:eps:CY2026Q1\tcurrent"]
    assert out[3:] == [
        "facts",
        "risk_metrics\t1 wanted, 0 planned, 0 had",
        "risk_metrics\tsymbol:FUNDA\tprovider off: filing",
        "characteristics\t3 wanted, 1 planned, 1 had",
        "characteristics\tsymbol:FUNDB\tno provider claims it",
        "line_durations\t1 wanted, 0 planned, 0 had",
        "line_durations\tsymbol:FUNDA\tno enabled provider answers it",
        "earnings\t2 wanted, 1 planned, 0 had",
        "earnings\tisin:XX0000000002\tno provider claims it",
        "dividends\t1 wanted, 0 planned, 0 had",
        "dividends\tisin:XX0000000002\tprovider off: divs"]
    # Past --limit, a request asks for nothing yet.
    assert download.main([*args, "--dry-run", "--limit", "1"]) == 0
    out = capsys.readouterr().out.splitlines()
    assert "characteristics\tsymbol:FUNDA\tdeferred by --limit" in out
    assert "earnings\tisin:US0000000001\tdeferred by --limit" in out
    assert "characteristics\t3 wanted, 0 planned, 1 had" in out


def test_a_run_logs_the_same_counts_one_line_per_fact(tmp_path, monkeypatch, registry, caplog):
    caplog.set_level(logging.INFO, logger="refdata.download")
    args = _coverage_case(tmp_path, monkeypatch, registry)
    assert download.main([*args, "--limit", "2"]) == 0
    lines = [r.getMessage() for r in caplog.records if r.getMessage().startswith("facts: ")]
    assert lines == ["facts: risk_metrics: 1 wanted, 0 planned, 0 had, 1 provider off: filing",
                     "facts: characteristics: 3 wanted, 1 planned, 1 had, 1 no provider claims it",
                     "facts: line_durations: 1 wanted, 0 planned, 0 had, 1 no enabled provider answers it",
                     "facts: earnings: 2 wanted, 0 planned, 0 had, 1 deferred by --limit, 1 no provider claims it",
                     "facts: dividends: 1 wanted, 0 planned, 0 had, 1 provider off: divs"]
    (run,) = (tmp_path / "data").iterdir()
    assert [(e["provider"], e.get("facts"), e["status"]) for e in run_entries(run)] == [
        ("comp", None, "ok"), ("issuer", ["characteristics"], "ok"), ("frames", None, "deferred")]


def test_a_version_1_list_plans_as_it_always_did(tmp_path, monkeypatch, registry, capsys):
    comp, issuer, _, _, _ = registry(_Facts("comp"), _Facts("issuer", {"characteristics"}),
                                     _Facts("filing", {"risk_metrics"}), _Frames(), FakeLookup("divs", ("dividends",)))
    doc = wanted_doc([fund(REF, "FUNDA", compositions=["2026-05-29"])],
                     [{"key": "isin:US0000000001", "scope": "direct", "missing": ["sector"], "lookups": {}}])
    reqs, unclaimed, cover = plan(doc, [comp, issuer])
    assert asked(reqs) == [("comp", None, (), ("issuer",))]
    assert (unclaimed, cover, issuer.claimed) == ([], {}, [])
    monkeypatch.setattr(download.config, "load", lambda known: config.Config(providers=("comp", "issuer", "frames")))
    w = write_wanted(tmp_path / "wanted.json", doc)
    assert download.main(["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w), "--dry-run"]) == 0
    assert capsys.readouterr().out == "fund\tcomp\tsymbol:FUNDA\tcurrent\n"
    assert download.main(["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w)]) == 0
    (run,) = (tmp_path / "data").iterdir()
    (entry,) = run_entries(run)
    assert set(entry) == {"kind", "provider", "ref", "asked", "status", "as_of", "dir"}
