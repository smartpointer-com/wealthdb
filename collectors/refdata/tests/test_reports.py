"""reports: a fund's inventory read from its annual or semi-annual report,
in each layout; the swisscanto and swissfunddata claims. Every fund,
holding, figure and identifier is invented."""

from datetime import date
from decimal import Decimal

import pytest

import fetcher
from providers import reports
from providers.base import Context, ProviderError
from providers.reports import number, own_title, parse_report, report_date
from providers.swisscanto import Swisscanto
from providers.swissfunddata import ENTRY, SwissFundData, documents
from tests.helpers import FakeClient, pdf
from wanted import Fund

# A Luxembourg schedule in English: a contents page that names the
# schedules, a table of net asset values, two sub-funds whose title sits
# at the foot of each page, derivatives after the total.
LU = [
    "Semi-Annual Report\nfor the financial period ended 30 June 2026\nQumbra Harbour Fund Range",
    "Schedule of Investments\nQumbra Brindle Qland UCITS ETF 3\nQumbra Qland Value UCITS ETF 5",
    "Qumbra Brindle Qland UCITS ETF QLD dis 23.48 22.91 2.49",
    "Description Quantity\nMarket\nValue QLD\n% of\nNet Assets\nCommon Stocks\nBanks (60.00%)\n"
    "Talveri Bank Ltd. 1,000 600,000 59.99\nTotal Banks 600,000 59.99\nMining (39.81%)\n"
    "Qorvant Mining Corp. 2,000 398,000 39.80\nTiny Resources Ltd. 10 40 0.00\nTotal Mining 398,040 39.80\n"
    "Qumbra Brindle Qland UCITS ETF\nSchedule of Investments as at 30 June 2026 (Expressed in QLD) (Unaudited)",
    "Description Quantity\nMarket\nValue QLD\n% of\nNet Assets\nMining (39.81%) (continued)\n"
    "Zelmaro Metals Corp. 50 1,000 0.10\nTotal Mining 1,000 0.10\nTotal Common Stocks 999,040 99.89\n"
    "TOTAL INVESTMENTS (Cost QLD 1) 999,040 99.89\nForward Foreign Exchange Contracts\n"
    "CHF 1,000 QLD 2,000 Example Bank London 02 July 2026 -5 0.00\n"
    "Qumbra Brindle Qland UCITS ETF\nSchedule of Investments as at 30 June 2026 (Expressed in QLD) (Unaudited)",
    "Description Quantity\nMarket\nValue QLD\n% of\nNet Assets\nCommon Stocks\nBanks (100.00%)\n"
    "Other Bank Ltd. 5 500 100.00\nQumbra Qland Value UCITS ETF\n"
    "Schedule of Investments as at 30 June 2026 (Expressed in QLD) (Unaudited)",
]


def test_a_schedule_reads_the_held_sub_fund_weighted_by_value():
    p = parse_report(LU, "Qumbra Harbour Fund Range - Qumbra Brindle Qland UCITS ETF QLD dis", "isin:LU0000000101",
                     provider="swissfunddata")
    (c,) = p.compositions
    assert (c["as_of"], c["line_count"], c["source_kind"]) == (date(2026, 6, 30), 4, "report")
    names = [ln["name"] for ln in p.lines]
    assert names == ["Talveri Bank Ltd.", "Qorvant Mining Corp.", "Tiny Resources Ltd.", "Zelmaro Metals Corp."]
    bank, mine, tiny, _ = p.lines
    assert (bank["industry"], mine["industry"], bank["asset_type"]) == ("Banks", "Mining", "Common Stocks")
    assert bank["sector"] is None and Decimal(bank["market_value"]) == 600000
    # The printed 99.89% spread by value: a holding printed at 0.00% keeps
    # its weight.
    assert sum(Decimal(ln["weight"]) for ln in p.lines) == pytest.approx(Decimal("99.89"))
    assert Decimal(tiny["weight"]) > 0


