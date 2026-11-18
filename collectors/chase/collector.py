"""The chase collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="chase",
    env_prefix="CHASE",
    session="profile",
    unattended=False,
    window="bounded",
    # chase writes no screenshots/ into a run.
    debug_subdirs=(),
    # `download --debug` keeps every non-roster /svc/ body here; the roster
    # bodies are raw/account-NN.json, one level up, out of the glob's reach.
    debug_globs=("raw/svc/*.json",),
)
