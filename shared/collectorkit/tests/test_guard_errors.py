"""cli.guard_errors: no error a collector prints carries a credential.

A Playwright error's call log repeats the request headers. Each case runs
in a process of its own, since the guard sets the process's exception
hooks and its log record factory. The secrets are synthetic."""
from __future__ import annotations

import re
import subprocess
import sys
import textwrap
import unittest
from pathlib import Path

KIT = Path(__file__).resolve().parents[1]
COLLECTORS = KIT.parents[1] / "collectors"
SECRET = "SYNTHETICBEARER0123456789"

# A stand-in for Playwright's request error: a first line that names the
# failure, then the call log with the headers.
PRELUDE = textwrap.dedent(f"""
    import logging, sys, threading
    sys.path.insert(0, {str(KIT)!r})
    from collectorkit import cli

    class TimeoutError(Exception):
        pass

    def leaky():
        return TimeoutError(
            "APIRequestContext.get: Timeout 30000ms exceeded.\\n"
            "Call log:\\n"
            "  - -> GET https://api.example.invalid/accounts\\n"
            "  -   authorization: Bearer {SECRET}\\n"
            "  -   cookie: session={SECRET}\\n")

    def fetch():
        raise leaky()
""")


def run(body: str, guard: bool = True) -> subprocess.CompletedProcess:
    # One line either way, so a traceback names the same line numbers.
    code = PRELUDE + ("cli.configure_logging()\n" if guard else "pass\n") + \
        textwrap.dedent(body)
    return subprocess.run([sys.executable, "-I", "-c", code],
                          capture_output=True, text=True, timeout=60)


class UncaughtError(unittest.TestCase):

    def test_an_error_that_ends_the_process_prints_no_call_log(self):
        out = run("fetch()\n")
        self.assertEqual(out.returncode, 1)
        self.assertNotIn(SECRET, out.stderr)
        self.assertNotIn("Call log:", out.stderr)
        self.assertIn("Traceback (most recent call last):", out.stderr)
        self.assertIn("in fetch", out.stderr)
        self.assertIn("TimeoutError: APIRequestContext.get: Timeout 30000ms "
                      "exceeded.", out.stderr)

    def test_without_the_guard_it_would(self):
        # The case above proves something only while the bare error leaks.
        out = run("fetch()\n", guard=False)
        self.assertIn(SECRET, out.stderr)

    def test_an_error_with_nothing_to_hide_prints_as_python_prints_it(self):
        body = "raise ValueError('no such account: 7\\nsecond line')\n"
        self.assertEqual(run(body).stderr, run(body, guard=False).stderr)

    def test_each_error_of_a_chain_is_cleaned(self):
        out = run("""
            try:
                fetch()
            except TimeoutError as exc:
                raise RuntimeError("the roster could not be read") from exc
        """)
        self.assertNotIn(SECRET, out.stderr)
        self.assertIn("The above exception was the direct cause", out.stderr)
        self.assertIn("RuntimeError: the roster could not be read",
                      out.stderr)

    def test_an_error_raised_while_handling_another_is_cleaned(self):
        out = run("""
            try:
                fetch()
            except TimeoutError:
                raise KeyError("x")
        """)
        self.assertNotIn(SECRET, out.stderr)
        self.assertIn("During handling of the above exception", out.stderr)

    def test_an_error_in_a_thread_prints_no_call_log(self):
        out = run("""
            t = threading.Thread(target=fetch, name="walker")
            t.start(); t.join()
        """)
        self.assertNotIn(SECRET, out.stderr)
        self.assertIn("Exception in thread walker", out.stderr)

    def test_a_url_secret_in_a_message_is_masked(self):
        # Built on a line of its own: a traceback prints the raise line.
        out = run(f"m = 'GET https://x.invalid/a?access_token={SECRET}"
                  "&page=2 failed'\nraise RuntimeError(m)\n")
        self.assertNotIn(SECRET, out.stderr)
        self.assertIn("page=2", out.stderr)


class LoggedError(unittest.TestCase):

    def test_log_exception_prints_no_call_log(self):
        out = run("""
            try:
                fetch()
            except TimeoutError:
                logging.getLogger("x").exception("the walk failed")
        """)
        self.assertEqual(out.returncode, 0)
        self.assertNotIn(SECRET, out.stderr)
        self.assertIn("the walk failed", out.stderr)
        self.assertIn("in fetch", out.stderr)

    def test_an_error_passed_as_an_argument_prints_no_call_log(self):
        out = run("""
            log = logging.getLogger("x")
            log.error("history failed: %s", leaky())
            log.error("history failed: %r", leaky())
            log.error("history failed: %(e)s", {"e": leaky()})
            log.error(leaky())
            log.error("text: %s", str(leaky()))
            log.error("an f-string: " + str(leaky()))
        """)
        self.assertNotIn(SECRET, out.stderr)
        self.assertEqual(out.stderr.count("Timeout 30000ms exceeded."), 6)

    def test_a_url_secret_in_an_argument_is_masked(self):
        out = run(f"""
            logging.getLogger("x").info(
                "GET %s", "https://x.invalid/a?token={SECRET}&page=2")
        """)
        self.assertNotIn(SECRET, out.stderr)
        self.assertIn("page=2", out.stderr)

    def test_a_line_with_nothing_to_hide_is_unchanged(self):
        out = run("""
            logging.getLogger("x").info(
                "GET https://x.invalid/a?page=%s&q=a+b%%20c of %s",
                2, "https://x.invalid/b?q=a+b%20c")
        """)
        self.assertIn("GET https://x.invalid/a?page=2&q=a+b%20c of "
                      "https://x.invalid/b?q=a+b%20c", out.stderr)
        self.assertNotIn("Logging error", out.stderr)

    def test_guarding_twice_changes_nothing(self):
        out = run("""
            cli.guard_errors(); cli.configure_logging()
            logging.getLogger("x").error("once: %s", leaky())
            fetch()
        """)
        self.assertNotIn(SECRET, out.stderr)
        self.assertEqual(out.stderr.count("once: "), 1)
        self.assertEqual(out.stderr.count("Traceback"), 1)


class EveryCollectorIsGuarded(unittest.TestCase):

    def test_a_module_that_sets_up_logging_itself_guards_errors(self):
        # cli.configure_logging guards errors. A collector module that
        # calls logging.basicConfig itself must call cli.guard_errors().
        found = 0
        for path in sorted(COLLECTORS.glob("*/*.py")):
            text = path.read_text(encoding="utf-8")
            if not re.search(r"logging\.basicConfig\(", text):
                continue
            found += 1
            self.assertIn("cli.guard_errors()", text,
                          f"{path} sets up logging without "
                          f"collectorkit.cli.guard_errors()")
        self.assertTrue(found, "no module that sets up logging itself")


if __name__ == "__main__":
    unittest.main()
