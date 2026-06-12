"""
Tests for landmarks URL/predicate helpers.

Run from the repo root inside the container:
    python3 -m unittest discover tests

Pure stdlib. Covers the SmartL3 feedback long-poll URL builder, which
login.py uses to detect MFA approval without re-firing the push, plus the
post-auth URL predicate it relies on.
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import landmarks as sq  # noqa: E402


class SmartL3ListenUrlTests(unittest.TestCase):
    MFA_URL = (
        "https://trade.swissquote.ch/sq-thirdlevel-plugin/"
        "#thirdlevel/urlId=0123456789abcdef0123456789abcdef"
    )

    def test_builds_observed_url(self):
        # Must match the request the SPA itself issues (captured from a live
        # login), modulo the timeout we choose.
        got = sq.smartl3_listen_url(self.MFA_URL, timeout_ms=20000)
        self.assertEqual(
            got,
            "https://trade.swissquote.ch/sq-thirdlevel-plugin/api/thirdlevel/"
            "smartL3/feedback/listen/0123456789abcdef0123456789abcdef"
            "?queryRedirectBaseUrl=true&cache=false&timeout=20000",
        )

    def test_timeout_is_parameterised(self):
        got = sq.smartl3_listen_url(self.MFA_URL, timeout_ms=5000)
        self.assertIn("timeout=5000", got)

    def test_no_url_id_returns_none(self):
        # Post-auth URL carries no urlId fragment — caller falls back.
        self.assertIsNone(
            sq.smartl3_listen_url(
                "https://trade.swissquote.ch/sqc-web-client-portal/",
                timeout_ms=20000,
            )
        )

    def test_trailing_slash_not_doubled(self):
        got = sq.smartl3_listen_url(self.MFA_URL, timeout_ms=20000)
        self.assertNotIn("//api/", got)


class IsPostAuthUrlTests(unittest.TestCase):
    def test_mfa_page_is_not_post_auth(self):
        self.assertFalse(
            sq.is_post_auth_url(
                "https://trade.swissquote.ch/sq-thirdlevel-plugin/"
                "#thirdlevel/urlId=abc"
            )
        )

    def test_auth_form_is_not_post_auth(self):
        self.assertFalse(
            sq.is_post_auth_url("https://trade.swissquote.ch/my.policy")
        )

    def test_ebanking_root_is_post_auth(self):
        self.assertTrue(
            sq.is_post_auth_url(
                "https://trade.swissquote.ch/sqc-web-client-portal/"
                "#accountOverview/main"
            )
        )


if __name__ == "__main__":
    unittest.main()
