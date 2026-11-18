"""The ubs-web collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="ubs-web",
    env_prefix="UBS_WEB",
    session="state",
    unattended=False,
    window="bounded",
)
