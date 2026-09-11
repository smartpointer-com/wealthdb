"""Bronze→silver tests for the ubs-psn collector's load.py.

ubs-psn's bronze is SWIFT MT messages (and PSN XML) inside Z*.zip
containers. These tests exercise the MT parsing → silver path
directly with synthetic SWIFT text — an MT535 holdings message into
the `holdings` table, plus the balance-line parser — and the
dump-level driver with synthetic zips: dated archive names
(`<OT>_<YYYYMMDD>.zip`, landed by `download --recover`) routing to the
same per-order-type loaders as `<OT>.zip`, and re-delivered batch
content converging instead of duplicating — for the snapshot tables
(upsert on their snapshot-scoped keys) and for the change-point master
tables (dedup bounded at the file's as-of date), including a replay
after the master data has since changed. Synthetic safekeeping ids /
ISINs / IBANs / amounts only.
"""
from __future__ import annotations

import json
import sqlite3
import sys
import zipfile
from datetime import datetime, timezone
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import silver  # noqa: E402

REL = "SFTPCH99"

# Minimal synthetic MT535 Statement of Holdings: one safekeeping
# account (:97A::SAFE//) with one FIN holding block carrying an ISIN.
MT535 = (
    "{1:F01TESTXXXXAXXX0000000000}{2:I535TESTXXXXXXXXN}{4:\n"
    ":16R:GENL\n"
    ":97A::SAFE//SK123\n"
    ":16S:GENL\n"
    ":16R:FIN\n"
    ":35B:ISIN CH0000000001\n"
    "EXAMPLE FUND\n"
    ":93B::AGGR//FAMT/100,\n"
    ":16S:FIN\n"
    "-}"
)


# Minimal synthetic MT515 confirmations. The first is an exchange fill,
# whose trade is timed to the second (:98C::TRAD//); the second is a
# fund order, priced at a valuation point and so dated to the day alone
# (:98A::TRAD//). Both settle two days later. Every id, ISIN and figure
# is invented, and the dates sit in a decade the source cannot have
# booked in.
def _mt515(seme: str, trade_tag: str, buse: str) -> str:
    return (
        "{1:F01TESTXXXXAXXX0000000000}{2:I515TESTXXXXXXXXN}{4:\n"
        ":16R:GENL\n"
        f":20C::SEME//{seme}\n"
        ":23G:NEWM\n"
        ":16S:GENL\n"
        ":16R:CONFDET\n"
        f"{trade_tag}\n"
        ":98A::SETT//20981204\n"
        f":22H::BUSE//{buse}\n"
        ":16R:CONFPRTY\n"
        ":97A::SAFE//SK123\n"
        ":97A::CASH//CASH123\n"
        ":16S:CONFPRTY\n"
        ":35B:ISIN XX0000000001\n"
        "EXAMPLE FUND\n"
        ":16S:CONFDET\n"
        ":16R:SETDET\n"
        ":16R:AMT\n"
        ":19A::SETT//USD1000,\n"
        ":16S:AMT\n"
        ":16S:SETDET\n"
        "-}"
    )


def test_load_mt515_dates_a_trade_to_the_day_it_was_struck(tmp_path):
    """Both of ISO 15022's trade-date tags are read.

    An exchange fill is timed to the second and a fund order is dated to
    the day; read for the timed tag alone, a fund order looked undated
    and fell through to its settlement date, which is a different day.
    """
    struck = int(datetime(2098, 12, 2, tzinfo=timezone.utc).timestamp())
    for seme, trade_tag in (
        ("TIMED01", ":98C::TRAD//20981202103000"),
        ("DATED01", ":98A::TRAD//20981202"),
    ):
        conn = _fresh_db(tmp_path / seme)
        with conn:
            assert loader.load_mt515(
                conn, 1700000000, REL, _mt515(seme, trade_tag, "BUYI")) == 1
        row = conn.execute(
            "SELECT timestamp FROM events WHERE event_external_id = ?",
            (f"mt515:{seme}",)).fetchone()
        # The timed form carries a time of day; both land on the day the
        # trade was struck rather than the day it settled.
        assert datetime.fromtimestamp(
            row["timestamp"], timezone.utc).date() == datetime(
                2098, 12, 2, tzinfo=timezone.utc).date(), seme
        assert row["timestamp"] >= struck, seme


