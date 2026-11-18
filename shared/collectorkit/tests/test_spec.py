"""A collector's declaration: `collectorkit.spec`.

The declaration itself (validation, derived paths, `describe`), and the
fleet's: every collector declares one, and what it declares agrees with
its wrapper, its scripts and its prune. The fleet checks read files and
import only the declarations, never a collector's own modules.
"""
from __future__ import annotations

import ast
import json
import os
import re
import time
from pathlib import Path

import pytest

from collectorkit import debugcap, prune, spec

COLLECTORS = Path(__file__).resolve().parents[3] / "collectors"
NAMES = sorted(d.name for d in COLLECTORS.iterdir()
               if (d / d.name).is_file()) if COLLECTORS.is_dir() else []
fleet = pytest.mark.skipif(not NAMES, reason="not run from a repo checkout")


def _spec(**overrides) -> spec.Collector:
    fields = dict(name="acme", env_prefix="ACME", session="key",
                  unattended=True, window="bounded")
    return spec.Collector(**{**fields, **overrides})


# ---- the declaration -------------------------------------------------------

def test_the_defaults_reclaim_screenshots_only():
    s = _spec()
    assert s.debug_subdirs == (debugcap.SCREENSHOTS_DIR,)
    assert s.debug_globs == ()


@pytest.mark.parametrize("overrides", [
    {"session": "cookie"},
    {"window": "partial"},
    {"window": None},                  # unattended with no download
    {"env_prefix": "acme"},
    {"env_prefix": "ACME-BANK"},
])
def test_a_bad_declaration_is_refused(overrides):
    with pytest.raises(ValueError):
        _spec(**overrides)


def test_a_collector_without_a_download_has_no_window():
    assert _spec(unattended=False, window=None).window is None


def test_paths_derive_from_the_name(tmp_path):
    p = spec.paths(_spec(name="acme-bank", env_prefix="ACME_BANK"), tmp_path)
    assert p == spec.Paths(
        env_file=Path("/secrets/acme-bank.env"),
        profile_dir=Path("/secrets/acme-bank-profile"),
        state_path=Path("/secrets/acme-bank-state.json"),
        silver_db=Path("/data/acme-bank.db"),
        migrations_dir=tmp_path / "migrations")
    moved = spec.paths(_spec(), tmp_path, secrets_dir=tmp_path / "s",
                       data_dir=tmp_path / "d")
    assert moved.env_file == tmp_path / "s" / "acme.env"
    assert moved.silver_db == tmp_path / "d" / "acme.db"


def _write_collector(root: Path, body: str) -> Path:
    root.mkdir(parents=True)
    (root / "collector.py").write_text(body)
    return root


def test_describe_prints_one_json_object_per_collector(tmp_path, capsys):
    a = _write_collector(tmp_path / "acme", (
        "from collectorkit.spec import Collector\n"
        "SPEC = Collector(name='acme', env_prefix='ACME', session='profile',\n"
        "                 unattended=False, window='full', debug_subdirs=(),\n"
        "                 debug_globs=('raw/*.html',))\n"))
    b = _write_collector(tmp_path / "other", (
        "from collectorkit.spec import Collector\n"
        "SPEC = Collector(name='other', env_prefix='OTHER', session='none',\n"
        "                 unattended=False, window=None)\n"))
    assert spec.main(["describe", str(a), str(b)]) == 0
    lines = capsys.readouterr().out.splitlines()
    assert [json.loads(line) for line in lines] == [
        {"name": "acme", "env_prefix": "ACME", "session": "profile",
         "unattended": False, "window": "full", "debug_subdirs": [],
         "debug_globs": ["raw/*.html"]},
        {"name": "other", "env_prefix": "OTHER", "session": "none",
         "unattended": False, "window": None,
         "debug_subdirs": [debugcap.SCREENSHOTS_DIR], "debug_globs": []}]


def test_load_refuses_a_dir_without_a_declaration(tmp_path):
    with pytest.raises(FileNotFoundError):
        spec.load(tmp_path)
    _write_collector(tmp_path / "bad", "SPEC = {'name': 'bad'}\n")
    with pytest.raises(TypeError):
        spec.load(tmp_path / "bad")


# ---- the fleet -------------------------------------------------------------

def _wrapper_value(name: str, key: str) -> str | None:
    text = (COLLECTORS / name / name).read_text(encoding="utf-8")
    m = re.search(rf"^{key}=(\S+)", text, re.M)
    return m.group(1).strip("'\"") if m else None


def _download_scripts(name: str) -> list[Path]:
    root = COLLECTORS / name
    return [p for p in (root / "download.py", root / "login.py") if p.is_file()]


