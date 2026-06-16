"""
Tests for the Pendo in-app-guide overlay dismissal.

A Pendo walkthrough overlay injects a full-page backdrop that
intercepts pointer events and blocks export-button clicks.
download.dismiss_guide_overlays() clears it (best-effort, never
fatal) before each export interaction.

Run from the repo root inside the container:
    python3 -m unittest discover tests
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import landmarks as sq  # noqa: E402
import download  # noqa: E402  (imports playwright + collectorkit; present in image)


class _FakePage:
    """Minimal page stub: records the evaluate() call and returns a
    canned value, or raises if `raises` is set."""

    def __init__(self, returns=0, raises=None):
        self._returns = returns
        self._raises = raises
        self.calls = []

    def evaluate(self, js, arg=None):
        self.calls.append((js, arg))
        if self._raises is not None:
            raise self._raises
        return self._returns


class PendoSelectorTests(unittest.TestCase):
    def test_selector_targets_pendo_overlay_nodes(self):
        sel = sq.PENDO_OVERLAY_SELECTOR
        # The root container, the backdrop, and the class-prefix catch-all.
        self.assertIn("#pendo-base", sel)
        self.assertIn("._pendo-backdrop", sel)
        self.assertIn("_pendo-", sel)


class DismissJsTests(unittest.TestCase):
    def test_js_uses_pendo_api_and_removes_nodes(self):
        js = download._DISMISS_GUIDE_OVERLAYS_JS
        # Clean dismissal via Pendo's own API ...
        self.assertIn("stopGuides", js)
        # ... then strip residual nodes, returning the count.
        self.assertIn("querySelectorAll", js)
        self.assertIn("remove()", js)
        self.assertIn("return", js)


class DismissGuideOverlaysTests(unittest.TestCase):
    def test_passes_selector_to_evaluate(self):
        page = _FakePage(returns=0)
        download.dismiss_guide_overlays(page)
        self.assertEqual(len(page.calls), 1)
        _js, arg = page.calls[0]
        self.assertEqual(arg, sq.PENDO_OVERLAY_SELECTOR)

    def test_overlay_present_does_not_raise(self):
        # Two overlay nodes removed — should log, not raise.
        page = _FakePage(returns=2)
        download.dismiss_guide_overlays(page)  # no exception

    def test_evaluate_failure_is_swallowed(self):
        # A flaky evaluate must never abort the export run.
        page = _FakePage(raises=RuntimeError("execution context destroyed"))
        download.dismiss_guide_overlays(page)  # no exception


if __name__ == "__main__":
    unittest.main()
