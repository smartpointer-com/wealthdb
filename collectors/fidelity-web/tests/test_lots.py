"""Unit tests for the lot step: the download side (which open and
closed positions it fetches, the index it writes) and the load side
(the lot parsers, `open_lots` and the 'closed_positions' rows of
`closed_lots`).

Every fixture is synthetic: invented account numbers, CUSIPs, symbols
and amounts, in the shapes the positions page's API returns
(root AGENTS.md §4).
"""

from __future__ import annotations

import base64
import gzip
import hashlib
import json
import logging
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402
import load  # noqa: E402
import lot_parsers  # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"

ACCT = "300000003"
ACCT_529 = "400000004"


def _pico(accounts):
    return base64.b64encode(gzip.compress(
        json.dumps({"accounts": accounts}).encode())).decode()


PICO = _pico([
    {"accountId": ACCT, "brokerageAccount": True},
    {"accountId": ACCT_529, "brokerageAccount": True, "cit": True},
    {"accountId": "5000005", "acctType": "Charitable"},
])


def _row(symbol, cusip, qty, cost, *, eligible=True, row_type="POSITION"):
    return {
        "rowType": row_type, "actNum": ACCT,
        "sym": {"name": symbol, "cusip": cusip, "desc": f"{symbol} FUND"},
        "meta": {"acctType": "Brokerage", "securityType": "Equity",
                 "holdingType": "Cash", "cstbasType": "Fidelity",
                 "isEligibleForLots": eligible},
        "qty": {"val": qty, "disp": str(qty)},
        "curVal": {"val": qty * 20.0, "disp": "$"},
        "lstPrStk": {"top": {"val": 20.0, "disp": "$20.00"}},
        "cstBasStk": {"top": {"val": cost, "disp": "$"}},
    }


def _positions(rows):
    return {"rowData": [{"rowType": "ACCOUNT", "actNum": ACCT}] + rows}


def _lot_table(lots, last_page=1):
    head = "".join(
        f'<th scope="col"><span>{h}</span></th>' for h in (
            "Acquired", "Term", "$ Total gain/loss", "% Total gain/loss",
            "Current value", "Quantity", "Average cost basis",
            "Cost basis total"))
    body = "".join(
        "<tr class=\"pvd-table__row posweb-lots-table-row\">"
        + "".join(f"<td>{c}</td>" for c in lot) + "</tr>" for lot in lots)
    return (f'<meta name="nextPage" content="1"><meta name="lastPage" '
            f'content="{last_page}"><div><table><thead><tr>{head}</tr>'
            f"</thead><tbody>{body}</tbody></table></div>")


LOT_A = ("Mar-04-2021", "Long", "+$500.00", "+50.00%", "$1,500.00", "75",
         "$13.3333", "$1,000.00")
LOT_B = ("Dec-15-2025", "Short", "-$20.00", "-4.00%", "$500.00", "25",
         "$20.80", "$520.00")


class FakeResponse:
    def __init__(self, status, body):
        self.status = status
        self._body = body

    @property
    def ok(self):
        return 200 <= self.status < 300

    def body(self):
        return self._body


class FakeApi:
    """Serves the four lot queries from fixtures and records each
    call's endpoint and body."""

    def __init__(self, positions, tables, fail=(), closed=None,
                 closed_lots=None):
        self.positions = positions
        self.tables = tables      # (cusip, page) -> html
        self.fail = set(fail)     # cusips whose lot query answers 500
        self.closed = closed or {}            # account -> closedpositions
        self.closed_lots = closed_lots or {}  # cusip -> closedlots list
        self.calls = []

    def post(self, url, data=None, headers=None, timeout=None):
        endpoint = url.rsplit("/", 1)[1]
        body = json.loads(data)
        self.calls.append((endpoint, body))
        if endpoint == "positions":
            answer = self.positions[body["accts"][0]]
        elif endpoint == "closedpositions":
            answer = self.closed.get(body["accts"][0], {"rowData": []})
        elif body["cusip"] in self.fail:
            return FakeResponse(500, b"")
        elif endpoint == "closedlots":
            answer = self.closed_lots[body["cusip"]]
        else:
            return FakeResponse(200, self.tables[
                (body["cusip"], body["pageNum"])].encode())
        return FakeResponse(200, json.dumps(answer).encode())

    def opened(self, endpoint="openlots"):
        return [b["cusip"] for e, b in self.calls if e == endpoint]