def test_load_mt515_keeps_a_fund_orders_own_vocabulary(tmp_path):
    """ISO states a fund order's direction as the operation (SUBS /
    REDM) rather than the party the holder was (BUYI / SELL). Only the
    market pair is folded to a common spelling; which vocabulary the
    bank used is part of what the confirmation says, and the gold
    adapter reads both."""
    for seme, buse, want in (
        ("MARKET01", "BUYI", "BUY"),
        ("MARKET02", "SELL", "SELL"),
        ("FUND0001", "SUBS", "SUBS"),
        ("FUND0002", "REDM", "REDM"),
    ):
        conn = _fresh_db(tmp_path / seme)
        with conn:
            loader.load_mt515(
                conn, 1700000000, REL,
                _mt515(seme, ":98A::TRAD//20981202", buse))
        row = conn.execute(
            "SELECT payload FROM events WHERE event_external_id = ?",
            (f"mt515:{seme}",)).fetchone()
        assert json.loads(row["payload"])["side"] == want, seme


def _fresh_db(tmp_path: Path) -> sqlite3.Connection:
    conn = loader.open_db(tmp_path / "ubs-psn.db")
    conn.row_factory = sqlite3.Row
    silver.apply_migrations(conn, loader.MIGRATIONS_DIR)
    return conn


def test_load_mt535_holdings(tmp_path):
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader.load_mt535(conn, 1700000000, REL, MT535)
    assert n == 1

    rows = conn.execute(
        "SELECT relationship_id, safekeeping_external_id, isin "
        "FROM holdings").fetchall()
    assert len(rows) == 1
    assert rows[0]["relationship_id"] == REL
    assert rows[0]["safekeeping_external_id"] == "SK123"
    assert rows[0]["isin"] == "CH0000000001"


def test_load_mt535_without_safe_is_noop(tmp_path):
    conn = _fresh_db(tmp_path)
    bad = "{4:\n:16R:FIN\n:35B:ISIN CH0000000001\n:16S:FIN\n-}"
    with conn:
        n = loader.load_mt535(conn, 1700000000, REL, bad)
    assert n == 0
    assert conn.execute("SELECT COUNT(*) FROM holdings").fetchone()[0] == 0


def _load_xml(loader_fn, conn, snapshot_at, xml_text):
    """Helper: parse a synthetic PSN XML blob and route it through a
    loader. Bypasses XML_LOADERS dispatch so tests exercise one loader
    at a time without needing zip fixtures.
    """
    type_code, entities = loader.parse_psn_xml(xml_text.encode("utf-8"))
    return loader_fn(conn, snapshot_at, REL, entities)


# One <PsNCashAccountPricingAndInterest> with a header + <Data> carrying
# two booking lines for the same synthetic account. Uses IBAN-spec
# placeholder letters and dummy amounts / settlement IDs.
TDCAPI_XML = """<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<Document xmlns="PsNMasterData">
  <PsNCashAccountPricingAndInterest>
    <Header><FlInf><TypeCd>TDCAPI</TypeCd></FlInf></Header>
    <Data>
      <ClntCashAccountPricingAndInterestData>
        <ClntKey><ClntId>CLNT9999</ClntId></ClntKey>
        <CshAcctPricingAndInterestInfo>
          <AcctId>ACCT_MT_FORM_1</AcctId>
          <SttlmId>SETTLE1</SttlmId>
          <SttlmBkngNum>00000001</SttlmBkngNum>
          <AcctCcyAmt>-1.00</AcctCcyAmt>
          <AcctCcyIsoCd>CHF</AcctCcyIsoCd>
          <SwiftPrdCd>CHG</SwiftPrdCd>
        </CshAcctPricingAndInterestInfo>
        <CshAcctPricingAndInterestInfo>
          <AcctId>ACCT_MT_FORM_1</AcctId>
          <SttlmId>SETTLE1</SttlmId>
          <SttlmBkngNum>00000002</SttlmBkngNum>
          <AcctCcyAmt>-2.00</AcctCcyAmt>
          <AcctCcyIsoCd>CHF</AcctCcyIsoCd>
          <SwiftPrdCd>CHG</SwiftPrdCd>
        </CshAcctPricingAndInterestInfo>
      </ClntCashAccountPricingAndInterestData>
    </Data>
  </PsNCashAccountPricingAndInterest>
</Document>"""


