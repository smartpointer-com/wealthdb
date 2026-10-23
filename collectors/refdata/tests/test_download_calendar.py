"""download: the dividend calendar a version 3 wanted list names, as a held
fund's fact and as a fact a share held directly wants, asked of fund,
security and bulk providers, and what a run says of it."""

import json
from datetime import date

import pytest

import config
import download
import load
from providers.base import Context, Parsed, Provider, read_meta, row, write_meta
from tests.helpers import TODAY, FakeProvider, held, make_run, plan, rows, run_entries, wanted_doc, write_wanted

REF = "symbol:FUNDA"
CAL = "dividend_calendar"


def _comp():
    """A provider of compositions alone, as FakeProvider."""
    p = FakeProvider()
    p.name = "comp"
    return p


class _Issuer(FakeProvider):
    """Serves the compositions of the funds it knows, as FakeProvider, and
    states their calendar beside them: one event, or none with `events`
    false, as for an accumulating class."""

    def __init__(self, name="issuer", events=True, **kw):
        super().__init__(**kw)
        self.name, self.events, self.facts = name, events, frozenset({CAL})

    def parse(self, indir):
        out = super().parse(indir)
        meta = read_meta(indir)
        if self.events:
            out.dividend_events = [row("dividend_events", ref=meta["ref"], provider=self.name, event_id="e1",
                                       as_of=date.fromisoformat(meta["as_of"]), kind="dividend",
                                       ex_date=date(2026, 6, 15), amount="0.25")]
        return out


class _Exchange(Provider):
    """A security provider that answers the calendar alone: it claims a
    security that wants it, its claim naming what `wants` holds, and
    states one event for it; with `missing`, its fetch names the
    calendar missing."""
    name = "exchange"
    serves_securities = True
    answers = frozenset({CAL})

    def __init__(self, missing=False):
        self.claimed, self.missing = [], missing

    def claim_security(self, sec, ctx):
        self.claimed.append((sec.key, sec.wants))
        return {"key": sec.key, "facts": list(sec.wants)} if sec.wants else None

    def fetch_security(self, claim, outdir, ctx):
        outdir.mkdir(parents=True, exist_ok=True)
        write_meta(outdir, key=claim["key"], **({"facts_missing": [CAL]} if self.missing else {}))

    def parse(self, indir):
        return Parsed(dividend_events=[row("dividend_events", ref=read_meta(indir)["key"], provider=self.name,
                                           event_id="n1", as_of=TODAY, kind="dividend", ex_date=date(2026, 6, 15))])


class _Filings(Provider):
    """A company lookup that answers a sector and the calendar: its claim
    names the calendar a share wants, never a receipt's."""
    name = "filings"
    serves_securities = True
    answers = frozenset({"sector", CAL})

    def __init__(self):
        self.claimed = []

    def claim_security(self, sec, ctx):
        self.claimed.append((sec.key, sec.wants))
        return {"key": sec.key, "facts": [] if sec.receipt else list(sec.wants)}


class _Daily(Provider):
    """A bulk provider: one request a day states the calendar of every
    receipt that wants it; its answer states an event for the first."""
    name = "daily"
    serves_bulk = True
    answers = frozenset({CAL})

    def plan_bulk(self, securities, ctx, today, fetched, max_age):
        keys = [s.key for s in securities if s.receipt and CAL in s.wants]
        ref = f"daily:{today}"
        return [(ref, {"keys": keys, "facts": [CAL, "unheard-of"]})] if keys and ref not in fetched else []

    def fetch_bulk(self, claim, outdir, ctx):
        outdir.mkdir(parents=True, exist_ok=True)
        write_meta(outdir, **claim, answered=claim["keys"][:1])


@pytest.fixture
def registry(monkeypatch):
    """The providers download and load know, these only: a reason names
    no real provider."""
    reg = {}
    monkeypatch.setattr(download, "REGISTRY", reg)
    monkeypatch.setattr(load, "REGISTRY", reg)

    def add(*providers):
        reg.update((p.name, p) for p in providers)
        return providers
    return add


def v3(funds=(), securities=()):
    return wanted_doc(funds, securities, version=3)


def share(key, calendar=None, receipt=False, missing=(), lookups=None, wants=(CAL,)):
    return {"key": key, "scope": "direct", "share_pct": None, "asset_class": "public_equity",
            "missing": list(missing), "lookups": lookups or {}, "periods": {}, "receipt": receipt,
            "wants": list(wants), "calendar": calendar}


def asked(reqs):
    return [(r.kind, r.provider, r.ref, r.as_of, r.facts) for r in reqs]


