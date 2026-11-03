"""Unit tests for pii_check.py.

Every value a rule must flag is built here at run time, its check digit
computed, so no line of this file holds one: the file passes the check
it tests.
"""
import base64
import hashlib
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import pii_check as p  # noqa: E402



def make_iban(country: str, bban: str) -> str:
    moved = bban + country + "00"
    check = 98 - int("".join(str(int(c, 36)) for c in moved)) % 97
    return f"{country}{check:02d}{bban}"


def make_card(partial: str) -> str:
    for d in "0123456789":
        if p.luhn_valid(partial + d):
            return partial + d
    raise AssertionError("no Luhn digit")


def make_ahv(nine: str) -> str:
    digits = "756" + nine
    total = sum(int(c) * (3 if i % 2 else 1) for i, c in enumerate(digits))
    return digits + str((10 - total % 10) % 10)


def token(n: int) -> str:
    """n letters and digits of a fixed hash, as a real token looks."""
    out, seed = "", 0
    while len(out) < n:
        digest = hashlib.sha256(f"pii-{seed}".encode()).digest()
        out += base64.b64encode(digest).decode().strip("=").replace("+", "").replace("/", "")
        seed += 1
    return out[:n]


ALLOW = p.load_allow(p.ALLOW_FILE)


def rules(text: str, kind: str = "code") -> list[str]:
    return [rule for rule, _ in p.scan_line(text, ALLOW, kind)]


class Placeholders(unittest.TestCase):
    def test_placeholder_shapes(self):
        for digits in ("0000000", "7777", "123456", "987654", "121212",
                       "123412341234", "100000002"):
            self.assertTrue(p.is_placeholder(digits), digits)
        for digits in ("48217733", "503918"):
            self.assertFalse(p.is_placeholder(digits), digits)

    def test_typed_finds_a_counting_run_inside(self):
        self.assertTrue(p.typed("90" + "123456" + "77"))
        self.assertFalse(p.typed("4829173355"))
        # Zero padding is common in real numbers and does not count.
        self.assertFalse(p.typed("0483" + "000001" + "93"))

    def test_shape_hides_every_digit_and_letter(self):
        self.assertEqual(p.shape("CH12-ab 9"), "XX##-XX #")


class Checksums(unittest.TestCase):
    def test_iban(self):
        iban = make_iban("CH", "0483927155603918" + "2")
        self.assertTrue(p.iban_valid(iban))
        self.assertFalse(p.iban_valid(iban[:-1] + "3"))
        self.assertFalse(p.iban_valid(iban + "1"))

    def test_card(self):
        self.assertTrue(p.card_brand(make_card("453914880343646")))
        self.assertFalse(p.card_brand("9" * 16))

    def test_ahv(self):
        self.assertTrue(p.ean13_valid(make_ahv("482917735")))

    def test_entropy(self):
        self.assertLess(p.entropy("aaaaaaaa"), 0.1)
        self.assertGreater(p.entropy(token(32)), 3.5)


