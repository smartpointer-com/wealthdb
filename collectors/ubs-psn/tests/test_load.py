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
after the master data has since changed.

The MT940 section at the end is about the two ways a cash movement can
be lost between bronze and silver: two entries the bank booked under
one :61: reference collapsing into one row, and an entry value-dated
outside the statement that carries it being deleted by the statement
that covers that date.

The last section covers cost: the book cost, average cost and
acquisition FX rate an MT535 holding states, an MT515's charges, and
the pass that fills both on rows loaded before migration 0005.

Synthetic safekeeping ids / ISINs / IBANs / amounts only.
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
def _mt515(seme: str, trade_tag: str, buse: str,
           extra_amounts: str = "") -> str:
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
        f"{extra_amounts}"
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


def _db_before(tmp_path: Path, version: int) -> sqlite3.Connection:
    """A silver DB with every migration numbered below `version` applied,
    and none from `version` on: the schema a DB loaded before that
    migration has."""
    conn = loader.open_db(tmp_path / "ubs-psn.db")
    conn.row_factory = sqlite3.Row
    for sql in sorted(loader.MIGRATIONS_DIR.glob("*.sql")):
        if int(sql.name[:4]) >= version:
            break
        conn.executescript(sql.read_text(encoding="utf-8"))
    assert silver.current_schema_version(conn) == version - 1
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


# ============================================================
# MT940: which entry a row is, and which statement owns it
# ============================================================

# Synthetic MT940 customer statement. Every account form, reference and
# amount is invented, and the dates sit in a decade the source cannot
# have booked in. `entries` are (:61: body, :86: narrative) pairs, in
# the order the statement prints them; the balance lines carry only the
# dates the loader reads off them.
def _mt940(stmt_no: str, opening_yymmdd: str, closing_yymmdd: str,
           entries: list[tuple[str, str]],
           acct: str = "ACCT_MT_FORM_1") -> str:
    lines = [
        "{1:F01TESTXXXXAXXX0000000000}{2:I940TESTXXXXXXXXN}{4:",
        ":20:TESTSTMT00000001",
        f":25:{acct}",
        f":28C:{stmt_no}",
        f":60F:C{opening_yymmdd}CHF1000,00",
    ]
    for line61, narrative in entries:
        lines.append(f":61:{line61}")
        lines.append(f":86:{narrative}")
    lines.append(f":62F:C{closing_yymmdd}CHF1000,00")
    lines.append("-}")
    return "\n".join(lines)


# The charge UBS books for a transfer carries the transfer's own :61:
# bank reference — the shape that used to cost one of the two rows.
CHARGE_61 = "9811131113D2,50NCHGNONREF//TESTBANKREF001"
TRANSFER_61 = "9811131113D100,00NTRFNONREF//TESTBANKREF001"

IBAN_1 = "CH99XXXX0000000000001"
IBAN_2 = "CH99XXXX0000000000002"


def _mt940_db(tmp_path: Path,
              accounts=((IBAN_1, "ACCT_MT_FORM_1"),)) -> sqlite3.Connection:
    """A silver DB with cash_accounts seeded, so the MT940 ':25:' →
    IBAN canonicalisation has something to resolve against and the rows
    land under the account id the rest of silver uses."""
    conn = _fresh_db(tmp_path)
    with conn:
        for iban, acct_mt_form in accounts:
            conn.execute(
                "INSERT INTO cash_accounts"
                "(snapshot_at, relationship_id, account_external_id, payload) "
                "VALUES (?, ?, ?, ?)",
                (1700000000, REL, iban,
                 json.dumps({"AcctId": acct_mt_form})),
            )
    return conn


def _movements(conn, account: str = IBAN_1) -> list[tuple[str, float, str]]:
    """(event id, amount, txn_type) per cash_movement row of an account."""
    rows = conn.execute(
        "SELECT event_external_id, payload FROM events "
        "WHERE kind = 'cash_movement' AND account_external_id = ? "
        "ORDER BY event_external_id", (account,)).fetchall()
    out = []
    for r in rows:
        p = json.loads(r["payload"])
        out.append((r["event_external_id"], float(p["amount"]), p["txn_type"]))
    return out


