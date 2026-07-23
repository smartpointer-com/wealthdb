"""Unit tests for collectorkit.launch — the shared browser launch
hardening, plus a repo-wide guard that every collector launch actually
applies it. Stdlib unittest, matching the rest of the collectorkit suite.

The guard is the point of the module: the prefs/args only bound a profile's
disk growth if *no* launch site is missed, so a new collector (or a new verb
on an existing one) that opens a browser without the shared helper fails
here rather than silently regrowing a cache next to the session cookie.
"""
import ast
import functools
import os
import re
import stat
import tempfile
import unittest
from pathlib import Path
from unittest import mock

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


class RedirectChmodBestEffortTest(unittest.TestCase):
    def test_eperm_on_bind_mounted_cache_root_does_not_abort(self):
        # In a container the cache root is a host-owned bind mount: the VM
        # file share accepts mkdir/symlink from the container uid but
        # rejects chmod with EPERM. The redirect must survive that — the
        # wrapper's host-side chmod is authoritative.
        with tempfile.TemporaryDirectory() as tmp:
            profile = Path(tmp) / "x-profile"
            profile.mkdir()
            root = Path(tmp) / "cacheroot"
            real_chmod = os.chmod

            def chmod_eperm(path, mode, **kw):
                if Path(path) == root or Path(path).parent == root:
                    raise PermissionError(1, "Operation not permitted", str(path))
                real_chmod(path, mode, **kw)

            with mock.patch.object(launch.os, "chmod", side_effect=chmod_eperm):
                target = launch.redirect_startup_cache(profile, cache_root=root)
            link = profile / "startupCache"
            self.assertTrue(link.is_symlink())
            self.assertEqual(os.readlink(link), str(target))
            self.assertTrue(target.is_dir())


class PasswordManagerOnTest(unittest.TestCase):
    def test_reenables_over_the_shared_set(self):
        # The by-hand login profiles layer this back over the shared set,
        # which turns the password manager off for the driven ones.
        prefs = launch.firefox_prefs(**launch.PASSWORD_MANAGER_ON)
        self.assertIs(prefs["signon.rememberSignons"], True)
        self.assertIs(prefs["signon.autofillForms"], True)

    def test_overrides_rather_than_adds(self):
        # Every key it flips must already exist in the shared set, so it is
        # a genuine override and can't silently introduce a new pref.
        for key in launch.PASSWORD_MANAGER_ON:
            self.assertIn(key, launch.FIREFOX_PREFS)


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


class FirefoxUserJsTest(unittest.TestCase):
    def _prefs(self, text: str) -> dict[str, str]:
        return dict(re.findall(r'^user_pref\("([^"]+)", (.+)\);$', text,
                               re.M))

    def test_js_literals_not_python_reprs(self):
        # `False` / `True` in a user.js is a syntax error Firefox drops the
        # whole line for — silently reverting the pref to its default.
        prefs = self._prefs(launch.firefox_user_js())
        self.assertEqual(prefs["browser.cache.disk.enable"], "false")
        self.assertEqual(prefs["browser.cache.memory.enable"], "true")
        self.assertNotIn("False", launch.firefox_user_js())
        self.assertNotIn("True", launch.firefox_user_js())

    def test_ints_bare_and_strings_quoted(self):
        prefs = self._prefs(launch.firefox_user_js(**{"a.str": "/tmp/x"}))
        self.assertEqual(prefs["browser.cache.disk.capacity"], "0")
        self.assertEqual(prefs["a.str"], '"/tmp/x"')

    def test_string_escaping(self):
        prefs = self._prefs(launch.firefox_user_js(**{"a.s": 'a"b\\c'}))
        self.assertEqual(prefs["a.s"], r'"a\"b\\c"')

    def test_carries_the_shared_set(self):
        prefs = self._prefs(launch.firefox_user_js())
        for key in launch.FIREFOX_PREFS:
            self.assertIn(key, prefs)

    def test_override_replaces_rather_than_duplicates(self):
        # Two lines for one key is the drift this renderer exists to stop;
        # the last would silently win.
        text = launch.firefox_user_js(
            **{"datareporting.policy.dataSubmissionEnabled": False})
        self.assertEqual(
            text.count('user_pref("datareporting.policy.dataSubmissionEnabled"'),
            1)

    def test_overrides_win(self):
        prefs = self._prefs(launch.firefox_user_js(
            **{"browser.cache.memory.enable": False}))
        self.assertEqual(prefs["browser.cache.memory.enable"], "false")

    def test_every_line_is_a_pref_or_comment(self):
        for line in launch.firefox_user_js().splitlines():
            self.assertTrue(line.startswith(("user_pref(", "//")),
                            f"unexpected user.js line: {line!r}")


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