# An Irish schedule in German: the title follows the heading and the day.
IE = [
    "Halbjahresbericht\n2026\nQumbra Quarrow ETF plc\nHalbjahresbericht für den Berichtszeitraum zum 30. Juni 2026",
    "Halbjahresbericht 7\nAufstellung des Wertpapierbestandes zum\n30. Juni 2026\nQumbra Qland UCITS ETF\n"
    "Menge\nZeitwert\nQLD\n% des Nettovermögens\nStammaktien\n"
    "Banken (70,50 %) (31. Dezember 2025: 70,00 %)\nTalveri Bank Ltd. 1.000 705.000 70,50\n"
    "Banken insgesamt 705.000 70,50\nBergbau (29,00 %) (31. Dezember 2025: 30,00 %)\n"
    "Qorvant Mining Corp. 2.000 290.000 29,00\nBergbau insgesamt 290.000 29,00\n"
    "Stammaktien, insgesamt 995.000 99,50\nDerivative Finanzinstrumente\n"
    "Offene Devisenterminkontrakte* ((0,01 %)) (31. Dezember 2025: 0,02 %)\n"
    "02.07.2026 CHF 806.700 QLD 1.084.000 (4.270) (0,00)",
]


def test_a_german_schedule_reads_its_numbers_and_stops_at_the_derivatives():
    p = parse_report(IE, "Qumbra Quarrow ETF Plc-Qland UCITS ETF QLD-dis", "isin:IE0000000102",
                     provider="swissfunddata")
    assert [(ln["name"], Decimal(ln["quantity"]), ln["industry"]) for ln in p.lines] == \
        [("Talveri Bank Ltd.", 1000, "Banken"), ("Qorvant Mining Corp.", 2000, "Bergbau")]
    assert p.compositions[0]["as_of"] == date(2026, 6, 30)
    assert sum(Decimal(ln["weight"]) for ln in p.lines) == pytest.approx(Decimal("99.50"))


# A Swiss inventory: a running header that carries the page number, a
# country block that ends in a TOTAL line, a section heading printed with
# a drop cap, numbers grouped by spaces.
CH = [
    "Qumbra Alpstream ETFs – Qumbra Qland 7 ® ETF Semi-Annual Report as of 31 December 2025\nMost important figures\n"
    "Class (CHF) A-dis XX0000000103",
    "Qumbra Alpstream ETFs – Qumbra Qland 8 ® ETF Semi-Annual Report as of 31 December 2025\nInventory of Fund assets\n"
    "Securities traded on an exchange\n Thereof\nSecurity Nominal Purchases1\nRegistered shares egistered shares\n"
    "Austria\nQORVANT MINING AG NPV* CHF 96 400 6 800 103 200 718 600 29.90\nTOTAL Austria 718 600 29.90\n"
    "Switzerland\nTALVERI BANK AG CHF0.10 (REGD)* CHF 4 150 1 230 2 080 3 300 1 247 900 69.95 3 300",
    "Qumbra Alpstream ETFs – Qumbra Qland 9 ® ETF Semi-Annual Report as of 31 December 2025\n Thereof\n"
    "ZELMARO FOODS AG CHF1* CHF 1 300 2 100 3 400 106 180 0.01\nTOTAL Switzerland 1 354 080 69.96\n"
    "Total Registered shares 2 072 680 99.86\nTotal securities and similar instruments 2 072 680 99.86\n"
    "Bank deposits at sight 1 662 140 0.08",
    "Qumbra Alpstream ETFs – Qumbra Qland Mid 10 ® ETF Semi-Annual Report as of 31 December 2025\nInventory of Fund assets\n"
    "Switzerland\nOTHER AG CHF1* CHF 1 1 1 1 100 100.00\nTOTAL Switzerland 100 100.00",
]


def test_an_inventory_takes_its_countries_from_the_totals_and_prints_its_percent():
    p = parse_report(CH, "Qumbra Alpstream ETFs - Qumbra Qland® ETF (CHF) A-dis", "isin:XX0000000103", provider="swissfunddata")
    assert [(ln["name"], ln["country"], Decimal(ln["weight"])) for ln in p.lines] == [
        ("QORVANT MINING AG NPV", "Austria", Decimal("29.90")),
        ("TALVERI BANK AG CHF0.10 (REGD)", "Switzerland", Decimal("69.95")),
        ("ZELMARO FOODS AG CHF1", "Switzerland", Decimal("0.01"))]
    assert {ln["asset_type"] for ln in p.lines} == {"Registered shares"}
    assert p.compositions[0]["as_of"] == date(2025, 12, 31)