def test_load_tdcapi_canonicalises_iban_and_keys_per_booking(tmp_path):
    """Two bookings under the same SttlmId but different SttlmBkngNum
    coexist under one (snapshot, account) — the composite settlement id
    is what keeps them apart. AcctId is looked up in cash_accounts and
    canonicalised to the IBAN.
    """
    conn = _fresh_db(tmp_path)
    with conn:
        # Seed cash_accounts so _resolve_iban can canonicalise AcctId.
        conn.execute(
            "INSERT INTO cash_accounts"
            "(snapshot_at, relationship_id, account_external_id, payload) "
            "VALUES (?, ?, ?, ?)",
            (1700000000, REL, "CH99XXXX0000000000001",
             '{"AcctId":"ACCT_MT_FORM_1"}'),
        )
        n = _load_xml(loader.load_tdcapi, conn, 1700000100, TDCAPI_XML)
    assert n == 2

    rows = conn.execute(
        "SELECT account_external_id, settlement_external_id "
        "FROM cash_account_pricing ORDER BY settlement_external_id"
    ).fetchall()
    assert [(r["account_external_id"], r["settlement_external_id"]) for r in rows] == [
        ("CH99XXXX0000000000001", "SETTLE1:00000001"),
        ("CH99XXXX0000000000001", "SETTLE1:00000002"),
    ]


def test_load_tdcapi_falls_back_to_raw_acctid_without_sdca(tmp_path):
    """If SDCA hasn't populated cash_accounts yet, the loader keeps the
    booking under the raw AcctId with a warning rather than dropping it."""
    conn = _fresh_db(tmp_path)
    with conn:
        n = _load_xml(loader.load_tdcapi, conn, 1700000100, TDCAPI_XML)
    assert n == 2
    accts = {r[0] for r in conn.execute(
        "SELECT account_external_id FROM cash_account_pricing").fetchall()}
    assert accts == {"ACCT_MT_FORM_1"}


# One <PsNPortfolioPerformance> covering two portfolios; the first
# carries MONTHLY + YTD (matching UBS's actual pattern), the second
# only MONTHLY. Synthetic portfolio ids + amounts.
TDPOPF_XML = """<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<Document xmlns="PsNMasterData">
  <PsNPortfolioPerformance>
    <Header><FlInf><TypeCd>TDPOPF</TypeCd></FlInf></Header>
    <Data>
      <ClntPrtflPerfData>
        <ClntKey><ClntId>CLNT9999</ClntId></ClntKey>
        <PrtflKey>
          <PrtflId>0999AAAAAAAA01</PrtflId>
          <PrtflPerfData>
            <PrtflPerfTp>MONTHLY</PrtflPerfTp>
            <PrdEndMktValAmt>100.00</PrdEndMktValAmt>
          </PrtflPerfData>
          <PrtflPerfData>
            <PrtflPerfTp>YTD</PrtflPerfTp>
            <PrdEndMktValAmt>100.00</PrdEndMktValAmt>
          </PrtflPerfData>
        </PrtflKey>
        <PrtflKey>
          <PrtflId>0999AAAAAAAA02</PrtflId>
          <PrtflPerfData>
            <PrtflPerfTp>MONTHLY</PrtflPerfTp>
            <PrdEndMktValAmt>200.00</PrdEndMktValAmt>
          </PrtflPerfData>
        </PrtflKey>
      </ClntPrtflPerfData>
    </Data>
  </PsNPortfolioPerformance>
</Document>"""


