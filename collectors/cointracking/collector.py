"""The cointracking collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: the one non-load-input a run dir can hold is
# screenshots/, where `download --debug` puts its captures. Every other
# file is a load input.
SPEC = Collector(
    name="cointracking",
    env_prefix="COINTRACKING",
    session="profile",
    unattended=True,
    window="full",
)
