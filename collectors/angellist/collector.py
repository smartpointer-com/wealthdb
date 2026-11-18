"""The angellist collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: the default screenshots/ is where `download --debug`
# puts its per-route captures.
SPEC = Collector(
    name="angellist",
    env_prefix="ANGELLIST",
    session="state",
    unattended=True,
    window="full",
)
