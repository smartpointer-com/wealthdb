"""The provider contract's helpers: silver rows, numbers, amounts, a
distribution's parts, identifiers, the run's cache and the runs before
it."""

import json
import traceback
from datetime import date

import pytest

import wanted
from providers.base import (COLUMNS, Context, ProviderError, amount, dec, direct_share, earlier_answers, earlier_docs,
                            earlier_metas, event_id, event_row, facts_missing, ident, kinds_paid, latest_through,
                            merge_events, percent, present, read_state, row, through_day, us_symbol)


def test_a_row_holds_every_column_but_run_at():
    r = row("counterparties", fund_ref="isin:XX0000000001", role="swap", name="Example Bank")
    assert set(r) == set(COLUMNS["counterparties"]) - {"run_at"}
    assert (r["lei"], r["payload"], r["name"]) == (None, {}, "Example Bank")
    # Each row has a payload of its own.
    assert row("lines")["payload"] is not row("lines")["payload"]
    with pytest.raises(KeyError, match="no column wieght"):
        row("lines", wieght="10")


@pytest.mark.parametrize("value,want", [
    ("1,234.50", "1234.5"), ("-1,234,567", "-1234567"), ("12.5%", "12.5"), ("1'234.5", "1234.5"),
    ("3.99E-01", "0.399"), ("-0", "0"), (60.0, "60"),
    # A comma that separates no groups of three could be a decimal mark.
    ("0,25", None), ("12,5", None), ("1,23", None), ("1,2345", None),
    ("", None), ("-", None), ("--", None), ("N/A", None), ("n/a", None), ("NA", None), ("abc", None), ("NaN", None),
    (None, None),
])
def test_dec(value, want):
    assert dec(value) == want


def test_a_fraction_reads_as_percent():
    assert (percent("0.25"), percent(0.001), percent("3.99E-01"), percent("abc"), percent(None)) == \
        ("25", "0.1", "39.9", None, None)


def test_a_payload_keeps_what_holds_a_value():
    payload = {"found": False, "listings": 0, "name": "", "lei": None, "tickers": ["QABC"]}
    assert present(payload) == {"tickers": ["QABC"]}
    # An answer of no or none is a value too, where the payload says so.
    assert present(payload, keep_falsy=True) == {"found": False, "listings": 0, "tickers": ["QABC"]}


def test_ident_reads_the_field_else_the_reference():
    f = wanted.Fund(ref="isin:xx0000000001 ")
    assert (ident(f, "isin"), ident(f, "cusip"), ident(f, "symbol")) == ("XX0000000001", "", "")
    s = wanted.Security(key="symbol:qabc", isin=" xx0000000002")
    assert (ident(s, "isin"), ident(s, "symbol")) == ("XX0000000002", "QABC")


def test_a_ticker_names_a_us_fund_only():
    assert us_symbol(wanted.Fund(ref="symbol:QABC")) == "QABC"
    assert us_symbol(wanted.Fund(ref="isin:US0000000001", symbol="QABC")) == "QABC"
    assert us_symbol(wanted.Fund(ref="isin:XX0000000001", symbol="QABC")) == ""


def test_once_asks_once_and_fails_again_with_its_first_traceback():
    calls = []

    def load():
        calls.append(1)
        raise ValueError("the list is gone")
    ctx = Context(client=None)
    depths = []
    for _ in range(3):
        try:
            ctx.once("list", load)
        except ValueError as e:
            depths.append(len(traceback.extract_tb(e.__traceback__)))
    assert calls == [1] and depths[0] == depths[1] == depths[2]
    assert ctx.once("other", lambda: 42) == 42 and ctx.once("other", lambda: 0) == 42


def test_a_dividend_event_names_a_kind_and_status_of_the_contract():
    r = row("dividend_events", ref="symbol:SPY", provider="example", event_id="x", kind="dividend")
    assert set(r) == set(COLUMNS["dividend_events"]) - {"run_at"}
    assert (r["status"], r["amount"], r["payload"]) == (None, None, {})
    assert row("dividend_events", kind="capital_gain", status="estimated")["status"] == "estimated"
    for bad in ({"kind": "interest"}, {"kind": None}, {"kind": "dividend", "status": "paid"}):
        with pytest.raises(ValueError, match="no dividend event is of kind"):
            row("dividend_events", **bad)


