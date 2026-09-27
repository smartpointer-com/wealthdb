"""
Tests for landmarks URL/predicate helpers.

Run from the repo root inside the container:
    python3 -m unittest discover tests

Pure stdlib. Covers the post-auth URL predicate login.py and download.py
both rely on to tell an authenticated landing from the F5 auth form.
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import landmarks as sq  # noqa: E402


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

    def test_trading_platform_is_post_auth(self):
        # Asking for the protected trigger URL does not always come back to
        # the SPA it was sent to: F5 can redirect an authenticated session
        # on to the Trading Platform, and reading that as "not logged in"
        # hung the login until its MFA window expired.
        self.assertTrue(
            sq.is_post_auth_url(
                "https://trade.swissquote.ch/eding_trading-platform/"
                "#portfoliooverview"
            )
        )

    def test_both_landing_spas_are_covered(self):
        # The predicate's accepted set and the URLs built from it must not
        # drift apart.
        for path in sq.POST_AUTH_PATHS:
            self.assertTrue(sq.is_post_auth_url(f"https://{sq.HOST}{path}"))
        self.assertIn(sq.EBANKING_PATH, sq.LOGIN_TRIGGER_URL)
        self.assertIn(sq.TRADING_PLATFORM_PATH, sq.TRADING_PLATFORM_BASE_URL)

    def test_the_auth_form_beats_a_post_auth_path(self):
        # F5 can carry the requested path through onto /my.policy; the form
        # is the stronger signal and must win.
        self.assertFalse(
            sq.is_post_auth_url(
                "https://trade.swissquote.ch/my.policy"
                "?url=/sqc-web-client-portal/"
            )
        )


if __name__ == "__main__":
    unittest.main()