class Rules(unittest.TestCase):
    def test_iban(self):
        iban = make_iban("CH", "04839271556039182")
        spaced = " ".join(iban[i:i + 4] for i in range(0, len(iban), 4))
        self.assertEqual(rules(f"pay to {iban}"), ["iban"])
        self.assertEqual(rules(f"IBAN {spaced} BIC"), ["iban"])
        counting = make_iban("CH", "00000" + "123456789" + "012")
        self.assertEqual(rules(counting), [])
        self.assertEqual(rules(make_iban("CH", "0" * 17)), [])

    def test_published_example_iban(self):
        self.assertEqual(rules("DE89" + "370400440532013000"), [])

    def test_card_in_groups_or_named(self):
        card = make_card("453914880343646")
        grouped = " ".join(card[i:i + 4] for i in range(0, 16, 4))
        self.assertEqual(rules(f"pan {grouped}"), ["card"])
        self.assertEqual(rules(f"ref {grouped}"), ["card"])
        self.assertEqual(rules(f"id {card}"), [])
        self.assertEqual(rules(f'"cardNumber": "{card}"'), ["card"])

    def test_published_test_cards_pass(self):
        self.assertEqual(rules("card " + "4111" * 4), [])
        self.assertEqual(rules("card " + "4111 1111 1111 1111"), [])

    def test_ahv(self):
        ahv = make_ahv("482917735")
        dotted = f"{ahv[:3]}.{ahv[3:7]}.{ahv[7:11]}.{ahv[11:]}"
        self.assertEqual(rules(dotted), ["ahv"])
        self.assertEqual(rules(ahv[:-1] + str((int(ahv[-1]) + 1) % 10)), [])

    def test_ssn(self):
        self.assertEqual(rules("-".join(["529", "41", "8376"])), ["ssn"])
        self.assertEqual(rules("-".join(["123", "45", "6789"])), [])
        self.assertEqual(rules("-".join(["000", "41", "8376"])), [])

    def test_account_reads_prose_only(self):
        line = "the account " + "0482-7713-96" + " was parsed"
        self.assertEqual(rules(line, "doc"), ["account"])
        self.assertEqual(rules(line, "message"), ["account"])
        self.assertEqual(rules(line, "code"), [])

    def test_account_ignores_dates_amounts_and_placeholders(self):
        for tail in ("2026-08-31", "2026-08", "03/31/21", "1234.56",
                     "900000", "12345678", "the 2026-07 run"):
            self.assertEqual(rules(f"account {tail}", "doc"), [], tail)

    def test_account_with_a_prefix_and_in_groups(self):
        self.assertEqual(rules("portfolio " + "U" + "4829173", "doc"), ["account"])
        self.assertEqual(rules("konto " + "4829 1733 5501", "doc"), ["account"])

    def test_masked(self):
        self.assertEqual(rules("the card ending in " + "4821", "message"), ["masked"])
        self.assertEqual(rules("xxxx" + "4821", "doc"), ["masked"])
        self.assertEqual(rules("xxxx" + "4821", "code"), [])
        self.assertEqual(rules("the period ending 2025", "doc"), [])
        self.assertEqual(rules("ending in 1234", "doc"), [])

    def test_amount_reads_messages_only(self):
        line = "booked CHF " + "1,284.35" + " twice"
        self.assertEqual(rules(line, "message"), ["amount"])
        self.assertEqual(rules(line, "doc"), [])
        self.assertEqual(rules("$" + "384.07" + " fee", "message"), ["amount"])
        for line in ("bump Go to 1.25", "cap at $20", "above CHF 1,500",
                     "pass $1 to the script", "2,000 rows"):
            self.assertEqual(rules(line, "message"), [], line)

    def test_secrets_of_known_formats(self):
        for value in ("gh" + "p_" + token(36), "AK" + "IA" + token(16).upper(),
                      "sk-" + "ant-" + token(40),
                      "eyJ" + token(12) + ".eyJ" + token(12) + "." + token(12)):
            self.assertIn("secret", rules(f"x = {value}"), value)
        self.assertEqual(rules("-----BEGIN " + "RSA PRIVATE KEY-----"), ["secret"])

    def test_secret_placeholders_pass(self):
        self.assertEqual(rules("access-" + "sandbox-" + "00000000-0000-4000-8000-000000000001"), [])
        for value in ("AAAA1B2C3D4E5F6", "test-token-" + token(12),
                      "Synthetic-pw-1!", "0123456789" + "abcdef" * 3):
            self.assertEqual(rules(f'password = "{value}"'), [], value)

    def test_literal_secrets(self):
        self.assertEqual(rules(f'api_key = "{token(24)}"'), ["secret"])
        self.assertEqual(rules(f"Authorization: Bearer {token(32)}"), ["secret"])
        self.assertEqual(rules(f"https://svc:{token(20)}@host.invalid/x"), ["secret"])
        self.assertEqual(rules('password = "${PASSWORD}"'), [])
        self.assertEqual(rules('SELECTOR = "#login-form-0-password-textfield"'), [])

    def test_paths(self):
        self.assertEqual(rules("/Users/" + "jkowalski" + "/data"), ["path"])
        self.assertEqual(rules("/Users/Shared/" + "jkowalski" + "/db"), ["path"])
        for line in ("/home/u/x", "/Users/alice/x", "$HOME/x", "/home/runner/w",
                     "/Users/Shared"):
            self.assertEqual(rules(line), [], line)

    def test_hosts(self):
        self.assertEqual(rules("ssh " + "nas-17" + ".local"), ["host"])
        self.assertEqual(rules("http://wealthdb" + ".local:3000"), [])
        self.assertEqual(rules("state = threading" + ".local()"), [])

    def test_emails(self):
        self.assertEqual(rules("j.kowalski" + "@" + "mailbox.ch"), ["email"])
        for line in ("someone@example.com", "git@github.com:org/repo",
                     "SNB1A@SNB.NSS.KZS.EID", "admin@wealthdb.local"):
            self.assertNotIn("email", rules(line), line)

    def test_phones(self):
        self.assertEqual(rules("call +41 79" + " 386 52 17"), ["phone"])
        self.assertEqual(rules("+41 00" + " 000 00 00"), [])
        self.assertEqual(rules("x = a +1 2"), [])


