"""log_cookie_jar names each session cookie by name, domain and length,
never by any part of its value. Synthetic cookies only."""
import logging
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import login  # noqa: E402


class _Context:
    def __init__(self, jar):
        self._jar = jar

    def cookies(self, _url):
        return self._jar


def test_no_cookie_value_reaches_the_log(caplog):
    jar = [{"name": "sid", "domain": ".schwab.com", "value": "EXAMPLESID"},
           {"name": "_abck", "domain": ".schwab.com",
            "value": "EXAMPLEABCKVALUE" * 3}]
    with caplog.at_level(logging.DEBUG, logger=login.log.name):
        login.log_cookie_jar(_Context(jar))
    assert "sid @ .schwab.com  len=10" in caplog.text
    assert "EXAMPLE" not in caplog.text
    assert "Akamai trust intact" in caplog.text
