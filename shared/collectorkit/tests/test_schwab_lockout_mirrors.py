"""The two Schwab collectors' lockout landmarks must stay the same code.

schwab-web and schwab-api both sign in through Schwab's gateway SPA, and
both stop at once on its terminal notice page, the route an account lockout
lands on. Each collector carries its own copy of that stop: the route test,
where the notice text renders, and the markers that read it as a lockout.
A fix that lands in one copy only would leave the other collector retrying
past a lock notice.

The copies are compared as docstring-stripped ASTs, the normalisation
`collectorkit.srcfp` fingerprints parsers with, so a comment or docstring
may differ while the code may not.

This test lives in collectorkit because it is the only suite that runs on
the HOST with the whole tree in view: each collector's pytest sees only its
own directory.
"""
from __future__ import annotations

import ast
from pathlib import Path

import pytest

from collectorkit.srcfp import _strip_docstrings

REPO = Path(__file__).resolve().parents[3]
SCHWAB_WEB = REPO / "collectors/schwab-web/landmarks.py"
SCHWAB_API = REPO / "collectors/schwab-api/oauth_landmarks.py"

MIRRORED = (
    "is_gateway_notice_url",
    "NOTICE_MESSAGE_SELECTORS",
    "LOCKOUT_TEXT_MARKERS",
    "looks_locked",
)


def _definition(path: Path, name: str) -> str:
    """`name`'s top-level definition in `path`, as a docstring-stripped,
    position-free AST dump."""
    assert path.is_file(), f"landmarks module not found at {path}"
    tree = ast.parse(path.read_text(encoding="utf-8"))
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            found = node.name == name
        elif isinstance(node, ast.Assign):
            found = any(isinstance(t, ast.Name) and t.id == name
                        for t in node.targets)
        else:
            found = False
        if found:
            _strip_docstrings(node)
            return ast.dump(node)
    raise AssertionError(f"{name} not defined at the top level of {path.name}")


@pytest.mark.parametrize("name", MIRRORED)
def test_the_lockout_landmark_is_the_same_code_in_both_collectors(name):
    assert _definition(SCHWAB_WEB, name) == _definition(SCHWAB_API, name), (
        f"{name} differs between {SCHWAB_WEB.relative_to(REPO)} and "
        f"{SCHWAB_API.relative_to(REPO)}: change both copies together")


def test_only_code_counts_as_a_difference(tmp_path):
    base = tmp_path / "a.py"
    base.write_text('MARKERS = ("x",)\n\n\n'
                    'def f(t):\n    """Doc."""\n    return t in MARKERS\n')
    reworded = tmp_path / "b.py"
    reworded.write_text('# A note.\nMARKERS = ("x",)  # why\n\n\n'
                        'def f(t):\n    """Other doc."""\n'
                        '    # step\n    return t in MARKERS\n')
    changed = tmp_path / "c.py"
    changed.write_text('MARKERS = ("x", "y")\n\n\n'
                       'def f(t):\n    """Doc."""\n    return t not in MARKERS\n')
    for name in ("MARKERS", "f"):
        assert _definition(base, name) == _definition(reworded, name)
        assert _definition(base, name) != _definition(changed, name)
