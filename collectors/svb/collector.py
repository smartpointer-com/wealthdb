"""The svb collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="svb",
    env_prefix="SVB",
    session="none",
    unattended=False,
    window=None,
)
