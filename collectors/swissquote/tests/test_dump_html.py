"""
The sign-in HTML dump masks the run's credentials.

login.py dumps the Mobile Level 3 page to `--screenshot-dir` right after the
form was submitted. The page can echo the username in a text input, a hidden
field or a bootstrap script, so the dump masks both credentials by value as
well as blanking the password inputs.

Run from the repo root inside the container:
    python3 -m unittest discover tests
"""

from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import login  # noqa: E402

USERNAME = "demo-user-4711"
PASSWORD = "Pw-not-real-93"


class _Page:
    def __init__(self, html: str):
        self._html = html

    def content(self) -> str:
        return self._html


class DumpHtmlTests(unittest.TestCase):
    def _dump(self, html: str, **kw) -> str:
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            login._dump_html(_Page(html), out, "mfa", **kw)
            return (out / "mfa.html").read_text(encoding="utf-8")

    def test_the_credentials_are_masked_wherever_the_page_carries_them(self):
        html = (f'<input type="text" name="user" value="{USERNAME}">'
                f'<input type="hidden" name="u" value="{USERNAME}">'
                f'<script>var s = {{"login": "{USERNAME}", '
                f'"pw": "{PASSWORD}"}};</script>'
                '<p>Mobile Level 3</p>')
        dumped = self._dump(html, secrets=(USERNAME, PASSWORD))
        self.assertNotIn(USERNAME, dumped)
        self.assertNotIn(PASSWORD, dumped)
        self.assertIn("Mobile Level 3", dumped)

    def test_the_password_input_is_blanked_without_secrets(self):
        # A hand-typed password is unknown to the run; the blanking of
        # password inputs still covers it.
        dumped = self._dump(
            f'<input type="password" name="pw" value="{PASSWORD}">',
            secrets=())
        self.assertNotIn(PASSWORD, dumped)

    def test_no_screenshot_dir_writes_nothing(self):
        login._dump_html(_Page("<p>x</p>"), None, "mfa",
                         secrets=(USERNAME, PASSWORD))


if __name__ == "__main__":
    unittest.main()
