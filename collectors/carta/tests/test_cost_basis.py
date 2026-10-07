"""
Unit tests for the cost-basis facts silver carries (migration 0004).

Covers the exercise detail matched to the certificate it produced, the
capital-account statement's inception-to-date lines, the fund's accepted
date, the federal Schedule K-1 face page, the migration's backfill of rows
loaded before it, and one bronze run loaded end to end. Synthetic data only:
invented ids, labels, dates and figures.
"""
from __future__ import annotations

import json
import shutil
import sqlite3
import sys
import zipfile
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import k1  # noqa: E402
import load  # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"


@pytest.fixture
def migrated():
    c = sqlite3.connect(":memory:")
    c.execute("PRAGMA foreign_keys = ON")
    load.silver.apply_migrations(c, MIGRATIONS_DIR)
    yield c
    c.close()


# ---- exercise detail -> certificate ----------------------------------------

def _xlsx(path: Path, *, label: str, date: str, shares, price, fmv) -> None:
    """A minimal exercise-detail xlsx: inline-string labels, numeric values
    trailing them, as Carta lays the sheet out."""
    def text(ref, s):
        return f'<c r="{ref}" t="inlineStr"><is><t>{s}</t></is></c>'

    def num(ref, v):
        return f'<c r="{ref}"><v>{v}</v></c>'

    rows = [
        text("A1", f"Exercise details for {label} (Example Holder)"),
        text("A4", f"Grant exercised on {date} - Cash exercise"),
        text("A5", "Shares exercised") + num("C5", shares),
        text("A6", "Exercise price") + num("C6", price),
        text("A8", "Fair market value on exercise date") + num("C8", fmv),
    ]
    sheet = ('<worksheet xmlns="http://schemas.openxmlformats.org/'
             'spreadsheetml/2006/main"><sheetData>'
             + "".join(f"<row>{r}</row>" for r in rows)
             + "</sheetData></worksheet>")
    path.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(path, "w") as z:
        z.writestr("xl/worksheets/sheet1.xml", sheet)


def _captable_edir(root: Path) -> Path:
    """One option grant (ES-1, id 11) exercised three times, and the four
    certificates on file: two of 1000 shares and one of 500 from ES-1, and
    one bought outright."""
    edir = root / "entities" / "corp_7"
    edir.mkdir(parents=True)
    (edir / "options.json").write_text(json.dumps({"rows": [
        {"id": 11, "label": "ES-1", "exercise_price": 0.5,
         "exercised": 2500, "quantity": 4000},
    ]}))
    (edir / "shares.json").write_text(json.dumps({"rows": [
        {"id": 21, "label": "CS-1", "exercise_from": "ES-1",
         "exercise_type": "ISO", "quantity": 1000, "cost": 500.0,
         "issue_date": "03/04/2098"},
        {"id": 22, "label": "CS-2", "exercise_from": "ES-1",
         "exercise_type": "NSO", "quantity": 1000, "cost": 500.0,
         "issue_date": "09/20/2098"},
        {"id": 23, "label": "CS-3", "exercise_from": "ES-1",
         "exercise_type": "NSO", "quantity": 500, "cost": 250.0,
         "issue_date": "06/02/2098"},
        {"id": 24, "label": "CS-4", "exercise_from": None,
         "exercise_type": None, "quantity": 10, "cost": 10.0,
         "issue_date": "01/01/2098"},
    ]}))
    xdir = edir / "exercises"
    _xlsx(xdir / "grant_11_er_31.xlsx", label="ES-1", date="03/01/2098",
          shares=1000, price=0.5, fmv=1.25)
    _xlsx(xdir / "grant_11_er_32.xlsx", label="ES-1", date="09/02/2098",
          shares=1000, price=0.5, fmv=2.5)
    _xlsx(xdir / "grant_11_er_33.xlsx", label="ES-1", date="06/01/2098",
          shares=500, price=0.5, fmv=2.0)
    return edir


def test_exercise_xlsx_reads_the_grant_from_its_title(tmp_path):
    p = tmp_path / "x.xlsx"
    _xlsx(p, label="ES-9", date="12/31/2098", shares=40, price=1, fmv=3.5)
    assert load._parse_exercise_xlsx(p) == {
        "grant_label": "ES-9", "date": "12/31/2098", "shares": "40",
        "exercise_price": "1", "fmv": "3.5"}


