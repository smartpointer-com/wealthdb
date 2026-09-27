"""Bronze→silver tests for the viac collector's load.py.

Seeds a minimal synthetic VIAC bronze dump (portfolio-inventory.json
+ per-portfolio assets.json + run.json) and runs the real loader,
asserting the projected silver rows. No real data — synthetic
portfolio numbers / ISINs / amounts only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import silver  # noqa: E402

RUN_SLUG = "20240101T000000Z"
PORT = "3.111.222.333.01"  # product_code '3' (p3a), portfolio_index '01'


def _write_json(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj), encoding="utf-8")


def _seed_bronze(root: Path) -> Path:
    run_dir = root / RUN_SLUG
    _write_json(run_dir / "run.json", {"utc": RUN_SLUG})
    _write_json(run_dir / "wealth" / "portfolio-inventory.json", {
        "p3a": [{
            "number": PORT,
            "name": "Portfolio 2024",
            "state": "ACTIVE",
            "index": 0,
            "sortIndex": 0,
        }],
        "pvb": [],
        "inv": [],
    })
    _write_json(run_dir / "positions" / PORT / "assets.json", {
        "cashAmount": 250.50,
        "interestRate": 0.5,
        "assetsByClasses": {
            "EQUITIES": [{
                "isin": "CH0000000001",
                "name": "Fund A",
                "currencyCode": "CHF",
                "subAssetClassType": "SHARES_SWITZERLAND",
                "amount": 10,          # → quantity (units)
                "ratio": 0.8,
                "ratioInChf": 1500.0,  # → market_value_chf
                "acquisitionPrice": 120.0,
                "assetPrice": 150.0,
                "rateOfReturn": 0.25,
            }],
        },
    })
    return run_dir


def _fresh_db(tmp_path: Path):
    conn = loader.open_db(tmp_path / "viac.db")
    version = silver.apply_migrations(conn, loader.MIGRATIONS_DIR)
    return conn, version


def test_load_accounts_positions_cash(tmp_path):
    run_dir = _seed_bronze(tmp_path / "bronze")
    conn, version = _fresh_db(tmp_path)
    loader.load_one_dump(conn, run_dir, version)

    # Account: product_code '3' (p3a), automated, CHF.
    acct = conn.execute(
        "SELECT account_external_id, product_code, portfolio_index, "
        "management_style, currency_code FROM accounts").fetchall()
    assert len(acct) == 1
    assert acct[0]["account_external_id"] == PORT
    assert acct[0]["product_code"] == "3"
    assert acct[0]["portfolio_index"] == "01"
    assert acct[0]["management_style"] == "automated"
    assert acct[0]["currency_code"] == "CHF"

    # Position: quantity = units (10), market_value_chf = ratioInChf.
    pos = conn.execute(
        "SELECT instrument_external_id, asset_class, quantity, "
        "market_value_chf, acquisition_price FROM positions").fetchall()
    assert len(pos) == 1
    assert pos[0]["instrument_external_id"] == "CH0000000001"
    assert pos[0]["asset_class"] == loader.asset_class_for("EQUITIES")
    assert float(pos[0]["quantity"]) == 10.0
    assert float(pos[0]["market_value_chf"]) == 1500.0
    assert float(pos[0]["acquisition_price"]) == 120.0

    # Instrument catalog row.
    instr = conn.execute(
        "SELECT isin, asset_class FROM instruments").fetchall()
    assert len(instr) == 1
    assert instr[0]["isin"] == "CH0000000001"

    # Cash balance from assets.cashAmount.
    cash = conn.execute(
        "SELECT currency, balance_kind, amount FROM cash_balances").fetchall()
    assert len(cash) == 1
    assert cash[0]["currency"] == "CHF"
    assert cash[0]["balance_kind"] == "cash"
    assert float(cash[0]["amount"]) == 250.50

    # dump_runs recorded last.
    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1


def test_asset_class_mapping(tmp_path):
    # The bronze→silver asset-class normalisation the loader applies.
    assert loader.asset_class_for("EQUITIES") == "equity"
    assert loader.asset_class_for("BONDS") == "bond"
    assert loader.asset_class_for(None) == "other"


def test_parse_account_id_product_split(tmp_path):
    # Portfolio number's first dotted segment → product_code.
    code, index = loader.parse_account_id(PORT)
    assert code == "3"
    assert index == "01"


# ============================================================
# Dump selection: run.json status gate (list_pending_dumps)
# ============================================================

def _seed_manifest(root: Path, slug: str, manifest: dict) -> Path:
    """Write only a run.json under <root>/<slug> — enough for
    list_pending_dumps to classify the dump."""
    d = root / slug
    _write_json(d / "run.json", manifest)
    return d


def _pending_names(root: Path, conn) -> set[str]:
    return {d.name for d in loader.list_pending_dumps(conn, root)}


def test_list_pending_skips_in_progress_and_dry_run(tmp_path):
    # download.py stamps run.json with a status; a crashed walk
    # ("in-progress") or a --dry-run shell ("dry-run") must NOT be loaded
    # — the manifest's presence alone does not prove the walk finished.
    bronze = tmp_path / "bronze"
    _seed_manifest(bronze, "20240101T000000Z", {"status": "in-progress"})
    _seed_manifest(bronze, "20240102T000000Z", {"status": "dry-run",
                                                 "dry_run": True})
    conn, _ = _fresh_db(tmp_path)
    assert _pending_names(bronze, conn) == set()


def test_list_pending_loads_complete_and_legacy_statusless(tmp_path):
    # status="complete" loads; a statusless manifest predates the
    # lifecycle and stays loadable (backward compat) — even one carrying
    # dry_run:true, which the old loader also ingested.
    bronze = tmp_path / "bronze"
    complete = _seed_manifest(bronze, "20240103T000000Z",
                              {"status": "complete"})
    legacy = _seed_manifest(bronze, "20240104T000000Z", {"utc": "x"})
    legacy_dry = _seed_manifest(bronze, "20240105T000000Z",
                                {"dry_run": True})
    conn, _ = _fresh_db(tmp_path)
    assert _pending_names(bronze, conn) == {
        complete.name, legacy.name, legacy_dry.name}


def test_list_pending_skips_run_dir_without_manifest(tmp_path):
    # No run.json at all (still-writing / crashed before the marker) is
    # skipped, as before.
    bronze = tmp_path / "bronze"
    (bronze / "20240106T000000Z").mkdir(parents=True)
    conn, _ = _fresh_db(tmp_path)
    assert _pending_names(bronze, conn) == set()


# ============================================================
# Parser generations — a re-parse replaces, it does not accumulate
# ============================================================

def _seed_position(conn, source, isin="CH0000000001", snapshot_at=1700000000):
    conn.execute(
        "INSERT OR REPLACE INTO positions (snapshot_at, account_external_id, "
        "instrument_external_id, asset_class, source, payload) "
        "VALUES (?, 'acct-1', ?, 'equity', ?, '{}')",
        (snapshot_at, isin, source))


def test_a_dump_load_does_not_wipe_another_dumps_reports(tmp_path):
    # The purge is per DOCUMENT, not per scope. `load_historical_reports_
    # phase` walks one dump's own index, so a scope-wide purge would delete
    # every other dump's report rows and refill none of them — the dump
    # being loaded here carries no documents at all, which is the worst
    # case of exactly that.
    run_dir = _seed_bronze(tmp_path / "bronze")
    conn, version = _fresh_db(tmp_path)
    _seed_position(conn, "report:D1")

    loader.load_one_dump(conn, run_dir, version)

    assert conn.execute(
        "SELECT COUNT(*) FROM positions WHERE source = 'report:D1'"
    ).fetchone()[0] == 1
    conn.close()


def test_transaction_resolves_its_instrument_by_name(tmp_path):
    """VIAC names the fund in free text and nowhere else, and that text
    IS the instrument's name in this silver — so the link is a lookup.

    A name matching two instruments resolves to nothing: guessing would
    put a trade on the wrong line of a portfolio holding both, and a
    wrong row is invisible from the outside where an unresolved one is
    not.
    """
    conn, _ = _fresh_db(tmp_path)
    conn.executescript(
        """
        INSERT INTO instruments(instrument_external_id, isin, name, asset_class,
                                first_seen_at, last_seen_at, payload) VALUES
            ('CH0000000001', 'CH0000000001', 'Example Equity Index', 'equity', 1, 1, '{}'),
            ('CH0000000011', 'CH0000000011', 'Example Twice Named',  'equity', 1, 1, '{}'),
            ('CH0000000012', 'CH0000000012', 'Example Twice Named',  'equity', 1, 1, '{}');
        """
    )
    assert loader._instrument_for_description(
        conn, "Example Equity Index") == "CH0000000001"
    assert loader._instrument_for_description(conn, "Example Twice Named") is None
    assert loader._instrument_for_description(conn, "Never Held") is None
    assert loader._instrument_for_description(conn, None) is None


def test_migration_backfills_the_instrument_on_rows_already_held(tmp_path):
    """The rows silver is already holding get the link too, or the gap
    would only close for whatever lands after the upgrade."""
    conn, _ = _fresh_db(tmp_path)
    conn.executescript(
        """
        INSERT INTO instruments(instrument_external_id, isin, name, asset_class,
                                first_seen_at, last_seen_at, payload) VALUES
            ('CH0000000001', 'CH0000000001', 'Example Equity Index', 'equity', 1, 1, '{}');
        INSERT INTO transactions(transaction_external_id, snapshot_at, occurred_at,
                                 account_external_id, type, kind, amount_chf,
                                 currency, payload) VALUES
            ('t1', 1, 1, 'P3A1', 'TRADE_BUY', 'buy', -100.0, 'CHF',
             '{"description":"Example Equity Index"}'),
            ('t2', 1, 2, 'P3A1', 'TRADE_BUY', 'buy',  -50.0, 'CHF',
             '{"description":"Never Held"}');
        """
    )
    # Re-run the backfill statement the migration carries.
    conn.execute(
        """
        UPDATE transactions
           SET instrument_external_id = (
               SELECT i.instrument_external_id FROM instruments i
                WHERE i.name = json_extract(transactions.payload, '$.description')
                  AND (SELECT COUNT(*) FROM instruments j WHERE j.name = i.name) = 1)
        """
    )
    got = {r["transaction_external_id"]: r["instrument_external_id"]
           for r in conn.execute(
               "SELECT transaction_external_id, instrument_external_id FROM transactions")}
    assert got == {"t1": "CH0000000001", "t2": None}


# ============================================================
# wealth_history: VIAC's decimals arrive in two wire shapes
# ============================================================

def _wealth_summary(value_for):
    """A summary.json carrying one date in each of the three series,
    with each scalar rendered by `value_for`."""
    return {
        "dailyWealth": [{"date": "2024-01-02", "value": value_for("1500.25")}],
        "dailyPerformance": [{"date": "2024-01-02", "value": value_for("0.125")}],
        "dailyInvestedAmounts": [
            {"date": "2024-01-02", "value": value_for("1400")}],
    }


def _bare(s):
    return float(s)


def _enveloped(s):
    return {"__type": loader.VIAC_DECIMAL_TYPE, "__value": s}


def _load_wealth(tmp_path, summary):
    run_dir = tmp_path / "bronze" / RUN_SLUG
    _write_json(run_dir / "wealth" / "summary.json", summary)
    conn, _ = _fresh_db(tmp_path)
    loader.load_wealth_history_phase(conn, 1700000000, run_dir)
    return conn


def test_wealth_history_reads_the_tagged_decimal_envelope(tmp_path):
    # The shape VIAC switched to; loading it used to abort the whole dump
    # with "type 'dict' is not supported".
    conn = _load_wealth(tmp_path, _wealth_summary(_enveloped))
    rows = [tuple(r) for r in conn.execute(
        "SELECT wealth_value, performance_value, invested_amount "
        "FROM wealth_history")]
    assert rows == [(1500.25, 0.125, 1400.0)]


def test_wealth_history_still_reads_a_bare_number(tmp_path):
    # Bronze is immutable, so every older run must keep loading to the
    # same rows it produced before the envelope existed.
    conn = _load_wealth(tmp_path, _wealth_summary(_bare))
    rows = [tuple(r) for r in conn.execute(
        "SELECT wealth_value, performance_value, invested_amount "
        "FROM wealth_history")]
    assert rows == [(1500.25, 0.125, 1400.0)]


def test_wealth_history_keeps_the_raw_rows_in_the_payload(tmp_path):
    # The payload is the provenance copy: it holds the envelope as served,
    # so the coercion is never the only record of what arrived.
    conn = _load_wealth(tmp_path, _wealth_summary(_enveloped))
    payload = json.loads(conn.execute(
        "SELECT payload FROM wealth_history").fetchone()[0])
    assert payload["wealth"]["value"] == {
        "__type": "VIAC_DECIMAL", "__value": "1500.25"}


def test_a_missing_series_leaves_its_column_null(tmp_path):
    summary = _wealth_summary(_enveloped)
    del summary["dailyInvestedAmounts"]
    conn = _load_wealth(tmp_path, summary)
    assert conn.execute(
        "SELECT invested_amount FROM wealth_history").fetchone()[0] is None


@pytest.mark.parametrize("raw,expected", [
    (None, None),
    (12, 12.0),
    (12.5, 12.5),
    ("12.5", 12.5),
    ({"__type": "VIAC_DECIMAL", "__value": "12.5"}, 12.5),
    ({"__type": "VIAC_DECIMAL", "__value": "-0.00000000000000000001"},
     -1e-20),
])
def test_viac_decimal_accepts_every_known_shape(raw, expected):
    assert loader.viac_decimal(raw) == expected


@pytest.mark.parametrize("raw", [
    True,                                              # bool is an int
    "not a number",
    [1],
    {"__type": "VIAC_MONEY", "__value": "12.5"},        # a tag we don't know
    {"__type": "VIAC_DECIMAL"},                         # no __value
    {"__type": "VIAC_DECIMAL", "__value": {"a": 1}},    # __value not scalar
])
def test_an_unknown_decimal_shape_raises_rather_than_nulling(raw):
    # Loudly, so the next wire change aborts the dump instead of writing a
    # series of silent nulls — which is how this one was caught.
    with pytest.raises(ValueError):
        loader.viac_decimal(raw)


def test_the_raised_message_never_carries_the_value():
    secret = "1234567.89"
    with pytest.raises(ValueError) as exc:
        loader.viac_decimal({"__type": "VIAC_TOTAL", "__value": secret})
    assert secret not in str(exc.value)