def test_mt940_keeps_both_entries_booked_under_one_bank_reference(tmp_path):
    """UBS books its own charge under the reference of the transfer that
    incurred it, so a :61: bank reference names a pair of entries rather
    than one. Identified by the reference alone, the second entry
    REPLACEd the first and a real booking disappeared — the charge, in
    every pair seen so far.

    The second entry is told apart by its position in the statement, and
    the first keeps the bare id, so a re-load of the same statement
    converges on the same two rows instead of minting new ones."""
    conn = _mt940_db(tmp_path)
    stmt = _mt940("7/1", "981113", "981113",
                  [(CHARGE_61, "CHARGE"), (TRANSFER_61, "TRANSFER")])
    with conn:
        assert loader.load_mt940(conn, 1700000000, REL, stmt) == (2, 2)

    base = f"mt940:{IBAN_1}:TESTBANKREF001"
    assert _movements(conn) == [
        (base, 2.50, "NCHG"),
        (f"{base}#1", 100.00, "NTRF"),
    ]

    before = _movements(conn)
    with conn:
        loader.load_mt940(conn, 1700000100, REL, stmt)
    assert _movements(conn) == before


def test_mt940_separates_identical_entries_without_a_bank_reference(tmp_path):
    """The same collision reaches the fallback id, which is synthesised
    from the entry's own content: two entries a statement prints
    identically are still two bookings, and the position that separates
    a reference pair separates these too."""
    conn = _mt940_db(tmp_path)
    same = "9811131113D7,00NCHGNONREF"
    with conn:
        assert loader.load_mt940(
            conn, 1700000000, REL,
            _mt940("8/1", "981113", "981113",
                   [(same, "FEE"), (same, "FEE")])) == (2, 2)
    ids = [eid for eid, _, _ in _movements(conn)]
    assert len(ids) == 2 and len(set(ids)) == 2
    assert ids[1] == f"{ids[0]}#1"


def test_mt940_forward_valued_entry_survives_the_next_statement(tmp_path):
    """An entry booked for a forward value date is timestamped outside
    the window of the statement that carries it, and inside the window
    of a later one. Deleting a value-date range therefore deleted a
    booking no statement would re-insert: the later statement does not
    carry it, and the statement that does was already loaded.

    Here the entry arrives booked on the 13th for value on the 15th, and
    the next statement covers the 15th and carries nothing at all."""
    conn = _mt940_db(tmp_path)
    forward = "9811151113C55,00NTRFNONREF//TESTBANKREF003"
    with conn:
        loader.load_mt940(conn, 1700000000, REL,
                          _mt940("9/1", "981113", "981113",
                                 [(forward, "TRANSFER")]))
    assert len(_movements(conn)) == 1

    with conn:
        loader.load_mt940(conn, 1700000100, REL,
                          _mt940("10/1", "981115", "981115", []))
    assert _movements(conn) == [
        (f"mt940:{IBAN_1}:TESTBANKREF003", 55.00, "NTRF")]


def test_mt940_reload_drops_an_entry_the_bank_has_amended_away(tmp_path):
    """The delete is what keeps a re-loaded statement honest: an entry
    the bank has since removed must not survive as a row nothing in
    bronze backs any more. Scoping the delete to the statement rather
    than to a date range keeps that property — a statement still
    replaces everything it owns, whatever the entries were value-dated
    to."""
    conn = _mt940_db(tmp_path)
    amended = "9811151113D30,00NMSCNONREF//TESTBANKREF004"
    with conn:
        loader.load_mt940(conn, 1700000000, REL,
                          _mt940("11/1", "981113", "981113",
                                 [(CHARGE_61, "CHARGE"), (amended, "MISC")]))
    assert len(_movements(conn)) == 2

    with conn:
        loader.load_mt940(conn, 1700000100, REL,
                          _mt940("11/1", "981113", "981113",
                                 [(CHARGE_61, "CHARGE")]))
    assert _movements(conn) == [
        (f"mt940:{IBAN_1}:TESTBANKREF001", 2.50, "NCHG")]


def test_mt940_statement_delete_stays_within_its_own_account(tmp_path):
    """:28C: numbers each account's statements separately, so two
    accounts routinely have a statement with the same number covering
    the same days. One account's statement must not delete the other's
    rows."""
    conn = _mt940_db(tmp_path, accounts=((IBAN_1, "ACCT_MT_FORM_1"),
                                         (IBAN_2, "ACCT_MT_FORM_2")))
    with conn:
        loader.load_mt940(conn, 1700000000, REL,
                          _mt940("12/1", "981113", "981113",
                                 [(CHARGE_61, "CHARGE")]))
        loader.load_mt940(conn, 1700000000, REL,
                          _mt940("12/1", "981113", "981113",
                                 [(TRANSFER_61, "TRANSFER")],
                                 acct="ACCT_MT_FORM_2"))
    assert _movements(conn, IBAN_1) == [
        (f"mt940:{IBAN_1}:TESTBANKREF001", 2.50, "NCHG")]
    assert _movements(conn, IBAN_2) == [
        (f"mt940:{IBAN_2}:TESTBANKREF001", 100.00, "NTRF")]


