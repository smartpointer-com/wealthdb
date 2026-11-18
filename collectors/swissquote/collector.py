"""The swissquote collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="swissquote",
    env_prefix="SWISSQUOTE",
    session="state",
    unattended=False,
    window="bounded",
)
