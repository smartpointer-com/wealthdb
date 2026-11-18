"""The ubs-psn collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

# debug_subdirs: screenshots/ holds legacy `download --debug` listing
# captures, never a load input. Each run's listing.json is provenance and
# is never nominated. The run dir's zips are the load inputs.
SPEC = Collector(
    name="ubs-psn",
    env_prefix="UBS_PSN",
    session="key",
    unattended=True,
    window="full",
)
