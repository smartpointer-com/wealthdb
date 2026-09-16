"""Source fingerprint of a parser's first-party import closure.

A persistent parse cache keys each entry on "the parsing logic that
produced it" so that changing the logic invalidates stale entries. Two
naive keys fall short:

* Hashing the parser module's raw source *bytes* is too eager — it fires
  on comment, formatting, and docstring edits (which happen constantly)
  even though none change what a parse produces — and too narrow: it
  misses the code the parser *imports* (a shared ``collectorkit`` helper,
  a sibling module), a change to which silently shifts the output.
* A hand-maintained version constant relies on a human remembering to
  bump it, and still ignores imported code.

``parser_fingerprint`` computes a key that:

* ignores comments, formatting, and docstrings — two sources that differ
  only in those hash the same (a position-stripped, docstring-stripped
  AST dump), while any change that affects execution changes the hash.
  For Python the AST *is* the exact semantic normalisation; it needs no
  external formatter and, unlike one, drops comments outright;
* spans the transitive FIRST-PARTY import closure of the given root
  modules — their own directory plus the ``collectorkit`` package — so
  editing an imported helper invalidates too. Third-party / stdlib
  imports are not followed (they can't be meaningfully AST-hashed);
* folds in the installed versions of the declared third-party extraction
  dependencies and the running Python version, since either can shift
  extraction for identical input bytes;
* folds in the self-reported version of any declared extraction BINARY.
  A collector that shells out to `pdftotext` has no Python distribution to
  pin, so without this a poppler upgrade that re-renders a column would
  change every parsed description while the fingerprint stood still —
  which is the drift such a collector is otherwise least able to see.

The result only ever *adds* invalidation relative to a raw-byte hash of
one file, so it cannot serve staler data than the eager scheme — it just
stops firing on edits that don't matter and starts firing on imported
edits that do.
"""
from __future__ import annotations

import ast
import hashlib
import importlib.util
import platform
import subprocess
from importlib.metadata import PackageNotFoundError
from importlib.metadata import version as _dist_version
from pathlib import Path
from types import ModuleType


def _strip_docstrings(tree: ast.AST) -> None:
    """Drop the docstring statement from every module/class/function so a
    docstring edit doesn't change the dump. Docstrings are documentation,
    not behaviour — no parser reads its own ``__doc__`` to produce rows."""
    for node in list(ast.walk(tree)):
        if not isinstance(
            node, (ast.Module, ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)
        ):
            continue
        body = node.body
        if (
            body
            and isinstance(body[0], ast.Expr)
            and isinstance(body[0].value, ast.Constant)
            and isinstance(body[0].value.value, str)
        ):
            node.body = body[1:]


def normalized_source_hash(path: Path) -> str:
    """A hash of ``path``'s Python source invariant to comments, formatting,
    and docstrings but sensitive to anything that affects execution. Falls
    back to a raw-byte hash — conservative, i.e. over-invalidating — if the
    file can't be parsed as Python."""
    raw = Path(path).read_bytes()
    try:
        tree = ast.parse(raw)
        _strip_docstrings(tree)
        norm = ast.dump(tree, annotate_fields=True, include_attributes=False)
    except SyntaxError:
        norm = "rawbytes:" + hashlib.sha256(raw).hexdigest()
    return hashlib.sha256(norm.encode("utf-8")).hexdigest()


def _dotted_prefixes(name: str) -> list[str]:
    """``"a.b.c"`` -> ``["a", "a.b", "a.b.c"]`` so a first-party dotted
    import pulls in the package ``__init__`` files, not only the leaf."""
    parts = name.split(".")
    return [".".join(parts[: i + 1]) for i in range(len(parts))]


def _first_party_origin(name: str, root_dirs: set[Path]) -> Path | None:
    """Resolve ``name`` to a source file iff it is first-party: a module (or
    package) living under one of the root modules' directories, or a
    ``collectorkit`` module. Anything else (stdlib, third-party,
    unresolvable) returns ``None`` and is not followed.

    The root-dir case is resolved by a direct file check rather than
    ``find_spec`` so it does not depend on the collector's directory being
    on ``sys.path`` at fingerprint time."""
    parts = name.split(".")
    for d in root_dirs:
        module_file = d.joinpath(*parts).with_suffix(".py")
        if module_file.is_file():
            return module_file.resolve()
        package_init = d.joinpath(*parts, "__init__.py")
        if package_init.is_file():
            return package_init.resolve()
    if parts[0] == "collectorkit":
        try:
            spec = importlib.util.find_spec(name)
        except (ImportError, AttributeError, ValueError):
            return None
        if spec and spec.origin and spec.origin not in ("built-in", "frozen"):
            return Path(spec.origin).resolve()
    return None