# A Swisscanto inventory: an ISIN per line, the sub-fund in each page's
# footer.
SC = [
    "Jahresbericht per 28.02.2026\nSwisscanto Brindle Tracker Q",
    "Inventar des Fondsvermögens am Ende der Rechnungsperiode\nAnteilsklasse NT QLD\nBezeichnung\n"
    "Aktien und sonstige Beteiligungswertpapiere und -rechte 1'000'000.00 99.80\n"
    "XX0000000111 QORVANT MINING CORP 36'100 5'400 2'500 39'000 QLD 18.40 717'600.00 71.62 a)\n"
    "XX0000000112 TALVERI BANK LTD 20'000 4'092 1'500 22'592 QLD 12.50 282'400.00 28.18 a)\n"
    "XX0000000113 SOLD OUT CORP 6'130 0 6'130 0 QLD 0.00 0.00 0.00\n"
    "XX0000000116 QORVANT PROPERTY FUND\nRESIDENTIAL\n 1 0 0 1 QLD 1.00 0.10 0.00 a)\n"
    "Jahresbericht per 28.02.2026\nSwisscanto Brindle Stock Tracker Qland 24/412\nSwisscanto Fondsleitung AG",
    "Inventar des Fondsvermögens am Ende der Rechnungsperiode\n"
    "XX0000000114 OTHER CORP 1 0 0 1 QLD 1.00 1.00 100.00 a)\n"
    "Jahresbericht per 28.02.2026\nSwisscanto Brindle Stock Tracker Qland Small 25/412\nSwisscanto Fondsleitung AG",
]


def test_an_isin_inventory_reads_each_position_and_leaves_out_one_sold():
    p = parse_report(SC, "Swisscanto Brindle Tracker Q - Stock Tracker Qland NT QLD", "isin:CH0000000115",
                     provider="swisscanto")
    # A long name wraps onto the next line, its figures onto the one after.
    assert [(ln["isin"], ln["currency"]) for ln in p.lines] == \
        [("XX0000000111", "QLD"), ("XX0000000112", "QLD"), ("XX0000000116", "QLD")]
    assert p.lines[2]["name"] == "QORVANT PROPERTY FUND RESIDENTIAL"
    assert p.lines[0]["asset_type"] == "Aktien und sonstige Beteiligungswertpapiere und -rechte"
    assert p.compositions[0]["as_of"] == date(2026, 2, 28)


def test_a_title_is_the_fullest_whose_words_the_name_holds():
    titles = ["Qumbra Qland UCITS ETF", "Qumbra Brindle Qland UCITS ETF", "Qumbra Brindle Qland hEUR UCITS ETF"]
    assert own_title(titles, "Qumbra Harbour - Qumbra Brindle Qland UCITS ETF QLD dis") == "Qumbra Brindle Qland UCITS ETF"
    assert own_title(["Swisscanto Brindle Stock Tracker Qland"],
                     "Swisscanto Brindle Tracker Q-Stock Tracker Qland") == "Swisscanto Brindle Stock Tracker Qland"
    # A title of fewer than three words is no sub-fund's.
    assert own_title(["UCITS ETF"], "Qumbra Brindle Qland UCITS ETF") is None
    # A ligature the text lost reads as the name's word.
    assert own_title(["Swisscanto Brindle Tracker Qualifed", "Swisscanto Brindle Tracker"],
                     "Swisscanto Brindle Tracker Qualified NT") == "Swisscanto Brindle Tracker Qualifed"
    assert own_title(titles, "Another Fund") is None
    with pytest.raises(ProviderError, match="no inventory for"):
        parse_report(LU, "Another Fund", "isin:LU0000000104", provider="swissfunddata")


def test_a_report_states_its_day_in_either_language():
    assert report_date(["Semi-Annual Report as of 31 December 2025"]) == date(2025, 12, 31)
    assert report_date(["Halbjahresbericht zum 30. Juni 2026"]) == date(2026, 6, 30)
    assert report_date(["Jahresbericht per 28.02.2026"]) == date(2026, 2, 28)
    assert report_date(["no day"]) is None


SC_API = {"fundDetails": {"summary": {"fullName": "Swisscanto Brindle Stock Tracker Qland NT QLD"}, "sections": [
    {"publications": [
        {"name": {"value": "funddetail.publication.priip"}, "files": [{"href": "https://x/priip.pdf",
                                                                       "fileLanguages": ["EN"]}]},
        {"name": {"value": "funddetail.publication.FYRREP"}, "files": [{"href": "https://x/JB_Q_deu.pdf",
                                                                        "fileLanguages": ["de"]}]},
        {"name": {"value": "funddetail.publication.HYRREP"}, "files": [{"href": "https://x/HJB_Q_deu.pdf",
                                                                        "fileLanguages": ["de"]}]}]}]}}


SC_PDF = pdf([p.splitlines() for p in SC])


