"""The manual collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="manual",
    env_prefix="MANUAL",
    session="none",
    unattended=False,
    window=None,
)