def test_each_certificate_takes_the_exercise_behind_it(tmp_path):
    edir = _captable_edir(tmp_path)
    matched = load._match_exercises(edir, load._read_exercises(edir))
    # Within a grant and a quantity, each certificate, oldest first, takes
    # the oldest exercise dated on or before its issue date.
    assert {cid: (ex["date"], ex["fmv"]) for cid, ex in matched.items()} == {
        21: ("03/01/2098", "1.25"),
        22: ("09/02/2098", "2.5"),
        23: ("06/01/2098", "2.0"),
    }


def test_a_certificate_issued_before_any_exercise_is_left_unmatched(tmp_path):
    edir = _captable_edir(tmp_path)
    shares = json.loads((edir / "shares.json").read_text())
    shares["rows"][0]["issue_date"] = "02/01/2098"   # before its exercise
    (edir / "shares.json").write_text(json.dumps(shares))
    matched = load._match_exercises(edir, load._read_exercises(edir))
    # CS-1 cannot take an exercise dated after it; CS-2 still takes the
    # older of the two 1000-share exercises.
    assert 21 not in matched
    assert matched[22]["date"] == "03/01/2098"


def test_the_xlsx_title_names_the_grant_when_no_grant_list_does(tmp_path):
    edir = _captable_edir(tmp_path)
    (edir / "options.json").write_text(json.dumps({"rows": []}))
    matched = load._match_exercises(edir, load._read_exercises(edir))
    assert sorted(matched) == [21, 22, 23]


def test_share_rows_carry_their_exercise_and_its_type(migrated, tmp_path):
    edir = _captable_edir(tmp_path)
    matched = load._match_exercises(edir, load._read_exercises(edir))
    load.load_securities(migrated, 0, 7, edir, held=True, val_price=None,
                         exercises=matched)
    rows = migrated.execute(
        "SELECT security_type, security_external_id, exercise_type, "
        "exercise_date, exercise_fmv FROM securities "
        "ORDER BY security_type, security_external_id").fetchall()
    assert rows == [
        ("option", 11, None, None, None),
        ("share", 21, "ISO", "03/01/2098", 1.25),
        ("share", 22, "NSO", "09/02/2098", 2.5),
        ("share", 23, "NSO", "06/01/2098", 2.0),
        ("share", 24, None, None, None),
    ]


def test_last_exercise_fmv_is_the_latest_exercise(tmp_path):
    edir = _captable_edir(tmp_path)
    assert load._last_exercise_fmv(load._read_exercises(edir)) == 2.5
    assert load._last_exercise_fmv([]) is None


# ---- capital-account statement ---------------------------------------------

_STATEMENT = """\
Statement of changes in investor's capital
                                 Statement period   Year to date   Inception to date
Beginning of period          $          93,940   $      85,422   $             —
Capital contributions                   10,371         20,683            103,457
Capital distributions                        —        (1,623)            (1,623)
Management fees                         (1,047)        (2,091)            (6,139)
Net operating income (loss)                 113            227                731
Net realized gain (loss)                     —              —                  —
Net unrealized gain (loss)               4,219          5,173             12,846
Carried interest accrued                  (843)        (1,038)            (2,519)
Ending balance               $         106,753   $     106,753   $        106,753
"""


def test_statement_states_every_inception_to_date_line():
    assert load._statement_from_text(_STATEMENT) == {
        "net_asset_value": "106753",
        "capital_contributed": "103457.00",
        "management_fees": "-6139",
        "net_operating_income": "731",
        "realized_gain": "0",            # the nil dash is a stated zero
        "unrealized_gain": "12846",
        "carried_interest": "-2519",
    }


def test_a_statement_without_a_line_leaves_it_null():
    text = "\n".join(line for line in _STATEMENT.splitlines()
                     if not line.startswith("Carried interest"))
    assert load._statement_from_text(text)["carried_interest"] is None


def test_statement_flows_read_the_nil_dash_as_zero():
    text = "Capital distributions     —     —     —\n"
    assert load._statement_flows_from_text(text) == (None, 0.0)


def _docs(root: Path, rows: list[dict]) -> Path:
    docs = root / "documents"
    docs.mkdir(parents=True, exist_ok=True)
    for row in rows:
        row.setdefault("document_name", f"document {row['id']}")
        (docs / f"doc_{row['id']}.pdf").write_bytes(f"pdf {row['id']}".encode())
    (docs / "index.json").write_text(json.dumps({"results": rows}))
    return docs