def test_swisscanto_claims_its_fund_and_fetches_and_reads_both_reports(tmp_path):
    site = FakeClient({"/funds/CH0000000115": SC_API, ".pdf": SC_PDF})
    ctx = Context(client=site)
    s = Swisscanto()
    claim = s.claim(Fund(ref="isin:CH0000000115", isin="CH0000000115", name="Swisscanto Tracker Qland NT"), ctx)
    assert claim["name"] == "Swisscanto Brindle Stock Tracker Qland NT QLD"
    assert claim["reports"] == [("annual", "https://x/JB_Q_deu.pdf"), ("semiannual", "https://x/HJB_Q_deu.pdf")]
    # Not Swisscanto's, or not Swiss: not asked.
    assert s.claim(Fund(ref="isin:CH0000000116", isin="CH0000000116", name="Other Fund"), ctx) is None
    assert s.fetch(claim, None, tmp_path / "f", ctx) == date(2026, 2, 28)
    parsed = s.parse(tmp_path / "f")
    # Each report is a composition; of two that name one day, the
    # semi-annual report's stands alone.
    assert len(parsed.compositions) == 1 and [ln["line_no"] for ln in parsed.lines] == [1, 2, 3]
    assert sum(1 for u in site.urls() if u.endswith(".pdf")) == 2


DOCS = ('<tr><td></td><td><a href="/sfdpub/en/funds/docs/9">\n  Qumbra Harbour - Qumbra Brindle Qland UCITS ETF QLD dis\n'
        '</a></td><td>8</td><td><a href="/sfdpub/docs/prp-1_01_01-20260505-en.pdf" title="PRIIPS-KID as of 05.05.2026">'
        '</a><a href="/sfdpub/docs/anr-1_01_01-20260612-en.pdf" title="Annual report as of 12.06.2026, published">'
        '</a><a href="/sfdpub/docs/sar-1_01_01-20260630-de.pdf" title="Semi-annual report as of 30.06.2026">'
        '</a></td></tr>')


def swissfunddata(docs=DOCS, report=b"%PDF-1.4"):
    # The disclaimer's answer leads to a page the site does not have.
    return FakeClient({"/funds/documents": docs, "sfd-entry": lambda url, data: 404 if data else "<p>entry</p>",
                       ".pdf": report})


def test_swissfunddata_states_the_category_once_and_claims_by_the_document_list():
    site = swissfunddata()
    ctx = Context(client=site)
    s = SwissFundData()
    claim = s.claim(Fund(ref="isin:LU0000000101", isin="LU0000000101", name="Qland ETF"), ctx)
    assert claim == {"ref": "isin:LU0000000101", "isin": "LU0000000101",
                     "name": "Qumbra Harbour - Qumbra Brindle Qland UCITS ETF QLD dis",
                     "reports": [("annual", "https://www.swissfunddata.ch/sfdpub/docs/anr-1_01_01-20260612-en.pdf"),
                                 ("semiannual", "https://www.swissfunddata.ch/sfdpub/docs/sar-1_01_01-20260630-de.pdf")]}
    s.claim(Fund(ref="isin:IE0000000102", isin="IE0000000102", name="Other"), ctx)
    assert [(u, d) for m, u, d in site.asked if m == "POST"] == [(ENTRY, {"_action_set": "I agree"})]
    # A US fund is not asked.
    assert s.claim(Fund(ref="isin:US0000000105", isin="US0000000105", name="Q"), ctx) is None


def test_swissfunddata_refuses_when_the_entry_comes_back():
    site = swissfunddata('<form><input name="_action_set" value="I agree"></form>')
    with pytest.raises(ProviderError, match="investor category"):
        SwissFundData().claim(Fund(ref="isin:LU0000000101", isin="LU0000000101", name="Q"), Context(client=site))
    assert documents("<p>nothing</p>") == (None, [])


def test_an_inventory_that_ends_before_a_country_total_keeps_its_holdings():
    pages = [CH[0], CH[1].replace("Austria\n", "").replace("TOTAL Austria 718 600 29.90\n", "")
             + "\nTotal securities and similar instruments 2 072 680 99.86"]
    p = parse_report(pages, "Qumbra Alpstream ETFs - Qumbra Qland® ETF (CHF) A-dis", "isin:XX0000000103",
                     provider="swissfunddata")
    assert [ln["name"] for ln in p.lines] == ["QORVANT MINING AG NPV", "TALVERI BANK AG CHF0.10 (REGD)"]
    assert {ln["country"] for ln in p.lines} == {None}