class Watchlists(unittest.TestCase):
    def test_terms_match_whole_words_in_any_case(self):
        w = p.Watchlist(["Brindlecove", "abc"])
        self.assertEqual(list(w.scan("at BRINDLECOVE bank")), ["BRINDLECOVE"])
        self.assertEqual(list(w.scan("Brindlecoves")), [])
        self.assertEqual(list(w.scan("abc")), [])

    def test_digit_terms_match_with_other_separators(self):
        w = p.Watchlist(["4471-3390-12"])
        self.assertEqual(list(w.scan("acct 4471 3390 12")), ["4471-3390-12"])
        self.assertEqual(list(w.scan("4471339012")), ["4471-3390-12"])

    def test_load_skips_comments_and_missing_files(self):
        with tempfile.TemporaryDirectory() as d:
            f = Path(d) / "w.txt"
            f.write_text("# a comment\n\nBrindlecove\n")
            w = p.Watchlist.load([f, Path(d) / "missing.txt"])
            self.assertEqual(list(w.scan("Brindlecove")), ["Brindlecove"])

    def test_check_reports_watch_hits(self):
        found = p.check([("f:1", "Brindlecove", "code")], ALLOW,
                        p.Watchlist(["Brindlecove"]))
        self.assertEqual([(f.where, f.rule) for f in found], [("f:1", "watch")])


class Known(unittest.TestCase):
    FULL = "ab12" * 10

    def test_a_known_finding_matches_commit_location_and_rule(self):
        known = frozenset({(self.FULL, "message:4", "amount")})
        hit = p.Finding("commit ab12ab12 message:4", "amount", "x")
        self.assertTrue(p.is_known(hit, known))
        for other in (p.Finding("commit ab12ab12 message:5", "amount", "x"),
                      p.Finding("commit ab12ab12 message:4", "masked", "x"),
                      p.Finding("commit cd34cd34 message:4", "amount", "x"),
                      p.Finding("f.txt:4", "amount", "x")):
            self.assertFalse(p.is_known(other, known), other)

    def test_load_known(self):
        with tempfile.TemporaryDirectory() as d:
            f = Path(d) / "known.txt"
            f.write_text(f"# a comment\n{self.FULL} message:4 amount  # why\nbad line\n")
            self.assertEqual(p.load_known(f), {(self.FULL, "message:4", "amount")})


class Sources(unittest.TestCase):
    DIFF = "\n".join([
        "diff --git a/docs/a.md b/docs/a.md",
        "index 1..2 100644",
        "--- a/docs/a.md",
        "+++ b/docs/a.md",
        "@@ -3,0 +4,2 @@ ctx",
        "+first",
        "+second",
        "@@ -9 +11 @@",
        "-gone",
        "+third",
        "diff --git a/x/go.sum b/x/go.sum",
        "--- a/x/go.sum",
        "+++ b/x/go.sum",
        "@@ -1 +1 @@",
        "+hash",
        "diff --git a/new/f.py b/new/f.py",
        "new file mode 100644",
        "--- /dev/null",
        "+++ b/new/f.py",
        "@@ -0,0 +1 @@",
        "+code",
    ])

    def test_diff_lines(self):
        self.assertEqual(list(p.diff_lines(self.DIFF)), [
            ("docs/a.md:4", "first", "doc"),
            ("docs/a.md:5", "second", "doc"),
            ("docs/a.md:11", "third", "doc"),
            ("new/f.py:1", "code", "code"),
            ("new/f.py (file name)", "new/f.py", "code"),
        ])

    def test_an_edited_message_drops_comments_and_the_verbose_diff(self):
        msg = "subject\n# a comment\nbody\n# ------------------------ >8 ---\n+diff"
        self.assertEqual(list(p.message_lines("m", msg, editing=True)), [
            ("m:1", "subject", "message"), ("m:3", "body", "message")])

    def test_a_recorded_message_keeps_every_line(self):
        self.assertEqual(len(list(p.message_lines("m", "subject\n# kept"))), 2)


