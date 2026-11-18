"""The raiffeisen_at collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="raiffeisen_at",
    env_prefix="RAIFFEISEN_AT",
    session="profile",
    unattended=False,
    window="bounded",
    # raiffeisen_at writes no in-bronze debug capture.
    debug_subdirs=(),
)