def test_a_contents_page_names_no_schedule():
    # Page numbers on lines of their own, and a table of net asset values
    # under a heading that reads like a section: neither is the schedule.
    toc = ("Contents\nSchedule of Investments\nQumbra Brindle Qland UCITS ETF\n3\nFunds\n"
           "Qumbra Brindle Qland UCITS ETF QLD dis 1,000 23.48 2.49")
    p = parse_report([LU[0], toc, *LU[2:]], "Qumbra Harbour Fund Range - Qumbra Brindle Qland UCITS ETF QLD dis",
                     "isin:LU0000000101", provider="swissfunddata")
    assert len(p.lines) == 4 and p.compositions[0]["payload"] == {"sub_fund": "Qumbra Brindle Qland UCITS ETF"}


def test_a_name_the_title_found_does_not_explain_is_refused():
    # SRX where the report would say otherwise: the title found is a
    # sibling's, so nothing is read rather than its inventory.
    with pytest.raises(ProviderError, match="srx"):
        parse_report(LU, "Qumbra Harbour Fund Range - Qumbra Brindle Qland SRX UCITS ETF QLD dis",
                     "isin:LU0000000101", provider="swissfunddata")


def test_numbers_read_in_either_convention_and_in_parentheses():
    assert number("(4.270)", ",") == Decimal("-4270") and number("-0,12", ",") == Decimal("-0.12")
    assert number("1'234.50", ".") == Decimal("1234.50") and number("1,234.50", ".") == Decimal("1234.50")


def test_a_search_that_finds_several_funds_claims_none():
    two = DOCS + DOCS.replace("docs/9", "docs/10")
    assert documents(two) == (None, [])


def test_swisscanto_asks_the_retail_classes_where_the_institutional_ones_do_not_know_the_fund():
    def details(url, params):
        return 404 if params["segment"] == "INSTITUTIONAL" else SC_API
    claim = Swisscanto().claim(Fund(ref="isin:CH0000000115", isin="CH0000000115", name="Swisscanto Qland"),
                               Context(client=FakeClient({"/funds/CH0000000115": details})))
    assert claim["name"] == "Swisscanto Brindle Stock Tracker Qland NT QLD"


def test_a_wrapped_name_never_takes_in_the_next_position():
    pages = [SC[0], SC[1].replace("XX0000000113 SOLD OUT CORP 6'130 0 6'130 0 QLD 0.00 0.00 0.00",
                                  "XX0000000113 SOLD OUT CORP 6'130 - 6'130 - QLD - - -")]
    p = parse_report(pages, "Swisscanto Brindle Tracker Q - Stock Tracker Qland NT QLD", "isin:CH0000000115",
                     provider="swisscanto")
    assert [ln["name"] for ln in p.lines] == ["QORVANT MINING CORP", "TALVERI BANK LTD", "QORVANT PROPERTY FUND RESIDENTIAL"]


def test_the_umbrella_atop_each_page_is_no_sub_fund():
    # Each schedule page opens with the umbrella's name and names no
    # sub-fund: the umbrella is not read as the held fund's title.
    pages = [LU[0]] + ["Qumbra Harbour Fund Range\n" + p.replace("Qumbra Brindle Qland UCITS ETF\nSchedule",
                                                                  "Schedule") for p in LU[1:]]
    with pytest.raises(ProviderError, match="no inventory"):
        parse_report(pages, "Qumbra Harbour Fund Range - Qumbra Brindle Qland UCITS ETF QLD dis",
                     "isin:LU0000000101", provider="swissfunddata")


def test_a_report_read_for_a_small_share_of_its_fund_is_refused():
    # Its other lines in a layout not read: one line of 20% is no
    # composition of the fund.
    page = SC[1].replace("71.62 a)", "10.00 a)").replace("28.18 a)", "10.00 a)")
    with pytest.raises(ProviderError, match="cover 20.00% of the fund"):
        parse_report([SC[0], page], "Swisscanto Brindle Tracker Q - Stock Tracker Qland NT QLD", "isin:CH0000000115",
                     provider="swisscanto")


SC_CLAIM = {"ref": "isin:CH0000000115", "isin": "CH0000000115", "name": "Swisscanto Brindle Stock Tracker Qland NT QLD",
            "reports": [("annual", "https://x/JB_Q_deu.pdf"), ("semiannual", "https://x/HJB_Q_deu.pdf")]}