def _partner_metrics(edir: Path, *, sharing: str, accepted: str | None) -> None:
    (edir / "fund-admin").mkdir(parents=True, exist_ok=True)
    (edir / "fund-admin" / "partner-metrics.json").write_text(json.dumps([{
        "partner": {"fund_id": "f-1", "fund_currency": "USD",
                    "accepted_date": accepted},
        "metrics": {"commitment": "247913.58", "net_asset_value": "104286.37",
                    "capital_contributed": "98765.43"},
        "sharing_date": sharing,
    }]))


def test_a_fund_row_carries_its_accepted_date(migrated, tmp_path):
    edir = tmp_path / "fund_9"
    _partner_metrics(edir, sharing="12/31/2098",
                     accepted="2097-05-06T00:00:00.000000")
    assert load.load_fund_metrics(migrated, 0, 9, edir) == 1
    assert migrated.execute(
        "SELECT accepted_date FROM fund_metrics").fetchone() == (
            "2097-05-06T00:00:00.000000",)


def test_a_statement_adds_its_lines_to_partner_metrics_of_the_same_day(
        migrated, tmp_path, monkeypatch):
    # partner-metrics shares its figures at the statement's own date: the
    # richer structured row keeps its NAV, book value and payload, and gains
    # the statement lines it lacks.
    edir = tmp_path / "fund_9"
    _partner_metrics(edir, sharing="12/31/2098", accepted=None)
    snap = load._date_ts("12/31/2098")
    load.load_fund_metrics(migrated, snap, 9, edir)
    docs = _docs(tmp_path, [{"id": 5, "document_type": "Capital account statements",
                             "document_date": "12/31/2098"}])
    monkeypatch.setattr(load, "_pdf_text", lambda pdf: _STATEMENT)
    assert load.load_statement_nav(migrated, docs, 9) == 1
    assert migrated.execute(
        "SELECT net_asset_value, capital_contributed, management_fees, "
        "carried_interest, json_extract(payload, '$.source') "
        "FROM fund_metrics").fetchall() == [
            ("104286.37", "98765.43", "-6139", "-2519", None)]


def _fund_run(root: Path, name: str, *, documents: bool) -> Path:
    # A fund-only bronze run sharing at the statement's own date; without
    # documents it is the shape a `download --no-documents` run leaves.
    run = root / name
    fdir = run / "entities" / "fund_9"
    fdir.mkdir(parents=True)
    (fdir / "meta.json").write_text(json.dumps(
        {"corporation_id": 9, "is_fund_investment": True}))
    _partner_metrics(fdir, sharing="12/31/2098", accepted=None)
    if documents:
        _docs(run, [{"id": 5, "document_type": "Capital account statements",
                     "document_date": "12/31/2098", "fund_id": 9}])
    (run / "run.json").write_text(json.dumps(
        {"status": "complete", "individual_id": "ind-1"}))
    return run


def test_a_run_without_documents_keeps_the_statement_lines(
        tmp_path, monkeypatch):
    # Partial on full: a later run with the same sharing date but no
    # statements rewrites the partner-metrics figures and leaves the
    # statement lines merged onto that row in place.
    monkeypatch.setattr(load, "_pdf_text", lambda pdf: _STATEMENT)
    conn = load.silver.open_db(tmp_path / "carta.db")
    load.silver.apply_migrations(conn, MIGRATIONS_DIR)
    assert load.load_run(conn, _fund_run(tmp_path, "20990101T000000Z",
                                         documents=True))
    assert load.load_run(conn, _fund_run(tmp_path, "20990102T000000Z",
                                         documents=False))
    assert [tuple(r) for r in conn.execute(
        "SELECT net_asset_value, management_fees, net_operating_income, "
        "realized_gain, unrealized_gain, carried_interest "
        "FROM fund_metrics")] == [
            ("104286.37", "-6139", "731", "0", "12846", "-2519")]
    conn.close()


# ---- Schedule K-1 face page ------------------------------------------------

def _w(x0: float, y0: float, text: str) -> str:
    x1 = x0 + 3.5 * len(text)
    return (f'<word xMin="{x0:.6f}" yMin="{y0:.6f}" xMax="{x1:.6f}" '
            f'yMax="{y0 + 8:.6f}">{text}</word>')


