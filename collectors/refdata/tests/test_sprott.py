"""sprott: the home page claims a fund; the holdings table the fund page
embeds as a data: link parses by its header words. Every fund, holding
and identifier is invented."""

import urllib.parse
from datetime import date

import pytest

from providers.base import Context, ProviderError
from providers.sprott import HOST, Sprott, parse_page
from tests.helpers import FakeClient
from wanted import Fund

CSV = ("Security,Market Value,Symbol,SEDOL,Quantity,Weight\n"
       "Qorvant Mining Corp.,6000.00,QRVM,0000001,100.00,60.0\n"
       "Talveri Resources Ltd.,3800.00,TLV CN,0000002,500.00,38.0\n"
       "Cash Equivalent,200.00,,,200.00,2.0\n")
PAGE = ('<div class="holdings-table"><p class="update-date">As of 10/13/2026</p>'
        '<a class="small" download="QURM-holdings.csv" href="data:application/csv;charset=utf-8,'
        + urllib.parse.quote(CSV) + '">Download</a></div>')
HOME = ('<a href="/qurm-sprott-quarry-miners-etf/">QURM</a>'
        '<a href="/qslv-sprott-silver-quarry-etf/">QSLV</a>')


def test_the_embedded_table():
    p = parse_page(PAGE, "symbol:QURM", provider="sprott")
    (c,) = p.compositions
    assert (c["as_of"], c["line_count"], c["weight_sum"]) == (date(2026, 10, 13), 3, "100")
    us, abroad, cash = p.lines
    assert (us["name"], us["ticker"], us["weight"], us["quantity"], us["market_value"]) == \
        ("Qorvant Mining Corp.", "QRVM", "60", "100", "6000")
    # A listing abroad keeps its ticker in the payload; SEDOL is not read.
    assert (abroad["ticker"], abroad["payload"].get("local_ticker")) == (None, "TLV CN")
    assert all("sedol" not in line["payload"] for line in p.lines)
    assert cash["asset_type"] == "Cash"


def test_a_page_without_a_table_or_a_date_is_refused():
    with pytest.raises(ProviderError, match="no holdings table"):
        parse_page("<p>As of 10/13/2026</p>", "symbol:QURM", provider="sprott")
    with pytest.raises(ProviderError, match="no holdings date"):
        parse_page(PAGE.replace("As of 10/13/2026", ""), "symbol:QURM", provider="sprott")


def site(page=PAGE):
    return FakeClient({"-sprott-": page, HOST: HOME})


def test_the_home_page_claims_a_fund(tmp_path):
    ctx = Context(client=site())
    s = Sprott()
    claim = s.claim(Fund(ref="isin:US0000000901", isin="US0000000901", symbol="QURM"), ctx)
    assert claim == {"ref": "isin:US0000000901", "ticker": "QURM", "page": "/qurm-sprott-quarry-miners-etf/"}
    # A fund abroad of the same ticker, and a ticker the site lacks, are not claimed.
    assert s.claim(Fund(ref="isin:XX0000000902", isin="XX0000000902", symbol="QURM"), ctx) is None
    assert s.claim(Fund(ref="symbol:QZZZ", symbol="QZZZ"), ctx) is None
    assert s.fetch(claim, None, tmp_path / "f", ctx) == date(2026, 10, 13)
    assert len(s.parse(tmp_path / "f").lines) == 3


@pytest.mark.parametrize("page,error", [
    ("<p>As of 10/13/2026</p><p>The holdings are shown once a day.</p>", "embeds no holdings table"),
    (PAGE.replace("As of 10/13/2026", ""), "states no holdings date"),
])
def test_a_page_without_a_table_or_a_date_is_not_stored(tmp_path, page, error):
    ctx = Context(client=site(page))
    s = Sprott()
    claim = s.claim(Fund(ref="symbol:QURM", symbol="QURM"), ctx)
    with pytest.raises(ProviderError, match=error):
        s.fetch(claim, None, tmp_path / "f", ctx)
    assert not (tmp_path / "f").exists()


def test_the_probe_reads_the_home_page():
    client = site()
    assert Sprott().probe(Context(client=client)) == "Sprott home page: 2 fund pages"
    assert client.urls() == [HOST + "/"]