def test_a_report_that_is_not_a_pdf_or_states_no_day_is_refused(tmp_path):
    with pytest.raises(ProviderError, match="is not a PDF"):
        Swisscanto().fetch(SC_CLAIM, None, tmp_path / "a", Context(client=FakeClient({".pdf": "<html>moved</html>"})))
    undated = pdf([["Jahresbericht", "Swisscanto Brindle Tracker Q"]])
    with pytest.raises(ProviderError, match="no report states the day"):
        Swisscanto().fetch(SC_CLAIM, None, tmp_path / "b", Context(client=FakeClient({".pdf": undated})))


def test_an_umbrella_report_is_fetched_once_for_each_sub_fund_a_run_holds(tmp_path, monkeypatch):
    site = FakeClient({".pdf": SC_PDF})
    ctx = Context(client=site)
    other = {**SC_CLAIM, "ref": "isin:CH0000000117", "name": "Swisscanto Brindle Stock Tracker Qland Small"}
    s = Swisscanto()
    s.fetch(SC_CLAIM, None, tmp_path / "f", ctx)
    s.fetch(other, None, tmp_path / "g", ctx)
    assert len(site.asked) == 2
    assert (tmp_path / "g" / "annual.pdf").read_bytes() == SC_PDF
    # The copies read once at load as well.
    read = []
    pages = reports.pdf_pages
    monkeypatch.setattr(reports, "pdf_pages", lambda path, last=None: read.append(path) or pages(path, last))
    monkeypatch.setattr(reports, "_pages", type(reports._pages)())
    assert s.parse(tmp_path / "f").lines and s.parse(tmp_path / "g").lines
    assert len(read) == 1


def test_one_report_that_does_not_read_leaves_the_other(tmp_path):
    ctx = Context(client=FakeClient({"HJB_Q": pdf([["Halbjahresbericht per 31.08.2026"]]), "JB_Q": SC_PDF}))
    s = Swisscanto()
    s.fetch(SC_CLAIM, None, tmp_path / "f", ctx)
    (c,) = s.parse(tmp_path / "f").compositions
    assert c["as_of"] == date(2026, 2, 28)
    (tmp_path / "f" / "annual.pdf").write_bytes(pdf([["Jahresbericht per 28.02.2026"]]))
    with pytest.raises(ProviderError, match="annual.pdf: .*; semiannual.pdf: "):
        s.parse(tmp_path / "f")


def test_swissfunddata_fetches_and_reads_a_fund_end_to_end(tmp_path):
    ctx = Context(client=swissfunddata(report=pdf([p.splitlines() for p in LU])))
    s = SwissFundData()
    claim = s.claim(Fund(ref="isin:LU0000000101", isin="LU0000000101", name="Qland ETF"), ctx)
    assert s.fetch(claim, None, tmp_path / "f", ctx) == date(2026, 6, 30)
    (c,) = s.parse(tmp_path / "f").compositions
    assert (c["fund_ref"], c["line_count"], c["payload"]) == \
        ("isin:LU0000000101", 4, {"sub_fund": "Qumbra Brindle Qland UCITS ETF"})


def test_the_report_probes():
    site = FakeClient({"/translations": {}})
    assert Swisscanto().probe(Context(client=site)) == "Swisscanto product API answered 200"
    site = swissfunddata()
    assert SwissFundData().probe(Context(client=site)) == "swissfunddata took the investor category"
    assert [m for m, _, _ in site.asked] == ["GET", "POST", "GET"]


@pytest.mark.parametrize("provider,routes,failed", [
    pytest.param(Swisscanto(), {"/funds/": 404}, None, id="swisscanto-neither-segment-knows-it"),
    pytest.param(Swisscanto(), {"/funds/": 503}, "503", id="swisscanto-fails"),
    pytest.param(SwissFundData(), {"sfd-entry": lambda url, data: 503 if data else "<p>entry</p>"}, "503",
                 id="swissfunddata-entry-fails"),
])
def test_a_source_that_does_not_know_the_fund_claims_none_and_one_that_fails_says_so(provider, routes, failed):
    fund = Fund(ref="isin:CH0000000118", isin="CH0000000118", name="Swisscanto Qland")
    ctx = Context(client=FakeClient(routes))
    if failed is None:
        assert provider.claim(fund, ctx) is None
        assert len(ctx.client.asked) == 2
    else:
        with pytest.raises(fetcher.FetchError, match=failed):
            provider.claim(fund, ctx)
