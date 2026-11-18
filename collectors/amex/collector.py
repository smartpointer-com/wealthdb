"""The amex collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="amex",
    env_prefix="AMEX",
    session="profile",
    # Every download signs in (see AGENTS.md), so none is unattended.
    unattended=False,
    window="bounded",
    # amex writes no in-bronze debug capture.
    debug_subdirs=(),
)