class ScratchRepo(unittest.TestCase):
    """A scratch repository with one commit, and no git config or
    watchlist of the machine's."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.repo = Path(self.tmp.name) / "repo"
        self.repo.mkdir()
        self.env = {**os.environ, "XDG_CONFIG_HOME": str(Path(self.tmp.name) / "cfg"),
                    "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1",
                    "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
                    "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com"}
        self.git("init", "-q", "-b", "main")
        (self.repo / "README.md").write_text("start\n")
        self.git("add", "README.md")
        self.git("commit", "-q", "-m", "start")

    def tearDown(self):
        self.tmp.cleanup()

    def git(self, *args):
        subprocess.run(["git", *args], cwd=self.repo, env=self.env, check=True)

    def run_check(self, *args):
        return subprocess.run([sys.executable, str(HERE.parent / "pii_check.py"), *args],
                              cwd=self.repo, env=self.env, capture_output=True, text=True)


class CommandLine(ScratchRepo):
    """The script against a scratch repository, as the hooks run it."""

    def test_staged_refuses_and_hides_the_value(self):
        iban = make_iban("CH", "04839271556039182")
        (self.repo / "f.txt").write_text(f"pay {iban}\n")
        self.git("add", "f.txt")
        run = self.run_check("staged")
        self.assertEqual(run.returncode, 1)
        self.assertIn("f.txt:1: iban: XX###", run.stderr)
        self.assertNotIn(iban, run.stderr)
        self.assertIn(iban, self.run_check("--show", "staged").stderr)

    def test_staged_clean(self):
        (self.repo / "f.txt").write_text("nothing here\n")
        self.git("add", "f.txt")
        self.assertEqual(self.run_check("staged").returncode, 0)

    def test_message(self):
        msg = Path(self.tmp.name) / "MSG"
        msg.write_text("fix: the account ending in " + "4821\n")
        run = self.run_check("message", str(msg))
        self.assertEqual(run.returncode, 1)
        self.assertIn("commit message:1: masked: ####", run.stderr)

    def test_range_reads_changes_and_messages(self):
        self.git("checkout", "-q", "-b", "topic")
        (self.repo / "g.md").write_text("ok\n")
        self.git("add", "g.md")
        self.git("commit", "-q", "-m", "booked CHF " + "1,284.35")
        run = self.run_check("range", "main")
        self.assertEqual(run.returncode, 1)
        self.assertIn("message:1: amount", run.stderr)

    def test_range_reads_each_commit_not_the_net_diff(self):
        iban = make_iban("CH", "04839271556039182")
        self.git("checkout", "-q", "-b", "topic")
        (self.repo / "f.txt").write_text(f"pay {iban}\n")
        self.git("add", "f.txt")
        self.git("commit", "-q", "-m", "add")
        (self.repo / "f.txt").write_text("pay nobody\n")
        self.git("add", "f.txt")
        self.git("commit", "-q", "-m", "remove")
        run = self.run_check("range", "main")
        self.assertEqual(run.returncode, 1)
        self.assertRegex(run.stderr, r"commit [0-9a-f]+ f\.txt:1: iban")

    def test_history_reads_every_commit_from_the_root(self):
        self.assertEqual(self.run_check("history").returncode, 0)
        self.git("commit", "-q", "--allow-empty", "-m", "the card ending in " + "4821")
        run = self.run_check("history")
        self.assertEqual(run.returncode, 1)
        self.assertIn("message:1: masked", run.stderr)

    def test_default_watchlist_is_read(self):
        cfg = Path(self.env["XDG_CONFIG_HOME"]) / "wealthdb-pii"
        cfg.mkdir(parents=True)
        (cfg / "curated.txt").write_text("Brindlecove\n")
        (self.repo / "f.txt").write_text("at Brindlecove\n")
        self.git("add", "f.txt")
        run = self.run_check("staged")
        self.assertEqual(run.returncode, 1)
        self.assertIn("f.txt:1: watch: XXXXXXXXXXX", run.stderr)

    def test_files(self):
        (self.repo / "h.md").write_text("j.kowalski" + "@" + "mailbox.ch\n")
        self.assertEqual(self.run_check("files", "h.md").returncode, 1)
        self.assertEqual(self.run_check("files", "README.md").returncode, 0)


class Pinned(ScratchRepo):
    """check_at.sh: the rules and allow list of a fixed commit judge a
    change, never the ones the change itself edits."""

    PII = Path("shared/pii")

    def setUp(self):
        super().setUp()
        (self.repo / self.PII).mkdir(parents=True)
        for name in ("pii_check.py", "check_at.sh"):
            (self.repo / self.PII / name).write_bytes((HERE.parent / name).read_bytes())
        self.git("add", str(self.PII))
        self.git("commit", "-q", "-m", "the checker, no allow list")
        self.iban = make_iban("CH", "04839271556039182")

    def check_at(self, rev, *args):
        return subprocess.run(["sh", str(self.PII / "check_at.sh"), rev, *args],
                              cwd=self.repo, env=self.env, capture_output=True, text=True)

    def stage(self, name, text):
        (self.repo / name).write_text(text)
        self.git("add", name)

    def test_a_value_added_to_the_allow_list_fails_its_own_commit(self):
        self.stage(str(self.PII / "allow.txt"), self.iban + "\n")
        self.stage("f.txt", f"pay {self.iban}\n")
        run = self.check_at("HEAD", "staged")
        self.assertEqual(run.returncode, 1)
        self.assertIn("shared/pii/allow.txt:1: iban", run.stderr)
        self.assertIn("f.txt:1: iban", run.stderr)
        # A person lets that commit through; from then on the value passes.
        self.git("commit", "-q", "-m", "allow a value")
        self.stage("g.txt", f"pay {self.iban}\n")
        self.assertEqual(self.check_at("HEAD", "staged").returncode, 0)

    def test_an_edited_rule_does_not_judge_its_own_commit(self):
        self.stage(str(self.PII / "pii_check.py"), "import sys\nsys.exit(0)\n")
        self.stage("f.txt", f"pay {self.iban}\n")
        self.assertEqual(self.check_at("HEAD", "staged").returncode, 1)

    def test_a_commit_without_the_checker_runs_the_working_tree(self):
        self.stage("f.txt", f"pay {self.iban}\n")
        self.assertEqual(self.check_at("HEAD~1", "staged").returncode, 1)

    def test_an_allow_list_the_commit_lacks_allows_nothing(self):
        (self.repo / self.PII / "allow.txt").write_text(self.iban + "\n")
        self.stage("f.txt", f"pay {self.iban}\n")
        self.assertEqual(self.check_at("HEAD", "staged").returncode, 1)

    def test_a_known_finding_the_commit_adds_is_still_reported(self):
        self.git("commit", "-q", "--allow-empty", "-m", "the card ending in " + "4821")
        sha = subprocess.run(["git", "rev-parse", "HEAD"], cwd=self.repo, env=self.env,
                             capture_output=True, text=True).stdout.strip()
        (self.repo / self.PII / "known.txt").write_text(f"{sha} message:1 masked\n")
        self.assertEqual(self.check_at("HEAD", "history").returncode, 1)
        self.git("add", str(self.PII / "known.txt"))
        self.git("commit", "-q", "-m", "rule on it")
        self.assertEqual(self.check_at("HEAD", "history").returncode, 0)

    def test_usage(self):
        self.assertEqual(self.check_at("HEAD").returncode, 2)


if __name__ == "__main__":
    unittest.main()
