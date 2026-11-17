"""The USD price fill (prices.py) against a synthetic DuckDB silver and
stub provider clients. No network; synthetic holdings and prices only.
"""
from __future__ import annotations

import sys
from datetime import date
from decimal import Decimal
from pathlib import Path

import duckdb
import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load as loader  # noqa: E402
import prices  # noqa: E402

D1, D2, D3 = date(2024, 1, 1), date(2024, 1, 2), date(2024, 1, 3)


def _fresh_db(tmp_path: Path) -> duckdb.DuckDBPyConnection:
    conn = duckdb.connect(str(tmp_path / "cointracking.duckdb"))
    loader.apply_migrations(conn)
    return conn


def _held(conn, sym: str, days) -> None:
    for d in days:
        conn.execute(
            "INSERT INTO positions_daily VALUES (?, 'cu_1', 'cu_1:ExchangeA', "
            "?, 1, 1)", [d, sym])


def _price_rows(conn, source: str) -> dict[date, Decimal]:
    return dict(conn.execute(
        "SELECT as_of_date, price_usd FROM coin_prices WHERE source = ?",
        [source]).fetchall())


class _Coins:
    """A price client that returns `rows`, or raises when given none."""

    def __init__(self, rows=None):
        self.rows = rows

    def ohlcv_historical(self, coin_id, from_ts, to_ts):
        if self.rows is None:
            raise RuntimeError("provider down")
        return self.rows

    fetch_fiat_to_usd = ohlcv_historical


def _coin_db(tmp_path):
    conn = _fresh_db(tmp_path)
    _held(conn, "BTC", (D1, D2, D3))
    conn.execute("INSERT INTO coin_mapping VALUES "
                 "('BTC', 'binance', 'BTCUSDT', NULL, 0)")
    conn.execute("INSERT INTO coin_prices VALUES (?, 'BTC', 100, 'binance', 0)",
                 [D3])
    return conn


def test_a_failed_price_fetch_keeps_the_stored_price(tmp_path):
    conn = _coin_db(tmp_path)
    assert prices.fetch_coin_prices(conn, _Coins(), "missing") == (0, 0)
    assert _price_rows(conn, "binance") == {D3: Decimal(100)}


def test_the_missing_fill_replaces_the_latest_priced_day(tmp_path):
    # The stored latest day was an intraday price; the fill replaces it
    # with the fetched close and adds the held days still unpriced. A day
    # outside the held range is not stored.
    conn = _coin_db(tmp_path)
    client = _Coins([(D1, 1.0), (D2, 2.0), (D3, 3.0), (date(2024, 1, 4), 4.0)])
    assert prices.fetch_coin_prices(conn, client, "missing") == (1, 3)
    assert _price_rows(conn, "binance") == {
        D1: Decimal(1), D2: Decimal(2), D3: Decimal(3)}


def test_the_full_fill_replaces_every_price_of_its_provider(tmp_path):
    # Full mode drops the provider's rows and fetches the whole held
    # range, so a stored day the fetch no longer returns is gone. Rows of
    # the other provider stay.
    conn = _coin_db(tmp_path)
    conn.execute("INSERT INTO coin_prices VALUES (?, 'BTC', 7, 'binance', 0)",
                 [D1])
    conn.execute(
        "INSERT INTO coin_prices VALUES (?, 'EUR', 1.1, 'frankfurter', 0)", [D1])
    client = _Coins([(D2, 2.0), (D3, 3.0)])
    assert prices.fetch_coin_prices(conn, client, "full") == (1, 2)
    assert _price_rows(conn, "binance") == {D2: Decimal(2), D3: Decimal(3)}
    assert _price_rows(conn, "frankfurter") == {D1: Decimal("1.1")}


def test_the_full_fx_fill_replaces_every_rate(tmp_path):
    conn = _fresh_db(tmp_path)
    _held(conn, "EUR", (D1, D2))
    conn.execute(
        "INSERT INTO coin_prices VALUES (?, 'EUR', 1.1, 'frankfurter', 0)", [D1])
    conn.execute("INSERT INTO coin_prices VALUES (?, 'BTC', 5, 'binance', 0)",
                 [D1])
    assert prices.fetch_fx_rates(conn, _Coins([(D2, 1.2)]), "full") == (1, 1)
    assert _price_rows(conn, "frankfurter") == {D2: Decimal("1.2")}
    assert _price_rows(conn, "binance") == {D1: Decimal(5)}


def test_a_failed_fx_fetch_keeps_the_stored_rate(tmp_path):
    conn = _fresh_db(tmp_path)
    _held(conn, "EUR", (D1, D2))
    conn.execute(
        "INSERT INTO coin_prices VALUES (?, 'EUR', 1.1, 'frankfurter', 0)", [D2])
    assert prices.fetch_fx_rates(conn, _Coins(), "missing") == (0, 0)
    assert _price_rows(conn, "frankfurter") == {D2: Decimal("1.1")}


