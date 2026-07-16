"""Bronze→silver tests for the ubs-psn collector's load.py.

ubs-psn's bronze is SWIFT MT messages (and PSN XML) inside Z*.zip
containers. These tests exercise the MT parsing → silver path
directly with synthetic SWIFT text: an MT535 holdings message into
the `holdings` table, plus the balance-line parser. Synthetic
safekeeping ids / ISINs / amounts only.
"""
from __future__ import annotations

import sqlite3
import sys
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
