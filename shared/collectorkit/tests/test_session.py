"""Unit tests for collectorkit.session.resolve_state_path — the
renamed-default legacy read-fallback. Stdlib unittest, matching the
rest of the collectorkit suite."""
import tempfile
import unittest
from pathlib import Path

from collectorkit import session


class ResolveStatePathTest(unittest.TestCase):
    def _default_legacy(self, tmp: str) -> tuple[Path, Path]:
        # <source>-state.json (new default) vs <source>_state.json (legacy).
        return Path(tmp) / "s-state.json", Path(tmp) / "s_state.json"

    def test_prefers_default_when_present(self):
        with tempfile.TemporaryDirectory() as tmp:
            default, legacy = self._default_legacy(tmp)
            default.write_text("{}")
            legacy.write_text("{}")
            self.assertEqual(
                session.resolve_state_path(default, default, legacy), default)

    def test_falls_back_to_legacy_when_default_absent(self):
        with tempfile.TemporaryDirectory() as tmp:
            default, legacy = self._default_legacy(tmp)
            legacy.write_text("{}")  # only the old-named session exists
            self.assertEqual(
                session.resolve_state_path(default, default, legacy), legacy)

    def test_returns_default_for_fresh_mint(self):
        # Neither exists yet: a fresh login must write to the NEW default.
        with tempfile.TemporaryDirectory() as tmp:
            default, legacy = self._default_legacy(tmp)
            self.assertEqual(
                session.resolve_state_path(default, default, legacy), default)

    def test_explicit_custom_path_never_redirected(self):
        # An explicit --state-path (!= default) is never swapped for the
        # legacy sibling, even if the legacy default file happens to exist.
        with tempfile.TemporaryDirectory() as tmp:
            default, legacy = self._default_legacy(tmp)
            legacy.write_text("{}")
            custom = Path(tmp) / "custom.json"
            self.assertEqual(
                session.resolve_state_path(custom, default, legacy), custom)


if __name__ == "__main__":
    unittest.main()