def _phrase(x0: float, y0: float, phrase: str) -> list[str]:
    out = []
    for word in phrase.split():
        out.append(_w(x0, y0, word))
        x0 += 3.5 * len(word) + 2
    return out


_K1_CAPTIONS = {"heading": "Partner's Capital Account Analysis",
                "net_income": "Current year net income (loss)",
                "withdrawals": "Withdrawals and distributions"}
# Item L as forms before 2020 print it.
_K1_CAPTIONS_PRE_2020 = {"heading": "Partner's capital account analysis:",
                         "net_income": "Current year increase (decrease)",
                         "withdrawals": "Withdrawals &amp; distributions"}


def _k1_face(captions: dict[str, str] = _K1_CAPTIONS) -> list[str]:
    """A synthetic federal face page, laid out as the form prints it: item L
    on the left (x < 330), boxes 1-13 in the middle (from x 333), boxes
    14-21 on the right (from x 454). Amounts sit a few points off their
    caption line, as preparers print them. `captions` words item L."""
    p = []
    p += _phrase(36, 60, "Schedule K-1")
    p += _phrase(231, 84, "For calendar year 2098, or tax year")
    p += _phrase(337, 100, "1 Ordinary business income (loss)")
    p += _phrase(454, 100, "14 Self-employment earnings (loss)")
    p += [_w(431, 108, "389.")]
    p += _phrase(454, 304, "19 Distributions")
    p += [_w(454, 312, "A"), _w(525, 312, "1,529.")]
    p += [_w(454, 324, "C"), _w(525, 324, "2,089.")]
    p += _phrase(454, 340, "20 Other information")
    p += [_w(454, 348, "A"), _w(525, 348, "917.")]
    p += _phrase(337, 364, "8 Net short-term capital gain (loss)")
    p += [_w(405, 372, "(1,247.)")]
    p += _phrase(333, 388, "9a Net long-term capital gain (loss)")
    p += [_w(410, 396, "12,483.")]
    p += _phrase(333, 412, "9b Collectibles (28%) gain (loss)")
    p += _phrase(333, 436, "9c Unrecaptured section 1250 gain")
    p += [_w(115, 468, "9.3176000"), _w(180, 468, "%")]
    p += _phrase(112, 616, captions["heading"])
    p += _phrase(54, 628, "Beginning capital account ~~~ $")
    p += [_w(266, 624, "104,286.")]
    p += _phrase(54, 640, "Capital contributed during the year ~~ $")
    p += [_w(273, 636, "51,374.")]
    p += _phrase(54, 652, captions["net_income"] + " ~~ $")
    p += [_w(273, 648, "-3,127.")]
    p += _phrase(54, 664, "Other increase (decrease) (attach explanation) ~ $")
    p += _phrase(54, 676, captions["withdrawals"] + " ~~ $ (")
    p += [_w(280, 672, "3,618."), _w(321, 676, ")")]
    p += _phrase(54, 688, "Ending capital account ~~ $")
    p += [_w(266, 684, "148,915.")]
    p += _phrase(54, 736, "Beginning ~~~ $")
    p += [_w(266, 732, "7,731.")]
    return p


def _bbox(*pages: list[str]) -> str:
    body = "".join('<page width="612" height="792">' + "".join(ws) + "</page>"
                   for ws in pages)
    return f"<html><body><doc>{body}</doc></body></html>"


_COVER = _phrase(36, 100, "Attached is your Schedule K-1 package.")


def test_k1_face_page_reads_item_l_gains_and_distributions():
    got = k1.parse_bbox(_bbox(_COVER, _k1_face()))
    assert {k: v for k, v in got.items() if k != "printed"} == {
        "tax_year": 2098,
        "beginning_capital": "104286",
        "contributions": "51374",
        "net_income": "-3127",
        "other_change": None,          # left blank on the form
        "distributions": "3618",       # inside the form's own parentheses
        "ending_capital": "148915",
        "short_term_gain": "-1247",
        "long_term_gain": "12483",
        "cash_distributions": "1529",
        "property_distributions": "2089",
    }
    assert got["printed"]["short_term_gain"] == "(1,247.)"


def _item_l(got: dict) -> tuple:
    return tuple(got[c] for c in ("beginning_capital", "contributions",
                                  "net_income", "distributions",
                                  "ending_capital"))


