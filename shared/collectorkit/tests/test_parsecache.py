"""Tests for collectorkit.parsecache: the parse cache, the pool, the codec."""
from __future__ import annotations

import json
import logging
import os
from concurrent.futures.process import BrokenProcessPool
from dataclasses import dataclass
from datetime import date, datetime
from decimal import Decimal

import collector_modules
import pytest

from collectorkit import extract, parsecache
from collectorkit.parsecache import ParseCache, ParsePool, from_json, to_json

SHA = "ab" * 32
OTHER_SHA = "cd" * 32
NS = "stmt.v1.0123abcd"


class Counter:
    """A worker that counts its calls; module-level calls only, so the
    pool parses in-process."""

    def __init__(self, result=None):
        self.calls = []
        self.result = result

    def __call__(self, arg):
        self.calls.append(arg)
        return self.result if self.result is not None else {"arg": arg}


# ------------------------------------------------------------------
# ParseCache
# ------------------------------------------------------------------

def test_put_then_get_from_memory_and_from_disk(tmp_path):
    cache = ParseCache(tmp_path)
    cache.put(SHA, NS, {"rows": [1, 2]})
    assert cache.get(SHA, NS) == {"rows": [1, 2]}
    assert (tmp_path / f"{SHA}.{NS}.json").is_file()
    assert ParseCache(tmp_path).get(SHA, NS) == {"rows": [1, 2]}
    assert ParseCache(tmp_path).get(SHA, "stmt.v2.0123abcd") is None


def test_an_error_result_is_never_stored(tmp_path):
    cache = ParseCache(tmp_path)
    cache.put(SHA, NS, {"_error": "ValueError()"})
    assert cache.get(SHA, NS) is None
    assert not list(tmp_path.iterdir())


def test_no_directory_is_memory_only():
    cache = ParseCache()
    cache.put(SHA, NS, [1])
    assert cache.get(SHA, NS) == [1]


def test_unreadable_entry_is_a_miss(tmp_path):
    (tmp_path / f"{SHA}.{NS}.json").write_text("{not json", encoding="utf-8")
    assert ParseCache(tmp_path).get(SHA, NS) is None


def test_unwritable_directory_leaves_memory_only(tmp_path):
    blocker = tmp_path / "file"
    blocker.write_text("x")
    cache = ParseCache(blocker / "cache")
    cache.put(SHA, NS, [1])
    assert cache.get(SHA, NS) == [1]


def test_a_secrets_directory_is_refused(tmp_path, monkeypatch, caplog):
    secrets = tmp_path / "secrets"
    monkeypatch.setattr(extract, "_SECRETS_DIRS", (secrets,))
    with caplog.at_level(logging.WARNING, "collectorkit.extract"):
        cache = ParseCache(secrets / "parse-cache")
    cache.put(SHA, NS, [1])
    assert cache.get(SHA, NS) == [1]
    assert not secrets.exists()
    assert "secrets directory" in caplog.text


def test_a_bad_key_is_refused(tmp_path):
    with pytest.raises(ValueError):
        ParseCache(tmp_path).put(SHA, "../escape", [1])
    with pytest.raises(ValueError):
        ParseCache(tmp_path).get("a.b", NS)


def test_sweep_keeps_only_the_named_namespaces(tmp_path):
    cache = ParseCache(tmp_path)
    for ns in ("a.v1.aaaa", "a.v1.bbbb", "b.v1.cccc.identity"):
        cache.put(SHA, ns, [ns])
        cache.put(OTHER_SHA, ns, [ns])
    (tmp_path / "unrelated.txt").write_text("x")
    assert cache.sweep({"a.v1.bbbb", "b.v1.cccc.identity"}) == 2
    names = sorted(p.name for p in tmp_path.iterdir())
    assert names == sorted([f"{s}.{ns}.json"
                            for s in (SHA, OTHER_SHA)
                            for ns in ("a.v1.bbbb", "b.v1.cccc.identity")]
                           + ["unrelated.txt"])
    assert cache.get(SHA, "a.v1.aaaa") is None
    assert cache.get(SHA, "a.v1.bbbb") == ["a.v1.bbbb"]


def test_default_dir_is_under_the_cache_root(monkeypatch, tmp_path):
    monkeypatch.delenv("WEALTHDB_CACHE_DIR", raising=False)
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
    assert parsecache.default_dir("chase") == \
        tmp_path / "wealthdb" / "chase" / "parse-cache"


