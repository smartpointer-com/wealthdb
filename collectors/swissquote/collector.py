"""The swissquote collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: where `download --debug` puts its landmark captures.
SPEC = Collector(
    name="swissquote",
    env_prefix="SWISSQUOTE",
    session="state",
    unattended=True,
    window="bounded",
)
