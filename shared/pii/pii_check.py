#!/usr/bin/env python3
"""pii_check — refuse identifiers and credentials in what is committed.

A coarse net under AGENTS.md §4, run by the pre-commit and commit-msg
hooks and by CI. It reads only what a change adds: the added lines of a
diff, the names of added files, and commit messages. Over a range of
commits it reads each commit's own additions, since a value one commit
adds and a later one removes stays in the history. Each rule looks for
one shape and then checks what a shape alone cannot:

- iban     an IBAN of its country's length whose mod-97 check holds
- card     a card number of a known brand that passes the Luhn check,
           in 4-digit groups, or bare on a line that names a card
- ahv      a Swiss AHV number whose EAN-13 check digit holds
- ssn      a US social security number in its dashed form
- account  six or more digits right after a word such as "account",
           "IBAN" or "customer" (prose only)
- masked   the last four digits of a masked number ("ending in 4821",
           "xxxx4821"; prose only)
- amount   a money amount (commit messages only)
- secret   private keys, tokens of known formats, bearer tokens,
           credentials in URLs, literal passwords and secrets
- path     a home directory under a real name (/Users/<name>/…)
- host     a .local host name
- email    an e-mail address outside the reserved example domains
- phone    an international phone number
- watch    a term from a private watchlist kept outside the repository

Prose is commit messages and documents (.md, .rst, .txt). The account
and masked rules read only prose: in a fixture, a real account number
looks like a synthetic one, so only the watchlist tells them apart.

Placeholders pass: one digit repeated, a counting run (123456…), mostly
zeros, and the values in allow.txt next to this file. A checksum rule
fires only on a valid value without a counting run, so a typed-out test
value rarely does. There is no other way past a rule: no pragma, no
per-file switch. A test that needs a valid value computes it at run time
(tests/test_pii_check.py shows how).

known.txt lists findings already ruled on in published history, by
commit, location and rule, never by value: history cannot change, so
they are not shown again. A commit's hash exists only once it is made,
so no change can list itself.

check_at.sh runs this file, allow.txt and known.txt as a fixed commit
holds them: HEAD in the hooks, the base of the change in CI. A change to
any of them is judged by the old one, so a value added to allow.txt
fails the commit that adds it, until a person lets that commit through.

A finding prints its location, its rule and the value's shape (digits
as #, letters as X), never the value: the CI log of a public repository
is public. --show adds the value, for a local run.

The watchlists are ${XDG_CONFIG_HOME:-~/.config}/wealthdb-pii/
watchlist.txt (written by watchlist.py from the local gold database)
and curated.txt (terms added by hand), one term per line, plus any
--watchlist FILE.

usage:
  pii_check.py staged              the index: what `git commit` records
  pii_check.py message FILE        a commit message (the commit-msg hook)
  pii_check.py range BASE [HEAD]   every commit of BASE..HEAD
  pii_check.py history [REV...]    every commit REV reaches (default HEAD)
  pii_check.py files [PATH...]     whole files (default: every tracked file)

Exit status: 0 clean, 1 findings, 2 a usage or git error.
"""
from __future__ import annotations

import codecs
import argparse
import math
import os
import re
import subprocess
import sys
from collections.abc import Iterable, Iterator
from dataclasses import dataclass
from pathlib import Path

HERE = Path(__file__).resolve().parent
ALLOW_FILE = HERE / "allow.txt"
KNOWN_FILE = HERE / "known.txt"
CONFIG_DIR = Path(os.environ.get("XDG_CONFIG_HOME")
                  or Path.home() / ".config") / "wealthdb-pii"
WATCHLISTS = ("watchlist.txt", "curated.txt")

# Lock files and checksums: hashes and versions, never a person's data.
SKIP_NAMES = frozenset({"go.sum", "package-lock.json", "yarn.lock",
                        "poetry.lock", "Pipfile.lock", "uv.lock"})
