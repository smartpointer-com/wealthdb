"""
The collector's own settings: $XDG_CONFIG_HOME/refdata.cfg, by default
~/.config/refdata.cfg, a JSON object. README.md, Settings, describes
each setting. This module reads the file as data, so nothing in it runs,
and refuses a file that does not parse, or that names a setting or a
provider this collector does not know: a typo must not pass for a
setting that is off.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path

FILE_NAME = "refdata.cfg"
DEFAULT_PROVIDERS = ("inbox", "sec-nport", "sec-nmfp", "sec-edgar", "gleif", "openfigi")
DEFAULT_MAX_AGE_DAYS = 30
DEFAULT_CONSTITUENT_FLOOR_PCT = 0.0001
SETTINGS = ("providers", "contact", "max_age_days", "engine", "constituents", "constituent_floor_pct",
            "fidelity_529_plans")


class ConfigError(Exception):
    """refdata.cfg exists and cannot be used. `str()` names the file."""


@dataclass(frozen=True)
class Config:
    providers: tuple = DEFAULT_PROVIDERS
    contact: str = ""
    max_age_days: int = DEFAULT_MAX_AGE_DAYS
    engine: str = ""
    constituents: bool = False
    constituent_floor_pct: float = DEFAULT_CONSTITUENT_FLOOR_PCT
    # Each 529 plan as (the broker's name prefix, the report's URL).
    fidelity_529_plans: tuple = ()
    path: Path | None = field(default=None, compare=False)


def path() -> Path:
    """Where refdata.cfg lives: in $XDG_CONFIG_HOME, else in ~/.config."""
    xdg = os.environ.get("XDG_CONFIG_HOME", "").strip()
    base = Path(xdg) if xdg else Path.home() / ".config"
    return base / FILE_NAME


def load(known_providers, where: Path | None = None) -> Config:
    """Read refdata.cfg; `known_providers` are the names the registry
    serves."""
    where = where or path()
    try:
        raw = where.read_bytes()
    except FileNotFoundError:
        return Config(path=where)
    except OSError as e:
        raise ConfigError(f"{where} cannot be read: {e}") from e
    try:
        doc = json.loads(raw)
    except ValueError as e:
        raise ConfigError(f"{where} is not valid JSON: {e}") from e
    if not isinstance(doc, dict):
        raise ConfigError(f"{where} must hold a JSON object.")
    unknown = sorted(set(doc) - set(SETTINGS))
    if unknown:
        raise ConfigError(f"{where} names settings this collector does not know: "
                          f"{', '.join(unknown)}. It knows: {', '.join(SETTINGS)}.")
    providers = doc.get("providers", list(DEFAULT_PROVIDERS))
    if not isinstance(providers, list) or not all(isinstance(p, str) for p in providers):
        raise ConfigError(f"{where}: providers is a list of names, such as [\"inbox\", \"sec-nport\"].")
    stray = [p for p in providers if p not in known_providers]
    if stray:
        raise ConfigError(f"{where}: providers names {', '.join(stray)}, which this collector does "
                          f"not have. It has: {', '.join(sorted(known_providers))}.")
    if len(set(providers)) != len(providers):
        raise ConfigError(f"{where}: providers names a provider twice.")
    contact = doc.get("contact", "")
    if not isinstance(contact, str):
        raise ConfigError(f"{where}: contact is a string, an e-mail address.")
    max_age = doc.get("max_age_days", DEFAULT_MAX_AGE_DAYS)
    if not isinstance(max_age, int) or isinstance(max_age, bool) or not 1 <= max_age <= 3650:
        raise ConfigError(f"{where}: max_age_days is a whole number of days, 1 to 3650.")
    engine = doc.get("engine", "")
    if not isinstance(engine, str):
        raise ConfigError(f"{where}: engine is a path to the wealthdb wrapper.")
    constituents = doc.get("constituents", False)
    if not isinstance(constituents, bool):
        raise ConfigError(f"{where}: constituents is true or false.")
    floor = doc.get("constituent_floor_pct", DEFAULT_CONSTITUENT_FLOOR_PCT)
    if not isinstance(floor, (int, float)) or isinstance(floor, bool) or not 0 <= floor <= 100:
        raise ConfigError(f"{where}: constituent_floor_pct is a share of net worth in percent, 0 to 100.")
    plans = doc.get("fidelity_529_plans", {})
    if not isinstance(plans, dict) or not all(
            isinstance(k, str) and k.strip() and not any(c.isspace() for c in k.strip())
            and isinstance(v, str) and v.startswith("https://") for k, v in plans.items()):
        raise ConfigError(f"{where}: fidelity_529_plans maps a broker's name prefix, one word, to the "
                          f"plan's annual report, an https:// URL.")
    if len({k.strip().upper() for k in plans}) != len(plans):
        raise ConfigError(f"{where}: fidelity_529_plans names a prefix twice.")
    return Config(providers=tuple(providers), contact=contact.strip(), max_age_days=max_age,
                  engine=engine.strip(), constituents=constituents, constituent_floor_pct=float(floor),
                  fidelity_529_plans=tuple((k.strip(), v) for k, v in sorted(plans.items())), path=where)


# The engine wrapper of the checkout this collector lives in: it runs
# that checkout's engine image, by the checkout's own tag.
CHECKOUT_ENGINE = Path(__file__).resolve().parent.parent.parent / "wealthdb" / "wealthdb"


def engine_command(cfg: Config, checkout_engine: Path = CHECKOUT_ENGINE) -> str:
    """The wealthdb wrapper to ask for the wanted list: REFDATA_WEALTHDB,
    else refdata.cfg's `engine`, else this checkout's wrapper, else
    `wealthdb` on PATH."""
    if os.environ.get("REFDATA_WEALTHDB", "").strip():
        return os.environ["REFDATA_WEALTHDB"].strip()
    if cfg.engine:
        return cfg.engine
    return str(checkout_engine) if checkout_engine.is_file() else "wealthdb"
