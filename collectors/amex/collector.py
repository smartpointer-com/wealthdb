"""The amex collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="amex",
    env_prefix="AMEX",
    session="profile",
    unattended=True,
    window="bounded",
    # amex writes no in-bronze debug capture.
    debug_subdirs=(),
)