class FakePage:
    """Answers the in-page `fetch` the lot step runs through
    ``page.evaluate``; it has no ``request`` attribute, so a call that
    bypassed the page would fail."""

    def __init__(self, api):
        self.api = api

    def evaluate(self, script, arg):
        assert "fetch(" in script
        resp = self.api.post(arg["url"], data=arg["body"])
        return {"status": resp.status,
                "text": resp.body().decode("utf-8")}


@pytest.fixture
def no_pause(monkeypatch):
    monkeypatch.setattr(download, "_lot_pause", lambda: None)
    monkeypatch.setattr(download, "_capture_positions_context", lambda page: {
        "body": {"accts": ["x"], "pico": PICO, "isRefresh": True,
                 "settings": {"groupBy": "0"}},
        "csrf": "token", "preset_view": "Overview"})


def _api(rows, **kw):
    tables = {("CUSIPAAA1", 1): _lot_table([LOT_A], last_page=2),
              ("CUSIPAAA1", 2): _lot_table([LOT_B], last_page=2),
              ("CUSIPBBB2", 1): _lot_table([LOT_A])}
    return FakeApi({ACCT: _positions(rows)}, tables, **kw)


ROWS = [_row("AAAA", "CUSIPAAA1", 100, 1520.0),
        _row("BBBB", "CUSIPBBB2", 75, 1000.0),
        _row("CORE", "CUSIPCORE", 10, 10.0, eligible=False,
             row_type="POSITION_CORE")]


def _run(tmp_path, slug, api, prev=None, refresh=False, tax_years=(2099,)):
    run_dir = tmp_path / slug
    run_dir.mkdir()
    result = download.scrape_lots(FakePage(api), run_dir, [ACCT, ACCT_529],
                                  prev, refresh=refresh, tax_years=tax_years)
    index = download.read_lot_index(run_dir / "lots")
    return result, index


# ============================================================
# Download side
# ============================================================

def test_first_run_fetches_every_eligible_position_and_every_page(
        tmp_path, no_pause):
    api = _api(ROWS)
    result, index = _run(tmp_path, "20990101T000000Z", api)
    assert result["status"] == "complete"
    assert api.opened() == ["CUSIPAAA1", "CUSIPAAA1", "CUSIPBBB2"]
    # The 529 account is never queried, and the core row has no lots.
    assert [(e, b["accts"]) for e, b in api.calls
            if e in ("positions", "closedpositions")] == [
        ("positions", [ACCT]), ("closedpositions", [ACCT])]
    by_pos = {e["position"]: e for e in index["open"]}
    assert set(by_pos) == {"CUSIPAAA1", "CUSIPBBB2"}
    assert by_pos["CUSIPAAA1"]["fetched_in"] == "20990101T000000Z"
    assert len(by_pos["CUSIPAAA1"]["records"]) == 2
    assert by_pos["CUSIPAAA1"]["quantity"] == 100
    assert by_pos["CUSIPAAA1"]["cost_basis_total"] == 1520.0


def test_the_lot_request_mirrors_the_page(tmp_path, no_pause):
    api = _api(ROWS[1:2])
    _run(tmp_path, "20990101T000000Z", api)
    endpoint, body = api.calls[2]
    assert endpoint == "openlots"
    assert body["acctNum"] == ACCT and body["symbol"] == "BBBB"
    assert body["pageSize"] == download.LOTS_PAGE_SIZE
    assert body["positionDetail"]["quantity"] == 75
    assert body["positionDetail"]["costBasis"] == "Fidelity"
    positions_body = api.calls[0][1]
    assert positions_body["pico"] == PICO and positions_body["isRefresh"] is False
    closed_body = api.calls[1][1]
    assert closed_body["taxYear"] == "2099"
    assert closed_body["ineligibleAccounts"] == [ACCT_529, "5000005"]


