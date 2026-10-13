"""
The collector's own settings: $XDG_CONFIG_HOME/refdata.cfg, by default
~/.config/refdata.cfg. It is a JSON object; a missing file holds the
defaults:

    {
      "providers": ["inbox", "sec-nport"],
      "contact": "name@example.org",
      "max_age_days": 30,
      "engine": "/path/to/wealthdb",
      "constituents": false
    }

- `providers` names the providers that run, in the order they are asked:
  the first that claims a fund or a security serves it. The default runs
  the local inbox and the providers of public data; every issuer
  provider stays off until this list names it, because enabling one
  accepts its terms (README.md).
- `contact` is the address the SEC requires in the User-Agent. The SEC
  providers do not run without it.
- `max_age_days`: a fund whose newest composition is younger is not
  fetched again; a security looked up more recently is not looked up
  again.
- `engine` is the wealthdb wrapper that prints the wanted list; the
  REFDATA_WEALTHDB environment variable wins over it, and `wealthdb` on
  PATH is the default.
- `constituents` lets the security providers look up the lines of held
  funds as well as the securities held directly.

The file is JSON, read as data, so nothing in it runs. A file that does
not parse, or names a setting or a provider this collector does not know,
is an error: a typo must not pass for a setting that is off.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path

FILE_NAME = "refdata.cfg"
DEFAULT_PROVIDERS = ("inbox", "sec-nport")
DEFAULT_MAX_AGE_DAYS = 30
SETTINGS = ("providers", "contact", "max_age_days", "engine", "constituents")


class ConfigError(Exception):
    """refdata.cfg exists and cannot be used. `str()` names the file."""


@dataclass(frozen=True)
class Config:
    providers: tuple = DEFAULT_PROVIDERS
    contact: str = ""
    max_age_days: int = DEFAULT_MAX_AGE_DAYS
    engine: str = ""
    constituents: bool = False
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
    return Config(providers=tuple(providers), contact=contact.strip(), max_age_days=max_age,
                  engine=engine.strip(), constituents=constituents, path=where)


def engine_command(cfg: Config) -> str:
    """The wealthdb wrapper to ask for the wanted list."""
    return os.environ.get("REFDATA_WEALTHDB", "").strip() or cfg.engine or "wealthdb"
