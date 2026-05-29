"""Unit tests for collectorkit. Stdlib unittest so they run with a bare
`python -m unittest` (and also under pytest in the collector containers)."""
import os
import sqlite3
import tempfile
import unittest
from pathlib import Path

from collectorkit import bronze, envfile, silver

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
        conn.close()

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


if __name__ == "__main__":
    unittest.main()