def test_unchanged_positions_keep_their_fetch(tmp_path, no_pause):
    _, first = _run(tmp_path, "20990101T000000Z", _api(ROWS))
    changed = [_row("AAAA", "CUSIPAAA1", 110, 1720.0), ROWS[1],
               _row("CCCC", "CUSIPCCC3", 5, 50.0)]
    api = _api(changed)
    api.tables[("CUSIPCCC3", 1)] = _lot_table([LOT_B])
    result, index = _run(tmp_path, "20990102T000000Z", api, prev=first)
    assert sorted(set(api.opened())) == ["CUSIPAAA1", "CUSIPCCC3"]
    by_pos = {e["position"]: e for e in index["open"]}
    assert by_pos["CUSIPBBB2"]["fetched_in"] == "20990101T000000Z"
    assert by_pos["CUSIPBBB2"]["records"] == first["open"][1]["records"]
    assert by_pos["CUSIPCCC3"]["fetched_in"] == "20990102T000000Z"
    assert result["accounts"][download.account_key(ACCT)]["open"]["kept"] == 1


def test_refresh_fetches_every_position(tmp_path, no_pause):
    _, first = _run(tmp_path, "20990101T000000Z", _api(ROWS))
    api = _api(ROWS)
    _run(tmp_path, "20990102T000000Z", api, prev=first, refresh=True)
    assert sorted(set(api.opened())) == ["CUSIPAAA1", "CUSIPBBB2"]


def test_a_failed_position_is_fetched_by_the_next_run(tmp_path, no_pause):
    result, first = _run(tmp_path, "20990101T000000Z",
                         _api(ROWS, fail={"CUSIPBBB2"}))
    assert result["status"] == "partial"
    assert first["status"] == "partial"
    failed = {e["position"]: e for e in first["open"]}["CUSIPBBB2"]
    assert "fetched_in" not in failed
    cov = download.phase_coverage({"lots_results": result})
    assert cov["lots"]["complete"] is False
    api = _api(ROWS)
    _run(tmp_path, "20990102T000000Z", api, prev=first)
    assert api.opened() == ["CUSIPBBB2"]


def _closed_row(cusip, proceeds, cost):
    return {"rowType": "POSITION", "actNum": ACCT,
            "sym": {"name": cusip, "cusip": cusip, "desc": "EXAMPLE BOND"},
            "meta": {"isEligibleForLots": True, "acctType": "Brokerage"},
            "proceedsAmt": {"val": proceeds}, "cstBasCloPos": {"val": cost},
            "totGLCloPos": {"val": proceeds - cost},
            "stGL": {"val": "", "disp": "--"},
            "ltGL": {"val": proceeds - cost}}


def _closed_lot(cusip, proceeds, cost, term="longTerm"):
    other = "shortTerm" if term == "longTerm" else "longTerm"
    return {"symbol": f"undefined({cusip})", "desc": "EXAMPLE BOND",
            "quantity": "10,000", "dateAcquired": "2090-01-02",
            "dateSold": "2099-02-03", "proceeds": f"${proceeds:,.2f}",
            "costBasis": f"${cost:,.2f}",
            term: f"-${cost - proceeds:.2f}", other: "--"}


def test_closed_positions_fetch_only_when_new_or_changed(tmp_path, no_pause):
    closed = {ACCT: {"rowData": [_closed_row("CUSIPDDD4", 9900.0, 10000.0),
                                 _closed_row("CUSIPEEE5", 4950.0, 5000.0)]}}
    lots = {"CUSIPDDD4": [_closed_lot("CUSIPDDD4", 9900.0, 10000.0)],
            "CUSIPEEE5": [_closed_lot("CUSIPEEE5", 4950.0, 5000.0)]}
    api = _api(ROWS, closed=closed, closed_lots=lots)
    _, first = _run(tmp_path, "20990101T000000Z", api)
    assert api.opened("closedlots") == ["CUSIPDDD4", "CUSIPEEE5"]
    body = [b for e, b in api.calls if e == "closedlots"][0]
    assert (body["startDate"], body["endDate"]) == ("2099-01-01",
                                                    "2099-12-31")
    item = first["closed"][0]
    assert (item["tax_year"], item["proceeds"], item["fetched_in"]) == (
        2099, 9900.0, "20990101T000000Z")
    lot_step = load._read_lot_step(tmp_path / "20990101T000000Z", {})
    [rid] = item["records"]
    assert lot_step[1][rid]["endpoint"] == "closedlots"
    assert lot_step[1][rid]["tax_year"] == 2099

    closed[ACCT]["rowData"][1] = _closed_row("CUSIPEEE5", 9900.0, 10000.0)
    api = _api(ROWS, closed=closed, closed_lots=lots)
    _, second = _run(tmp_path, "20990102T000000Z", api, prev=first)
    assert api.opened("closedlots") == ["CUSIPEEE5"]
    assert second["closed"][0]["fetched_in"] == "20990101T000000Z"


