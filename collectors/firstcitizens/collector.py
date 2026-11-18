"""The firstcitizens collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="firstcitizens",
    env_prefix="FIRSTCITIZENS",
    session="profile",
    unattended=True,
    window="bounded",
    # firstcitizens writes no in-bronze debug capture.
    debug_subdirs=(),
)
