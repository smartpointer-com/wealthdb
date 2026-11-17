"""A content-addressed parse cache and one parse pool per load.

A statement's parse is a pure function of its bytes and of the parser that
read them. So a load keys each result on the document's sha256 and on a
namespace that holds the parser's fingerprint, stores it once, and replays
it on every later sighting: within one run, across runs, and across a
``load --force``. A parser edit moves its fingerprint and with it the
namespace, so the old entries are never read again; :meth:`ParseCache.sweep`
removes them.

:class:`ParsePool` spreads the misses of one load over a process pool.
Every miss is enqueued while the runs are scanned, dispatched once, and
resolved at its insertion point. A pool that loses a worker is dropped and
the rest of its work is parsed in-process, so a crashed worker slows a load
down rather than failing it.

Two rules hold for both:

* **An error result is never cached.** A worker reports a failure as a dict
  with an ``"_error"`` key; it is handed back and parsed again next run.
* **The cache never stops a load.** An unreadable entry is a miss, and a
  directory that cannot be written (or that lies inside a secrets
  directory) leaves the cache in memory only.

Results are JSON. :func:`to_json` and :func:`from_json` carry a parser's
dataclass results (``Decimal`` amounts, ``date`` days, nested lists)
through it exactly. :func:`cached` is one document's parse through the
cache, with the codec when a result type is given.

This module is the loaders'. No parser imports it.
"""
from __future__ import annotations

import dataclasses
import json
import logging
import re
import types
import typing
from concurrent.futures import ProcessPoolExecutor
from concurrent.futures.process import BrokenProcessPool
from datetime import date, datetime
from decimal import Decimal
from pathlib import Path

from collectorkit import bronze
from collectorkit.extract import cache_root, make_private_dir, usable_cache_dir

log = logging.getLogger("collectorkit.parsecache")

# A sidecar is `<sha>.<namespace>.json`: the sha has no dot, the namespace
# may have several.
_ENTRY_RE = re.compile(r"^(?P<sha>[^.]+)\.(?P<namespace>.+)\.json$")
_NAMESPACE_RE = re.compile(r"^[A-Za-z0-9_.-]+$")


def default_dir(collector: str) -> Path:
    """A collector's parse-cache directory: ``parse-cache`` under its cache
    root (``collectorkit.extract.cache_root``), beside the extraction
    cache (``collectorkit.extract.default_dir``)."""
    return cache_root(collector) / "parse-cache"


def is_error(result) -> bool:
    """Whether a worker's result reports a failure."""
    return isinstance(result, dict) and "_error" in result