def _collector_py_files(*, skip_tests):
    """First-party collector .py files. Vendored `.venv` trees and bytecode
    caches are never first-party and are always skipped — walking a collector's
    `.venv` would parse thousands of site-packages files (and could match a
    launch call inside Playwright itself). `skip_tests` additionally drops
    collector `tests/`, which drive stub contexts rather than a real browser."""
    for path in sorted(COLLECTORS.rglob("*.py")):
        parts = path.parts
        if ".venv" in parts or "__pycache__" in parts:
            continue
        if skip_tests and "tests" in parts:
            continue
        yield path


@functools.cache
def _launch_sites():
    """Every browser-launch call across the collectors, as a tuple of
    (path, lineno, engine, node). Cached: the scan parses the whole
    first-party tree, and several guards below share the one result."""
    sites = []
    for path in _collector_py_files(skip_tests=True):
        tree = ast.parse(path.read_text(), filename=str(path))
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            f = node.func
            # Camoufox(...) — the patched-Firefox stealth launcher.
            if isinstance(f, ast.Name) and f.id == "Camoufox":
                sites.append((path, node.lineno, "firefox", node))
            elif isinstance(f, ast.Attribute):
                # <pw>.firefox.launch_persistent_context(...)
                if f.attr == "launch_persistent_context":
                    sites.append((path, node.lineno, "firefox", node))
                # <pw>.chromium.launch(...)
                elif (f.attr == "launch" and isinstance(f.value, ast.Attribute)
                      and f.value.attr == "chromium"):
                    sites.append((path, node.lineno, "chromium", node))
    return tuple(sites)


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
        # The prefs live in one place; a copy would drift out of sync. Tests
        # are included: a test that hardcodes the pref set drifts too.
        for path in _collector_py_files(skip_tests=False):
            text = path.read_text()
            self.assertNotIn("signon.rememberSignons", text,
                             f"{path} inlines a pref set — use "
                             f"collectorkit.launch.firefox_prefs()")


@unittest.skipUnless(COLLECTORS.is_dir(), "collectors/ not present")
class EntrypointProfilesHardenedTest(unittest.TestCase):
    """Profiles seeded from bash, not Python.

    A browser started as a plain binary from an entrypoint takes no
    Playwright prefs and is invisible to the AST scan above, leaving its
    disk cache free to grow unbounded next to the session cookie. Its
    prefs have to arrive as a rendered `user.js`, so any entrypoint that
    hand-writes prefs, or starts a browser without seeding the profile
    from the shared renderer, fails here.
    """

    def _entrypoints(self):
        return sorted(COLLECTORS.rglob("entrypoint.sh"))

    def test_entrypoints_exist(self):
        # Guards the guard: a rename would otherwise make this vacuous.
        self.assertTrue(self._entrypoints())

    def test_no_entrypoint_hand_writes_prefs(self):
        # A mention of user.js in a comment is fine; a shell redirect into
        # one (`cat > "$FXPROFILE/user.js"`) is the drift being blocked.
        redirect = re.compile(r">\s*[\"']?\S*user\.js")
        for path in self._entrypoints():
            text = path.read_text()
            self.assertNotIn("user_pref(", text,
                             f"{path} hand-writes prefs — render them from "
                             f"collectorkit.launch.firefox_user_js()")
            self.assertIsNone(redirect.search(text),
                              f"{path} writes a user.js — render it from "
                              f"collectorkit.launch.firefox_user_js()")

    def test_browser_launching_entrypoint_seeds_from_the_renderer(self):
        # A `firefox`/`chrome` binary started from bash gets its profile
        # from a seeder that goes through the shared renderer.
        for path in self._entrypoints():
            text = path.read_text()
            launches = [ln.strip() for ln in text.splitlines()
                        if re.match(r"^\s*(firefox|google-chrome|chromium)\s",
                                    ln)]
            if not launches:
                continue
            seeder = path.parent / "fxprofile.py"
            self.assertTrue(
                seeder.is_file(),
                f"{path} starts a browser but {seeder.name} is missing")
            self.assertIn(
                "firefox_user_js", seeder.read_text(),
                f"{seeder} must render prefs via "
                f"collectorkit.launch.firefox_user_js()")
            # An actual invocation, not a mention in a comment — matching
            # the bare name would let a stray comment satisfy the guard.
            invoked = re.search(rf"^\s*python3?\s+\S*{re.escape(seeder.name)}",
                                text, re.M)
            self.assertIsNotNone(
                invoked,
                f"{path} starts a browser without invoking {seeder.name}")


