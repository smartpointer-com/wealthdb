"""The fidelity-web collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="fidelity-web",
    env_prefix="FIDELITY_WEB",
    session="profile",
    unattended=True,
    window="bounded",
)
