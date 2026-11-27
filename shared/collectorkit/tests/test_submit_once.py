"""loginform.submit_once: a sign-in or a code goes out with one gesture."""
from __future__ import annotations

import logging
import unittest

from collectorkit import loginform


class Control:
    """A stand-in for a Playwright locator: present or not, visible,
    enabled, and whether its gesture raises."""

    def __init__(self, name, log, *, present=True, visible=True,
                 enabled=True, raises=False, probe_raises=False):
        self.name, self.log = name, log
        self.present, self.visible, self.enabled = present, visible, enabled
        self.raises, self.probe_raises = raises, probe_raises

    def count(self):
        if self.probe_raises:
            raise RuntimeError("detached")
        return 1 if self.present else 0

    def is_visible(self):
        return self.visible

    def is_enabled(self):
        return self.enabled

    def click(self, timeout=None):
        self.log.append(("click", self.name))
        if self.raises:
            raise RuntimeError("element was detached from the DOM\nCall log:\n"
                               "  - authorization: Bearer SYNTHETICSECRET")

    def press(self, key):
        self.log.append((key, self.name))
        if self.raises:
            raise RuntimeError("target closed")


class SubmitOnce(unittest.TestCase):

    def setUp(self):
        self.done = []

    def control(self, name, **kw):
        return Control(name, self.done, **kw)

    def test_the_first_usable_control_gets_the_one_gesture(self):
        got = loginform.submit_once([(self.control("button"), "click"),
                                     (self.control("field"), "enter")])
        self.assertEqual(got, "click")
        self.assertEqual(self.done, [("click", "button")])

    def test_a_click_that_raises_is_not_followed_by_enter(self):
        with self.assertLogs("collectorkit.loginform", logging.WARNING) as logs:
            got = loginform.submit_once(
                [(self.control("button", raises=True), "click"),
                 (self.control("field"), "enter")], what="the sign-in")
        self.assertEqual(got, "click")
        self.assertEqual(self.done, [("click", "button")])
        text = "\n".join(logs.output)
        self.assertIn("may not have gone out", text)
        self.assertNotIn("SYNTHETICSECRET", text)

    def test_an_enter_that_raises_is_not_followed_by_a_click(self):
        got = loginform.submit_once(
            [(self.control("field", raises=True), "enter"),
             (self.control("button"), "click")])
        self.assertEqual(got, "enter")
        self.assertEqual(self.done, [("Enter", "field")])

    def test_a_control_not_there_or_unusable_is_passed_over(self):
        got = loginform.submit_once([
            (self.control("gone", present=False), "click"),
            (self.control("hidden", visible=False), "click"),
            (self.control("disabled", enabled=False), "click"),
            (self.control("detached", probe_raises=True), "click"),
            (self.control("field"), "enter")])
        self.assertEqual(got, "enter")
        self.assertEqual(self.done, [("Enter", "field")])

    def test_a_disabled_field_still_takes_enter(self):
        got = loginform.submit_once(
            [(self.control("field", enabled=False), "enter")])
        self.assertEqual(got, "enter")

    def test_nothing_there_sends_nothing(self):
        got = loginform.submit_once(
            [(self.control("gone", present=False), "click")])
        self.assertIsNone(got)
        self.assertEqual(self.done, [])
        self.assertIsNone(loginform.submit_once([]))

    def test_an_unknown_gesture_is_refused(self):
        with self.assertRaises(ValueError):
            loginform.submit_once([(self.control("x"), "tap")])


if __name__ == "__main__":
    unittest.main()