def test_a_run_writes_one_bundle_beside_its_index(tmp_path, no_pause):
    """Every response of the step lands in one compressed file, and the
    load reads what the download wrote."""
    _run(tmp_path, "20990101T000000Z", _api(ROWS))
    run_dir = tmp_path / "20990101T000000Z"
    assert sorted(p.name for p in (run_dir / "lots").iterdir()) == [
        "index.json.zst", "lots.jsonl.zst"]
    meta = {"accounts_enumerated": [ACCT]}
    lot_step = load._read_lot_step(run_dir, meta)
    assert [r["endpoint"] for r in lot_step[1].values()] == [
        "positions", "closedpositions", "openlots", "openlots", "openlots"]
    c = sqlite3.connect(":memory:")
    load.apply_migrations(c, MIGRATIONS_DIR)
    assert load._load_open_lots(c, 4070908800, run_dir.name, lot_step) == 3


def test_a_bundle_cut_short_loads_what_it_holds(migrated, tmp_path, caplog):
    slug = "20990101T000000Z"
    dump = _dump(tmp_path, slug, [_item(slug, qty=75, cost=1000.0)],
                 [_lot_table([LOT_A])])
    with open(dump / "lots" / "lots.jsonl", "a") as fh:
        fh.write('{"id": 1, "endpoint": "openl')
    with caplog.at_level(logging.WARNING):
        n = load._load_open_lots(migrated, 4070908800, slug, _step(dump))
    assert n == 1
    assert "line 2 does not parse" in caplog.text


def test_each_tax_year_is_queried_and_kept_apart(tmp_path, no_pause):
    closed = {ACCT: {"rowData": [_closed_row("CUSIPDDD4", 9900.0, 10000.0)]}}
    lots = {"CUSIPDDD4": [_closed_lot("CUSIPDDD4", 9900.0, 10000.0)]}
    api = _api(ROWS, closed=closed, closed_lots=lots)
    _, first = _run(tmp_path, "20990101T000000Z", api,
                    tax_years=(2098, 2099))
    assert [b["taxYear"] for e, b in api.calls
            if e == "closedpositions"] == ["2098", "2099"]
    assert [(b["startDate"], b["endDate"]) for e, b in api.calls
            if e == "closedlots"] == [("2098-01-01", "2098-12-31"),
                                      ("2099-01-01", "2099-12-31")]
    assert [e["tax_year"] for e in first["closed"]] == [2098, 2099]
    api = _api(ROWS, closed=closed, closed_lots=lots)
    _run(tmp_path, "20990102T000000Z", api, prev=first,
         tax_years=(2098, 2099))
    assert api.opened("closedlots") == []


def test_the_default_tax_years_are_last_and_this(tmp_path, no_pause):
    api = _api(ROWS)
    result, _ = _run(tmp_path, "20990101T000000Z", api, tax_years=None)
    this_year = download.datetime.now(download.timezone.utc).year
    assert result["tax_years"] == [this_year - 1, this_year]


def test_the_step_stops_after_repeated_failures(tmp_path, no_pause):
    rows = [_row(f"SYM{i}", f"CUSIP{i:04d}", 1, 1.0) for i in range(8)]
    api = FakeApi({ACCT: _positions(rows)}, {},
                  fail={f"CUSIP{i:04d}" for i in range(8)})
    result, index = _run(tmp_path, "20990101T000000Z", api)
    assert len(api.opened()) == download.LOT_MAX_CONSECUTIVE_FAILURES
    assert result["status"] == "partial" and "in a row" in result["error"]
    assert index["status"] == "partial"
    assert not any("fetched_in" in e for e in index["open"])


