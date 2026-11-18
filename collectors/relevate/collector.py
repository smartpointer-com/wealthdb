"""The relevate collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="relevate",
    env_prefix="RELEVATE",
    session="state",
    unattended=True,
    window="bounded",
)
