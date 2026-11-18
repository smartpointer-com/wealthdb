"""Unit tests for cointracking login.py's 2FA-failure message.

When the page does not land on /dashboard after the 2FA submit, login()
raises with the start of the page's markup, and that text can reach a log.
A re-render can bring the sign-in form back, so the message masks both
credentials by value, as well as blanking the password inputs.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import login  # noqa: E402

USERNAME = "demo-user@example.invalid"
PASSWORD = "Pw-not-real-93"


class _Locator:
    first = property(lambda self: self)

    def count(self):
        return 0

    def fill(self, *a, **kw):
        pass

    def click(self, *a, **kw):
        pass


class _Page:
    """Signs in, shows the 2FA prompt, then never reaches /dashboard."""

    def __init__(self, html: str):
        self.url = "about:blank"
        self._html = html

    def goto(self, url, **kw):
        self.url = url

    def locator(self, selector):
        return _Locator()

    def wait_for_selector(self, *a, **kw):
        pass

    def wait_for_url(self, *a, **kw):
        raise TimeoutError("still on the 2FA page")

    def content(self):
        return self._html


def test_a_failed_2fa_submit_masks_the_credentials(monkeypatch):
    monkeypatch.setattr(login, "prompt_for_2fa", lambda: "123456")
    page = _Page(f'<input type="email" name="email" value="{USERNAME}">'
                 f'<input type="password" name="pw" value="{PASSWORD}">'
                 f'<script>var u = "{USERNAME}"; var p = "{PASSWORD}";'
                 '</script><p>Invalid 2FA code</p>')
    with pytest.raises(RuntimeError) as err:
        login.login(page, USERNAME, PASSWORD)
    message = str(err.value)
    assert "did not land on /dashboard" in message
    assert "Invalid 2FA code" in message
    assert USERNAME not in message
    assert PASSWORD not in message
