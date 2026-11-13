"""Run parser functions from two checkouts over the same documents and
compare what they return, by hashes and counts only.

A change inside a parser closure must keep every stored document's
result, or say which ones it changes. This tool runs each
``MODULE:FUNCTION`` from the old and from the new checkout over every
unique document under a bronze directory and compares the two results.

* A document is a file under ``--bronze`` that matches a ``--glob``
  pattern. Files with the same bytes run once (unique by SHA-256).
* Each side runs in its own interpreter (``python -I``) with ``sys.path``
  set to its root, then to the ``shared/collectorkit`` of the checkout
  that holds the root, so the kit comes from the same tree as the
  parser.
* The function is called with the document's path. Its result is
  reduced to a hash of a canonical JSON form; an exception is reduced to
  a hash of its type and text. The parser's own output is discarded.

It prints counts and the SHA-256 of each mismatching file, never a path
or a parsed value.

Exit status: 0 when the two sides agree on every document, 1 when they
differ, 2 on a usage error, when a document cannot be read, or when a
side cannot start or import a function.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import subprocess
import sys
import tempfile
from pathlib import Path

# Runs inside each side's interpreter. It reads one job file and writes one
# result file, so nothing the parser prints can mix with the results.
WORKER = r'''
import dataclasses, datetime, decimal, enum, hashlib, json, sys

job = json.load(open(sys.argv[1], encoding="utf-8"))
sys.path[:0] = job["sys_path"]


def canon(o):
    if isinstance(o, enum.Enum):
        return [type(o).__qualname__, canon(o.value)]
    if o is None or isinstance(o, (bool, int, str)):
        return o
    if isinstance(o, float):
        return ["float", repr(o)]
    if isinstance(o, decimal.Decimal):
        return ["decimal", str(o)]
    if isinstance(o, (datetime.date, datetime.time)):
        return [type(o).__name__, o.isoformat()]
    if isinstance(o, (bytes, bytearray)):
        return ["bytes", bytes(o).hex()]
    if isinstance(o, (list, tuple)):
        return [canon(v) for v in o]
    if isinstance(o, (set, frozenset)):
        return ["set", sorted((canon(v) for v in o), key=json.dumps)]
    if isinstance(o, dict):
        pairs = [[canon(k), canon(v)] for k, v in o.items()]
        return ["dict", sorted(pairs, key=lambda p: json.dumps(p[0]))]
    if dataclasses.is_dataclass(o) and not isinstance(o, type):
        return [type(o).__qualname__, canon(
            {f.name: getattr(o, f.name) for f in dataclasses.fields(o)})]
    if hasattr(o, "__dict__"):
        return [type(o).__qualname__, canon(vars(o))]
    return [type(o).__qualname__, str(o)]


def digest(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


out = {}
for spec in job["specs"]:
    module, _, name = spec.partition(":")
    try:
        __import__(module)
        fn = getattr(sys.modules[module], name)
    except BaseException as e:
        json.dump({"fatal": spec, "error": type(e).__name__},
                  open(job["out"], "w", encoding="utf-8"))
        raise SystemExit(3)
    for sha, path in job["files"]:
        try:
            result = "ok:" + json.dumps(canon(fn(path)), ensure_ascii=False)
        except (Exception, SystemExit) as e:
            result = "raise:%s.%s: %s" % (type(e).__module__,
                                          type(e).__qualname__, e)
        out[spec + "\0" + sha] = digest(result)
json.dump({"results": out}, open(job["out"], "w", encoding="utf-8"))
'''


def _kit_path(root: Path) -> list[str]:
    """The ``shared/collectorkit`` of the checkout that holds `root`, if
    any."""
    for d in (root, *root.parents):
        kit = d / "shared" / "collectorkit"
        if (kit / "collectorkit" / "__init__.py").is_file():
            return [str(kit)]
    return []


def unique_documents(bronze: Path, globs: list[str]) -> tuple[int, list[tuple[str, str]]]:
    """``(files seen, [(sha256, path), ...])``: one path per distinct
    content, the first in path order, sorted by hash."""
    paths = sorted({p for g in globs for p in bronze.rglob(g)
                    if p.is_file() and not p.is_symlink()})
    first: dict[str, str] = {}
    for p in paths:
        h = hashlib.sha256()
        with p.open("rb") as fh:
            for chunk in iter(lambda: fh.read(1 << 20), b""):
                h.update(chunk)
        first.setdefault(h.hexdigest(), str(p))
    return len(paths), sorted(first.items())


def _start(python: str, root: Path, specs: list[str],
           files: list[tuple[str, str]], work: Path, tag: str):
    job, out = work / f"{tag}.job.json", work / f"{tag}.out.json"
    job.write_text(json.dumps({
        "sys_path": [str(root), *_kit_path(root)],
        "specs": specs, "files": files, "out": str(out)}), encoding="utf-8")
    proc = subprocess.Popen([python, "-I", "-c", WORKER, str(job)], cwd=root,
                            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL)
    return proc, out


def _collect(side: str, procs) -> dict[str, str]:
    results: dict[str, str] = {}
    for proc, out in procs:
        proc.wait()
        try:
            data = json.loads(out.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            raise RuntimeError(f"{side} side: the worker wrote no result "
                               f"(exit status {proc.returncode})") from None
        if "fatal" in data:
            raise RuntimeError(f"{side} side: cannot load {data['fatal']}"
                               f" ({data['error']})")
        results.update(data["results"])
    return results


def run(old: Path, new: Path, files: list[tuple[str, str]], specs: list[str],
        *, python: str = sys.executable, jobs: int = 1) -> dict[str, dict]:
    """Per spec: ``{"same": n, "differ": [sha256, ...]}``."""
    with tempfile.TemporaryDirectory(prefix="parse_diff.") as tmp:
        work = Path(tmp)
        shards = [files[i::jobs] for i in range(jobs)] if files else [[]]
        procs = {side: [_start(python, root, specs, shard, work, f"{side}{i}")
                        for i, shard in enumerate(shards)]
                 for side, root in (("old", old), ("new", new))}
        got = {side: _collect(side, p) for side, p in procs.items()}
    report: dict[str, dict] = {}
    for spec in specs:
        same, differ = 0, []
        for sha, _ in files:
            key = spec + "\0" + sha
            if got["old"][key] == got["new"][key]:
                same += 1
            else:
                differ.append(sha)
        report[spec] = {"same": same, "differ": differ}
    return report


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="parse_diff.py",
        description="Run parser functions from two checkouts over the unique "
                    "documents of a bronze directory and compare their "
                    "results. Prints counts and file hashes, never data.")
    p.add_argument("specs", nargs="+", metavar="MODULE:FUNCTION",
                   help="A parser function, importable from each root; it is "
                        "called with one document path.")
    p.add_argument("--old", type=Path, required=True,
                   help="The root that holds the old parser modules, e.g. "
                        "<old checkout>/collectors/<name>.")
    p.add_argument("--new", type=Path, required=True,
                   help="The same root in the new checkout.")
    p.add_argument("--bronze", type=Path, required=True,
                   help="The directory searched for documents.")
    p.add_argument("--glob", action="append", default=None, metavar="PATTERN",
                   help="File pattern of a document, searched recursively "
                        "(repeatable; default: *.pdf).")
    p.add_argument("--python", default=sys.executable,
                   help="The interpreter both sides run in (default: this "
                        "one). It must provide the parsers' packages.")
    p.add_argument("--jobs", type=int, default=1,
                   help="Worker processes per side (default: 1).")
    args = p.parse_args(argv)
    for spec in args.specs:
        module, colon, name = spec.partition(":")
        if not (module and colon and name):
            p.error(f"{spec!r}: expected MODULE:FUNCTION")
    for flag, d in (("--old", args.old), ("--new", args.new),
                    ("--bronze", args.bronze)):
        if not d.is_dir():
            p.error(f"{flag} {d}: not a directory")
    if args.jobs < 1:
        p.error("--jobs must be at least 1")
    try:
        seen, files = unique_documents(args.bronze, args.glob or ["*.pdf"])
    except OSError as e:  # the class only: the message names the path
        print(f"parse_diff: cannot read a document ({e.__class__.__name__})",
              file=sys.stderr)
        return 2
    print(f"documents: {len(files)} unique of {seen} files")
    try:
        report = run(args.old.resolve(), args.new.resolve(), files, args.specs,
                     python=args.python, jobs=args.jobs)
    except RuntimeError as e:
        print(f"parse_diff: {e}", file=sys.stderr)
        return 2
    except OSError as e:  # --python does not start
        print(f"parse_diff: cannot run {args.python} ({e.__class__.__name__})",
              file=sys.stderr)
        return 2
    differs = False
    for spec, r in report.items():
        print(f"{spec}: {r['same']} same, {len(r['differ'])} differ")
        for sha in r["differ"]:
            print(f"  differs: {sha}")
        differs = differs or bool(r["differ"])
    return 1 if differs else 0


if __name__ == "__main__":
    raise SystemExit(main())