def _imported_names(tree: ast.AST) -> list[str]:
    """Every module name referenced by an import in ``tree`` (including
    lazy imports inside functions, which ``ast.walk`` still reaches).
    Relative imports are skipped — the collectors use absolute imports."""
    names: list[str] = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            names += [a.name for a in node.names]
        elif isinstance(node, ast.ImportFrom):
            if node.level:  # relative import
                continue
            if node.module:
                names.append(node.module)
                # `from collectorkit import pdf` -> submodule collectorkit.pdf
                names += [f"{node.module}.{a.name}" for a in node.names]
    return names


def _closure_files(roots: list[ModuleType]) -> list[Path]:
    """The transitive first-party source files reachable from ``roots``."""
    root_dirs: set[Path] = set()
    start: list[Path] = []
    for r in roots:
        f = Path(r.__file__).resolve()
        start.append(f)
        root_dirs.add(f.parent)

    files: set[Path] = set(start)
    to_visit = list(start)
    while to_visit:
        f = to_visit.pop()
        # A package __init__ is hashed (it executes on import) but its imports
        # are NOT followed: a re-exporting __init__ ("from .x import ...") would
        # otherwise pull the whole package into the fingerprint, so an unrelated
        # sibling edit would needlessly invalidate the cache. A module the
        # parser actually depends on is still reached directly, by following the
        # concrete modules' own imports (and `from pkg import sub` resolves the
        # submodule directly, not via the __init__).
        if f.name == "__init__.py":
            continue
        try:
            tree = ast.parse(f.read_bytes())
        except (SyntaxError, OSError):
            continue
        for name in _imported_names(tree):
            for prefix in _dotted_prefixes(name):
                origin = _first_party_origin(prefix, root_dirs)
                if origin is not None and origin not in files:
                    files.add(origin)
                    to_visit.append(origin)
    return sorted(files)


def _tool_version(argv: tuple[str, ...]) -> str:
    """What an extraction binary reports about itself, normalised to one
    line.

    Version banners go to stdout on some tools and stderr on others
    (`pdftotext -v` is the latter), so both are captured. Every failure mode
    — the binary absent, non-zero exit, a hang — collapses to
    ``"unavailable"`` rather than raising: a fingerprint is a cache and
    regeneration key, and refusing to compute one would fail the load over a
    diagnostic. "unavailable" is itself a value, so a tool that disappears
    between runs still moves the fingerprint.
    """
    try:
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        return "unavailable"
    banner = (proc.stdout + proc.stderr).strip().splitlines()
    return banner[0].strip() if banner else "unavailable"


def parser_fingerprint(
    roots: list[ModuleType],
    extra_dists: tuple[str, ...] = (),
    extra_tools: tuple[tuple[str, ...], ...] = (),
) -> str:
    """A 32-hex-char fingerprint of the parsing logic rooted at ``roots``:
    the normalised source of their first-party import closure, plus the
    installed versions of ``extra_dists`` (the third-party extraction
    stack), the self-reported version of each argv in ``extra_tools`` (the
    extraction BINARIES, for collectors that shell out rather than import),
    and the running Python version. Suitable as a cache-key /
    sidecar-filename component (hex only). Location-independent: only file
    *contents* feed the hash, never their paths, so the same code in two
    checkouts fingerprints identically."""
    parts = ["src:" + h for h in sorted(
        normalized_source_hash(f) for f in _closure_files(roots)
    )]
    for dist in extra_dists:
        try:
            parts.append(f"dist:{dist}={_dist_version(dist)}")
        except PackageNotFoundError:
            parts.append(f"dist:{dist}=unavailable")
    for tool in extra_tools:
        parts.append(f"tool:{tool[0]}={_tool_version(tool)}")
    parts.append("py:" + platform.python_version())
    return hashlib.sha256("\n".join(parts).encode("utf-8")).hexdigest()[:32]