DOC_SUFFIXES = frozenset({".md", ".rst", ".txt"})


def compact(value: str) -> str:
    """The value upper-cased, with spaces, dashes, dots and slashes gone."""
    return re.sub(r"[\s./-]", "", value).upper()


@dataclass(frozen=True)
class Finding:
    where: str
    rule: str
    value: str

    def render(self, show: bool) -> str:
        out = f"{self.where}: {self.rule}: {shape(self.value)}"
        return f"{out}  [{self.value}]" if show else out


def shape(value: str) -> str:
    """The value with every digit as # and every letter as X."""
    return re.sub(r"[A-Za-z]", "X", re.sub(r"\d", "#", value))


def counting(digits: str) -> bool:
    """Every digit one up, or every digit one down, from the last."""
    steps = {(int(digits[i + 1]) - int(digits[i])) % 10
             for i in range(len(digits) - 1)}
    return steps in ({1}, {9})


def is_placeholder(digits: str) -> bool:
    """One digit repeated, a counting run, a short block repeated, or
    mostly zeros."""
    if len(set(digits)) <= 1 or counting(digits):
        return True
    for period in (2, 3, 4):
        if len(digits) >= 2 * period and \
                digits == (digits[:period] * len(digits))[:len(digits)]:
            return True
    return digits.count("0") * 10 >= len(digits) * 7


def typed(digits: str) -> bool:
    """A placeholder, or a value that holds a counting run of six."""
    return is_placeholder(digits) or any(
        counting(digits[i:i + 6]) for i in range(len(digits) - 5))


def load_allow(path: Path) -> frozenset[str]:
    """The values of an allow file: one per line, # comments, spaces,
    dashes, dots and case ignored. No file allows nothing."""
    if not path.is_file():
        return frozenset()
    return frozenset(compact(v) for v in (line.split("#", 1)[0].strip()
                                          for line in path.read_text().splitlines())
                     if v)


def load_known(path: Path) -> frozenset[tuple[str, str, str]]:
    """The (full commit hash, location, rule) of every ruled finding: one
    per line, `<hash> <location> <rule>`, # comments."""
    if not path.is_file():
        return frozenset()
    entries = set()
    for line in path.read_text().splitlines():
        fields = line.split("#", 1)[0].split()
        if len(fields) == 3:
            entries.add((fields[0], fields[1], fields[2]))
    return frozenset(entries)


def is_known(finding: Finding, known: frozenset[tuple[str, str, str]]) -> bool:
    """Whether a finding in a recorded commit is one already ruled on. The
    finding names the commit by its short hash; the list by its full one."""
    m = re.fullmatch(r"commit ([0-9a-f]+) (\S+)", finding.where)
    return m is not None and any(
        full.startswith(m.group(1)) and loc == m.group(2) and rule == finding.rule
        for full, loc, rule in known)


# ---- checksums ---------------------------------------------------------

# IBAN length per country (the SWIFT IBAN registry).
IBAN_LENGTHS = {
    "AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16,
    "BG": 22, "BH": 22, "BR": 29, "BY": 28, "CH": 21, "CR": 22, "CY": 28,
    "CZ": 24, "DE": 22, "DK": 18, "DO": 28, "EE": 20, "EG": 29, "ES": 24,
    "FI": 18, "FO": 18, "FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18,
    "GR": 27, "GT": 28, "HR": 21, "HU": 28, "IE": 22, "IL": 23, "IQ": 23,
    "IS": 26, "IT": 27, "JO": 30, "KW": 30, "KZ": 20, "LB": 28, "LC": 32,
    "LI": 21, "LT": 20, "LU": 20, "LV": 21, "LY": 25, "MC": 27, "MD": 24,
    "ME": 22, "MK": 19, "MR": 27, "MT": 31, "MU": 30, "NL": 18, "NO": 15,
    "PK": 24, "PL": 28, "PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22,
    "SA": 24, "SC": 31, "SD": 18, "SE": 24, "SI": 19, "SK": 24, "SM": 27,
    "ST": 25, "SV": 28, "TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22,
    "VG": 24, "XK": 20,
}


