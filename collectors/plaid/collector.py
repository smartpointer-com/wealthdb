"""The plaid collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: the one debug artefact a run writes is `download
# --debug`'s HTTP trace, under screenshots/. Every other file in a
# complete run is a load input.
SPEC = Collector(
    name="plaid",
    env_prefix="PLAID",
    session="token",
    unattended=True,
    window="bounded",
)
