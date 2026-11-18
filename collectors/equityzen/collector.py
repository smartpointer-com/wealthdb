"""The equityzen collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: the default screenshots/ is where `download --debug`
# puts its captures.
SPEC = Collector(
    name="equityzen",
    env_prefix="EQUITYZEN",
    session="profile",
    unattended=True,
    window="full",
)