def iban_valid(iban: str) -> bool:
    if len(iban) != IBAN_LENGTHS.get(iban[:2], 0):
        return False
    moved = iban[4:] + iban[:4]
    return int("".join(str(int(c, 36)) for c in moved)) % 97 == 1


def luhn_valid(digits: str) -> bool:
    total = 0
    for i, c in enumerate(reversed(digits)):
        d = int(c) * (2 if i % 2 else 1)
        total += d - 9 if d > 9 else d
    return total % 10 == 0


def card_brand(digits: str) -> bool:
    """Visa, Mastercard, Amex, Discover, JCB, Diners or UnionPay, by
    prefix and length."""
    n, p2, p4 = len(digits), int(digits[:2]), int(digits[:4])
    return ((digits[0] == "4" and n in (13, 16, 19))
            or ((51 <= p2 <= 55 or 2221 <= p4 <= 2720) and n == 16)
            or (p2 in (34, 37) and n == 15)
            or ((p4 == 6011 or 644 <= p4 // 10 <= 649 or p2 == 65)
                and 16 <= n <= 19)
            or (3528 <= p4 <= 3589 and 16 <= n <= 19)
            or ((300 <= p4 // 10 <= 305 or p2 in (36, 38, 39))
                and 14 <= n <= 19)
            or (p2 == 62 and 16 <= n <= 19))


def ean13_valid(digits: str) -> bool:
    total = sum(int(c) * (3 if i % 2 else 1) for i, c in enumerate(digits[:12]))
    return (10 - total % 10) % 10 == int(digits[12])


def entropy(value: str) -> float:
    """Shannon entropy in bits per character."""
    counts = {c: value.count(c) for c in set(value)}
    return -sum(k / len(value) * math.log2(k / len(value))
                for k in counts.values())


# ---- rules -------------------------------------------------------------

IBAN_RE = re.compile(r"\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{1,4}){3,8}\b")
CARD_RE = re.compile(r"(?<![\d.,/-])\d(?:[ -]?\d){12,18}(?![\d,/]|[.-]\d)")
CARD_GROUPS_RE = re.compile(r"\d{4}([ -])\d{4}\1\d{4}\1\d{1,7}"
                            r"|\d{4}([ -])\d{6}\2\d{4,5}")
CARD_WORD_RE = re.compile(r"(?i)card|karte|\bpan\b|visa|mastercard|amex")
AHV_RE = re.compile(r"\b756\.?\d{4}\.?\d{4}\.?\d{2}\b")
SSN_RE = re.compile(r"\b(?!000|666|9\d\d)\d{3}-(?!00)\d{2}-(?!0000)\d{4}\b")
ACCOUNT_RE = re.compile(
    r"(?i)\b(?:account|acct|a/c|konto|depot|portfolio|contract|vertrag"
    r"|customer|kunde|client|member|policy|police|mandate|relationship"
    r"|routing|aba|iban|ssn|tin|tax[ _-]?id|ahv)"
    r"[^\d\n]{0,20}?(?<![\w.])((?-i:[A-Z]{1,3})[ -]?\d(?:[./-]?\d){5,}"
    r"|\d{4}(?: \d{4})+|\d(?:[./-]?\d){5,})(?![\w.]*\d)")
DATE_RE = re.compile(r"^(\d{4}[-/.]\d{2}([-/.]\d{2})?|\d{2}[-/.]\d{2}[-/.]\d{2,4})$")
DECIMAL_RE = re.compile(r"^\d+\.\d{2}$")
MASKED_RE = re.compile(
    r"(?i)\b(?:ending(?:\s+in)?|ends\s+(?:in|with)"
    r"|last\s+(?:4|four)(?:\s+digits)?)\s*[:#]?\s*(\d{4})\b(?![-/.]\d)"
    r"|(?:\b[xX]{4,}|\*{4,}|[•·]{4,})[ -]?(\d{4})\b")
# A currency before a number, or cents after three or more digits. A
# bare "1.25" is more often a version than money.
AMOUNT_RE = re.compile(
    r"(?:\b(?:CHF|USD|EUR|GBP|CAD|AUD|JPY)\s?|[$€£]\s?)-?\d[\d',]*\d(?:\.\d{2})?"
    r"|[$€£]\s?\d\.\d{2}|\b\d{1,3}(?:[',]\d{3})+\.\d{2}\b|\b\d{3,}\.\d{2}\b")
PATH_RE = re.compile(r"(?<![\w$])(?:/Users/Shared|/Users|/home|[A-Z]:\\\\?Users)"
                     r"[/\\]{1,2}([A-Za-z0-9][A-Za-z0-9._-]*)")
HOST_RE = re.compile(r"(?<![\w.])([a-z0-9][a-z0-9-]*)\.local\b(?![(\w])")
EMAIL_RE = re.compile(r"(?<![\w.+-])[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+"
                      r"(?:\.[A-Za-z0-9-]+)*\.[a-z]{2,})\b")
PHONE_RE = re.compile(r"(?<![\w+])\+\d{1,3}(?:[ .-]?\(?\d{1,4}\)?)"
                      r"(?:[ .-]?\d{2,4}){2,4}\b")

PRIVATE_KEY_RE = re.compile(r"-----BEGIN (?:[A-Z]+ )*PRIVATE KEY-----")
TOKEN_RES = (
    re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}"),
    re.compile(r"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b"),
    re.compile(r"\bgh[pousr]_[A-Za-z0-9]{36,}\b"),
    re.compile(r"\bgithub_pat_[A-Za-z0-9_]{40,}"),
    re.compile(r"\bglpat-[A-Za-z0-9_-]{20,}"),
    re.compile(r"\bxox[abprs]-[A-Za-z0-9-]{10,}"),
    re.compile(r"\bsk-ant-[A-Za-z0-9_-]{20,}"),
    re.compile(r"\bsk-(?:proj-)?[A-Za-z0-9_-]{32,}"),
    re.compile(r"\bAIza[0-9A-Za-z_-]{35}\b"),
    re.compile(r"\baccess-(?:sandbox|development|production)-[0-9a-f-]{30,}"),
)
BEARER_RE = re.compile(r"(?i)\bbearer\s+([A-Za-z0-9._~+/-]{20,}=*)")
URL_CRED_RE = re.compile(r"[a-z][a-z0-9+.-]*://[^/\s:@'\"]+:([^/\s@'\"]+)@")
ASSIGN_RE = re.compile(
    r"(?i)[\w-]*(?:password|passwd|pwd|secret|token|api[_-]?key|apikey"
    r"|cookie|session[_-]?id)[\w-]*[\"']?\s*[:=]\s*[\"']([^\"'\s]{12,})[\"']")
# What marks a literal as a stand-in, not a credential: a telling word, a
# character repeated, a counting run, a template, a URL or a selector.
PLACEHOLDER_RE = re.compile(r"(?i)test|fake|dummy|example|sample|placeholder"
                            r"|synthetic|changeme|redact|(.)\1{3}|0123|1234"
                            r"|abcd|[<>{}$%]|\.\.\.|://|^[#/.]")

PATH_NAMES = frozenset({
    "alice", "bob", "carol", "dave", "eve", "jane", "john", "jdoe", "me",
    "you", "user", "username", "name", "someone", "example", "test",
    "runner", "ubuntu", "demo", "app", "root", "foo", "bar",
    # macOS's folder for all users; a name below it is still caught.
    "shared",
})
EMAIL_DOMAINS_RE = re.compile(r"(?i)(^|\.)(example\.(com|net|org)|example"
                              r"|test|invalid|local|localhost|noreply\.github\.com)$")


def account_value(token: str) -> bool:
    digits = re.sub(r"\D", "", token)
    return not (is_placeholder(digits) or DATE_RE.match(token)
                or DECIMAL_RE.match(token))


def literal_secret(value: str) -> bool:
    return (len(value) >= 16 and not PLACEHOLDER_RE.search(value)
            and entropy(value) >= 3.2 and re.search(r"\d", value) is not None
            and re.search(r"[A-Za-z]", value) is not None)


def scan_line(text: str, allow: frozenset[str],
              kind: str = "code") -> Iterator[tuple[str, str]]:
    """Every (rule, value) the line holds. kind is "message", "doc" or
    "code"; messages and docs are prose."""
    def allowed(value: str) -> bool:
        return compact(value) in allow

    for m in IBAN_RE.finditer(text):
        iban = compact(m.group())[:IBAN_LENGTHS.get(m.group()[:2], 0)]
        if iban and iban_valid(iban) and not allowed(iban) \
                and not typed(re.sub(r"\D", "", iban[4:])):
            yield "iban", iban
    named = CARD_WORD_RE.search(text) is not None
    for m in CARD_RE.finditer(text):
        digits = re.sub(r"\D", "", m.group())
        grouped = CARD_GROUPS_RE.fullmatch(m.group()) is not None
        if (grouped or (named and digits == m.group())) and card_brand(digits) \
                and luhn_valid(digits) and not typed(digits) \
                and not allowed(digits):
            yield "card", m.group()
    for m in AHV_RE.finditer(text):
        digits = re.sub(r"\D", "", m.group())
        if ean13_valid(digits) and not typed(digits[3:]) and not allowed(digits):
            yield "ahv", m.group()
    for m in SSN_RE.finditer(text):
        if not is_placeholder(m.group().replace("-", "")) and not allowed(m.group()):
            yield "ssn", m.group()
    if kind != "code":
        for m in ACCOUNT_RE.finditer(text):
            if account_value(m.group(1)):
                yield "account", m.group(1)
        for m in MASKED_RE.finditer(text):
            last4 = m.group(1) or m.group(2)
            # "the period ending 2025" names a year, not a number.
            if not (m.group(1) and 1900 <= int(last4) <= 2099) \
                    and not is_placeholder(last4):
                yield "masked", last4
    if kind == "message":
        for m in AMOUNT_RE.finditer(text):
            # A round figure ("CHF 1,500", "$20") states a limit, not a
            # balance.
            digits = re.sub(r"\D", "", m.group()).rstrip("0")
            if len(digits) > 2 and not typed(digits):
                yield "amount", m.group()
    for m in PRIVATE_KEY_RE.finditer(text):
        yield "secret", m.group()
    for pattern in TOKEN_RES:
        for m in pattern.finditer(text):
            if not PLACEHOLDER_RE.search(m.group()):
                yield "secret", m.group()
    for pattern in (BEARER_RE, URL_CRED_RE, ASSIGN_RE):
        for m in pattern.finditer(text):
            if literal_secret(m.group(1)):
                yield "secret", m.group(1)
    for m in PATH_RE.finditer(text):
        if len(m.group(1)) > 2 and m.group(1).lower() not in PATH_NAMES:
            yield "path", m.group()
    for m in HOST_RE.finditer(text):
        if not allowed(m.group()):
            yield "host", m.group()
    for m in EMAIL_RE.finditer(text):
        if not EMAIL_DOMAINS_RE.search(m.group(1)) and not allowed(m.group()):
            yield "email", m.group()
    for m in PHONE_RE.finditer(text):
        digits = re.sub(r"\D", "", m.group())
        if 9 <= len(digits) <= 15 and not is_placeholder(digits[2:]):
            yield "phone", m.group()


# ---- watchlists --------------------------------------------------------

class Watchlist:
    """Literal terms, matched case-insensitively and never inside a
    longer word. A term with four or more digits also matches with its
    separators written differently ("CH12 3456" finds "ch123456")."""

    def __init__(self, terms: Iterable[str]) -> None:
        words = sorted({t for t in terms if len(t) >= 4}, key=len, reverse=True)
        self.pattern = re.compile(
            "|".join(rf"(?<![A-Za-z0-9]){re.escape(t)}(?![A-Za-z0-9])"
                     for t in words), re.I) if words else None
        self.compacted = {compact(t): t for t in words
                          if sum(c.isdigit() for c in t) >= 4}

    @classmethod
    def load(cls, paths: Iterable[Path]) -> Watchlist:
        terms = []
        for path in paths:
            if path.is_file():
                terms += [line.strip() for line in path.read_text().splitlines()
                          if line.strip() and not line.lstrip().startswith("#")]
        return cls(terms)

    def scan(self, text: str) -> Iterator[str]:
        found = set()
        if self.pattern:
            for m in self.pattern.finditer(text):
                found.add(m.group().lower())
                yield m.group()
        if self.compacted:
            flat = compact(text)
            for key, term in self.compacted.items():
                if key in flat and term.lower() not in found:
                    yield term


# ---- sources -----------------------------------------------------------

# (where, text, kind): a line to check, where it is, and what it is.
Line = tuple[str, str, str]


def git(*args: str) -> str:
    run = subprocess.run(["git", "-c", "core.quotepath=off", *args],
                         capture_output=True, text=True, errors="replace")
    if run.returncode != 0:
        raise SystemExit(f"pii_check: git {args[0]}: {run.stderr.strip()}")
    return run.stdout


def skipped(path: str) -> bool:
    return Path(path).name in SKIP_NAMES


def kind_of(path: str) -> str:
    return "doc" if Path(path).suffix.lower() in DOC_SUFFIXES else "code"


def new_name(raw: str) -> str:
    """The file a diff header names on its b/ side, "" for none. git
    quotes a name holding a quote, a backslash or a tab, and ends one
    holding a space with a tab."""
    name = raw.rstrip("\t")
    if name.startswith('"'):
        name = codecs.escape_decode(name[1:-1].encode())[0].decode("utf-8", "replace")
    return name[2:] if name.startswith("b/") else ""


def diff_lines(diff: str) -> Iterator[Line]:
    """Every added line of a unified diff, and the name of every added or
    renamed file. A hunk header counts its lines, so an added line that
    reads like a file header stays an added line. Only a newline ends a
    line: a form feed inside one does not."""
    path, line, left = "", 0, 0
    for raw in diff.split("\n"):
        if left:
            if raw.startswith("+"):
                if path and not skipped(path):
                    yield f"{path}:{line}", raw[1:].rstrip("\r"), kind_of(path)
                line += 1
                left -= 1
        elif raw.startswith("+++ "):
            path = new_name(raw[4:])
        elif raw.startswith("@@ "):
            m = re.match(r"@@ -\S+ \+(\d+)(?:,(\d+))?", raw)
            line = int(m.group(1)) if m else 0
            left = int(m.group(2) or 1) if m else 0
        elif raw.startswith("diff --git "):
            path = ""
    for m in re.finditer(r'^diff --git .* ("?b/.*)\n(?:new file mode|similarity)',
                         diff, re.M):
        name = new_name(m.group(1))
        yield f"{name} (file name)", name, "code"


def diff_of(*args: str) -> str:
    return git("diff", "--no-color", "--no-ext-diff", "-U0",
               "--diff-filter=ACMR", "-M", *args)


def message_lines(where: str, message: str, editing: bool = False) -> Iterator[Line]:
    """The lines of a commit message. A message being edited drops git's
    # comments and the diff `git commit -v` appends below its scissors;
    a recorded one keeps every line."""
    for i, text in enumerate(message.splitlines(), 1):
        if editing and text.startswith("# ------------------------ >8"):
            break
        if not (editing and text.startswith("#")):
            yield f"{where}:{i}", text, "message"


def log_lines(*revs: str) -> Iterator[Line]:
    """Each commit's own added lines, new file names and message, for the
    commits `git log REVS` lists. No --diff-filter: in `git log` it drops
    every commit without a matching file, and with it that message. A
    deleted file adds no lines anyway. NUL delimits an entry and its
    parts: a message cannot hold one, and a diff line starts with its
    prefix. Two open an entry, as a message's own last newline puts the
    one that closes it at the start of a line too."""
    log = git("log", "-p", "--no-color", "--no-ext-diff", "-U0", "-M",
              "--format=%x00%x00%h%x00%B%x00", *revs)
    for entry in ("\n" + log).split("\n\x00\x00")[1:]:
        sha, message, diff = entry.split("\x00", 2)
        yield from message_lines(f"commit {sha} message", message)
        for where, text, kind in diff_lines(diff):
            yield f"commit {sha} {where}", text, kind


def file_lines(paths: list[str]) -> Iterator[Line]:
    names = paths or git("ls-files", "-z").split("\0")
    for name in filter(None, names):
        path = Path(name)
        if skipped(name) or not path.is_file():
            continue
        data = path.read_bytes()
        if b"\0" in data[:8192]:
            continue
        for i, text in enumerate(data.decode("utf-8", "replace").splitlines(), 1):
            yield f"{name}:{i}", text, kind_of(name)
        yield f"{name} (file name)", name, "code"


def check(lines: Iterable[Line], allow: frozenset[str],
          watch: Watchlist) -> list[Finding]:
    findings = []
    for where, text, kind in lines:
        seen = set()
        for rule, value in [*scan_line(text, allow, kind),
                            *(("watch", t) for t in watch.scan(text))]:
            if (rule, value) not in seen:
                seen.add((rule, value))
                findings.append(Finding(where, rule, value))
    return findings


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="pii_check.py",
        description="Refuse identifiers and credentials in a change.")
    ap.add_argument("--show", action="store_true",
                    help="print each matched value too (never in CI)")
    ap.add_argument("--watchlist", action="append", type=Path, default=[],
                    metavar="FILE", help="another file of private terms")
    sub = ap.add_subparsers(dest="mode", required=True)
    sub.add_parser("staged", help="the index")
    msg = sub.add_parser("message", help="a commit message file")
    msg.add_argument("file", type=Path)
    rng = sub.add_parser("range", help="every commit of BASE..HEAD")
    rng.add_argument("base")
    rng.add_argument("head", nargs="?", default="HEAD")
    his = sub.add_parser("history", help="every commit REV reaches")
    his.add_argument("revs", nargs="*", default=["HEAD"], metavar="REV")
    fil = sub.add_parser("files", help="whole files")
    fil.add_argument("paths", nargs="*")
    args = ap.parse_args(argv)

    if args.mode == "staged":
        lines = diff_lines(diff_of("--cached"))
    elif args.mode == "message":
        lines = message_lines("commit message", args.file.read_text(), editing=True)
    elif args.mode == "range":
        lines = log_lines(f"{args.base}..{args.head}")
    elif args.mode == "history":
        lines = log_lines(*args.revs)
    else:
        lines = file_lines(args.paths)
    watch = Watchlist.load([*(CONFIG_DIR / n for n in WATCHLISTS),
                            *args.watchlist])
    known = load_known(KNOWN_FILE)
    findings = [f for f in check(lines, load_allow(ALLOW_FILE), watch)
                if not is_known(f, known)]
    if not findings:
        return 0
    print(f"pii_check: {len(findings)} finding(s) in the {args.mode} check:",
          file=sys.stderr)
    for f in findings:
        print("  " + f.render(args.show), file=sys.stderr)
    print("Replace each with a synthetic value (AGENTS.md §4). A test that\n"
          "needs a valid checksum computes its value at run time; allow.txt\n"
          "holds published test values only.", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