@pytest.mark.parametrize("mask", [0o022, 0])
def test_the_cache_dirs_it_creates_are_private(tmp_path, mask):
    old = os.umask(mask)
    try:
        ParseCache(tmp_path / "root" / "parse-cache").put(SHA, NS, [1])
    finally:
        os.umask(old)
    for d in (tmp_path / "root", tmp_path / "root" / "parse-cache"):
        assert d.stat().st_mode & 0o777 == 0o700


# ------------------------------------------------------------------
# cached
# ------------------------------------------------------------------

@dataclass
class _Parsed:
    day: date
    amount: Decimal
    lines: list[str]


def test_cached_parses_once_and_decodes_the_kind(tmp_path):
    calls = []

    def parse(path, scale):
        calls.append((path, scale))
        return _Parsed(date(2031, 1, 2), Decimal("1.10"), ["a"])

    first = parsecache.cached(ParseCache(tmp_path), SHA, NS, parse, "a.pdf",
                              2, kind=_Parsed)
    again = parsecache.cached(ParseCache(tmp_path), SHA, NS, parse, "a.pdf",
                              2, kind=_Parsed)
    assert first == again == _Parsed(date(2031, 1, 2), Decimal("1.10"), ["a"])
    assert calls == [("a.pdf", 2)]
    assert json.loads((tmp_path / f"{SHA}.{NS}.json").read_text()) == {
        "day": "2031-01-02", "amount": "1.10", "lines": ["a"]}


def test_cached_without_a_kind_stores_the_result_as_it_is(tmp_path):
    worker = Counter({"rows": [1]})
    for _ in range(2):
        assert parsecache.cached(ParseCache(tmp_path), SHA, NS, worker,
                                 "a") == {"rows": [1]}
    assert worker.calls == ["a"]


def test_cached_stores_no_error_and_no_raise(tmp_path):
    failing = Counter({"_error": "ValueError()"})
    cache = ParseCache(tmp_path)
    for _ in range(2):
        assert parsecache.is_error(parsecache.cached(cache, SHA, NS, failing,
                                                     "a"))
    assert len(failing.calls) == 2

    def boom():
        raise ValueError("unreadable")

    with pytest.raises(ValueError):
        parsecache.cached(cache, OTHER_SHA, NS, boom)
    assert not list(tmp_path.iterdir())


# ------------------------------------------------------------------
# ParsePool
# ------------------------------------------------------------------

def test_one_worker_parses_in_process_and_caches(tmp_path):
    worker = Counter()
    with ParsePool(ParseCache(tmp_path), 1) as pool:
        pool.enqueue(SHA, NS, worker, "a")
        pool.enqueue(OTHER_SHA, NS, worker, "b")
        pool.dispatch()
        assert pool.resolve(SHA, NS, worker, "a") == {"arg": "a"}
        assert pool.resolve(SHA, NS, worker, "a") == {"arg": "a"}
        assert pool.resolve(OTHER_SHA, NS, worker, "b") == {"arg": "b"}
    assert worker.calls == ["a", "b"]


def test_a_second_load_calls_no_parser(tmp_path):
    first, second = Counter(), Counter()
    for worker in (first, second):
        with ParsePool(ParseCache(tmp_path), 1) as pool:
            for sha in (SHA, OTHER_SHA):
                pool.enqueue(sha, NS, worker, sha)
            pool.dispatch()
            assert [pool.resolve(sha, NS, worker, sha)["arg"]
                    for sha in (SHA, OTHER_SHA)] == [SHA, OTHER_SHA]
    assert len(first.calls) == 2
    assert second.calls == []


def test_an_error_result_is_parsed_again_next_load(tmp_path):
    worker = Counter({"_error": "RuntimeError()"})
    for _ in range(2):
        with ParsePool(ParseCache(tmp_path), 1) as pool:
            assert parsecache.is_error(pool.resolve(SHA, NS, worker, "a"))
    assert worker.calls == ["a", "a"]


def test_the_pool_parses_in_worker_processes(tmp_path):
    with ParsePool(ParseCache(tmp_path), 2) as pool:
        for sha, arg in ((SHA, "ab"), (OTHER_SHA, "cd")):
            pool.enqueue(sha, NS, str.upper, arg)
        pool.dispatch()
        assert pool._pool is not None
        assert pool.resolve(SHA, NS, str.upper, "ab") == "AB"
        assert pool.resolve(OTHER_SHA, NS, str.upper, "cd") == "CD"
    assert ParseCache(tmp_path).get(OTHER_SHA, NS) == "CD"


class _BrokenFuture:
    def result(self):
        raise BrokenProcessPool("a worker died")