@fleet
@pytest.mark.parametrize("name", NAMES)
def test_every_collector_declares_itself_as_its_wrapper_does(name):
    s = spec.load(COLLECTORS / name)
    assert s.name == name == _wrapper_value(name, "NAME")
    # A wrapper that sets no ENV_PREFIX gets its name upper-cased, a
    # hyphen read as an underscore (paths-lib.sh).
    assert s.env_prefix == (_wrapper_value(name, "ENV_PREFIX") or name.upper().replace("-", "_"))


@fleet
@pytest.mark.parametrize("name", NAMES)
def test_the_window_is_what_the_download_parser_says(name):
    s = spec.load(COLLECTORS / name)
    if not (COLLECTORS / name / "download.py").is_file():
        assert s.window is None
        return
    full = any(re.search(r"full_history=True|warn_lookback_ignored",
                         p.read_text(encoding="utf-8"))
               for p in _download_scripts(name))
    assert s.window == ("full" if full else "bounded")


def _path_constants(path: Path) -> dict[str, Path]:
    """The module-level `NAME = Path("/literal")` constants of `path`."""
    found = {}
    for node in ast.parse(path.read_text(encoding="utf-8")).body:
        if (isinstance(node, ast.Assign) and len(node.targets) == 1
                and isinstance(node.targets[0], ast.Name)
                and isinstance(node.value, ast.Call)
                and getattr(node.value.func, "id", None) == "Path"
                and len(node.value.args) == 1
                and isinstance(node.value.args[0], ast.Constant)):
            found[node.targets[0].id] = Path(node.value.args[0].value)
    return found


@fleet
@pytest.mark.parametrize("name", NAMES)
def test_the_scripts_default_paths_are_the_derived_ones(name):
    root = COLLECTORS / name
    derived = spec.paths(spec.load(root), root)._asdict()
    pairs = {"DEFAULT_ENV_FILE": "env_file", "DEFAULT_PROFILE_DIR": "profile_dir",
             "DEFAULT_STATE_PATH": "state_path", "DEFAULT_SILVER_DB": "silver_db"}
    for script in ("explore.py", "login.py", "download.py", "load.py"):
        if not (root / script).is_file():
            continue
        for const, value in _path_constants(root / script).items():
            if const in pairs:
                assert value == derived[pairs[const]], f"{script}: {const}"


# One tree shaped for every declaration: a complete run holding a
# screenshots/ dir, files the fleet's debug globs match and load inputs
# beside them; a stale crashed run; a fresh one, still in flight. The
# collectors' own prune tests use their real run layouts.
_COMPLETE = "20260101T000000Z"
_CRASHED = "20260102T000000Z"
_IN_FLIGHT = "20260103T000000Z"
_FILES = ("run.json", "data.json", "screenshots/trace.jsonl",
          "raw/svc/body.json", "raw/keep.json",
          "transactions/acct/page-1.html", "transactions/acct/tx.csv")


def _tree(root: Path) -> Path:
    old = time.time() - 3 * 3600
    for slug, status in ((_COMPLETE, "complete"), (_CRASHED, "in-progress"),
                         (_IN_FLIGHT, "in-progress")):
        d = root / slug
        for rel in _FILES:
            (d / rel).parent.mkdir(parents=True, exist_ok=True)
            (d / rel).write_text(json.dumps({"status": status}))
        if slug != _IN_FLIGHT:
            for p in [d, *d.rglob("*")]:
                os.utime(p, (old, old))
    return root


@fleet
@pytest.mark.parametrize("name", [n for n in NAMES
                                  if (COLLECTORS / n / "prune.py").is_file()])
def test_each_declaration_prunes_its_debug_artefacts_and_crashed_runs(
        name, tmp_path):
    s = spec.load(COLLECTORS / name)
    config = prune.spec_config(s)
    targets, skipped = prune.plan_prune(_tree(tmp_path), config, 3600.0)
    got = {str(t["path"].relative_to(tmp_path)) for t in targets}
    complete = tmp_path / _COMPLETE
    want = {f"{_COMPLETE}/{d}" for d in s.debug_subdirs}
    want |= {str(p.relative_to(tmp_path)) for g in s.debug_globs
             for p in complete.glob(g)}
    want.add(_CRASHED)
    assert got == want
    assert [x["path"].name for x in skipped] == [_IN_FLIGHT]
    for target in targets:
        config.validate_target(target["path"], tmp_path)
    # A readable manifest with no status predates the field: kept whole.
    (complete / "run.json").write_text("{}")
    assert prune.classify(complete, config)[0] == prune.COMPLETE
    for rel in ("run.json", "data.json", "raw/keep.json",
                "transactions/acct/tx.csv"):
        with pytest.raises(SystemExit):
            config.validate_target(complete / rel, tmp_path)


@fleet
@pytest.mark.parametrize("name", NAMES)
def test_every_prune_builds_its_config_from_the_declaration(name):
    p = COLLECTORS / name / "prune.py"
    if p.is_file():
        assert "prune.spec_config(SPEC" in p.read_text(encoding="utf-8"), \
            f"{name}/prune.py hand-builds its PruneConfig"
