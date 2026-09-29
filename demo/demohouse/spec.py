"""Loading the household spec and the catalogue.

The generator reads these files and nothing else. Their hashes, and one
of the generator's own code, are recorded in every silver file, so an
append run can refuse to splice a history made from different inputs or
a different generator onto the one already on disk.
"""

import hashlib
import json
import pathlib

from . import dates

DEMO_DIR = pathlib.Path(__file__).resolve().parent.parent
SPEC_PATH = DEMO_DIR / "household.json"
# The silver schema the kind's adapter reads; every file is written from it.
SCHEMA_PATH = DEMO_DIR.parent / "wealthdb" / "internal" / "silver" / "synthetic" / "testdata" / "silver_schema.sql"
CODE_FILES = sorted([DEMO_DIR / "generate.py", *(DEMO_DIR / "demohouse").glob("*.py")]) + [SCHEMA_PATH]
CATALOGUE_DIR = DEMO_DIR / "catalogue"
CATALOGUE_FILES = ("instruments.json", "merchants.json", "payers.json")


class Inputs:
    """The spec and the catalogue, indexed for the simulation."""

    def __init__(self, spec, instruments, splits, merchants, payers, spec_hash, catalogue_hash, code_hash):
        self.spec = spec
        self.instruments = instruments
        self.splits = splits
        self.merchants = merchants
        self.payers = payers
        self.spec_hash = spec_hash
        self.catalogue_hash = catalogue_hash
        self.code_hash = code_hash

    @property
    def history_start(self):
        return dates.parse(self.spec["calendar"]["history_start"])


def code_hash():
    """A hash of the generator's own source and the schema it writes.
    Keyed randomness keeps a history stable only for the same code, so
    any edit to the simulation counts as a different generator and an
    append refuses it."""
    digest = hashlib.sha256()
    for path in CODE_FILES:
        digest.update(path.name.encode() + b"\0" + path.read_bytes() + b"\0")
    return digest.hexdigest()


def _read(path):
    raw = path.read_bytes()
    return raw, json.loads(raw)


def load(spec_path=SPEC_PATH, catalogue_dir=CATALOGUE_DIR):
    spec_raw, spec = _read(pathlib.Path(spec_path))
    catalogue_dir = pathlib.Path(catalogue_dir)
    digest = hashlib.sha256()
    parts = {}
    for name in CATALOGUE_FILES:
        raw, data = _read(catalogue_dir / name)
        digest.update(name.encode() + b"\0" + raw + b"\0")
        parts[name] = data
    instruments = _instrument_versions(parts["instruments.json"], spec)
    merchants = {m["id"]: m for m in parts["merchants.json"]["merchants"]}
    payers = {p["id"]: p for p in parts["payers.json"]["payers"]}
    splits = parts["instruments.json"].get("splits", [])
    return Inputs(spec, instruments, splits, merchants, payers,
                  hashlib.sha256(spec_raw).hexdigest(), digest.hexdigest(), code_hash())


def _instrument_versions(cat, spec):
    """Index instruments, each with the descriptive versions it passes
    through: the catalogue entry from the start of history, then one
    version per rename from its date."""
    start = dates.epoch(dates.parse(spec["calendar"]["history_start"]))
    out = {}
    for inst in cat["instruments"]:
        entry = dict(inst)
        entry["versions"] = [_version(inst, start, inst["name"])]
        out[inst["id"]] = entry
    for r in cat.get("renames", []):
        entry = out[r["instrument"]]
        entry["versions"].append(_version(entry, dates.epoch(dates.parse(r["date"])), r["name"]))
    for entry in out.values():
        entry["versions"].sort(key=lambda v: v["valid_from"])
    return out


def _version(inst, valid_from, name):
    return {
        "instrument_id": inst["id"], "valid_from": valid_from,
        "asset_class": inst["asset_class"], "vehicle": inst["vehicle"],
        "isin": inst.get("isin"), "cusip": None, "symbol": inst.get("symbol"),
        "name": name, "currency": inst["currency"],
    }


def name_on(instrument, day):
    """The instrument's name in force on `day`."""
    at = dates.epoch(day)
    name = instrument["versions"][0]["name"]
    for v in instrument["versions"]:
        if v["valid_from"] <= at:
            name = v["name"]
    return name