@pytest.mark.parametrize("fetch", [
    lambda conn, mode: prices.fetch_coin_prices(conn, _Coins(), mode),
    lambda conn, mode: prices.fetch_fx_rates(conn, _Coins(), mode),
    prices.fill,
])
def test_an_unknown_mode_is_refused(tmp_path, fetch):
    with pytest.raises(ValueError, match="unknown fetch mode"):
        fetch(_fresh_db(tmp_path), "some")


def test_the_coin_mapping_is_looked_up_once(tmp_path, monkeypatch):
    # A held coin is looked up the first time and read from coin_mapping
    # after; a coin with no provider pair is remembered as unmapped.
    conn = _fresh_db(tmp_path)
    _held(conn, "BTC", (D1,))
    _held(conn, "XYZ", (D1,))
    calls = []

    def build_mapping(client, missing):
        calls.append(list(missing))
        return {"BTC": "BTCUSDT"}
    monkeypatch.setattr(prices, "build_mapping", build_mapping)
    assert prices.ensure_coin_mapping(conn, _Coins()) == {"BTC": "BTCUSDT"}
    assert prices.ensure_coin_mapping(conn, _Coins()) == {"BTC": "BTCUSDT"}
    assert calls == [["BTC", "XYZ"]]
    assert sorted(conn.execute(
        "SELECT instrument_external_id, provider_coin_id FROM coin_mapping"
    ).fetchall()) == [("BTC", "BTCUSDT"), ("XYZ", None)]


@pytest.mark.parametrize("mode", ["missing", "full"])
def test_fill_runs_coins_then_fx_then_the_gaps(tmp_path, monkeypatch, mode):
    conn = _fresh_db(tmp_path)
    steps = []
    monkeypatch.setattr(prices, "BinanceClient", lambda api_key: "coins")
    monkeypatch.setattr(prices, "FrankfurterClient", lambda: "fx")
    monkeypatch.setattr(prices, "fetch_coin_prices",
                        lambda c, client, mode: steps.append(
                            ("coins", client, mode)) or (0, 0))
    monkeypatch.setattr(prices, "fetch_fx_rates",
                        lambda c, client, mode: steps.append(
                            ("fx", client, mode)) or (0, 0))
    monkeypatch.setattr(prices, "backfill_first_day_gaps",
                        lambda c: steps.append(("gaps",)))
    prices.fill(conn, mode)
    assert steps == [("coins", "coins", mode), ("fx", "fx", mode), ("gaps",)]


def _one_export(tmp_path: Path) -> list[str]:
    run = tmp_path / "bronze" / "20240201T000000Z"
    (run / "cu_1").mkdir(parents=True)
    (run / "cu_1" / "trades.csv").write_text(
        '"Type","Buy","Cur.","Sell","Cur.","Fee","Cur.","Exchange",'
        '"Group","Comment","Date","LPN","Tx-ID"\n'
        '"Deposit","1","BTC","","","","","ExchangeA","","",'
        '"2024-01-10 10:00:00","",""\n', encoding="utf-8")
    (run / "run.json").write_text(
        '{"portfolios": [{"id": "1", "name": "test"}], "status": "complete"}',
        encoding="utf-8")
    return ["--bronze-dir", str(tmp_path / "bronze"),
            "--silver-db", str(tmp_path / "silver.duckdb")]


def test_load_fills_missing_prices_unless_told_not_to(tmp_path, monkeypatch):
    modes = []
    monkeypatch.setattr(prices, "fill", lambda conn, mode: modes.append(mode))
    argv = _one_export(tmp_path)
    assert loader.main([*argv, "--no-fetch-prices"]) == 0
    assert modes == []
    assert loader.main(argv) == 0
    assert modes == ["missing"]


def test_backfill_counts_only_the_rows_it_writes(tmp_path, monkeypatch):
    # A row a fetch wrote in the same second carries the same fetched_at;
    # it is not a backfilled gap.
    monkeypatch.setattr(prices.time, "time", lambda: 1000.0)
    conn = _fresh_db(tmp_path)
    _held(conn, "BTC", (D1, D2))
    conn.execute(
        "INSERT INTO coin_prices VALUES (?, 'BTC', 5, 'binance', 1000)", [D2])
    assert prices.backfill_first_day_gaps(conn) == 1
    assert prices.backfill_first_day_gaps(conn) == 0
    assert _price_rows(conn, "binance") == {D1: Decimal(5), D2: Decimal(5)}