def test_an_events_id_joins_its_kind_and_days_a_day_unstated_blank():
    assert event_id("dividend", date(2026, 9, 17), date(2026, 9, 18), date(2026, 9, 30)) == \
        "dividend|2026-09-17|2026-09-18|2026-09-30"
    assert event_id("capital_gain", "2026-12-17", None) == "capital_gain|2026-12-17||"


def test_facts_missing_names_the_facts_a_fetch_did_not_get():
    assert facts_missing([]) == {} and facts_missing(iter(())) == {}
    assert facts_missing(["dividend_calendar"]) == {"facts_missing": ["dividend_calendar"]}



@pytest.mark.parametrize("values,want", [
    (("USD 1.6714",), ("1.6714", "USD")), (("$0.25",), ("0.25", "USD")), (("0.4012 EUR",), ("0.4012", "EUR")),
    (("1’234.50",), ("1234.5", None)), (("1,234.5",), ("1234.5", None)), ((0.1234,), ("0.1234", None)),
    # A number as it stands, a leading dot or an exponent too.
    ((".25",), ("0.25", None)), (("1.2E-3",), ("0.0012", None)), (("$.25",), ("0.25", "USD")),
    # A raw value first, its text naming the currency.
    ((0.5, "CHF 0.50"), ("0.5", "CHF")), ((None, "$1.00"), ("1", "USD")),
    # A blank, a zero, no number: none.
    (("",), (None, None)), (("0.00",), (None, None)), ((0,), (None, None)), (("N/A",), (None, None)),
    ((True,), (None, None)), ((None,), (None, None)),
])
def test_an_amount_reads_its_number_and_currency_as_a_source_writes_them(values, want):
    assert amount(*values) == want


def test_a_distributions_parts_sum_by_kind():
    kinds = (("income", "dividend"), ("short", "capital_gain"), ("long", "capital_gain"))
    assert kinds_paid({"income": "0.5", "short": "0.01", "long": "0.025"}, kinds) == {
        "dividend": "0.5", "capital_gain": "0.035"}
    assert kinds_paid({"income": None, "short": "0.01", "long": None}, kinds) == {"capital_gain": "0.01"}
    assert kinds_paid({"income": None}, kinds) == {}


def _event(amount, currency="USD", ref="isin:XX0000000001", **payload):
    return row("dividend_events", ref=ref, provider="ishares", event_id="dividend|2026-06-15|2026-06-15|2026-06-19",
               as_of=date(2026, 6, 30), kind="dividend", ex_date=date(2026, 6, 15), amount=amount, currency=currency,
               payload=payload)


def test_rows_of_one_id_are_one_event_their_parts_summed():
    regular, special = _event("0.25", label="regular"), _event("0.1", label="special")
    (e,) = merge_events([regular, special, dict(regular)])
    # A row stated twice is one part.
    assert (e["amount"], e["currency"], e["ex_date"]) == ("0.35", "USD", date(2026, 6, 15))
    assert e["payload"] == {"parts": [{"amount": "0.25", "currency": "USD", "label": "regular"},
                                      {"amount": "0.1", "currency": "USD", "label": "special"}]}
    # Two currencies, or a part without an amount: the first part stands.
    assert merge_events([_event("0.25"), _event("0.2", "EUR")])[0]["amount"] == "0.25"
    assert merge_events([_event(None, None), _event("0.2")])[0]["amount"] is None
    # One row, or rows of other refs, stand as they are, in their order.
    other = _event("0.3", ref="isin:XX0000000002")
    assert merge_events([regular, other]) == [regular, other]