def test_load_tdpopf_one_row_per_portfolio(tmp_path):
    """TDPOPF collapses per-portfolio period blocks (MONTHLY + YTD)
    into a single row per portfolio; both live under portfolios's
    nested payload."""
    conn = _fresh_db(tmp_path)
    with conn:
        n = _load_xml(loader.load_tdpopf, conn, 1700000100, TDPOPF_XML)
    assert n == 2

    rows = conn.execute(
        "SELECT portfolio_external_id, payload "
        "FROM portfolio_performance ORDER BY portfolio_external_id"
    ).fetchall()
    assert [r["portfolio_external_id"] for r in rows] == [
        "0999AAAAAAAA01", "0999AAAAAAAA02"]

    # The first portfolio's payload must retain BOTH period-type blocks.
    import json
    first = json.loads(rows[0]["payload"])
    perf = first["PrtflPerfData"]
    assert isinstance(perf, list) and len(perf) == 2
    assert {p["PrtflPerfTp"] for p in perf} == {"MONTHLY", "YTD"}


def test_parse_mt_balance():
    # :62F: closing-balance line, form <C|D>YYMMDD<CCY><amount>.
    bal = loader.parse_mt_balance("C231231CHF1234,56")
    assert bal is not None
    assert bal["currency_iso"] == "CHF"
    assert bal["credit_debit"] == "C"
    # SWIFT comma decimal is normalised to a dot.
    assert float(bal["amount"]) == 1234.56


# ============================================================
# Dump driver: dated archive zips + re-delivery convergence
# ============================================================

# Synthetic TDFXR batch: CHF base with two quote currencies.
TDFXR_XML = """<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<Document xmlns="PsNMasterData">
  <PsNForeignExchangeRate>
    <Header><FlInf><TypeCd>TDFXR</TypeCd></FlInf></Header>
    <Data>
      <ForeignExchangeRateData>
        <ForeignExchangeRateBase>
          <BaseCcyIsoCd>CHF</BaseCcyIsoCd>
        </ForeignExchangeRateBase>
        <ForeignExchangeRateInfo>
          <CcyIsoCd>USD</CcyIsoCd><MidRate>0.90</MidRate>
        </ForeignExchangeRateInfo>
        <ForeignExchangeRateInfo>
          <CcyIsoCd>EUR</CcyIsoCd><MidRate>0.95</MidRate>
        </ForeignExchangeRateInfo>
      </ForeignExchangeRateData>
    </Data>
  </PsNForeignExchangeRate>
</Document>"""

# Synthetic TDFWD batch: one open forward contract.
TDFWD_XML = """<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<Document xmlns="PsNMasterData">
  <PsNForwardContract>
    <Header><FlInf><TypeCd>TDFWD</TypeCd></FlInf></Header>
    <Data>
      <ClntFwdCtrctData>
        <FwdCtrctInf>
          <CtrctId>FWD0001</CtrctId>
          <CtrctAmt>1.00</CtrctAmt>
        </FwdCtrctInf>
      </ClntFwdCtrctData>
    </Data>
  </PsNForwardContract>
</Document>"""


def _write_zip(dump_dir: Path, name: str, entries: dict[str, str]) -> None:
    dump_dir.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(dump_dir / name, "w") as zf:
        for ename, data in entries.items():
            zf.writestr(ename, data)


def _rows(conn, table: str) -> list[tuple]:
    cur = conn.execute(f"SELECT * FROM {table}")
    return sorted(tuple(r) for r in cur.fetchall())


def test_dated_zip_routes_to_order_type_loader(tmp_path):
    # ZAH_20260528.zip (an archive copy landed by `download --recover`)
    # carries the same batch content as ZAH.zip and must reach the same
    # MT535 loader; snapshot_at still comes from the inner filename's
    # as-of prefix, not the dump directory.
    conn = _fresh_db(tmp_path)
    dump = tmp_path / "20260601T120000Z"
    _write_zip(dump, "ZAH_20260528.zip", {"2026-05-28_ZAH_synthetic.txt": MT535})
    stats = loader.load_dump(conn, dump, REL)
    assert stats["skipped"] is False
    rows = conn.execute(
        "SELECT safekeeping_external_id, isin, snapshot_at "
        "FROM holdings").fetchall()
    assert len(rows) == 1
    assert rows[0]["safekeeping_external_id"] == "SK123"
    assert rows[0]["snapshot_at"] == int(
        datetime(2026, 5, 28, tzinfo=timezone.utc).timestamp())


