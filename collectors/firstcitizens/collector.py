"""The firstcitizens collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="firstcitizens",
    env_prefix="FIRSTCITIZENS",
    session="profile",
    # Every download signs in (see AGENTS.md), so none is unattended.
    unattended=False,
    window="bounded",
    # firstcitizens writes no in-bronze debug capture.
    debug_subdirs=(),
)
