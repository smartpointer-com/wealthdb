"""Unit tests for collectorkit. Stdlib unittest so they run with a bare
`python -m unittest` (and also under pytest in the collector containers)."""
import contextlib
import logging
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
            (root / "20260601T000000Z").write_text("")   # a file, not a run
            (root / "20260529T071530Z_bak").mkdir()       # a look-alike
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

    def test_read_manifest_is_a_json_object_or_none(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "run.json"
            self.assertIsNone(bronze.read_manifest(path))  # absent
            for raw in (b"{", b"[]", b"\xff"):
                path.write_bytes(raw)
                self.assertIsNone(bronze.read_manifest(path))
            path.write_text('{"status": "complete"}')
            self.assertEqual(bronze.read_manifest(path),
                             {"status": "complete"})
            self.assertEqual(bronze.run_status(path), "complete")

    def test_manifest_is_complete(self):
        with tempfile.TemporaryDirectory() as d:
            run = Path(d)
            self.assertFalse(bronze.manifest_is_complete(run))  # no manifest
            (run / "run.json").write_text("{")
            self.assertFalse(bronze.manifest_is_complete(run))  # corrupt
            for meta, want in (('{"status": "complete"}', True),
                               ("{}", True),     # statusless: written last
                               ('{"status": "in-progress"}', False),
                               ('{"status": "dry-run"}', False)):
                (run / "run.json").write_text(meta)
                self.assertIs(bronze.manifest_is_complete(run), want, meta)
            (run / "manifest.json").write_text('{"status": "complete"}')
            (run / "run.json").unlink()
            self.assertTrue(bronze.manifest_is_complete(run, "manifest.json"))

    def test_safe_stem_keeps_one_path_segment(self):
        for value, want in (("7000001", "7000001"),
                            ("07/31/2026", "07-31-2026"),
                            ("EAZ/ELOOE_1", "EAZ-ELOOE_1"),
                            ("../../etc", "etc"),
                            (".hidden", "hidden"),
                            ("a b\tc", "a-b-c"),
                            ("2026-01-31_doc.v2", "2026-01-31_doc.v2"),
                            (12345, "12345")):
            self.assertEqual(bronze.safe_stem(value), want, value)
        self.assertEqual(bronze.safe_stem("..."), "item")
        self.assertEqual(bronze.safe_stem("", fallback="account"), "account")

    def test_log_id_shortens_long_ids_only(self):
        self.assertEqual(bronze.log_id("ACCT-0001"), "ACCT-0001")
        self.assertEqual(bronze.log_id("0123456789abcdef0123456789abcdef"),
                         "01234567…")
        self.assertEqual(bronze.log_id("x" * 12), "x" * 12)
        self.assertEqual(bronze.log_id("a/b" * 5), "a-ba-ba-…")


class EnvFileTest(unittest.TestCase):
    def test_a_syntax_error_names_the_line_and_never_shows_it(self):
        # bash -n quotes the whole line near an unexpected token, so its
        # text would print a secret written beside a stray character.
        shapes = ("export CK_SECRET=SYNTHETIC-VALUE)x\n",
                  "export CK_SECRET='SYNTHETIC-VALUE')\n",
                  "export CK_SECRET=<SYNTHETIC-VALUE>\n")
        with tempfile.TemporaryDirectory() as d:
            envf = Path(d) / "bad.env"
            for shape in shapes:
                envf.write_text("export CK_OK=1\n" + shape)
                with self.assertRaises(ValueError) as caught:
                    envfile.source_env_file(envf)
                message = str(caught.exception)
                self.assertIn("at line 2", message)
                self.assertIn(str(envf), message)
                self.assertNotIn("SYNTHETIC-VALUE", message)
                self.assertNotIn("CK_SECRET", message)

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


@contextlib.contextmanager
def _isolated_environ(**values):
    """os.environ as it was, plus `values`, restored afterwards."""
    saved = dict(os.environ)
    os.environ.update(values)
    try:
        yield
    finally:
        os.environ.clear()
        os.environ.update(saved)


class ReadEnvFileTest(unittest.TestCase):
    """An env file means what bash makes of it: quoting, escapes and
    `$`-expansion are the shell's, never a hand-written parser's."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def _write(self, text, name="test.env"):
        path = self.dir / name
        path.write_text(text)
        return path

    def test_bash_quoting_rules(self):
        path = self._write(
            "# a comment line\n"
            "export CK_EXPORTED=plain\n"
            'CK_SPACES="a b  c"\n'
            "CK_SINGLE='lit $x `y` \\z \"q\"'\n"
            'CK_DOUBLE="dq \\"q\\" \\$lit back\\\\slash \'s\'"\n'
            "CK_UNQUOTED=a\\ b\n"
            "CK_COMMENTED=value # a trailing comment\n"
            "CK_HASH=ab#cd\n"
            "CK_EMPTY=\n"
            "CK_EQUALS=a=b=c\n")
        with _isolated_environ():
            got = envfile.read_env_file(path)
        self.assertEqual(got, {
            "CK_EXPORTED": "plain",
            "CK_SPACES": "a b  c",
            "CK_SINGLE": 'lit $x `y` \\z "q"',
            "CK_DOUBLE": "dq \"q\" $lit back\\slash 's'",
            "CK_UNQUOTED": "a b",
            "CK_COMMENTED": "value",
            "CK_HASH": "ab#cd",
            "CK_EMPTY": "",
            "CK_EQUALS": "a=b=c",
        })

    def test_dollar_expands_against_the_environment(self):
        # Double quotes and bare words expand `$NAME`, as bash does; the
        # file sees the environment it is sourced from.
        path = self._write('CK_DQ="x-$CK_BASE-y"\nCK_BARE=x$CK_UNSET\n'
                           "CK_SQ='x-$CK_BASE-y'\n")
        with _isolated_environ(CK_BASE="base"):
            os.environ.pop("CK_UNSET", None)
            got = envfile.read_env_file(path)
        self.assertEqual(got, {"CK_DQ": "x-base-y", "CK_BARE": "x",
                               "CK_SQ": "x-$CK_BASE-y"})

    def test_returns_only_what_the_file_set_or_changed(self):
        path = self._write("CK_SAME=same\nCK_CHANGED=new\nCK_NEW=1\n"
                           "PATH=/nowhere\nunset CK_GONE\n")
        with _isolated_environ(CK_SAME="same", CK_CHANGED="old",
                               CK_GONE="x", CK_UNTOUCHED="y"):
            got = envfile.read_env_file(path)
        self.assertEqual(got, {"CK_CHANGED": "new", "CK_NEW": "1"})

    def test_output_and_startup_files_do_not_interfere(self):
        # A file that prints cannot corrupt the env read-back, and BASH_ENV
        # cannot make bash run a second file.
        startup = self._write("export CK_FROM_STARTUP=1\n", "startup.sh")
        path = self._write("echo noise\nprintf '\\0\\0'\nCK_A=1\n")
        with _isolated_environ(BASH_ENV=str(startup), ENV=str(startup)):
            got = envfile.read_env_file(path)
        self.assertEqual(got, {"CK_A": "1"})

    def test_a_relative_path_is_the_file_not_a_path_lookup(self):
        path = self._write("CK_REL=1\n", "rel.env")
        cwd = os.getcwd()
        os.chdir(self.dir)
        try:
            with _isolated_environ():
                self.assertEqual(envfile.read_env_file(Path("rel.env")),
                                 {"CK_REL": "1"})
        finally:
            os.chdir(cwd)
        self.assertTrue(path.is_file())

    def test_a_missing_file_raises(self):
        with self.assertRaises(FileNotFoundError):
            envfile.read_env_file(self.dir / "nope.env")

    def test_a_syntax_error_fails_and_never_shows_the_line(self):
        path = self._write("export CK_OK=1\nCK_SECRET='SYNTHETIC-VALUE\n")
        with self.assertRaises(ValueError) as caught:
            envfile.read_env_file(path)
        message = str(caught.exception)
        self.assertIn(str(path), message)
        self.assertIn("not valid bash", message)
        self.assertNotIn("SYNTHETIC-VALUE", message)

    def test_a_sourcing_error_fails_and_never_shows_the_line(self):
        # An unquoted space makes the rest of the value a command; bash
        # names it in its error, and it may be half a password.
        path = self._write("CK_OK=1\nCK_SECRET=SYNTHETIC VALUE-TAIL\n")
        with self.assertRaises(ValueError) as caught:
            envfile.read_env_file(path)
        message = str(caught.exception)
        self.assertIn("at line 2", message)
        self.assertNotIn("VALUE-TAIL", message)
        self.assertNotIn("CK_SECRET", message)


class LoadEnvFileTest(unittest.TestCase):
    """load_env_file: the file wins for the named credential keys, the
    environment wins for the rest."""

    LOG = logging.getLogger("test-envfile")

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "creds.env"

    def tearDown(self):
        self.tmp.cleanup()

    def test_override_semantics(self):
        self.path.write_text("CK_CRED='fromfile$1'\nCK_PLAIN=fromfile\n"
                             "CK_FRESH=fresh\n")
        with _isolated_environ(CK_CRED="hostcred", CK_PLAIN="hostplain"):
            os.environ.pop("CK_FRESH", None)
            envfile.load_env_file(self.path, {"CK_CRED"}, logger=self.LOG)
            self.assertEqual(os.environ["CK_CRED"], "fromfile$1")
            self.assertEqual(os.environ["CK_PLAIN"], "hostplain")
            self.assertEqual(os.environ["CK_FRESH"], "fresh")

    def test_warning_names_lengths_never_values(self):
        self.path.write_text("CK_CRED=SYNTHETIC-FILE\n")
        with _isolated_environ(CK_CRED="SYNTHETIC-HOST-VALUE"), \
                self.assertLogs(self.LOG, "WARNING") as logs:
            envfile.load_env_file(self.path, ("CK_CRED",), logger=self.LOG,
                                  warn_on_override=True)
        text = "\n".join(logs.output)
        self.assertIn("len=20", text)
        self.assertIn("len=14", text)
        self.assertNotIn("SYNTHETIC", text)

    def test_no_warning_when_the_values_agree(self):
        self.path.write_text("CK_CRED=same\n")
        with _isolated_environ(CK_CRED="same"), \
                self.assertNoLogs(self.LOG, "WARNING"):
            envfile.load_env_file(self.path, ("CK_CRED",), logger=self.LOG,
                                  warn_on_override=True)

    def test_a_bash_error_exits_without_the_line(self):
        self.path.write_text("CK_CRED=SYNTHETIC VALUE-TAIL\n")
        with self.assertRaises(SystemExit) as caught:
            envfile.load_env_file(self.path, ("CK_CRED",), logger=self.LOG)
        self.assertIn(str(self.path), str(caught.exception))
        self.assertNotIn("VALUE-TAIL", str(caught.exception))

    def test_a_missing_file_raises(self):
        with self.assertRaises(FileNotFoundError):
            envfile.load_env_file(self.path, (), logger=self.LOG)


class SourceNamedEnvFileTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "named.env"
        self.path.write_text("CK_NAMED=fromfile\n")

    def tearDown(self):
        self.tmp.cleanup()

    def test_the_explicit_file_wins_over_the_environment(self):
        with _isolated_environ(CK_NAMED="host"):
            os.environ.pop("CK_NAMED_ENV_FILE", None)
            self.assertEqual(
                envfile.source_named_env_file(self.path, "CK_NAMED_ENV_FILE"),
                self.path)
            self.assertEqual(os.environ["CK_NAMED"], "fromfile")

    def test_the_env_var_names_the_file(self):
        with _isolated_environ(CK_NAMED_ENV_FILE=str(self.path)):
            os.environ.pop("CK_NAMED", None)
            envfile.source_named_env_file(None, "CK_NAMED_ENV_FILE")
            self.assertEqual(os.environ["CK_NAMED"], "fromfile")

    def test_nothing_named_sources_nothing(self):
        with _isolated_environ():
            os.environ.pop("CK_NAMED_ENV_FILE", None)
            self.assertIsNone(
                envfile.source_named_env_file(None, "CK_NAMED_ENV_FILE"))

    def test_a_named_file_that_is_missing_exits(self):
        with self.assertRaises(SystemExit):
            envfile.source_named_env_file(self.path.with_name("nope.env"),
                                          "CK_NAMED_ENV_FILE")

    def test_a_bash_error_exits(self):
        self.path.write_text("CK_NAMED='unterminated\n")
        with self.assertRaises(SystemExit):
            envfile.source_named_env_file(self.path, "CK_NAMED_ENV_FILE")


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


if __name__ == "__main__":
    unittest.main()