def test_a_held_funds_calendar_is_asked_as_any_fact(registry):
    comp, issuer = registry(_comp(), _Issuer())
    # The composition is had: the calendar is asked for alone.
    reqs, _, cover = plan(v3([held(wants=[CAL], compositions=["2026-07-10"])]), [comp, issuer])
    assert asked(reqs) == [("fund", "issuer", REF, None, (CAL,))] and not reqs[0].compositions
    assert cover[CAL][REF].requests == reqs
    # Gold's newest calendar day within max_age: had.
    fresh = held(wants=[CAL], compositions=["2026-07-10"], facts={CAL: ["2026-06-30", "2026-07-01"]})
    reqs, _, cover = plan(v3([fresh]), [comp, issuer])
    assert reqs == [] and cover[CAL][REF].had
    # The provider that serves the composition states the calendar with it.
    reqs, _, _ = plan(v3([held(wants=[CAL])]), [issuer, comp])
    assert asked(reqs) == [("fund", "issuer", REF, None, (CAL,))] and reqs[0].compositions


@pytest.mark.parametrize("events", [True, False])
def test_a_calendar_fetched_counts_for_max_age_whether_or_not_it_states_events(registry, tmp_path, events):
    """An accumulating class states no event: its calendar counts as
    fetched all the same. A request for the calendar alone loads its
    events and no composition."""
    comp, issuer = registry(_comp(), _Issuer(events=events))
    doc = v3([held(wants=[CAL], compositions=["2026-07-10"])])
    reqs, _, _ = plan(doc, [comp, issuer])
    data = tmp_path / "data"
    (entry,) = run_entries(make_run(data, "20260715T010000Z", reqs))
    assert (entry["provider"], entry["facts"], entry["facts_only"]) == ("issuer", [CAL], True)
    db = tmp_path / "refdata.db"
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    assert rows(db, "SELECT COUNT(*) FROM compositions") == [(0,)]
    assert rows(db, "SELECT ref, provider, event_id FROM dividend_events") == \
        ([(REF, "issuer", "e1")] if events else [])
    earlier = download.fetched_before(data)
    assert earlier.facts == {("issuer", REF, CAL): date(2026, 7, 15)}
    reqs, _, cover = plan(doc, [comp, issuer], earlier=earlier)
    assert reqs == [] and cover[CAL][REF].had
    later = asked(plan(doc, [comp, issuer], earlier=earlier, today=date(2026, 8, 15))[0])
    assert [a for a in later if a[1] == "issuer"] == [("fund", "issuer", REF, None, (CAL,))]


def test_a_share_asks_for_its_calendar_once_it_is_older_than_max_age(registry):
    (exchange,) = registry(_Exchange())
    reqs, _, cover = plan(v3(securities=[share("symbol:QSHR")]), [exchange])
    assert asked(reqs) == [("security", "exchange", "symbol:QSHR", None, (CAL,))]
    assert exchange.claimed == [("symbol:QSHR", (CAL,))] and cover[CAL]["symbol:QSHR"].requests == reqs
    # Gold's calendar within max_age: the provider is not even asked to
    # claim it; past max_age it is.
    exchange.claimed.clear()
    reqs, _, cover = plan(v3(securities=[share("symbol:QSHR", calendar="2026-07-01")]), [exchange])
    assert (reqs, exchange.claimed, cover[CAL]["symbol:QSHR"].had) == ([], [], True)
    reqs, _, _ = plan(v3(securities=[share("symbol:QSHR", calendar="2026-06-01")]), [exchange])
    assert [r.ref for r in reqs] == ["symbol:QSHR"]
    # A security that wants no calendar is never asked of a provider that
    # answers nothing else.
    assert plan(v3(securities=[share("symbol:QFND", wants=())]), [exchange])[0] == []


def test_an_earlier_runs_calendar_request_counts_until_max_age_has_passed(registry, tmp_path):
    (exchange,) = registry(_Exchange())
    doc = v3(securities=[share("symbol:QSHR")])
    reqs, _, _ = plan(doc, [exchange])
    data = tmp_path / "data"
    (entry,) = run_entries(make_run(data, "20260715T010000Z", reqs))
    assert (entry["status"], entry["facts"]) == ("ok", [CAL])
    earlier = download.fetched_before(data)
    assert earlier.wants == {("exchange", "symbol:QSHR", CAL): date(2026, 7, 15)}
    reqs, _, cover = plan(doc, [exchange], earlier=earlier)
    assert reqs == [] and cover[CAL]["symbol:QSHR"].had
    assert [r.ref for r in plan(doc, [exchange], earlier=earlier, today=date(2026, 8, 15))[0]] == ["symbol:QSHR"]


def test_a_fetch_that_names_the_calendar_missing_keeps_it_due(registry, tmp_path):
    (exchange,) = registry(_Exchange(missing=True))
    reqs, _, _ = plan(v3(securities=[share("symbol:QSHR")]), [exchange])
    data = tmp_path / "data"
    (entry,) = run_entries(make_run(data, "20260715T010000Z", reqs))
    assert (entry["status"], entry["facts"]) == ("ok", [])
    assert download.fetched_before(data).wants == {}


