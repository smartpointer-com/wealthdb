"""Tests for tools/move_check.py over invented modules."""
from __future__ import annotations

import importlib.util
import subprocess
from pathlib import Path

TOOL = Path(__file__).resolve().parents[1] / "tools" / "move_check.py"
_spec = importlib.util.spec_from_file_location("move_check", TOOL)
move_check = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(move_check)

OLD = '''"""A module."""
import re

LIMIT = 3
PATTERN = re.compile(r"\\d+")


def walk(page):
    return page.goto(LIMIT)


def pure(text):
    return PATTERN.findall(text)


class Bundle:
    def add(self, x):
        return x
'''

OLD_AFTER = '''"""A module."""
from rules import LIMIT, PATTERN, pure, Bundle  # noqa: F401


def walk(page):
    return page.goto(LIMIT)
'''

RULES = '''import re

LIMIT = 3
PATTERN = re.compile(r"\\d+")


def pure(text):
    return PATTERN.findall(text)


class Bundle:
    def add(self, x):
        return x
'''


def _write(tmp_path, **files):
    paths = []
    for name, text in files.items():
        path = tmp_path / f"{name}.py"
        path.write_text(text)
        paths.append(str(path))
    return paths


def _run(tmp_path, old, *new, capsys):
    (old_path,) = _write(tmp_path, old=old)
    new_paths = _write(tmp_path, **dict(new))
    rc = move_check.main([old_path, *new_paths])
    return rc, capsys.readouterr().out


def test_a_clean_move_passes(tmp_path, capsys):
    rc, out = _run(tmp_path, OLD, ("after", OLD_AFTER), ("rules", RULES),
                   capsys=capsys)
    assert rc == 0, out
    assert out.endswith("3 functions and classes compared, 2 moved; "
                        "2 bindings compared; 0 failures, 0 notes\n")


def test_a_changed_body_fails(tmp_path, capsys):
    rules = RULES.replace("return x", "return [x]")
    rc, out = _run(tmp_path, OLD, ("after", OLD_AFTER), ("rules", rules),
                   capsys=capsys)
    assert rc == 1
    assert "changed: Bundle\n" in out


def test_a_name_the_old_module_no_longer_binds_fails(tmp_path, capsys):
    after = OLD_AFTER.replace("pure, Bundle", "Bundle")
    rc, out = _run(tmp_path, OLD, ("after", after), ("rules", RULES),
                   capsys=capsys)
    assert rc == 1
    assert out.splitlines()[0] == "no longer bound by the old module: pure"


def test_a_missing_added_or_twice_defined_function_fails(tmp_path, capsys):
    rules = RULES.replace("def pure", "def renamed")
    rules += "\n\ndef walk(page):\n    return page.goto(LIMIT)\n"
    rc, out = _run(tmp_path, OLD, ("after", OLD_AFTER), ("rules", rules),
                   capsys=capsys)
    assert rc == 1
    lines = out.splitlines()
    assert "missing: pure" in lines
    assert "added: renamed" in lines
    assert "in 2 modules, was 1: walk" in lines


def test_a_receiving_module_that_existed_is_compared_as_it_was(
        tmp_path, capsys):
    held = "def held():\n    return 1\n"
    (before,) = _write(tmp_path, before=held)
    rc, out = _run(tmp_path, OLD, ("after", OLD_AFTER),
                   ("rules", held + "\n\n" + RULES), capsys=capsys)
    assert rc == 1 and "added: held" in out.splitlines()
    rc = move_check.main([str(tmp_path / "old.py"), str(tmp_path / "after.py"),
                          str(tmp_path / "rules.py"), "--before", before])
    assert rc == 0, capsys.readouterr().out


def test_a_changed_binding_is_a_note_only(tmp_path, capsys):
    rules = RULES.replace("LIMIT = 3", "LIMIT = 4")
    rc, out = _run(tmp_path, OLD, ("after", OLD_AFTER), ("rules", rules),
                   capsys=capsys)
    assert rc == 0
    assert "binding changed: LIMIT\n" in out


def test_the_old_module_is_read_from_git(tmp_path, capsys):
    git = ["git", "-c", "user.name=t", "-c", "user.email=t@example.com"]
    subprocess.run(["git", "init", "-q"], cwd=tmp_path, check=True)
    (tmp_path / "mod.py").write_text(OLD)
    subprocess.run(["git", "add", "mod.py"], cwd=tmp_path, check=True)
    subprocess.run([*git, "commit", "-qm", "x"], cwd=tmp_path, check=True)
    (tmp_path / "mod.py").write_text(OLD_AFTER)
    (tmp_path / "rules.py").write_text(RULES)
    rc = move_check.main([f"HEAD:{tmp_path / 'mod.py'}",
                          str(tmp_path / "mod.py"), str(tmp_path / "rules.py")])
    assert rc == 0, capsys.readouterr().out


def test_an_unreadable_file_is_a_usage_error(tmp_path, capsys):
    assert move_check.main([str(tmp_path / "absent.py"),
                            str(tmp_path / "absent.py")]) == 2