def test_mt940_statement_without_a_number_says_so(tmp_path, caplog):
    """The statement mark is the period plus the :28C: number, and UBS
    closes a Z40 every day — so without :28C: two statements it issued
    for the same account and day would look like one, and the second
    would delete the first's rows and re-insert only its own. Every Z40
    seen so far carries the tag; if that ever stops, the load log has to
    say so rather than merge in silence."""
    conn = _mt940_db(tmp_path)
    stmt = _mt940("", "981113", "981113", [(CHARGE_61, "CHARGE")])
    stmt = stmt.replace(":28C:\n", "")
    with caplog.at_level("WARNING"), conn:
        loader.load_mt940(conn, 1700000000, REL, stmt)
    assert ":28C:" in caplog.text
    # The entry still lands: the warning is about what a *second*
    # same-day statement would do, not a reason to drop this one.
    assert len(_movements(conn)) == 1


def test_mt940_reload_replaces_a_row_the_old_id_scheme_left_behind(tmp_path):
    """A row already in silver carries no statement mark — the fixed
    loader deletes on a mark the loader that wrote it never set. The
    thing that keeps it from surviving as a duplicate is that the first
    entry under a bank reference keeps the bare id, so the re-load
    rewrites that row in place.

    Migration 0004 clears the slice anyway, so this is the safety net
    rather than the repair path; it is asserted because the argument for
    a bare first id is exactly this, and a later change that suffixed
    every id (`#0` for the first) would quietly double the slice."""
    conn = _mt940_db(tmp_path)
    old_id = f"mt940:{IBAN_1}:TESTBANKREF001"
    with conn:
        conn.execute(
            "INSERT INTO events(event_external_id, timestamp, relationship_id, "
            " account_external_id, kind, currency_iso, payload) "
            "VALUES (?, ?, ?, ?, 'cash_movement', 'CHF', ?)",
            (old_id, loader.parse_yymmdd("981113"), REL, IBAN_1,
             json.dumps({"value_date": "981113", "credit_debit": "D",
                         "amount": "100.00", "txn_type": "NTRF",
                         "bank_ref": "TESTBANKREF001", "account": IBAN_1})),
        )
    with conn:
        loader.load_mt940(conn, 1700000000, REL,
                          _mt940("13/1", "981113", "981113",
                                 [(CHARGE_61, "CHARGE"),
                                  (TRANSFER_61, "TRANSFER")]))
    assert _movements(conn) == [
        (old_id, 2.50, "NCHG"),
        (f"{old_id}#1", 100.00, "NTRF"),
    ]


def test_migration_0004_clears_only_what_has_to_re_derive(tmp_path):
    """0004 throws the cash_movement slice away so the fixed loader can
    write it again from bronze: the rows the id collision lost were
    never written, and the survivors carry no statement mark, so neither
    is reconstructible from silver. dump_runs goes with it because the
    load skip is per dump — it is the only thing that would stop the
    re-derive.

    Everything else stays. The balances a statement writes are keyed per
    snapshot and rewritten identically by the replay, and the other
    event kinds have nothing to do with either defect, so widening the
    delete to them would be throwing away rows for no reason."""
    conn = _db_before(tmp_path, 4)

    with conn:
        conn.execute(
            "INSERT INTO events(event_external_id, timestamp, relationship_id, "
            " account_external_id, kind, currency_iso, payload) VALUES "
            "('mt940:X:R1', 911915200, ?, ?, 'cash_movement', 'CHF', '{}'), "
            "('SEME1', 911915200, ?, 'SK123', 'trade_confirmation', 'CHF', '{}')",
            (REL, IBAN_1, REL))
        conn.execute(
            "INSERT INTO cash_balances(snapshot_at, relationship_id, "
            " account_external_id, balance_kind, currency_iso, payload) "
            "VALUES (1700000000, ?, ?, 'closing', 'CHF', '{}')", (REL, IBAN_1))
        conn.execute(
            "INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) "
            "VALUES (1700000000, 3, '/nonexistent/20260101T000000Z')")

    assert silver.apply_migrations(conn, loader.MIGRATIONS_DIR) >= 4
    assert _rows(conn, "dump_runs") == []
    assert [r["kind"] for r in conn.execute("SELECT kind FROM events")] == [
        "trade_confirmation"]
    assert len(_rows(conn, "cash_balances")) == 1


