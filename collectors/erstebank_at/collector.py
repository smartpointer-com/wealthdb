"""The erstebank_at collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="erstebank_at",
    env_prefix="ERSTEBANK_AT",
    session="profile",
    unattended=False,
    window="bounded",
    # erstebank_at writes no in-bronze debug capture.
    debug_subdirs=(),
)
