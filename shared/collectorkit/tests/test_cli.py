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
    # The full-history download group, through the public entry point.
    p = argparse.ArgumentParser()
    cli.add_standard_args(p, verb="download", full_history=True)
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


def _standard(verb: str, **kw) -> argparse.ArgumentParser:
    p = argparse.ArgumentParser()
    cli.add_standard_args(p, verb=verb, **kw)
    return p


class AddStandardArgsTest(unittest.TestCase):
    def test_download_accepts_the_window_flag(self):
        p = _standard("download")
        ns = p.parse_args(["--lookback", "1y", "-v"])
        self.assertEqual(ns.lookback, "1y")
        self.assertTrue(ns.verbose)

    def test_download_lookback_takes_a_date_too(self):
        ns = _standard("download").parse_args(["--lookback", "2020-01-01"])
        self.assertEqual(ns.lookback, "2020-01-01")

    def test_download_rejects_the_retired_window_flags(self):
        # There is one window flag. The old per-facet vocabulary is gone
        # rather than aliased, so a stale invocation fails loudly instead
        # of being silently reinterpreted.
        p = _standard("download")
        for flag in ("--since", "--until", "--documents-since",
                     "--documents-until"):
            with self.assertRaises(SystemExit, msg=flag):
                p.parse_args([flag, "2020-01-01"])

    def test_full_history_download_accepts_lookback_structurally(self):
        # The criterion-#1 guarantee: a full-history collector wires the SAME
        # group, so `--lookback` PARSES CLEANLY rather than argparse exit-2.
        # The narrowing-is-impossible signal is a runtime warning
        # (resolve_standard), not a parse rejection.
        p = _standard("download")  # full-history uses the same download group
        self.assertEqual(p.parse_args(["--lookback", "all"]).lookback, "all")

    def test_load_has_force_not_dates(self):
        p = _standard("load")
        self.assertTrue(p.parse_args(["--force"]).force)
        self.assertTrue(p.parse_args(["-v"]).verbose)
        with self.assertRaises(SystemExit):
            p.parse_args(["--lookback", "1y"])

    def test_login_and_prune_take_only_verbose(self):
        for verb in ("login", "prune"):
            p = _standard(verb)
            self.assertTrue(p.parse_args(["-v"]).verbose)
            with self.assertRaises(SystemExit):
                p.parse_args(["--force"])

    def test_unknown_verb_rejected(self):
        with self.assertRaises(ValueError):
            cli.add_standard_args(argparse.ArgumentParser(), verb="explore")


class ResolveStandardTest(unittest.TestCase):
    def test_bounded_delegates_to_resolve_lookback(self):
        ns = _standard("download").parse_args(["--lookback", "2020-01-01"])
        since, until = cli.resolve_standard(ns, verb="download")
        self.assertEqual(since.isoformat(), "2020-01-01")
        self.assertEqual(until, cli._today_utc())

    def test_full_history_returns_none_and_warns(self):
        ns = _standard("download").parse_args(["--lookback", "4w"])
        log = logging.getLogger("test.resolve.full")
        with self.assertLogs(log, level="WARNING") as cm:
            result = cli.resolve_standard(
                ns, verb="download", full_history=True,
                log=log, what="the full snapshot")
        self.assertEqual(result, (None, None))
        joined = "\n".join(cm.output)
        self.assertIn("4w", joined)
        self.assertIn("the full snapshot", joined)

    def test_full_history_silent_when_no_window_flag(self):
        ns = _standard("download").parse_args([])
        log = logging.getLogger("test.resolve.full.silent")
        with self.assertNoLogs(log, level="WARNING"):
            result = cli.resolve_standard(ns, verb="download",
                                          full_history=True, log=log)
        self.assertEqual(result, (None, None))

    def test_non_download_verb_is_noop(self):
        ns = _standard("load").parse_args(["--force"])
        self.assertEqual(cli.resolve_standard(ns, verb="load"), (None, None))


class WarnNotImplementedTest(unittest.TestCase):
    def test_names_flag_source_and_todo(self):
        log = logging.getLogger("test.notimpl")
        with self.assertLogs(log, level="WARNING") as cm:
            cli.warn_not_implemented("--no-documents", "carta", log)
        joined = "\n".join(cm.output)
        self.assertIn("--no-documents", joined)
        self.assertIn("carta", joined)
        self.assertIn("TODO", joined)

    def test_debug_noop_message(self):
        log = logging.getLogger("test.debugnoop")
        with self.assertLogs(log, level="WARNING") as cm:
            cli.warn_debug_noop("viac", log)
        joined = "\n".join(cm.output)
        self.assertIn("--debug", joined)
        self.assertIn("viac", joined)
        self.assertIn("TODO", joined)


if __name__ == "__main__":
    unittest.main()