def test_a_face_page_from_before_2020_reads_its_item_l():
    got = k1.parse_bbox(_bbox(_COVER, _k1_face(_K1_CAPTIONS_PRE_2020)))
    assert _item_l(got) == ("104286", "51374", "-3127", "3618", "148915")


def test_a_typographic_apostrophe_marks_the_face_page_too():
    captions = dict(_K1_CAPTIONS, heading="Partner\u2019s Capital Account Analysis")
    got = k1.parse_bbox(_bbox(_COVER, _k1_face(captions)))
    assert _item_l(got) == ("104286", "51374", "-3127", "3618", "148915")


def test_k1_caption_numbers_are_not_amounts():
    lines = k1._lines(k1.bbox_pages(_bbox(_k1_face()))[0])
    boxes = k1._boxes(lines, 329, 446)
    assert k1._first_amount(boxes.get(("mid", "9c"), [])) is None
    assert k1._first_amount(boxes.get(("mid", "9b"), [])) is None
    assert k1._first_amount(boxes[("mid", "1")]) == "389."


def test_a_document_with_no_federal_face_page_yields_nothing():
    assert k1.parse_bbox(_bbox(_COVER)) is None


def test_k1_decimal():
    assert k1._decimal("1,234.") == "1234"
    assert k1._decimal("(1,234.50)") == "-1234.50"
    assert k1._decimal("-$12.") == "-12"
    assert k1._decimal("(9.)", magnitude=True) == "9"
    assert k1._decimal("STMT") is None


def test_k1_rows_load_once_per_document(migrated, tmp_path, monkeypatch,
                                        caplog):
    docs = _docs(tmp_path, [
        {"id": 1, "document_type": "Tax - Schedule K-1", "fund_id": 9},
        {"id": 2, "document_type": "Tax", "fund_id": 9},          # a 1042-S
        {"id": 3, "document_type": "Capital account statements", "fund_id": 9},
    ])
    calls = []

    def fake_bbox(pdf, timeout=None):
        calls.append(pdf.name)
        return _bbox(_COVER, _k1_face()) if pdf.name == "doc_1.pdf" else _bbox(_COVER)

    monkeypatch.setattr(k1, "bbox_xhtml", fake_bbox)
    assert load.load_k1_capital_accounts(migrated, docs) == 1
    assert calls == ["doc_1.pdf", "doc_2.pdf"]   # statements are not tax documents
    row = migrated.execute(
        "SELECT doc_id, entity_external_id, tax_year, beginning_capital, "
        "distributions, property_distributions, long_term_gain, "
        "json_extract(payload, '$.long_term_gain') "
        "FROM k1_capital_accounts").fetchone()
    assert row == (1, 9, 2098, "104286", "3618", "2089", "12483", "12,483.")
    assert caplog.messages == []   # the 1042-S is not typed as a K-1
    # The same bytes in a later run are not parsed again.
    calls.clear()
    assert load.load_k1_capital_accounts(migrated, docs) == 0
    assert calls == ["doc_2.pdf"]


def test_a_k1_without_its_year_takes_the_index_year(migrated, tmp_path,
                                                   monkeypatch):
    docs = _docs(tmp_path, [{"id": 1, "document_type": "Tax", "fund_id": 9,
                             "tax_year": "2097"}])
    face = [w for w in _k1_face() if 'yMin="84.' not in w]   # the year line
    monkeypatch.setattr(k1, "bbox_xhtml", lambda pdf, timeout=None: _bbox(face))
    load.load_k1_capital_accounts(migrated, docs)
    assert migrated.execute(
        "SELECT tax_year FROM k1_capital_accounts").fetchone() == (2097,)


def test_a_k1_typed_document_without_a_face_page_warns(migrated, tmp_path,
                                                      monkeypatch, caplog):
    docs = _docs(tmp_path, [{"id": 1, "document_type": "Tax - Schedule K-1",
                             "fund_id": 9}])
    monkeypatch.setattr(k1, "bbox_xhtml", lambda pdf, timeout=None: _bbox(_COVER))
    assert load.load_k1_capital_accounts(migrated, docs) == 0
    assert any("doc_1.pdf" in m and "no federal K-1 face page" in m
               for m in caplog.messages)


