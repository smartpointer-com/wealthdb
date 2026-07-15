"""Unit tests for collectorkit.launch — the shared browser launch
hardening, plus a repo-wide guard that every collector launch actually
applies it. Stdlib unittest, matching the rest of the collectorkit suite.

The guard is the point of the module: the prefs/args only bound a profile's
disk growth if *no* launch site is missed, so a new collector (or a new verb
on an existing one) that opens a browser without the shared helper fails
here rather than silently regrowing a cache next to the session cookie.
"""
import ast
import unittest
from pathlib import Path

from collectorkit import launch

# tests/ -> collectorkit/ -> shared/ -> repo root
COLLECTORS = Path(__file__).resolve().parents[3] / "collectors"


class FirefoxPrefsTest(unittest.TestCase):
    def test_disk_cache_disabled(self):
        prefs = launch.firefox_prefs()
        self.assertIs(prefs["browser.cache.disk.enable"], False)
        self.assertEqual(prefs["browser.cache.disk.capacity"], 0)
        self.assertIs(prefs["browser.cache.disk.smart_size.enabled"], False)

    def test_memory_cache_stays_enabled(self):
        # Within-run caching is the whole benefit and never reaches disk.
        self.assertIs(
            launch.firefox_prefs()["browser.cache.memory.enable"], True)

    def test_history_and_favicons_disabled(self):
        prefs = launch.firefox_prefs()
        self.assertIs(prefs["places.history.enabled"], False)
        self.assertIs(prefs["browser.chrome.site_icons"], False)

    def test_telemetry_persistence_disabled(self):
        prefs = launch.firefox_prefs()
        self.assertIs(prefs["datareporting.policy.dataSubmissionEnabled"], False)
        self.assertIs(prefs["datareporting.healthreport.uploadEnabled"], False)
        self.assertIs(prefs["toolkit.telemetry.archive.enabled"], False)

    def test_password_manager_disabled(self):
        # Regression: a saved credential autofilling on top of a collector's
        # programmatic fill doubled the password field and broke login.
        prefs = launch.firefox_prefs()
        self.assertIs(prefs["signon.rememberSignons"], False)
        self.assertIs(prefs["signon.autofillForms"], False)
        self.assertIs(prefs["signon.generation.enabled"], False)
        self.assertIs(
            prefs["signon.management.page.breach-alerts.enabled"], False)

    def test_indexeddb_untouched(self):
        # storage/ is app/session state financial SPAs log in against, not an
        # HTTP cache. Nothing here may disable or bound it.
        for key in launch.firefox_prefs():
            self.assertNotIn("indexedDB", key)
            self.assertNotIn("storage.default", key)

    def test_overrides_win(self):
        prefs = launch.firefox_prefs(**{"browser.cache.memory.enable": False})
        self.assertIs(prefs["browser.cache.memory.enable"], False)

    def test_returns_fresh_copy(self):
        launch.firefox_prefs()["browser.cache.disk.enable"] = True
        self.assertIs(
            launch.firefox_prefs()["browser.cache.disk.enable"], False)


class ChromiumArgsTest(unittest.TestCase):
    def test_caches_pinned_not_zero(self):
        # A Chromium cache size of 0 means "pick a default", so the caches
        # are pinned to 1 byte instead.
        args = launch.chromium_args()
        self.assertIn("--disk-cache-size=1", args)
        self.assertIn("--media-cache-size=1", args)
        self.assertIn("--disable-gpu-shader-disk-cache", args)
        self.assertNotIn("--disk-cache-size=0", args)

    def test_extra_args_appended(self):
        args = launch.chromium_args("--no-sandbox")
        self.assertEqual(args[-1], "--no-sandbox")
        self.assertIn("--disk-cache-size=1", args)

    def test_returns_fresh_list(self):
        launch.chromium_args().append("--mutated")
        self.assertNotIn("--mutated", launch.chromium_args())


def _hardened_call(node: ast.Call, kwarg: str, helper: str) -> bool:
    """True when `node` passes `kwarg=launch.<helper>(...)`."""
    for kw in node.keywords:
        if kw.arg != kwarg:
            continue
        v = kw.value
        return (isinstance(v, ast.Call) and isinstance(v.func, ast.Attribute)
                and v.func.attr == helper
                and isinstance(v.func.value, ast.Name)
                and v.func.value.id == "launch")
    return False


def _launch_sites():
    """Every browser-launch call across the collectors, as
    (path, lineno, engine). Collector `tests/` are skipped — they drive stub
    contexts, never a real browser."""
    for path in sorted(COLLECTORS.rglob("*.py")):
        if "tests" in path.parts or "__pycache__" in path.parts:
            continue
        tree = ast.parse(path.read_text(), filename=str(path))
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            f = node.func
            # Camoufox(...) — the patched-Firefox stealth launcher.
            if isinstance(f, ast.Name) and f.id == "Camoufox":
                yield path, node.lineno, "firefox", node
            elif isinstance(f, ast.Attribute):
                # <pw>.firefox.launch_persistent_context(...)
                if f.attr == "launch_persistent_context":
                    yield path, node.lineno, "firefox", node
                # <pw>.chromium.launch(...)
                elif (f.attr == "launch" and isinstance(f.value, ast.Attribute)
                      and f.value.attr == "chromium"):
                    yield path, node.lineno, "chromium", node


@unittest.skipUnless(COLLECTORS.is_dir(), "collectors/ not present")
class LaunchSitesHardenedTest(unittest.TestCase):
    """Every real browser launch in the repo goes through collectorkit.launch."""

    def test_every_launch_site_is_hardened(self):
        unhardened = []
        for path, lineno, engine, node in _launch_sites():
            kwarg, helper = (
                ("firefox_user_prefs", "firefox_prefs") if engine == "firefox"
                else ("args", "chromium_args"))
            if not _hardened_call(node, kwarg, helper):
                rel = path.relative_to(COLLECTORS.parent)
                unhardened.append(f"{rel}:{lineno} ({engine}) "
                                  f"needs {kwarg}=launch.{helper}(...)")
        self.assertEqual(unhardened, [], "unhardened browser launch(es):\n"
                         + "\n".join(unhardened))

    def test_scan_finds_the_known_launch_sites(self):
        # Guards the guard: an AST matcher that silently matches nothing
        # would make the test above vacuously pass.
        sites = list(_launch_sites())
        engines = [e for _, _, e, _ in sites]
        self.assertGreaterEqual(engines.count("firefox"), 14)
        self.assertGreaterEqual(engines.count("chromium"), 5)

    def test_no_collector_redefines_the_pref_set(self):
        # The prefs live in one place; a copy would drift out of sync.
        for path in sorted(COLLECTORS.rglob("*.py")):
            if "__pycache__" in path.parts:
                continue
            text = path.read_text()
            self.assertNotIn("signon.rememberSignons", text,
                             f"{path} inlines a pref set — use "
                             f"collectorkit.launch.firefox_prefs()")


if __name__ == "__main__":
    unittest.main()