# ============================================================
# Cost an MT535 holding states, and an MT515's charges
# ============================================================

# FIN blocks for a synthetic MT535. Every ISIN and figure is invented.
# A holding in its reference currency: BOOK, and a narrative with AVER
# and AHOD but no AEXR.
FIN_DOMESTIC = (
    ":35B:ISIN XX0000000002\n"
    "EXAMPLE EQUITY\n"
    ":90B::MRKT//ACTU/CHF12,5\n"
    ":93B::AGGR//UNIT/40,\n"
    ":19A::HOLD//CHF500,\n"
    ":19A::HOLD//CHF500,\n"
    ":19A::BOOK//CHF400,\n"
    ":70C::SUBB//?AQPR:90A::AVER//INDC/CHF10,\n"
    "?AHLD:19A::AHOD//CHF400,\n"
)
# A holding in a foreign currency: the narrative adds AEXR, the average
# rate the units were bought at, into the reference currency.
FIN_FOREIGN = (
    ":35B:ISIN XX0000000003\n"
    "/XX/00000003\n"
    "EXAMPLE FOREIGN EQUITY\n"
    ":90B::MRKT//ACTU/SEK9,\n"
    ":92B::EXCH//SEK/CHF/0,125\n"
    ":93B::AGGR//UNIT/1000,\n"
    ":19A::HOLD//SEK9000,\n"
    ":19A::HOLD//CHF1125,\n"
    ":19A::BOOK//SEK8000,5\n"
    ":70C::SUBB//?AQPR:90A::AVER//INDC/SEK8,0005\n"
    "?AHLD:19A::AHOD//SEK8000,5\n"
    "?AFXH:92B::AEXR//SEK/CHF/0,1275\n"
)
# An alternative-fund unit: no market price, and neither BOOK nor a
# narrative.
FIN_ALTERNATIVE = (
    ":35B:ISIN XX0000000004\n"
    "EXAMPLE PRIVATE MARKETS FUND\n"
    ":90E::MRKT//UKWN\n"
    ":93B::AGGR//UNIT/3,\n"
    ":19A::HOLD//CHF300,\n"
    ":19A::HOLD//CHF300,\n"
)


def _mt535_with(*fins: str) -> str:
    body = "".join(f":16R:FIN\n{fin}:16S:FIN\n" for fin in fins)
    return ("{1:F01TESTXXXXAXXX0000000000}{2:I535TESTXXXXXXXXN}{4:\n"
            ":16R:GENL\n:97A::SAFE//SK123\n:16S:GENL\n" + body + "-}")


def _holding_costs(conn) -> dict[str, tuple]:
    cols = ", ".join(loader.HOLDING_COST_COLUMNS)
    return {r[0]: tuple(r)[1:] for r in conn.execute(
        f"SELECT isin, {cols} FROM holdings")}


def test_load_mt535_promotes_the_cost_a_holding_states(tmp_path):
    """BOOK, AVER and AEXR land in columns, as printed: the figures in
    the instrument currency, the FX rate with both of its currencies.
    A holding that states none of them keeps every cost column NULL,
    never 0."""
    conn = _fresh_db(tmp_path)
    with conn:
        assert loader.load_mt535(
            conn, 1700000000, REL,
            _mt535_with(FIN_DOMESTIC, FIN_FOREIGN, FIN_ALTERNATIVE)) == 3
    assert _holding_costs(conn) == {
        "XX0000000002": (400.0, "CHF", 10.0, None, None, None),
        "XX0000000003": (8000.5, "SEK", 8.0005, 0.1275, "SEK", "CHF"),
        "XX0000000004": (None, None, None, None, None, None),
    }


def test_holding_cost_keeps_a_mismatched_average_cost_unlabelled(caplog):
    """BOOK and AVER share one currency column. Should a holding ever
    state them in two currencies, the average cost stays NULL rather
    than carry the book cost's currency, and the load log says so."""
    entries = {
        "19A": [":BOOK//CHF400,"],
        "70C": [":SUBB//?AQPR:90A::AVER//INDC/SEK10,\n"
                "?AHLD:19A::AHOD//CHF400,"],
    }
    with caplog.at_level("WARNING"):
        assert loader._holding_cost(entries) == (
            400.0, "CHF", None, None, None, None)
    assert "AVER" in caplog.text


