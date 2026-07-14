"""Unit tests for the collectorkit.cli shared date-window contract.

Focused on the full-download escape hatch — the accept-only ``--lookback``
that collectors which always fetch their complete history expose so
``wealthdb-refresh`` can hand every collector the same flag. Stdlib
unittest, matching the rest of the collectorkit suite."""
import argparse
import logging
import unittest

from collectorkit import cli


def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser()
    cli.add_full_download_lookback_arg(p)
    return p


class FullDownloadLookbackArgTest(unittest.TestCase):
    def test_accepts_every_preset(self):
        for preset in cli.LOOKBACK_CHOICES:
            self.assertEqual(_parser().parse_args(["--lookback", preset]).lookback,
                             preset)

    def test_defaults_to_none(self):
        # Absent flag -> None, so warn_lookback_ignored stays silent and the
        # collector proceeds with its normal full download.
        self.assertIsNone(_parser().parse_args([]).lookback)

    def test_rejects_unknown_window(self):
        # A typo like "1m" (not a preset) fails loudly at parse time rather
        # than being silently swallowed.
        with self.assertRaises(SystemExit):
            _parser().parse_args(["--lookback", "1m"])

    def test_adds_no_date_range_flags(self):
        # Only --lookback is added — an explicit --since/--until would be
        # silently ignored, which is more surprising than rejecting it.
        with self.assertRaises(SystemExit):
            _parser().parse_args(["--since", "2020-01-01"])


class WarnLookbackIgnoredTest(unittest.TestCase):
    def test_warns_when_set(self):
        log = logging.getLogger("test.warn.set")
        with self.assertLogs(log, level="WARNING") as cm:
            cli.warn_lookback_ignored("4w", log, what="the full snapshot")
        joined = "\n".join(cm.output)
        self.assertIn("4w", joined)
        self.assertIn("the full snapshot", joined)

    def test_silent_when_none(self):
        log = logging.getLogger("test.warn.none")
        # assertNoLogs is 3.10+, which collectorkit already requires.
        with self.assertNoLogs(log, level="WARNING"):
            cli.warn_lookback_ignored(None, log, what="the full snapshot")


if __name__ == "__main__":
    unittest.main()
