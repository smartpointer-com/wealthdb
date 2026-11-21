"""demo/run_tests.py's groups: which tests share a process.

A module with setUpModule runs whole, a class with setUpClass runs
whole, and any other test runs alone. A module that fails to import
still runs, so it fails.
"""

import pathlib
import sys
import tempfile
import textwrap
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import run_tests  # noqa: E402

PACKAGE = "run_tests_groups"
MODULES = {
    "test_a.py": """
        import unittest

        def setUpModule():
            pass

        class A(unittest.TestCase):
            def test_one(self):
                pass

            def test_two(self):
                pass

        class B(unittest.TestCase):
            def test_one(self):
                pass
    """,
    "test_b.py": """
        import unittest

        class Shared(unittest.TestCase):
            @classmethod
            def setUpClass(cls):
                pass

            def test_one(self):
                pass

            def test_two(self):
                pass

        class Plain(unittest.TestCase):
            def test_x(self):
                pass

            def test_y(self):
                pass
    """,
    "test_c.py": """
        import run_tests_groups_missing  # noqa: F401
    """,
}


class Groups(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.top = pathlib.Path(tmp.name)
        pkg = self.top / PACKAGE
        pkg.mkdir()
        (pkg / "__init__.py").write_text("")
        for name, body in MODULES.items():
            (pkg / name).write_text(textwrap.dedent(body))
        self.addCleanup(self.forget)

    def forget(self):
        for name in [m for m in sys.modules if m == PACKAGE or m.startswith(PACKAGE + ".")]:
            del sys.modules[name]
        while str(self.top) in sys.path:
            sys.path.remove(str(self.top))

    def test_each_process_runs_a_module_a_class_or_a_test(self):
        self.assertEqual(run_tests.groups(self.top / PACKAGE, self.top), [
            f"{PACKAGE}.test_a",
            f"{PACKAGE}.test_b.Plain.test_x",
            f"{PACKAGE}.test_b.Plain.test_y",
            f"{PACKAGE}.test_b.Shared",
            f"{PACKAGE}.test_c",
        ])

    def test_the_demo_tests_are_grouped_by_default(self):
        names = run_tests.groups()
        self.assertIn(f"tests.test_run_tests.{type(self).__name__}.{self._testMethodName}", names)
        self.assertEqual(len(names), len(set(names)))


if __name__ == "__main__":
    unittest.main()