def test_a_reissued_k1_replaces_the_row_for_its_document(migrated, tmp_path,
                                                         monkeypatch):
    docs = _docs(tmp_path, [{"id": 1, "document_type": "Tax", "fund_id": 9}])
    monkeypatch.setattr(k1, "bbox_xhtml",
                        lambda pdf, timeout=None: _bbox(_k1_face()))
    load.load_k1_capital_accounts(migrated, docs)
    (docs / "doc_1.pdf").write_bytes(b"amended")
    load.load_k1_capital_accounts(migrated, docs)
    assert migrated.execute(
        "SELECT COUNT(*) FROM k1_capital_accounts").fetchone() == (1,)


# ---- migration 0004 backfill ------------------------------------------------

def test_migration_backfills_rows_loaded_before_it(tmp_path):
    older = tmp_path / "migrations"
    older.mkdir()
    for f in MIGRATIONS_DIR.glob("000[1-3]_*.sql"):
        shutil.copy(f, older)
    c = sqlite3.connect(":memory:")
    load.silver.apply_migrations(c, older)
    c.execute(
        "INSERT INTO securities (snapshot_at, entity_external_id, "
        " security_type, security_external_id, payload) VALUES "
        "(0, 7, 'share', 21, ?), (0, 7, 'option', 11, ?)",
        (json.dumps({"exercise_type": "NSO"}), json.dumps({})))
    c.execute(
        "INSERT INTO fund_metrics (snapshot_at, entity_external_id, payload) "
        "VALUES (0, 9, ?), (1, 9, ?)",
        (json.dumps({"partner": {"accepted_date": "2097-05-06T00:00:00"}}),
         json.dumps({"source": "capital_account_statement"})))
    c.commit()
    assert load.silver.apply_migrations(c, MIGRATIONS_DIR) == 4
    assert c.execute("SELECT security_external_id, exercise_type, "
                     "exercise_fmv FROM securities ORDER BY 1").fetchall() == [
        (11, None, None), (21, "NSO", None)]
    assert c.execute("SELECT snapshot_at, accepted_date, management_fees "
                     "FROM fund_metrics ORDER BY 1").fetchall() == [
        (0, "2097-05-06T00:00:00", None), (1, None, None)]


# ---- one bronze run, end to end ---------------------------------------------

def test_a_run_loads_every_cost_basis_fact(tmp_path, monkeypatch):
    run = tmp_path / "20990101T000000Z"
    edir = _captable_edir(run)
    (edir / "meta.json").write_text(json.dumps(
        {"corporation_id": 7, "is_fund_investment": False}))
    (edir / "holdings-dashboard.json").write_text(json.dumps(
        {"held_since": "2098-01-01"}))
    fdir = run / "entities" / "fund_9"
    fdir.mkdir(parents=True)
    (fdir / "meta.json").write_text(json.dumps(
        {"corporation_id": 9, "is_fund_investment": True}))
    _partner_metrics(fdir, sharing="12/31/2098",
                     accepted="2097-05-06T00:00:00.000000")
    _docs(run, [
        {"id": 5, "document_type": "Capital account statements",
         "document_date": "09/30/2098", "fund_id": 9},
        {"id": 6, "document_type": "Tax - Schedule K-1", "fund_id": 9},
    ])
    (run / "run.json").write_text(json.dumps(
        {"status": "complete", "individual_id": "ind-1"}))
    monkeypatch.setattr(load, "_pdf_text", lambda pdf: _STATEMENT)
    monkeypatch.setattr(k1, "bbox_xhtml",
                        lambda pdf, timeout=None: _bbox(_k1_face()))

    conn = load.silver.open_db(tmp_path / "carta.db")
    load.silver.apply_migrations(conn, MIGRATIONS_DIR)
    assert load.load_run(conn, run)

    def q(sql):
        return [tuple(r) for r in conn.execute(sql).fetchall()]

    assert q("SELECT security_external_id, exercise_date, exercise_fmv "
             "FROM securities WHERE exercise_fmv IS NOT NULL "
             "ORDER BY 1") == [(21, "03/01/2098", 1.25),
                               (22, "09/02/2098", 2.5),
                               (23, "06/01/2098", 2.0)]
    assert q("SELECT sharing_date, accepted_date, realized_gain, "
             "unrealized_gain FROM fund_metrics ORDER BY snapshot_at") == [
        ("09/30/2098", None, "0", "12846"),
        ("12/31/2098", "2097-05-06T00:00:00.000000", None, None)]
    assert q("SELECT entity_external_id, tax_year, ending_capital "
             "FROM k1_capital_accounts") == [(9, 2098, "148915")]
    conn.close()