class _BrokenExecutor:
    made = 0

    def __init__(self, max_workers):
        type(self).made += 1
        self.shut = None

    def submit(self, fn, arg):
        return _BrokenFuture()

    def shutdown(self, wait=True, cancel_futures=False):
        self.shut = (wait, cancel_futures)


def test_a_broken_pool_falls_back_to_in_process(tmp_path, monkeypatch, caplog):
    monkeypatch.setattr(parsecache, "ProcessPoolExecutor", _BrokenExecutor)
    _BrokenExecutor.made = 0
    worker = Counter()
    pool = ParsePool(ParseCache(tmp_path), 4)
    for sha, arg in ((SHA, "a"), (OTHER_SHA, "b")):
        pool.enqueue(sha, NS, worker, arg)
    pool.dispatch()
    executor = pool._pool
    with caplog.at_level(logging.WARNING, "collectorkit.parsecache"):
        assert pool.resolve(SHA, NS, worker, "a") == {"arg": "a"}
    assert "lost a worker" in caplog.text
    assert executor.shut == (False, True)
    assert pool._pool is None
    assert pool.resolve(OTHER_SHA, NS, worker, "b") == {"arg": "b"}
    assert worker.calls == ["a", "b"]
    # A later dispatch makes a fresh pool.
    pool.enqueue("ef" * 32, NS, worker, "c")
    pool.enqueue("01" * 32, NS, worker, "d")
    pool.dispatch()
    assert _BrokenExecutor.made == 2
    pool.close()


def test_sha_for_hashes_each_path_once(tmp_path, monkeypatch):
    doc = tmp_path / "a.pdf"
    doc.write_bytes(b"%PDF-1.4 synthetic")
    seen = []
    real = parsecache.bronze.sha256_file
    monkeypatch.setattr(parsecache.bronze, "sha256_file",
                        lambda p: seen.append(p) or real(p))
    pool = ParsePool(None, 1)
    assert pool.sha_for(doc) == pool.sha_for(str(doc))
    assert len(seen) == 1


# ------------------------------------------------------------------
# The codec
# ------------------------------------------------------------------

def _roundtrip(value, cls):
    return from_json(json.loads(json.dumps(to_json(value))), cls)


def test_chase_statements_survive_the_codec():
    sp = collector_modules.load("chase", "statement_parser")
    deposit = sp.ParsedStatement(
        period_start=date(2024, 12, 19), period_end=date(2025, 1, 17),
        segments=[sp.StatementSegment(
            beginning_balance=Decimal("1000.10"), ending_balance=None,
            transactions=[
                sp.StatementTxn(date(2024, 12, 31), Decimal("-12.30"),
                                "SYNTHETIC PAYEE", check_number="101"),
                sp.StatementTxn(date(2025, 1, 2), Decimal("5E+1"), "")])])
    card = sp.ParsedCardStatement(
        period_start=date(2025, 1, 1), period_end=date(2025, 1, 31),
        previous_balance=Decimal("0.00"), new_balance=Decimal("10.00"),
        transactions=[sp.StatementTxn(date(2025, 1, 5), Decimal("10.00"),
                                      "SHOP", kind="STMT_PURCHASE")])
    for value, cls in ((deposit, sp.ParsedStatement),
                       (card, sp.ParsedCardStatement)):
        back = _roundtrip(value, cls)
        assert back == value
        assert repr(back) == repr(value)


def test_amex_statements_survive_the_codec():
    sp = collector_modules.load("amex", "statement_parser")
    stmt = sp.ParsedCardStatement(
        period_end=date(2025, 2, 28), previous_balance=Decimal("-0.00"),
        payments_credits=Decimal("-100.00"), new_charges=Decimal("60.10"),
        transactions=[sp.StatementTxn(date(2025, 2, 3), "SHOP",
                                      Decimal("60.10"), "STMT_PURCHASE",
                                      posting_date=True)])
    back = _roundtrip(stmt, sp.ParsedCardStatement)
    assert repr(back) == repr(stmt)


@dataclass
class Shape:
    at: datetime
    pair: tuple[int, str]
    many: tuple[Decimal, ...]
    by: dict[str, list[Decimal | None]]


def test_codec_reads_tuples_dicts_and_datetimes():
    value = Shape(datetime(2025, 1, 2, 3, 4, 5), (1, "x"),
                  (Decimal("1.0"), Decimal("2")), {"k": [None, Decimal("3.30")]})
    assert repr(_roundtrip(value, Shape)) == repr(value)