def test_a_run_defers_what_exceeds_its_fetch_budget(
        tmp_path, no_pause, monkeypatch):
    monkeypatch.setattr(download, "LOT_MAX_FETCHES_PER_RUN", 1)
    result, first = _run(tmp_path, "20990101T000000Z", _api(ROWS))
    assert result["status"] == "complete"
    counts = result["accounts"][download.account_key(ACCT)]["open"]
    assert (counts["fetched"], counts["deferred"]) == (1, 1)
    by_pos = {e["position"]: e for e in first["open"]}
    assert "fetched_in" in by_pos["CUSIPAAA1"]
    assert "fetched_in" not in by_pos["CUSIPBBB2"]
    # The next run picks up the deferred position and keeps the other.
    api = _api(ROWS)
    _run(tmp_path, "20990102T000000Z", api, prev=first)
    assert api.opened() == ["CUSIPBBB2"]


def test_sigterm_unwinds_instead_of_being_ignored():
    with pytest.raises(KeyboardInterrupt):
        download._exit_on_sigterm(15, None)


def test_the_index_is_written_compact_and_read_in_either_form(tmp_path):
    lots_dir = tmp_path / "lots"
    lots_dir.mkdir()
    download.write_lot_index(lots_dir, {"open": [{"position": "X"}]})
    assert [p.name for p in lots_dir.iterdir()] == ["index.json.zst"]
    assert download.read_lot_index(lots_dir) == {"open": [{"position": "X"}]}
    (lots_dir / "index.json.zst").unlink()
    (lots_dir / "index.json").write_text('{"open": []}')
    assert download.read_lot_index(lots_dir) == {"open": []}
    (lots_dir / "index.json").write_text('{"open": [')
    assert download.read_lot_index(lots_dir) is None


def test_previous_index_is_the_newest_earlier_one(tmp_path):
    for slug, marker in (("20990101T000000Z", "a"), ("20990102T000000Z", "b"),
                         ("20990103T000000Z", "c")):
        (tmp_path / slug / "lots").mkdir(parents=True)
        (tmp_path / slug / "lots" / "index.json").write_text(
            json.dumps({"marker": marker}))
    (tmp_path / "20990102T120000Z").mkdir()     # a dump with no lot step
    assert download.previous_lot_index(
        tmp_path, "20990103T000000Z") == {"marker": "b"}
    # A dump that never completed is never loaded, so its fetches don't count.
    (tmp_path / "20990102T000000Z" / "run.json").write_text(
        json.dumps({"status": "in-progress"}))
    assert download.previous_lot_index(
        tmp_path, "20990103T000000Z") == {"marker": "a"}
    assert download.previous_lot_index(tmp_path, "20990101T000000Z") is None


def test_pico_marks_529_and_non_brokerage_accounts_ineligible():
    accounts = download.decode_pico(PICO)
    assert download.lot_ineligible_accounts(accounts) == ["400000004",
                                                          "5000005"]
    assert download.decode_pico("not base64 at all") == []


def test_only_read_endpoints_can_be_called():
    with pytest.raises(ValueError):
        download._poswebex_post(None, {}, "state/save", {})


def test_openlots_last_page_rejects_a_non_table():
    assert download.openlots_last_page(_lot_table([LOT_A], 3)) == 3
    assert download.openlots_last_page("<html>Log in</html>") is None


# ============================================================
# Parser
# ============================================================

def test_parse_open_lots_reads_every_column():
    lots = lot_parsers.parse_open_lots(_lot_table([LOT_A, LOT_B]))
    assert len(lots) == 2
    a = lots[0]
    assert (a["acquired_date"], a["term"], a["quantity"], a["unit_cost"],
            a["cost_basis"], a["unrealized_gain_loss"], a["current_value"]) \
        == ("2021-03-04", "LONG", 75.0, 13.3333, 1000.0, 500.0, 1500.0)
    assert lots[1]["unrealized_gain_loss"] == -20.0
    assert a["cells"]["% Total gain/loss"] == "+50.00%"