def test_a_lookup_sees_only_the_wants_due_and_asks_for_those_its_claim_names(registry):
    (filings,) = registry(_Filings())
    looked = {"filings": "2026-07-10"}
    # The lookup is due, the calendar had: the claim sees no want.
    reqs, _, _ = plan(v3(securities=[share("symbol:QSHR", calendar="2026-07-10", missing=["sector"])]), [filings])
    assert asked(reqs) == [("security", "filings", "symbol:QSHR", None, ())]
    assert filings.claimed == [("symbol:QSHR", ())]
    # The lookup had, the calendar due: a request for the calendar.
    reqs, _, cover = plan(v3(securities=[share("symbol:QSHR", missing=["sector"], lookups=looked)]), [filings])
    assert asked(reqs) == [("security", "filings", "symbol:QSHR", None, (CAL,))]
    assert cover[CAL]["symbol:QSHR"].requests == reqs
    # A claim that names no want makes no request for it alone.
    reqs, _, cover = plan(v3(securities=[share("symbol:QRCP", receipt=True, lookups=looked)]), [filings])
    assert reqs == [] and cover[CAL]["symbol:QRCP"].reason == "no provider claims it"
    # Both due: one request, for the calendar too.
    reqs, _, _ = plan(v3(securities=[share("symbol:QSHR")]), [filings])
    assert asked(reqs) == [("security", "filings", "symbol:QSHR", None, (CAL,))]


def test_a_bulk_request_asks_for_the_calendar_its_claim_names(registry, tmp_path):
    (daily,) = registry(_Daily())
    doc = v3(securities=[share("symbol:QRCA", receipt=True), share("symbol:QRCB", receipt=True),
                         share("symbol:QSHR")])
    reqs, _, cover = plan(doc, [daily])
    # A fact the claim names that no security wants is none it asks for.
    assert asked(reqs) == [("bulk", "daily", f"daily:{TODAY}", None, (CAL,))]
    assert [k for k, c in cover[CAL].items() if c.requests] == ["symbol:QRCA", "symbol:QRCB"]
    assert cover[CAL]["symbol:QSHR"].reason == "no provider claims it"
    data = tmp_path / "data"
    (entry,) = run_entries(make_run(data, "20260715T010000Z", reqs))
    assert entry["facts"] == [CAL]
    earlier = download.fetched_before(data)
    # The answer states the first receipt's calendar: it is had, the other
    # left.
    assert earlier.wants == {("daily", "symbol:QRCA", CAL): date(2026, 7, 15)}
    reqs, _, cover = plan(doc, [daily], earlier=earlier)
    assert reqs == [] and download.tally(cover)[0][1:4] == (3, 0, 1)


def _calendar_case(tmp_path, monkeypatch, registry):
    """A wanted list whose calendars end every way the coverage tells:
    refdata.cfg enables comp and exchange; issuer (a fund's calendar) and
    daily (a receipt's) are registered but off."""
    registry(_comp(), _Issuer(), _Exchange(), _Daily())
    monkeypatch.setattr(download.config, "load", lambda known: config.Config(providers=("comp", "exchange")))
    monkeypatch.setattr(download, "today_utc", lambda: TODAY)
    w = write_wanted(tmp_path / "wanted.json", v3(
        [held(wants=[CAL], compositions=["2026-07-10"])],
        [share("symbol:QSHR"), share("symbol:QOLD", calendar="2026-07-01")]))
    return ["--bronze-dir", str(tmp_path / "data"), "--wanted", str(w)]


def test_dry_run_counts_the_calendars_of_funds_and_securities(tmp_path, monkeypatch, registry, capsys):
    args = _calendar_case(tmp_path, monkeypatch, registry)
    assert download.main([*args, "--dry-run"]) == 0
    assert capsys.readouterr().out.splitlines() == [
        "security\texchange\tsymbol:QSHR\tcurrent",
        "facts",
        "dividend_calendar\t3 wanted, 1 planned, 1 had",
        "dividend_calendar\tsymbol:FUNDA\tprovider off: issuer"]
    # Off for a security too, a fund provider is not named for it.
    _, _, cover = plan(v3(securities=[share("symbol:QSHR")]), [download.REGISTRY["comp"]])
    assert cover[CAL]["symbol:QSHR"].reason == "provider off: exchange, daily"


def test_a_run_logs_the_calendars_count_and_records_its_request(tmp_path, monkeypatch, registry, caplog):
    caplog.set_level("INFO", logger="refdata.download")
    args = _calendar_case(tmp_path, monkeypatch, registry)
    assert download.main(args) == 0
    assert "facts: dividend_calendar: 3 wanted, 1 planned, 1 had, 1 provider off: issuer" in caplog.text
    (run,) = (tmp_path / "data").iterdir()
    assert [(e["kind"], e["provider"], e["facts"]) for e in run_entries(run)] == [("security", "exchange", [CAL])]
    assert json.loads((run / "run.json").read_text())["status"] == "complete"


def test_a_security_request_without_wants_records_no_facts(registry, tmp_path):
    (filings,) = registry(_Filings())
    reqs, _, _ = plan(v3(securities=[share("symbol:QSHR", calendar="2026-07-10", missing=["sector"])]), [filings])
    (entry,) = download.execute(reqs, tmp_path, Context(client=None))
    assert "facts" not in entry
