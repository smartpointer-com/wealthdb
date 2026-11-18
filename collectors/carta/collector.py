"""The carta collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: `download --debug` writes the landing-page capture to
# <run>/screenshots/. Naming it is what lets prune reclaim it from a dump
# that is otherwise worth keeping.
SPEC = Collector(
    name="carta",
    env_prefix="CARTA",
    session="profile",
    unattended=True,
    window="full",
)