class ParseCache:
    """Maps ``(sha256, namespace)`` to a parse result: an in-process map,
    backed by one JSON sidecar per entry, ``<dir>/<sha>.<namespace>.json``.

    ``get`` reads memory, then the sidecar; ``put`` writes both. With no
    directory, or one that is refused or cannot be written, the cache is
    memory-only for the run (logged at debug)."""

    def __init__(self, directory=None):
        self._mem: dict[tuple[str, str], object] = {}
        self._dir = usable_cache_dir(directory)
        self._dir_ready: bool | None = None

    def _sidecar(self, sha: str, namespace: str) -> Path | None:
        if not _NAMESPACE_RE.match(namespace) or "." in sha or "/" in sha:
            raise ValueError(f"not a cache key: {sha!r}, {namespace!r}")
        if self._dir is None:
            return None
        return self._dir / f"{sha}.{namespace}.json"

    def get(self, sha: str, namespace: str):
        """The cached result, or None."""
        key = (sha, namespace)
        if key in self._mem:
            return self._mem[key]
        path = self._sidecar(sha, namespace)
        if path is None or not path.is_file():
            return None
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as e:
            log.debug("parse-cache: unreadable %s: %s", path, e)
            return None
        self._mem[key] = data
        return data

    def put(self, sha: str, namespace: str, result) -> None:
        """Store a result. An error result is refused, never stored."""
        if is_error(result):
            return
        path = self._sidecar(sha, namespace)
        self._mem[(sha, namespace)] = result
        if path is None or not self._ensure_dir():
            return
        tmp = path.with_name(path.name + ".tmp")
        try:
            tmp.write_text(json.dumps(result, ensure_ascii=False),
                           encoding="utf-8")
            tmp.replace(path)
        except OSError as e:
            log.debug("parse-cache: could not write %s: %s", path, e)
            tmp.unlink(missing_ok=True)

    def sweep(self, keep_namespaces) -> int:
        """Delete every sidecar whose namespace is not in
        ``keep_namespaces``, and return how many went. Run after a load
        with the namespaces of the parsers it holds: the entries of an
        older parser are never read again."""
        if self._dir is None or not self._dir.is_dir():
            return 0
        keep = set(keep_namespaces)
        removed = 0
        for path in self._dir.iterdir():
            m = _ENTRY_RE.match(path.name)
            if m is None or m["namespace"] in keep:
                continue
            try:
                path.unlink()
                removed += 1
            except OSError as e:
                log.debug("parse-cache: could not remove %s: %s", path, e)
        self._mem = {k: v for k, v in self._mem.items() if k[1] in keep}
        return removed

    def _ensure_dir(self) -> bool:
        if self._dir_ready is None:
            try:
                make_private_dir(self._dir)
                self._dir_ready = True
            except OSError as e:
                log.debug("parse-cache: memory only (cannot create %s): %s",
                          self._dir, e)
                self._dir_ready = False
        return self._dir_ready


def cached(cache: ParseCache, sha: str, namespace: str, parse, *args,
           kind=None):
    """``parse(*args)``, read from `cache` when this document was parsed
    under `namespace` before, and stored there when not.

    With `kind`, a dataclass type or a type hint, the result goes
    through :func:`to_json` into the cache and comes back out through
    ``from_json(data, kind)``. Without it, the result is stored as it
    is, so it must be JSON already. A parse that raises stores nothing,
    and neither does an error result: the next call parses such a
    document again, and the caller decides whether a load makes one."""
    hit = cache.get(sha, namespace)
    if hit is not None:
        return hit if kind is None else from_json(hit, kind)
    parsed = parse(*args)
    cache.put(sha, namespace, parsed if kind is None else to_json(parsed))
    return parsed


class ParsePool:
    """One process pool and one :class:`ParseCache` shared by a whole load.

    ``enqueue`` every document while the runs are scanned, ``dispatch``
    once, then ``resolve`` each document where its rows are inserted.
    Cache hits are never enqueued, and work is deduplicated on (sha,
    namespace). No pool is made for fewer than two misses, nor with one
    worker: then ``resolve`` parses in-process, which is also what a
    test's monkeypatched parser needs. A caller may ``resolve`` without
    enqueueing at all.

    A worker is a module-level function of one argument, so it pickles
    under spawn. If the pool loses a worker, ``resolve`` drops it and
    parses that document and every one still outstanding in-process. A
    later ``dispatch`` makes a fresh pool."""

    def __init__(self, cache: ParseCache | None, workers: int):
        self._cache = cache if cache is not None else ParseCache()
        self._workers = max(1, workers)
        self._work: dict[tuple[str, str], tuple] = {}
        self._futures: dict[tuple[str, str], object] = {}
        self._sha_by_path: dict[str, str] = {}
        self._pool: ProcessPoolExecutor | None = None

    def sha_for(self, path) -> str:
        """The sha256 of a document, hashed once per path for the load."""
        key = str(path)
        sha = self._sha_by_path.get(key)
        if sha is None:
            sha = bronze.sha256_file(path)[0]
            self._sha_by_path[key] = sha
        return sha

    def enqueue(self, sha: str, namespace: str, worker, arg) -> None:
        if self._cache.get(sha, namespace) is not None:
            return
        self._work.setdefault((sha, namespace), (worker, arg))

    def dispatch(self) -> None:
        work = {k: v for k, v in self._work.items() if k not in self._futures}
        self._work = {}
        if self._workers == 1 or len(work) < 2:
            return                  # resolve() parses these in-process
        if self._pool is None:
            self._pool = ProcessPoolExecutor(
                max_workers=min(len(work), self._workers))
        try:
            for key, (worker, arg) in work.items():
                self._futures[key] = self._pool.submit(worker, arg)
        except BrokenProcessPool:
            self._break("submitting")

    def resolve(self, sha: str, namespace: str, worker, arg):
        """The result for one document: cached, from the pool, or parsed
        here. An error result is returned and not cached."""
        cached = self._cache.get(sha, namespace)
        if cached is not None:
            return cached
        future = self._futures.get((sha, namespace))
        result = None
        if future is not None:
            try:
                result = future.result()
            except BrokenProcessPool:
                self._break("parsing")
                future = None
        if future is None:
            result = worker(arg)
        self._cache.put(sha, namespace, result)
        return result

    def _break(self, stage: str) -> None:
        """Drop a pool that lost a worker. Its outstanding documents are
        parsed in-process as they are resolved."""
        log.warning("parse pool lost a worker while %s; parsing the %d "
                    "outstanding document(s) in-process", stage,
                    len(self._futures))
        self._futures.clear()
        if self._pool is not None:
            self._pool.shutdown(wait=False, cancel_futures=True)
            self._pool = None

    def close(self) -> None:
        if self._pool is not None:
            self._pool.shutdown()
            self._pool = None

    def __enter__(self) -> ParsePool:
        return self

    def __exit__(self, *exc) -> bool:
        self.close()
        return False