def test_parse_open_lots_maps_columns_by_header():
    html = ('<meta name="lastPage" content="1"><table><thead><tr>'
            "<th>Quantity</th><th>Acquired</th><th>Term</th></tr></thead>"
            "<tbody><tr><td>1,234.5</td><td>Various</td><td>--</td></tr>"
            "</tbody></table>")
    [lot] = lot_parsers.parse_open_lots(html)
    assert lot["quantity"] == 1234.5
    assert lot["acquired_date"] == "Various"
    assert lot["term"] is None
    assert "cost_basis" not in lot


def test_parse_open_lots_refuses_a_page_without_a_lot_table():
    with pytest.raises(ValueError):
        lot_parsers.parse_open_lots("<html><body>Session expired</body></html>")


@pytest.mark.parametrize("short, long_, term, gain", [
    ("--", "-$12.50", "LONG", -12.5),
    ("+$3.00", "--", "SHORT", 3.0),
    ("+$3.00", "-$1.00", None, 2.0),
    ("--", "--", None, None),
])
def test_closed_lot_row_reads_the_term_from_its_gain_column(
        short, long_, term, gain):
    row = lot_parsers.closed_lot_row({
        "desc": "EXAMPLE CORP", "quantity": "1,200", "dateAcquired":
        "2090-01-02", "dateSold": "2099-02-03", "proceeds": "$1,000.00",
        "costBasis": "$1,012.50", "shortTerm": short, "longTerm": long_})
    assert (row["term"], row["realized_gain_loss"]) == (term, gain)
    assert (row["quantity"], row["proceeds"], row["cost_basis"],
            row["acquired_date"], row["disposed_date"]) == (
        1200.0, 1000.0, 1012.5, "2090-01-02", "2099-02-03")


# ============================================================
# Load side
# ============================================================

@pytest.fixture
def migrated():
    c = sqlite3.connect(":memory:")
    load.apply_migrations(c, MIGRATIONS_DIR)
    yield c
    c.close()


def _write_step(dump, index, bodies):
    """A dump's lot step: its index, and a plain bundle holding
    ``bodies`` as records 0, 1, …"""
    (dump / "lots").mkdir(parents=True)
    (dump / "run.json").write_text(json.dumps({
        "status": "complete", "accounts_enumerated": [ACCT]}))
    with open(dump / "lots" / "lots.jsonl", "w") as fh:
        for rid, body in enumerate(bodies):
            fh.write(json.dumps({"id": rid, "body": body}) + "\n")
    (dump / "lots" / "index.json").write_text(json.dumps(index))


def _step(dump):
    return load._read_lot_step(
        dump, json.loads((dump / "run.json").read_text()))


def _dump(tmp_path, slug, open_items, tables):
    dump = tmp_path / slug
    _write_step(dump, {"open": open_items}, tables)
    return dump


def _item(slug, qty=100, cost=1520.0, records=(0,)):
    return {"account": download.account_key(ACCT), "position": "CUSIPAAA1",
            "symbol": "AAAA", "cusip": "CUSIPAAA1", "quantity": qty,
            "cost_basis_total": cost, "fetched_in": slug,
            "records": list(records)}


def test_load_writes_the_fetched_lots(migrated, tmp_path, caplog):
    slug = "20990101T000000Z"
    page2 = _lot_table([LOT_B], 2)
    dump = _dump(tmp_path, slug, [_item(slug, records=(0, 1))],
                 [_lot_table([LOT_A], 2), page2])
    with caplog.at_level(logging.WARNING):
        n = load._load_open_lots(migrated, 4070908800, slug, _step(dump))
    assert n == 2
    assert "sum to" not in caplog.text
    rows = migrated.execute(
        "SELECT account_external_id, instrument_key, lot_index, cusip, "
        "quantity, cost_basis, acquired_date, term, source_sha256, payload "
        "FROM open_lots ORDER BY lot_index").fetchall()
    assert [r[:8] for r in rows] == [
        (ACCT, "AAAA", 0, "CUSIPAAA1", 75.0, 1000.0, "2021-03-04", "LONG"),
        (ACCT, "AAAA", 1, "CUSIPAAA1", 25.0, 520.0, "2025-12-15", "SHORT")]
    assert rows[1][8] == hashlib.sha256(page2.encode()).hexdigest()
    assert json.loads(rows[1][9])["page"] == 2


