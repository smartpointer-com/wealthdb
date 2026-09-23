"""Unit tests for collectorkit. Stdlib unittest so they run with a bare
`python -m unittest` (and also under pytest in the collector containers)."""
import os
import sqlite3
import tempfile
import unittest
from pathlib import Path

import argparse

from collectorkit import bronze, cli, envfile, parse, session, silver

INIT_SQL = (
    "CREATE TABLE schema_meta (silver_schema_version INTEGER NOT NULL);\n"
    "INSERT INTO schema_meta (silver_schema_version) VALUES (1);\n"
)
ADD_SQL = (
    "CREATE TABLE foo (id INTEGER);\n"
    "INSERT INTO schema_meta (silver_schema_version) VALUES (2);\n"
)
BAD_SQL = "CREATE TABLE bar (id INTEGER);\n"  # forgets to bump schema_meta


class MigrationsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.migrations = self.root / "migrations"
        self.migrations.mkdir()
        (self.migrations / "0001_initial.sql").write_text(INIT_SQL)
        (self.migrations / "0002_add.sql").write_text(ADD_SQL)
        self.db = self.root / "silver.db"

    def tearDown(self):
        self.tmp.cleanup()

    def test_apply_and_idempotent(self):
        conn = silver.open_db(self.db)
        self.assertEqual(silver.current_schema_version(conn), 0)
        self.assertEqual(silver.apply_migrations(conn, self.migrations), 2)
        # foo table exists (0002 ran)
        self.assertTrue(conn.execute(
            "SELECT name FROM sqlite_master WHERE name='foo'").fetchone())
        # re-running is a no-op
        self.assertEqual(silver.apply_migrations(conn, self.migrations), 2)
        conn.close()

    def test_open_db_pragmas(self):
        conn = silver.open_db(self.db)
        self.assertEqual(conn.execute("PRAGMA foreign_keys").fetchone()[0], 1)
        self.assertEqual(
            conn.execute("PRAGMA journal_mode").fetchone()[0], "wal")
        # synchronous: 1 == NORMAL
        self.assertEqual(conn.execute("PRAGMA synchronous").fetchone()[0], 1)
        conn.close()

    def test_transaction_commits_a_block_that_completes(self):
        conn = silver.open_db(self.db)
        conn.execute("CREATE TABLE t (x INTEGER)")
        with silver.transaction(conn):
            conn.execute("INSERT INTO t VALUES (1)")
        self.assertEqual(conn.execute("SELECT COUNT(*) FROM t").fetchone()[0], 1)
        self.assertFalse(conn.in_transaction)
        conn.close()

    def test_transaction_rolls_back_a_block_that_raises(self):
        conn = silver.open_db(self.db)
        conn.execute("CREATE TABLE t (x INTEGER)")
        with self.assertRaises(ValueError):
            with silver.transaction(conn):
                conn.execute("INSERT INTO t VALUES (1)")
                raise ValueError("stop")
        self.assertEqual(conn.execute("SELECT COUNT(*) FROM t").fetchone()[0], 0)
        self.assertFalse(conn.in_transaction)
        conn.close()

    def test_transaction_surfaces_the_blocks_own_error(self):
        # A block that ended the transaction itself and then raised: the
        # rollback has nothing to undo and must not mask the real error.
        conn = silver.open_db(self.db)
        with self.assertRaises(ValueError):
            with silver.transaction(conn):
                conn.commit()
                raise ValueError("the block's own error")
        conn.close()

    def test_open_db_default_isolation_pragmas(self):
        # The implicit-transaction opener also gets WAL + synchronous=NORMAL
        # (foreign keys stay on); isolation_level is left at sqlite3's
        # default so `with conn:` callers keep their BEGIN/COMMIT semantics.
        conn = silver.open_db_default_isolation(self.db)
        # default isolation is sqlite3's "" (deferred implicit txns), not
        # the manual None that open_db uses — the PRAGMAs must not change it.
        self.assertEqual(conn.isolation_level, "")
        self.assertEqual(conn.execute("PRAGMA foreign_keys").fetchone()[0], 1)
        self.assertEqual(
            conn.execute("PRAGMA journal_mode").fetchone()[0], "wal")
        self.assertEqual(conn.execute("PRAGMA synchronous").fetchone()[0], 1)
        conn.close()

    def test_both_openers_leave_the_db_owner_only(self):
        # Silver is the source's financial record; sqlite3 would create it
        # under the process umask, which in a collector container is 022.
        for opener in (silver.open_db, silver.open_db_default_isolation):
            with self.subTest(opener=opener.__name__):
                self.db.unlink(missing_ok=True)
                conn = opener(self.db)
                conn.close()
                self.assertEqual(self.db.stat().st_mode & 0o777, 0o600)

    def test_a_wider_mode_is_narrowed_on_reopen(self):
        # `load --force` deletes and recreates the DB, so a mode set once
        # does not survive a rebuild — every open has to re-assert it.
        silver.open_db(self.db).close()
        self.db.chmod(0o644)
        silver.open_db(self.db).close()
        self.assertEqual(self.db.stat().st_mode & 0o777, 0o600)

    def test_apply_under_default_isolation(self):
        # A connection using the *default* transaction model (not the
        # manual isolation_level=None of open_db) must still persist
        # migrations — verified by reopening the DB fresh.
        conn = sqlite3.connect(str(self.db))
        conn.execute("PRAGMA foreign_keys = ON")
        self.assertEqual(silver.apply_migrations(conn, self.migrations), 2)
        conn.close()
        reopened = sqlite3.connect(str(self.db))
        self.assertEqual(silver.current_schema_version(reopened), 2)
        reopened.close()

    def test_migration_must_bump_version(self):
        (self.migrations / "0003_bad.sql").write_text(BAD_SQL)
        conn = silver.open_db(self.db)
        with self.assertRaises(SystemExit):
            silver.apply_migrations(conn, self.migrations)
        conn.close()


