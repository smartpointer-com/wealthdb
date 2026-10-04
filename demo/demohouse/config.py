"""Rendering the demo root's wealthdb.cfg and ledger CSVs.

Every path in the config is relative to the config file, so the config
names nothing outside the demo root and gold resolves every file inside
it. The config carries exactly what the pipeline needs plus a small,
deliberate sample of the optional blocks; unknown keys are refused by
the engine, so `wealthdb load` validates every block.
"""

import csv
import io
import json
import re

DEMO_MARKER = ".wealthdb-demo"
WEB_PORT = 3100
MCP_PORT = 3400

EQUITY_TRANSFER_COLUMNS = ("silver_source_id", "account", "occurred_at", "direction", "quantity",
                           "cost_basis", "value", "currency", "instrument", "note")
PIN_COLUMNS = ("silver_source_id", "account", "occurred_at", "amount", "currency", "spend_detailed", "note")
OVERRIDE_COLUMNS = ("verb", "silver_source_id", "account", "occurred_at", "amount", "currency",
                    "silver_source_id_b", "account_b", "occurred_at_b", "amount_b", "currency_b", "note")


def render(spec):
    """wealthdb.cfg as a JSON string."""
    sources = []
    for src in spec["sources"]:
        entry = {"id": src["id"], "kind": "synthetic", "path": f'silver/{src["id"]}.db'}
        if src["id"] == "fx":
            entry["fx_priority"] = 1
        sources.append(entry)
    # The FX source leads, as a rates feed conventionally does.
    sources.sort(key=lambda s: s["id"] != "fx")
    cfg = {
        "gold_db": "wealthdb.db",
        "default_currency": "USD",
        "silver_sources": sources,
        "account_overrides": spec["config"]["account_overrides"],
        "declared_accounts": spec["declared"],
        "returns_policy_overrides": spec["config"]["returns_policy_overrides"],
        "returns_transfer_matching": {"enabled": True, "tolerance_pct": 0},
        "inception_overrides": spec["config"]["inception_overrides"],
        "equity_transfers": "overrides/equity_transfers.csv",
        "spending": {
            "internal_transfer_matching": {"tolerance_pct": 0, "names": match_names(spec)},
            "rules": spec["config"]["spending_rules"],
            "pins": "overrides/spending_pins.csv",
            "transfer_overrides": "overrides/transfer_overrides.csv",
        },
        "income": {"rules": spec["config"]["income_rules"]},
        "web": {"enabled": True, "port": WEB_PORT},
        "mcp": {"enabled": True, "port": MCP_PORT},
    }
    return json.dumps(cfg, indent=2, ensure_ascii=False) + "\n"


def match_names(spec):
    """One matcher `names` entry per labelled account: a narrative that
    prints an account's label (its institution's short name and its tag,
    the way household.py writes the other leg of every move) names that
    account as the far side."""
    out = []
    for src in spec["sources"]:
        for a in src["accounts"]:
            if "tag" in a:
                out.append({"source": src["id"], "account": a["id"],
                            "match": _literal(f'{src["short"]} {a["tag"]}') + r"\b"})
    return out


def _literal(text):
    """`text` as a pattern that matches itself, in syntax both Python and
    Go's RE2 read the same way (re.escape also escapes spaces, which RE2
    refuses)."""
    return re.sub(r"([\\.+*?()|\[\]{}^$])", r"\\\1", text)


def csv_text(columns, rows):
    out = io.StringIO()
    w = csv.DictWriter(out, fieldnames=columns, lineterminator="\n")
    w.writeheader()
    for r in rows:
        w.writerow({c: r.get(c, "") for c in columns})
    return out.getvalue()