def test_earlier_answers_are_a_providers_served_requests_of_complete_runs(tmp_path):
    def run(slug, status, requests):
        (tmp_path / slug).mkdir()
        (tmp_path / slug / "run.json").write_text(json.dumps({"status": status, "requests": requests}))
    ok = {"kind": "bulk", "provider": "six", "ref": "r", "status": "ok", "dir": "bulk/six/r"}
    run("20260714T120000Z", "complete", [ok, {**ok, "status": "error"}, {**ok, "dir": None},
                                         {**ok, "provider": "jpx"}, {**ok, "kind": "security"}])
    run("20260715T120000Z", "in-progress", [ok])
    run("20260716T120000Z", "complete", [ok])
    # A run whose manifest does not read counts for nothing.
    (tmp_path / "20260717T120000Z").mkdir()
    (tmp_path / "20260717T120000Z" / "run.json").write_text("{")
    got = [(r.name, e["kind"], f.relative_to(tmp_path).as_posix()) for r, e, f in earlier_answers(tmp_path, "six")]
    assert got == [("20260714T120000Z", "bulk", "20260714T120000Z/bulk/six/r"),
                   ("20260714T120000Z", "security", "20260714T120000Z/bulk/six/r"),
                   ("20260716T120000Z", "bulk", "20260716T120000Z/bulk/six/r")]
    assert len(list(earlier_answers(tmp_path, "six", "bulk"))) == 2
    assert list(earlier_answers(None, "six")) == [] and list(earlier_answers(tmp_path / "none", "six")) == []


def test_earlier_files_and_metas_are_those_that_read_and_the_latest_through_is_the_newest(tmp_path):
    def kept(slug, meta, name="meta.json"):
        folder = tmp_path / slug / "bulk" / "six" / "r"
        folder.mkdir(parents=True)
        (folder / name).write_text(meta)
        (tmp_path / slug / "run.json").write_text(json.dumps({"status": "complete", "requests": [
            {"kind": "bulk", "provider": "six", "ref": "r", "status": "ok", "dir": "bulk/six/r"}]}))
    kept("20260710T120000Z", '{"through": "2026-07-09"}')
    kept("20260711T120000Z", "{")
    kept("20260712T120000Z", "[1]")
    kept("20260713T120000Z", '{"through": "2026-07-01"}')
    kept("20260714T120000Z", '{"through": "x"}')
    assert [d for _, d in earlier_docs(tmp_path, "six")] == [{"through": "2026-07-09"}, [1], {"through": "2026-07-01"},
                                                             {"through": "x"}]
    assert [f.parts[-4] for f, _ in earlier_metas(tmp_path, "six")] == ["20260710T120000Z", "20260713T120000Z",
                                                                        "20260714T120000Z"]
    assert [through_day(m) for _, m in earlier_metas(tmp_path, "six")] == [date(2026, 7, 9), date(2026, 7, 1), None]
    assert latest_through(tmp_path, "six") == date(2026, 7, 9) and latest_through(tmp_path, "six", "fund") is None
    assert through_day({}) is None and through_day({"through": 7}) is None


@pytest.mark.parametrize("text,refusal", [
    ("{", "does not read"), ("[]", r"does not read \(no object\)"), ('{"a": "x"}', r"does not read \(.*'x'")])
def test_state_a_provider_keeps_is_read_or_refused_never_guessed(tmp_path, text, refusal):
    path = tmp_path / "state.json"
    assert read_state(path, "the state", date.fromisoformat) == {}
    path.write_text('{"a": "2026-07-05"}')
    assert read_state(path, "the state", date.fromisoformat) == {"a": date(2026, 7, 5)}
    path.write_text(text)
    with pytest.raises(ProviderError, match=refusal + ".*; the state cannot be kept without it"):
        read_state(path, "the state", date.fromisoformat)


def test_a_direct_share_is_public_equity_held_directly_and_no_receipt():
    share = wanted.Security(key="isin:XX0000000001", asset_class="public_equity")
    assert direct_share(share)
    for other in (wanted.Security(key="k", asset_class="public_equity", receipt=True),
                  wanted.Security(key="k", asset_class="public_equity", scope="constituent"),
                  wanted.Security(key="k", asset_class="fund")):
        assert not direct_share(other)


def test_an_event_row_takes_its_id_from_its_days_unless_given_one():
    days = {"ex_date": date(2026, 9, 10), "pay_date": date(2026, 9, 17)}
    r = event_row("symbol:SPY", "example", date(2026, 10, 1), "dividend", days, amount="0.5")
    assert (r["event_id"], r["ex_date"], r["record_date"], r["pay_date"], r["as_of"], r["amount"]) == (
        "dividend|2026-09-10||2026-09-17", date(2026, 9, 10), None, date(2026, 9, 17), date(2026, 10, 1), "0.5")
    assert event_row("symbol:SPY", "example", date(2026, 10, 1), "dividend", days, event_id="n1")["event_id"] == "n1"