def test_redelivered_content_converges(tmp_path):
    # The same batch content arriving twice — once via the queue files,
    # once via recovered dated archive copies in a later dump — must
    # leave silver identical: fx_rates and the contract tables upsert on
    # their snapshot-scoped keys, holdings on its PK.
    conn = _fresh_db(tmp_path)
    xml_inner = {
        "2026-05-31_TDFXR_synthetic.xml": TDFXR_XML,
        "2026-05-31_TDFWD_synthetic.xml": TDFWD_XML,
    }
    mt_inner = {"2026-05-31_ZAH_synthetic.txt": MT535}

    original = tmp_path / "20260601T120000Z"
    _write_zip(original, "ZME.zip", xml_inner)
    _write_zip(original, "ZAH.zip", mt_inner)
    recovered = tmp_path / "20260701T120000Z"
    _write_zip(recovered, "ZME_20260531.zip", xml_inner)
    _write_zip(recovered, "ZAH_20260531.zip", mt_inner)

    tables = ("fx_rates", "forward_contracts", "holdings")
    assert loader.load_dump(conn, original, REL)["skipped"] is False
    before = {t: _rows(conn, t) for t in tables}
    assert len(before["fx_rates"]) == 2          # USD + EUR quotes
    assert len(before["forward_contracts"]) == 1
    assert len(before["holdings"]) == 1

    assert loader.load_dump(conn, recovered, REL)["skipped"] is False
    after = {t: _rows(conn, t) for t in tables}
    assert after == before


# Synthetic SDCA batch: one cash account whose book balance (part of
# the dedup-compared payload) is parameterised, so successive batches
# can carry unchanged or changed master data.
SDCA_XML_TMPL = """<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<Document xmlns="PsNMasterData">
  <PsNCashAccount>
    <Header><FlInf><TypeCd>SDCA</TypeCd></FlInf></Header>
    <Data>
      <ClntCshAcctData>
        <ClntKey><ClntId>CLNT9999</ClntId></ClntKey>
        <CshAcctInfo>
          <Iban>CH99XXXX0000000000001</Iban>
          <CcyIsoCd>CHF</CcyIsoCd>
          <BookBal>{book_bal}</BookBal>
        </CshAcctInfo>
      </ClntCshAcctData>
    </Data>
  </PsNCashAccount>
</Document>"""


def test_master_data_recover_replay_converges(tmp_path):
    # Change-point master tables must converge under --recover replays
    # too. Queue pulls land SDCA state A (as-of 06-01), A again (06-08,
    # dedups against 06-01), then B (06-15, a new change-point). A later
    # recover dump replays all three batches as dated ZMD archive
    # copies. Because the dedup compare is bounded at each file's as-of
    # date, the A@06-01 copy dedups against its own row, the A@06-08
    # copy dedups against A@06-01 exactly as the original delivery did,
    # and the B@06-15 copy dedups against its own row — no collision at
    # an already-occupied key, no spurious change-point rows.
    conn = _fresh_db(tmp_path)
    a = SDCA_XML_TMPL.format(book_bal="100.00")
    b = SDCA_XML_TMPL.format(book_bal="200.00")

    pulls = (
        ("20260601T120000Z", "2026-06-01_SDCA_synthetic.xml", a),
        ("20260608T120000Z", "2026-06-08_SDCA_synthetic.xml", a),
        ("20260615T120000Z", "2026-06-15_SDCA_synthetic.xml", b),
    )
    for run, inner, xml in pulls:
        dump = tmp_path / run
        _write_zip(dump, "ZMD.zip", {inner: xml})
        assert loader.load_dump(conn, dump, REL)["skipped"] is False

    before = _rows(conn, "cash_accounts")
    assert [r["snapshot_at"] for r in conn.execute(
        "SELECT snapshot_at FROM cash_accounts ORDER BY snapshot_at")] == [
        int(datetime(2026, 6, 1, tzinfo=timezone.utc).timestamp()),
        int(datetime(2026, 6, 15, tzinfo=timezone.utc).timestamp()),
    ]

    recovered = tmp_path / "20260701T120000Z"
    for _, inner, xml in pulls:
        stamp = inner[:10].replace("-", "")
        _write_zip(recovered, f"ZMD_{stamp}.zip", {inner: xml})
    assert loader.load_dump(conn, recovered, REL)["skipped"] is False
    assert _rows(conn, "cash_accounts") == before