def test_parse_amount_ccy_reads_the_iso_negative_sign():
    """ISO 15022 marks a negative amount with a leading N. A currency
    code that itself begins with N still reads as unsigned."""
    assert loader._parse_amount_ccy("NCHF12,5") == ("CHF", -12.5)
    assert loader._parse_amount_ccy("NOK12,5") == ("NOK", 12.5)
    assert loader._parse_amount_ccy("NNOK12,") == ("NOK", -12.0)
    assert loader._parse_amount_ccy("EUR--") == (None, None)


def _trade_payload(conn, seme: str) -> dict:
    row = conn.execute("SELECT payload FROM events WHERE event_external_id = ?",
                       (f"mt515:{seme}",)).fetchone()
    return json.loads(row["payload"])


def test_load_mt515_structures_the_charges(tmp_path):
    """`:19A::CHAR//` joins the transaction tax and stamp duty as payload
    keys. A confirmation that states no charges carries both keys as
    null."""
    conn = _fresh_db(tmp_path)
    with conn:
        loader.load_mt515(conn, 1700000000, REL, _mt515(
            "CHARGED1", ":98A::TRAD//20981202", "BUYI",
            ":19A::CHAR//SEK12,34\n:19A::STAM//SEK1,\n"))
        loader.load_mt515(conn, 1700000000, REL, _mt515(
            "PLAIN001", ":98A::TRAD//20981202", "SELL"))
    charged = _trade_payload(conn, "CHARGED1")
    assert (charged["charges_amount"], charged["charges_currency"]) == (
        12.34, "SEK")
    assert (charged["stamp_duty_amount"], charged["stamp_duty_currency"]) == (
        1.0, "SEK")
    plain = _trade_payload(conn, "PLAIN001")
    assert (plain["charges_amount"], plain["charges_currency"]) == (None, None)


def test_backfill_fills_rows_loaded_before_migration_0005(tmp_path):
    """Rows loaded before 0005 hold the cost only as raw SWIFT text in
    their payload. The loader's pass fills them from that text, with no
    bronze read, and a filled row equals a freshly loaded one. A second
    pass finds nothing left to fill."""
    charged = _mt515("CHARGED1", ":98A::TRAD//20981202", "BUYI",
                     ":19A::CHAR//SEK5,\n")
    plain = _mt515("PLAIN001", ":98A::TRAD//20981202", "SELL")

    # What a fresh load writes.
    fresh = _fresh_db(tmp_path / "fresh")
    with fresh:
        loader.load_mt535(fresh, 1700000000, REL, _mt535_with(
            FIN_DOMESTIC, FIN_FOREIGN, FIN_ALTERNATIVE))
        loader.load_mt515(fresh, 1700000000, REL, charged)
        loader.load_mt515(fresh, 1700000000, REL, plain)
    want_costs = _holding_costs(fresh)
    want_events = _rows(fresh, "events")

    # The same rows as a pre-0005 loader wrote them: payload only, and
    # no charges keys in a confirmation's payload.
    old = _db_before(tmp_path / "old", 5)
    with old:
        for r in fresh.execute(
                "SELECT snapshot_at, relationship_id, "
                "safekeeping_external_id, isin, payload FROM holdings"):
            old.execute("INSERT INTO holdings VALUES (?, ?, ?, ?, ?)",
                        tuple(r))
        for r in fresh.execute("SELECT * FROM events"):
            p = json.loads(r["payload"])
            del p["charges_amount"], p["charges_currency"]
            old.execute("INSERT INTO events VALUES (?, ?, ?, ?, ?, ?, ?)",
                        (*tuple(r)[:6], loader.canonical_json(p)))

    assert silver.apply_migrations(old, loader.MIGRATIONS_DIR) >= 5
    assert set(_holding_costs(old).values()) == {(None,) * 6}
    with old:
        # The alternative fund states no cost and is not a row to fill.
        assert loader.backfill_cost_fields(old) == (2, 2)
    assert _holding_costs(old) == want_costs
    assert _rows(old, "events") == want_events

    with old:
        assert loader.backfill_cost_fields(old) == (0, 0)