def test_load_skips_positions_fetched_by_an_earlier_dump(migrated, tmp_path):
    dump = _dump(tmp_path, "20990102T000000Z",
                 [_item("20990101T000000Z")], [])
    assert load._load_open_lots(migrated, 4070995200, dump.name,
                                _step(dump)) == 0


def test_load_warns_when_lots_miss_the_position(migrated, tmp_path, caplog):
    slug = "20990101T000000Z"
    dump = _dump(tmp_path, slug, [_item(slug, qty=101, cost=1000.0)],
                 [_lot_table([LOT_A])])
    with caplog.at_level(logging.WARNING):
        load._load_open_lots(migrated, 4070908800, slug, _step(dump))
    assert "sum to quantity 75.0000, the position states 101.0000" \
        in caplog.text
    assert "cost_basis" not in caplog.text


def test_load_is_idempotent(migrated, tmp_path):
    slug = "20990101T000000Z"
    dump = _dump(tmp_path, slug, [_item(slug, qty=75, cost=1000.0)],
                 [_lot_table([LOT_A])])
    load._load_open_lots(migrated, 4070908800, slug, _step(dump))
    load._load_open_lots(migrated, 4070908800, slug, _step(dump))
    assert migrated.execute("SELECT COUNT(*) FROM open_lots").fetchone()[0] == 1


def _closed_dump(tmp_path, slug, lots, proceeds=9900.0, cost=10000.0):
    dump = tmp_path / slug
    _write_step(dump, {"closed": [{
        "account": download.account_key(ACCT), "position": "CUSIPDDD4",
        "symbol": "CUSIPDDD4", "cusip": "CUSIPDDD4", "tax_year": 2099,
        "proceeds": proceeds, "cost_basis": cost, "gain_loss": proceeds - cost,
        "fetched_in": slug, "records": [0]}]}, [json.dumps(lots)])
    return slug, _step(dump)


def test_load_writes_closed_position_lots(migrated, tmp_path, caplog):
    name, step = _closed_dump(tmp_path, "20990101T000000Z", [
        _closed_lot("CUSIPDDD4", 4950.0, 5000.0),
        _closed_lot("CUSIPDDD4", 4950.0, 5000.0)])
    with caplog.at_level(logging.WARNING):
        assert load._load_closed_positions(migrated, name, step) == 2
    assert "sum to" not in caplog.text
    rows = migrated.execute(
        "SELECT document_kind, account_external_id, tax_year, instrument_key, "
        "cusip, security_name, quantity, acquired_date, disposed_date, "
        "proceeds, cost_basis, realized_gain_loss, term, action "
        "FROM closed_lots").fetchall()
    # Two identical lots of one closed position stay two.
    assert rows == [("closed_positions", ACCT, 2099, "CUSIPDDD4", "CUSIPDDD4",
                     "EXAMPLE BOND", 10000.0, "2090-01-02", "2099-02-03",
                     4950.0, 5000.0, -50.0, "LONG", None)] * 2


def test_a_new_fetch_replaces_the_closed_positions_rows(migrated, tmp_path):
    name, step = _closed_dump(tmp_path, "20990101T000000Z",
                              [_closed_lot("CUSIPDDD4", 9900.0, 10000.0)])
    load._load_closed_positions(migrated, name, step)
    name, step = _closed_dump(tmp_path, "20990102T000000Z", [
        _closed_lot("CUSIPDDD4", 9900.0, 10000.0),
        _closed_lot("CUSIPDDD4", 990.0, 1000.0)], 10890.0, 11000.0)
    load._load_closed_positions(migrated, name, step)
    assert sorted(r[0] for r in migrated.execute(
        "SELECT proceeds FROM closed_lots")) == [990.0, 9900.0]


def test_load_warns_when_closed_lots_miss_the_position(
        migrated, tmp_path, caplog):
    name, step = _closed_dump(tmp_path, "20990101T000000Z",
                              [_closed_lot("CUSIPDDD4", 4950.0, 5000.0)])
    with caplog.at_level(logging.WARNING):
        load._load_closed_positions(migrated, name, step)
    assert "sum to proceeds 4950.00, the position states 9900.00" \
        in caplog.text