class BronzeTest(unittest.TestCase):
    def test_ts_slug_shape(self):
        slug = bronze.ts_slug()
        self.assertRegex(slug, r"^\d{8}T\d{6}Z$")
        self.assertTrue(bronze.RUN_DIR_RE.match(slug))

    def test_short_token_is_stable_and_bounded(self):
        self.assertEqual(bronze.short_token("abc"), bronze.short_token("abc"))
        self.assertRegex(bronze.short_token("abc"), r"^[0-9a-f]{16}$")
        self.assertEqual(len(bronze.short_token("abc", 8)), 8)

    def test_short_token_separates_a_shared_prefix(self):
        # The reason it hashes rather than slices: source tokens routinely
        # share a long prefix, and a slice would give two entities one
        # bronze filename and silently lose one of them.
        prefix = "SharedCustomerPrefix" * 3
        self.assertNotEqual(bronze.short_token(prefix + "AAAA"),
                            bronze.short_token(prefix + "AAAB"))

    def test_atomic_write_json_roundtrip(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "sub" / "a.json"
            bronze.atomic_write_json(p, {"b": 2, "a": 1})
            text = p.read_text()
            self.assertIn('"a": 1', text)
            # sorted keys: "a" before "b"
            self.assertLess(text.index('"a"'), text.index('"b"'))
            self.assertFalse((Path(d) / "sub" / "a.json.tmp").exists())

    def test_iter_run_dirs_filters(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root / "20260529T071530Z").mkdir()
            (root / "not-a-run").mkdir()
            (root / "20260101T000000Z").mkdir()
            names = [p.name for p in bronze.iter_run_dirs(root)]
            self.assertEqual(names, ["20260101T000000Z", "20260529T071530Z"])

    def test_canonical_json_stable(self):
        self.assertEqual(bronze.canonical_json({"b": 1, "a": 2}),
                         '{"a":2,"b":1}')

    def test_canonical_json_default_str(self):
        # Decimal / date serialise via default=str rather than raising.
        from decimal import Decimal
        from datetime import date
        self.assertEqual(
            bronze.canonical_json({"amt": Decimal("1.50"),
                                   "d": date(2026, 1, 2)}),
            '{"amt":"1.50","d":"2026-01-02"}')

    def test_sha256_file_digest_and_size(self):
        import hashlib as _h
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "blob"
            data = b"wealthdb" * 1000
            p.write_bytes(data)
            digest, size = bronze.sha256_file(p)
            self.assertEqual(digest, _h.sha256(data).hexdigest())
            self.assertEqual(size, len(data))
            # small chunk size yields the same digest
            self.assertEqual(bronze.sha256_file(p, chunk_size=7)[0], digest)

    def test_sha256_file_memoises_by_identity(self):
        import hashlib as _h
        from unittest import mock
        # Distinct mtimes (also distinct at second resolution, so the test
        # holds even on a filesystem that truncates sub-second mtime).
        t1 = 1_600_000_000_000_000_000
        t2 = 1_700_000_000_000_000_000
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "blob"
            data = b"wealthdb" * 1000
            p.write_bytes(data)
            os.utime(p, ns=(t1, t1))
            # (a) same content -> the correct digest and size.
            first = bronze.sha256_file(p)
            self.assertEqual(first, (_h.sha256(data).hexdigest(), len(data)))
            # (b) a memo hit returns the cached tuple without re-reading:
            # the (dev, inode, size, mtime) key is unchanged, so even with
            # Path.open sabotaged the digest still comes back.
            with mock.patch.object(
                    Path, "open",
                    side_effect=AssertionError("memo hit re-read the file")):
                self.assertEqual(bronze.sha256_file(p), first)
            # (c) an in-place rewrite advances mtime, busting the key so the
            # new content is re-hashed rather than served stale.
            new_data = b"rewritten-" * 500
            p.write_bytes(new_data)
            os.utime(p, ns=(t2, t2))
            self.assertEqual(
                bronze.sha256_file(p),
                (_h.sha256(new_data).hexdigest(), len(new_data)))

    def test_parse_run_ts_roundtrips_ts_slug(self):
        from datetime import datetime as _dt, timezone as _tz
        for slug in ("20260101T000000Z", "20260529T071530Z",
                     "19700101T000000Z"):
            epoch = bronze.parse_run_ts(slug)
            self.assertEqual(
                bronze.ts_slug(_dt.fromtimestamp(epoch, tz=_tz.utc)),
                slug)
        # spot-check: epoch math matches stdlib
        expected = int(_dt(2026, 5, 29, 7, 15, 30,
                           tzinfo=_tz.utc).timestamp())
        self.assertEqual(bronze.parse_run_ts("20260529T071530Z"), expected)

    def test_parse_run_ts_rejects_bad_format(self):
        with self.assertRaises(ValueError):
            bronze.parse_run_ts("not-a-run-dir")


class EnvFileTest(unittest.TestCase):
    def test_bash_source_handles_quoting(self):
        for k in ("CK_FOO", "CK_BAZ", "CK_DOLLAR"):
            os.environ.pop(k, None)
        with tempfile.TemporaryDirectory() as d:
            envf = Path(d) / "test.env"
            envf.write_text(
                "export CK_FOO=bar\n"
                'CK_BAZ="qux with spaces"\n'
                "CK_DOLLAR='literal$notexpanded'\n"
            )
            self.assertTrue(envfile.source_env_file(envf))
            self.assertEqual(os.environ["CK_FOO"], "bar")
            self.assertEqual(os.environ["CK_BAZ"], "qux with spaces")
            self.assertEqual(os.environ["CK_DOLLAR"], "literal$notexpanded")
        for k in ("CK_FOO", "CK_BAZ", "CK_DOLLAR"):
            os.environ.pop(k, None)

    def test_prefer_file_overrides_environment(self):
        os.environ["CK_PREF"] = "from-env"
        try:
            with tempfile.TemporaryDirectory() as d:
                envf = Path(d) / "test.env"
                envf.write_text("CK_PREF=from-file\n")
                # Default: environment wins.
                self.assertTrue(envfile.source_env_file(envf))
                self.assertEqual(os.environ["CK_PREF"], "from-env")
                # prefer_file=True: file wins.
                self.assertTrue(
                    envfile.source_env_file(envf, prefer_file=True))
                self.assertEqual(os.environ["CK_PREF"], "from-file")
        finally:
            os.environ.pop("CK_PREF", None)

    def test_resolve_credential_precedence(self):
        os.environ.pop("CK_CRED", None)
        self.assertEqual(envfile.resolve_credential("flagval", "CK_CRED", "--x"),
                         "flagval")
        os.environ["CK_CRED"] = "envval"
        self.assertEqual(envfile.resolve_credential(None, "CK_CRED", "--x"),
                         "envval")
        os.environ.pop("CK_CRED", None)
        with self.assertRaises(SystemExit):
            envfile.resolve_credential(None, "CK_CRED", "--x")


# --------------------------------------------------------------------------
# Equivalence proof for the hoisted hand-rolled parser.
#
# `envfile.load_env_file` replaces a hand-rolled KEY=VALUE parser that was
# triplicated in schwab-api/login.py, schwab-web/login.py and
# fidelity-web/download.py. `_oracle_load` is an INDEPENDENT verbatim copy
# of that pre-refactor parser; the tests below assert the shared helper is
# byte-identical to it, and that it does NOT match a bash source
# (`source_env_file`) on shapes a real credential file can hit — which is
# why the parser was hoisted verbatim rather than migrated to bash.
# --------------------------------------------------------------------------

_OVR = frozenset({"CRED_A", "CRED_B"})

# (name, content). Realistic + edge shapes a credential env file can carry.
_GOOD_FIXTURES = [
    ("plain", "CRED_A=value\n"),
    ("double_quoted", 'CRED_A="value"\n'),
    ("single_quoted", "CRED_A='value'\n"),
    ("export_prefix", "export CRED_A=value\n"),
    ("blank_and_comment", "\n   \n# a comment\nCRED_A=value\n"),
    ("empty_value", "CRED_A=\n"),
    ("empty_value_quoted", 'CRED_A=""\n'),
    ("eq_in_value", "CRED_A=a=b=c\n"),
    ("hash_in_value_unquoted", "CRED_A=ab#cd\n"),
    ("hash_in_value_quoted", 'CRED_A="ab#cd"\n'),
    ("dollar_double_quoted", 'CRED_A="abc$def"\n'),
    ("dollar_single_quoted", "CRED_A='abc$def'\n"),
    ("dollar_unquoted", "CRED_A=abc$def\n"),
    ("backtick_double_quoted", 'CRED_A="a`echo x`b"\n'),
    ("bang_double_quoted", 'CRED_A="abc!def"\n'),
    ("spaces_in_value_quoted", 'CRED_A="a b c"\n'),
    ("spaces_in_value_unquoted", "CRED_A=a b c\n"),
    ("spaces_around_eq", "CRED_A = value\n"),
    ("trailing_inline_comment", "CRED_A=value # note\n"),
    ("crlf", "CRED_A=value\r\n"),
    ("indented", "    CRED_A=value\n"),
    ("single_side_quote", 'CRED_A="foo\n'),  # unmatched -> left intact
    ("override_and_plain", "CRED_A=fromfile\nPLAIN=alsofile\n"),
    ("non_override_only", "PLAIN=fromfile\n"),
    ("multi_override", "CRED_A=aaa\nCRED_B=bbb\n"),
]

_MALFORMED_FIXTURES = [
    ("no_equals", "CRED_A\n"),
    ("empty_key", "=value\n"),
    ("empty_key_after_export", "export =value\n"),
]

_BASE_ENVS = [
    {},
    {"CRED_A": "hostval", "PLAIN": "hostplain"},
]


def _oracle_load(path, override_vars, base_env):
    """Independent verbatim copy of the pre-refactor hand-rolled parser.
    Returns the resulting env mapping (does not touch os.environ)."""
    env = dict(base_env)
    with open(path, "r", encoding="utf-8") as fh:
        for lineno, raw in enumerate(fh, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("export "):
                line = line[len("export "):]
            if "=" not in line:
                raise SystemExit(
                    f"env file {path}:{lineno}: not a KEY=VALUE line: "
                    f"{raw.rstrip()!r}"
                )
            key, _, value = line.partition("=")
            key = key.strip()
            v = value.strip()
            if len(v) >= 2 and v[0] == v[-1] and v[0] in ("'", '"'):
                v = v[1:-1]
            if not key:
                raise SystemExit(f"env file {path}:{lineno}: empty key")
            if key in override_vars:
                env[key] = v
            else:
                env.setdefault(key, v)
    return env


class HandRolledEnvFileEquivalenceTest(unittest.TestCase):
    """Proves envfile.load_env_file == the parser it replaced, and that a
    bash source would diverge on realistic credential shapes."""

    def _run_helper(self, path, override_vars, base_env, warn):
        import logging
        saved = dict(os.environ)
        try:
            os.environ.clear()
            os.environ.update(base_env)
            envfile.load_env_file(path, override_vars,
                                  logger=logging.getLogger("test-envfile"),
                                  warn_on_override=warn)
            return dict(os.environ)
        finally:
            os.environ.clear()
            os.environ.update(saved)

    def test_helper_matches_oracle_on_good_fixtures(self):
        with tempfile.TemporaryDirectory() as d:
            for name, content in _GOOD_FIXTURES:
                p = Path(d) / f"{name}.env"
                p.write_text(content)
                for base in _BASE_ENVS:
                    expected = _oracle_load(p, _OVR, base)
                    # warn flag must not change the resolved mapping.
                    for warn in (False, True):
                        got = self._run_helper(p, _OVR, base, warn)
                        self.assertEqual(
                            got, expected,
                            msg=f"{name} base={base} warn={warn}")

    def test_helper_matches_oracle_on_malformed(self):
        with tempfile.TemporaryDirectory() as d:
            for name, content in _MALFORMED_FIXTURES:
                p = Path(d) / f"{name}.env"
                p.write_text(content)
                with self.assertRaises(SystemExit, msg=f"oracle {name}"):
                    _oracle_load(p, _OVR, {})
                with self.assertRaises(SystemExit, msg=f"helper {name}"):
                    self._run_helper(p, _OVR, {}, False)

    def test_override_semantics(self):
        # Override keys: file wins over an inherited host value.
        # Non-override keys: setdefault (host value wins).
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "ovr.env"
            p.write_text("CRED_A=fromfile\nPLAIN=fromfile\n")
            got = self._run_helper(
                p, _OVR, {"CRED_A": "hostcred", "PLAIN": "hostplain"}, True)
            self.assertEqual(got["CRED_A"], "fromfile")   # file wins
            self.assertEqual(got["PLAIN"], "hostplain")   # env wins

    def test_bash_source_would_diverge_on_credential_shapes(self):
        # The decisive evidence: for shapes a real credential file can
        # hit, a bash source (source_env_file) yields a DIFFERENT value
        # than the byte-preserving hand parser. If these ever stop
        # diverging, revisit whether a bash source could replace it.
        diverging = {
            "dollar_double_quoted": 'CRED_A="abc$def"\n',   # $def expands
            "dollar_unquoted": "CRED_A=abc$def\n",
            "backtick_double_quoted": 'CRED_A="a`echo x`b"\n',  # cmd subst
            "crlf": "CRED_A=value\r\n",                     # keeps \r
            "trailing_inline_comment": "CRED_A=value # note\n",
        }
        with tempfile.TemporaryDirectory() as d:
            for name, content in diverging.items():
                p = Path(d) / f"{name}.env"
                p.write_text(content)
                hand = self._run_helper(p, _OVR, {}, False).get("CRED_A")
                saved = dict(os.environ)
                try:
                    os.environ.clear()
                    os.environ["PATH"] = saved.get("PATH", "/usr/bin:/bin")
                    envfile.source_env_file(p, prefer_file=True)
                    bash = os.environ.get("CRED_A")
                finally:
                    os.environ.clear()
                    os.environ.update(saved)
                self.assertNotEqual(
                    hand, bash,
                    msg=f"{name}: expected divergence but both = {hand!r}")

    def test_bash_source_matches_on_single_quoted_and_plain(self):
        # Following the documented single-quote convention, both agree.
        agreeing = {
            "plain": "CRED_A=value\n",
            "single_quoted": "CRED_A='value'\n",
            "dollar_single_quoted": "CRED_A='abc$def'\n",
            "double_quoted_plain": 'CRED_A="value"\n',
        }
        with tempfile.TemporaryDirectory() as d:
            for name, content in agreeing.items():
                p = Path(d) / f"{name}.env"
                p.write_text(content)
                hand = self._run_helper(p, _OVR, {}, False).get("CRED_A")
                saved = dict(os.environ)
                try:
                    os.environ.clear()
                    os.environ["PATH"] = saved.get("PATH", "/usr/bin:/bin")
                    envfile.source_env_file(p, prefer_file=True)
                    bash = os.environ.get("CRED_A")
                finally:
                    os.environ.clear()
                    os.environ.update(saved)
                self.assertEqual(hand, bash, msg=name)


def _build_parser():
    # The bounded download group, through the public entry point.
    p = argparse.ArgumentParser()
    cli.add_standard_args(p, verb="download")
    return p


class LookbackTest(unittest.TestCase):
    """The window contract: one --lookback flag naming a start, taking
    either a preset or a date, with the window always running to today."""

    def test_default_with_no_flags(self):
        from datetime import timedelta
        ns = _build_parser().parse_args([])
        since, until = cli.resolve_lookback(ns)
        self.assertEqual(until, cli._today_utc())
        self.assertEqual(until - since,
                         timedelta(days=cli.DEFAULT_LOOKBACK_DAYS))

    def test_lookback_presets(self):
        from datetime import timedelta
        for preset, days in [("1w", 7), ("4w", 28), ("3m", 90), ("6m", 180),
                             ("1y", 365), ("2y", 730), ("5y", 1825)]:
            ns = _build_parser().parse_args(["--lookback", preset])
            since, until = cli.resolve_lookback(ns)
            self.assertEqual(until - since, timedelta(days=days), preset)

    def test_lookback_all_is_30_years(self):
        from datetime import timedelta
        ns = _build_parser().parse_args(["--lookback", "all"])
        since, until = cli.resolve_lookback(ns)
        self.assertEqual(until - since, timedelta(days=365 * 30))

    def test_lookback_accepts_an_iso_date(self):
        from datetime import date
        ns = _build_parser().parse_args(["--lookback", "2020-01-01"])
        since, until = cli.resolve_lookback(ns)
        self.assertEqual(since, date(2020, 1, 1))
        self.assertEqual(until, cli._today_utc())

    def test_until_is_always_today(self):
        # There is no upper-bound flag: every window ends now, so a
        # collector never has to reason about a stale right edge.
        for argv in ([], ["--lookback", "all"], ["--lookback", "2020-01-01"]):
            _, until = cli.resolve_lookback(_build_parser().parse_args(argv))
            self.assertEqual(until, cli._today_utc(), argv)

    def test_rejects_a_value_that_is_neither_preset_nor_date(self):
        # argparse turns an ArgumentTypeError into exit 2.
        with self.assertRaises(SystemExit):
            _build_parser().parse_args(["--lookback", "last-tuesday"])

    def test_rejects_a_start_in_the_future(self):
        ns = _build_parser().parse_args(["--lookback", "2999-01-01"])
        with self.assertRaises(SystemExit):
            cli.resolve_lookback(ns)

    def test_default_days_override(self):
        from datetime import timedelta
        ns = _build_parser().parse_args([])
        since, until = cli.resolve_lookback(ns, default_days=30)
        self.assertEqual(until - since, timedelta(days=30))


class DefaultDataRootTest(unittest.TestCase):
    def _resolve(self, xdg):
        saved = os.environ.get("XDG_DATA_HOME")
        try:
            if xdg is None:
                os.environ.pop("XDG_DATA_HOME", None)
            else:
                os.environ["XDG_DATA_HOME"] = xdg
            return cli.default_data_root()
        finally:
            if saved is None:
                os.environ.pop("XDG_DATA_HOME", None)
            else:
                os.environ["XDG_DATA_HOME"] = saved

    def test_falls_back_to_local_share_when_unset(self):
        self.assertEqual(self._resolve(None), Path.home() / ".local" / "share" / "wealthdb")

    def test_honours_xdg_data_home(self):
        self.assertEqual(self._resolve("/custom/xdg"), Path("/custom/xdg") / "wealthdb")


class ParseTest(unittest.TestCase):
    def test_iso_date_to_epoch_bare_date(self):
        from datetime import datetime as _dt, timezone as _tz
        expected = int(_dt(2026, 1, 1, tzinfo=_tz.utc).timestamp())
        self.assertEqual(parse.iso_date_to_epoch("2026-01-01"), expected)

    def test_iso_date_to_epoch_with_trailing_time(self):
        from datetime import datetime as _dt, timezone as _tz
        expected = int(_dt(2026, 5, 29, tzinfo=_tz.utc).timestamp())
        # Should produce the same midnight epoch regardless of trailing time.
        for inp in ("2026-05-29", "2026-05-29T07:15:30",
                    "2026-05-29T07:15:30.123456", "2026-05-29 noise"):
            self.assertEqual(parse.iso_date_to_epoch(inp), expected, inp)

    def test_iso_date_to_epoch_none_on_falsy_or_bad(self):
        for bad in (None, "", "   ", "not-a-date", "20260101",
                    "2026/01/01", "2026-13-99"):
            self.assertIsNone(parse.iso_date_to_epoch(bad), bad)


class SessionTest(unittest.TestCase):
    def test_save_and_load_roundtrip(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "sub" / "state.json"
            payload = {
                "saved_at": "2026-05-29T00:00:00+00:00",
                "cookies": [{"name": "sid", "value": "x"}],
            }
            session.save_state(p, payload)
            self.assertTrue(p.is_file())
            self.assertEqual(p.stat().st_mode & 0o777, 0o600)
            self.assertEqual(session.load_state(p), payload)
            # tmp sibling cleaned up by rename
            self.assertFalse((Path(d) / "sub" / "state.json.tmp").exists())

    def test_save_state_mkdir_parents(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "a" / "b" / "c" / "state.json"
            session.save_state(p, {"k": 1})
            self.assertTrue(p.is_file())

    def test_load_missing_returns_none(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertIsNone(session.load_state(Path(d) / "nope.json"))

    def test_load_invalid_returns_none(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "bad.json"
            p.write_text("{not valid json")
            self.assertIsNone(session.load_state(p))

    def test_secure_file_tightens_perms(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "f"
            p.write_text("hi")
            p.chmod(0o644)
            self.assertTrue(session.secure_file(p))
            self.assertEqual(p.stat().st_mode & 0o777, 0o600)

    def test_iso_now_format(self):
        s = session.iso_now()
        self.assertRegex(
            s, r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\+00:00$")


COLLECTORS = Path(__file__).resolve().parents[3] / "collectors"

# The library modules a collector reaches for by name. A reference to one it
# never imported is a NameError, and collectors call these from inside
# best-effort handlers that swallow exceptions at DEBUG — so the failure is
# silent, and what it takes down is a diagnostic nobody notices is missing.
_KIT_MODULES = frozenset({
    "bronze", "cli", "debugcap", "envfile", "launch", "parse", "pdftotext",
    "prune", "session", "silver",
})


@unittest.skipUnless(COLLECTORS.is_dir(), "collectors/ not present")
class CollectorImportsTest(unittest.TestCase):
    """Every collectorkit module a collector names is one it imported.

    There is no Python linter in the build, so an undefined name reaches a
    real run. This catches the one shape that recurs: a module using
    `debugcap.x` (or a sibling) that never imported it.
    """

    def test_every_referenced_kit_module_is_imported(self):
        import ast
        scanned = 0
        for path in sorted(COLLECTORS.glob("*/*.py")):
            tree = ast.parse(path.read_text(encoding="utf-8"), str(path))
            bound, used = set(), set()
            for node in ast.walk(tree):
                if isinstance(node, ast.Import):
                    bound.update((a.asname or a.name).split(".")[0]
                                 for a in node.names)
                elif isinstance(node, ast.ImportFrom):
                    bound.update(a.asname or a.name for a in node.names)
                elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    bound.add(node.name)
                elif isinstance(node, ast.Name) and isinstance(node.ctx,
                                                               ast.Store):
                    bound.add(node.id)
                elif (isinstance(node, ast.Attribute)
                        and isinstance(node.value, ast.Name)
                        and node.value.id in _KIT_MODULES):
                    used.add(node.value.id)
            scanned += 1
            missing = sorted(used - bound)
            self.assertFalse(
                missing,
                f"{path} calls {missing} without importing it — "
                f"a NameError inside a best-effort handler")
        self.assertTrue(scanned, "no collector sources found to scan")


if __name__ == "__main__":
    unittest.main()


GENERATIONS_SQL = (
    "CREATE TABLE parser_generations (\n"
    "    scope      TEXT    NOT NULL PRIMARY KEY,\n"
    "    generation TEXT    NOT NULL,\n"
    "    stamped_at INTEGER NOT NULL);\n"
)


class ParserGenerationTest(unittest.TestCase):
    """What a document-fed pass asks before it re-derives its rows: did the
    parser that wrote them differ from the one running now?"""

    def setUp(self):
        self.conn = sqlite3.connect(":memory:")
        self.conn.executescript(GENERATIONS_SQL)

    def tearDown(self):
        self.conn.close()

    def test_an_unstamped_scope_reads_as_stale(self):
        # This is what heals a silver DB predating the stamp: its rows came
        # from a generation it never recorded, so they are re-derived once.
        self.assertTrue(silver.stale_generation(self.conn, "statement", "g1"))

    def test_a_stamped_scope_stays_current_until_the_parser_moves(self):
        silver.stamp_generation(self.conn, "statement", "g1")
        self.assertFalse(silver.stale_generation(self.conn, "statement", "g1"))
        self.assertTrue(silver.stale_generation(self.conn, "statement", "g2"))

    def test_one_scope_does_not_vouch_for_another(self):
        silver.stamp_generation(self.conn, "statement", "g1")
        self.assertTrue(silver.stale_generation(self.conn, "supplied", "g1"))

    def test_re_stamping_replaces_rather_than_accumulates(self):
        silver.stamp_generation(self.conn, "statement", "g1")
        silver.stamp_generation(self.conn, "statement", "g2")
        self.assertEqual(
            [("g2",)],
            self.conn.execute(
                "SELECT generation FROM parser_generations").fetchall())
