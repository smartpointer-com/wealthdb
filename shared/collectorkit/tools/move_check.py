"""Check that a module split moved code without changing it.

A move takes functions, classes and module-level bindings out of one
module and puts them into others. This tool compares the modules as
they were with the modules they became:

* Every function and class is defined as often as before (once, as a
  rule), and its AST (`ast.dump`, which carries no line numbers) is the
  same. No function or class is added or lost.
* Every module-level binding (a constant, a compiled pattern) is bound
  to the same expression as before.
* The new version of the split module still binds every public
  function, class and binding the old one defined, by definition or by
  import, so an importer of the old module finds each name where it
  was. A private name (a leading underscore) has no importer outside
  the module and its tests, so one the old module no longer binds is a
  note.

Imports and every other top-level statement (the docstring, the main
guard) are not compared. A binding that changed is reported but does
not fail the check; a function or class that changed, or a public name
the old module no longer binds, does.

    move_check.py OLD NEW [NEW ...] [--before PEER ...]

OLD is the split module as it was: a path, or `REV:PATH` to read the
file as git holds it at REV. The first NEW is its new version; the
others are the modules that received code. A receiving module that
existed before is also named with `--before`, as it was, so the code
it already held is compared too.

Exit status: 0 when the move kept everything, 1 when it did not, 2 on a
usage error or a file that cannot be read or parsed.
"""
from __future__ import annotations

import argparse
import ast
import subprocess
import sys
from pathlib import Path

DEFS = (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)


def read_source(spec: str) -> str:
    """The text of `spec`: a path, or `REV:PATH` from git."""
    if ":" in spec and not Path(spec).exists():
        rev, path = spec.split(":", 1)
        top = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"], capture_output=True,
            text=True, check=True, cwd=Path(path).resolve().parent).stdout
        rel = Path(path).resolve().relative_to(Path(top.strip()))
        return subprocess.run(
            ["git", "show", f"{rev}:{rel.as_posix()}"], capture_output=True,
            text=True, check=True, cwd=top.strip()).stdout
    return Path(spec).read_text(encoding="utf-8")


def _binding_names(node: ast.stmt) -> list[str]:
    """The names a module-level assignment binds."""
    targets = (node.targets if isinstance(node, ast.Assign)
               else [node.target])
    names = []
    for target in targets:
        for sub in ast.walk(target):
            if isinstance(sub, ast.Name):
                names.append(sub.id)
    return names


class Module:
    """The top level of one module: its definitions, its bindings, and
    every name it binds, imports included."""

    def __init__(self, source: str):
        self.defs: dict[str, str] = {}
        self.bindings: dict[str, str] = {}
        self.names: set[str] = set()
        self.duplicates: set[str] = set()
        for node in ast.parse(source).body:
            if isinstance(node, DEFS):
                self._add(self.defs, node.name, ast.dump(node))
            elif isinstance(node, (ast.Assign, ast.AnnAssign)):
                value = ast.dump(node.value) if node.value else ""
                for name in _binding_names(node):
                    self._add(self.bindings, name, value)
            elif isinstance(node, (ast.Import, ast.ImportFrom)):
                for alias in node.names:
                    self.names.add((alias.asname or alias.name).split(".")[0])

    def _add(self, table: dict, name: str, dump: str) -> None:
        if name in table:
            self.duplicates.add(name)
        table[name] = dump
        self.names.add(name)


def _tally(modules: list[Module], table: str) -> dict[str, list[str]]:
    """Each name of one table across `modules`, with its dumps."""
    out: dict[str, list[str]] = {}
    for module in modules:
        for name, dump in getattr(module, table).items():
            out.setdefault(name, []).append(dump)
    return {name: sorted(dumps) for name, dumps in out.items()}


def _differences(before: dict, after: dict) -> list[str]:
    lines = []
    for name in sorted(set(before) | set(after)):
        was, now = before.get(name, []), after.get(name, [])
        if not now:
            lines.append(f"missing: {name}")
        elif not was:
            lines.append(f"added: {name}")
        elif len(was) != len(now):
            lines.append(f"in {len(now)} modules, was {len(was)}: {name}")
        elif was != now:
            lines.append(f"changed: {name}")
    return lines


def compare(old: list[Module],
            new: list[Module]) -> tuple[list[str], list[str]]:
    """The failures and the notes of a move from `old` to `new`. The
    first module of each list is the split one."""
    failures = [f"defined twice in one module: {n}"
                for m in new for n in sorted(m.duplicates & set(m.defs))]
    failures += _differences(_tally(old, "defs"), _tally(new, "defs"))
    notes = [f"binding {line}" for line in _differences(
        _tally(old, "bindings"), _tally(new, "bindings"))]
    dropped = sorted((set(old[0].defs) | set(old[0].bindings)) - new[0].names)
    failures += [f"no longer bound by the old module: {n}"
                 for n in dropped if not n.startswith("_")]
    notes += [f"private name no longer bound: {n}"
              for n in dropped if n.startswith("_")]
    return failures, notes


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        description=__doc__.split("\n\n")[0],
        formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("old",
                   help="the split module as it was: PATH or REV:PATH")
    p.add_argument("new", nargs="+",
                   help="the new version of the split module, then the "
                        "modules that received code")
    p.add_argument("--before", action="append", default=[],
                   metavar="PEER",
                   help="a receiving module that existed before, as it "
                        "was: PATH or REV:PATH")
    args = p.parse_args(argv)
    try:
        old = [Module(read_source(path)) for path in [args.old, *args.before]]
        new = [Module(read_source(path)) for path in args.new]
    except (OSError, SyntaxError, subprocess.CalledProcessError) as e:
        print(f"move_check: {e}", file=sys.stderr)
        return 2
    failures, notes = compare(old, new)
    for line in failures + notes:
        print(line)
    moved = sum(1 for n in old[0].defs if n not in new[0].defs)
    print(f"{sum(len(m.defs) for m in old)} functions and classes compared, "
          f"{moved} moved; {sum(len(m.bindings) for m in old)} bindings "
          f"compared; {len(failures)} failures, {len(notes)} notes")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