class StartupCacheRootTest(unittest.TestCase):
    def test_env_override_wins(self):
        with mock.patch.dict(os.environ,
                             {"WEALTHDB_STARTUPCACHE_DIR": "/cache/startupcache"}):
            self.assertEqual(launch.startup_cache_root(),
                             Path("/cache/startupcache"))

    def test_xdg_cache_home_default(self):
        with mock.patch.dict(os.environ, {"XDG_CACHE_HOME": "/x/cache"}):
            os.environ.pop("WEALTHDB_STARTUPCACHE_DIR", None)
            self.assertEqual(launch.startup_cache_root(),
                             Path("/x/cache/wealthdb/startupcache"))

    def test_home_fallback(self):
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("WEALTHDB_STARTUPCACHE_DIR", None)
            os.environ.pop("XDG_CACHE_HOME", None)
            self.assertEqual(launch.startup_cache_root(),
                             Path.home() / ".cache" / "wealthdb" / "startupcache")


class RedirectStartupCacheTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        self.profile = self.root / "carta-profile"
        self.profile.mkdir()
        self.cache = self.root / "cache"

    def tearDown(self):
        self._tmp.cleanup()

    def _link(self):
        return self.profile / "startupCache"

    def test_creates_symlink_under_cache_root(self):
        target = launch.redirect_startup_cache(self.profile, self.cache)
        self.assertEqual(target, self.cache / "carta-profile")
        self.assertTrue(self._link().is_symlink())
        self.assertEqual(os.readlink(self._link()), str(target))
        self.assertTrue(target.is_dir())

    def test_replaces_preexisting_real_dir(self):
        # The current on-disk state: a real startupCache/ dir with content.
        real = self._link()
        real.mkdir()
        (real / "cache.bin").write_bytes(b"x" * 32)
        launch.redirect_startup_cache(self.profile, self.cache)
        self.assertTrue(self._link().is_symlink())
        # The stale bytes are gone from the profile — the fresh target is empty.
        self.assertEqual(list((self.cache / "carta-profile").iterdir()), [])

    def test_idempotent_when_already_correct(self):
        target1 = launch.redirect_startup_cache(self.profile, self.cache)
        # A file in the target would be lost to an unwanted rmtree/relink.
        (target1 / "keep").write_text("x")
        target2 = launch.redirect_startup_cache(self.profile, self.cache)
        self.assertEqual(target1, target2)
        self.assertTrue((target2 / "keep").exists())
        self.assertTrue(self._link().is_symlink())

    def test_repoints_a_wrong_symlink(self):
        (self._link()).symlink_to(self.root / "elsewhere")
        target = launch.redirect_startup_cache(self.profile, self.cache)
        self.assertEqual(os.readlink(self._link()), str(target))
        self.assertEqual(target, self.cache / "carta-profile")

    def test_two_profiles_key_to_two_targets(self):
        p2 = self.root / "angellist-fxprofile"
        p2.mkdir()
        t1 = launch.redirect_startup_cache(self.profile, self.cache)
        t2 = launch.redirect_startup_cache(p2, self.cache)
        self.assertEqual(t1, self.cache / "carta-profile")
        self.assertEqual(t2, self.cache / "angellist-fxprofile")
        self.assertNotEqual(t1, t2)

    def test_cache_root_and_target_are_0700(self):
        target = launch.redirect_startup_cache(self.profile, self.cache)
        self.assertEqual(stat.S_IMODE(self.cache.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o700)

    def test_default_root_from_env(self):
        # No cache_root passed → falls back to startup_cache_root() (env).
        with mock.patch.dict(os.environ,
                             {"WEALTHDB_STARTUPCACHE_DIR": str(self.cache)}):
            target = launch.redirect_startup_cache(self.profile)
        self.assertEqual(target, self.cache / "carta-profile")


class PrepareProfileDirTest(unittest.TestCase):
    def test_creates_chmods_and_redirects(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            profile = root / "schwab-web-profile"  # does not exist yet
            cache = root / "cache"
            returned = launch.prepare_profile_dir(profile, cache_root=cache)
            self.assertEqual(returned, profile)
            self.assertTrue(profile.is_dir())
            self.assertEqual(stat.S_IMODE(profile.stat().st_mode), 0o700)
            link = profile / "startupCache"
            self.assertTrue(link.is_symlink())
            self.assertEqual(os.readlink(link),
                             str(cache / "schwab-web-profile"))

    def test_session_state_is_left_in_place(self):
        # Only startupCache moves; cookies/keys/prefs stay in the profile.
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            profile = root / "carta-profile"
            profile.mkdir()
            for name in ("cookies.sqlite", "key4.db", "cert9.db", "prefs.js"):
                (profile / name).write_bytes(b"secret")
            launch.prepare_profile_dir(profile, cache_root=root / "cache")
            for name in ("cookies.sqlite", "key4.db", "cert9.db", "prefs.js"):
                f = profile / name
                self.assertTrue(f.is_file() and not f.is_symlink(),
                                f"{name} must not be moved or symlinked")


_SECRETS_PROFILE = re.compile(
    r'DEFAULT_PROFILE_DIR\s*=\s*Path\(\s*["\']/secrets/')


@functools.cache
def _persistent_profile_files():
    """Collector files that seed a persistent (~/.secrets) browser profile,
    as a tuple of (path, text). Cached; shared by the guards below."""
    firefox = {p for p, _l, e, _n in _launch_sites() if e == "firefox"}
    files = []
    for path in _collector_py_files(skip_tests=True):
        text = path.read_text()
        declares_secrets_profile = bool(_SECRETS_PROFILE.search(text))
        # A launch against a profile it does not build with mkdtemp — the
        # ephemeral tempdir case is the one exception (see class docstring).
        persistent_launch = path in firefox and "mkdtemp" not in text
        if declares_secrets_profile or persistent_launch:
            files.append((path, text))
    return tuple(files)


@unittest.skipUnless(COLLECTORS.is_dir(), "collectors/ not present")
class PersistentProfileStartupCacheTest(unittest.TestCase):
    """Every persistent browser profile relocates its startupCache.

    A persistent profile lives under ~/.secrets and must not accrue
    regenerable cache next to the session cookie. startupCache is the one
    cache no pref disables, so each profile-prep site routes the profile
    through collectorkit.launch (prepare_profile_dir / redirect_startup_cache),
    which symlinks it out to the cache root. A new browser collector that
    seeds a /secrets profile without the redirect fails here.

    An ephemeral `tempfile.mkdtemp` profile (angellist's passive download) is
    exempt: it lives under /tmp and dies with the run, so it never reaches
    ~/.secrets and its startupCache needs no relocation.
    """

    def test_scan_finds_the_persistent_profiles(self):
        # Guards the guard: an empty set would make the check below vacuous.
        self.assertGreaterEqual(
            len(list(_persistent_profile_files())), 12)

    def test_each_persistent_profile_redirects_startup_cache(self):
        missing = []
        for path, text in _persistent_profile_files():
            if ("launch.prepare_profile_dir" not in text
                    and "launch.redirect_startup_cache" not in text):
                missing.append(str(path.relative_to(COLLECTORS.parent)))
        self.assertEqual(
            missing, [],
            "persistent profile(s) that never relocate startupCache — route "
            "the profile through collectorkit.launch.prepare_profile_dir():\n"
            + "\n".join(missing))

    def test_ephemeral_download_profile_is_exempt(self):
        # angellist's passive download drives an ephemeral mkdtemp profile; it
        # must not be dragged through the persistent-profile prep.
        files = {p for p, _t in _persistent_profile_files()}
        self.assertNotIn(COLLECTORS / "angellist" / "download.py", files)


if __name__ == "__main__":
    unittest.main()