# ============================================================
# The codec: a parser's dataclass results as JSON
# ============================================================

def to_json(value):
    """A parse result as JSON-ready data. A dataclass becomes a dict of its
    fields, a ``Decimal`` its exact string (``"1.10"`` stays ``"1.10"``),
    a ``date`` or ``datetime`` its ISO form; lists, tuples, dicts and
    scalars pass through. :func:`from_json` reverses it from the type
    hints."""
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        return {f.name: to_json(getattr(value, f.name))
                for f in dataclasses.fields(value)}
    if isinstance(value, Decimal):
        return str(value)
    if isinstance(value, (date, datetime)):
        return value.isoformat()
    if isinstance(value, (list, tuple)):
        return [to_json(v) for v in value]
    if isinstance(value, dict):
        return {k: to_json(v) for k, v in value.items()}
    return value


def from_json(data, cls):
    """Rebuild a value of type ``cls`` from :func:`to_json`'s data: a
    dataclass from its field hints, ``Decimal``, ``date``, ``datetime``,
    ``list[X]``, ``tuple[X, ...]``, ``dict[K, V]``, ``X | None`` and
    plain scalars."""
    if data is None:
        return None
    origin = typing.get_origin(cls)
    if origin in (typing.Union, types.UnionType):
        options = [a for a in typing.get_args(cls) if a is not type(None)]
        if len(options) != 1:
            raise TypeError(f"cannot decode the union {cls!r}")
        return from_json(data, options[0])
    if origin is list:
        (item,) = typing.get_args(cls)
        return [from_json(v, item) for v in data]
    if origin is tuple:
        args = typing.get_args(cls)
        if len(args) == 2 and args[1] is Ellipsis:
            return tuple(from_json(v, args[0]) for v in data)
        return tuple(from_json(v, a) for v, a in zip(data, args, strict=True))
    if origin is dict:
        key, item = typing.get_args(cls)
        return {from_json(k, key): from_json(v, item) for k, v in data.items()}
    if dataclasses.is_dataclass(cls):
        hints = typing.get_type_hints(cls)
        return cls(**{f.name: from_json(data[f.name], hints[f.name])
                      for f in dataclasses.fields(cls) if f.name in data})
    if cls is Decimal:
        return Decimal(data)
    if cls is datetime:
        return datetime.fromisoformat(data)
    if cls is date:
        return date.fromisoformat(data)
    return data
