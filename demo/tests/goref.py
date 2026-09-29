"""The engine's vocabularies, read from its Go source.

The generator stamps taxonomy values, account kinds, tax wrappers and
transaction kinds that the engine validates. Reading them from the Go
files rather than restating them here means a value renamed in the
engine fails these tests instead of silently becoming an uncategorised
row in gold.
"""

import pathlib
import re

CANONICAL = pathlib.Path(__file__).resolve().parents[2] / "wealthdb" / "internal" / "canonical"


def _read(name):
    return (CANONICAL / name).read_text()


def _consts(text, type_name):
    """{ConstName: value} for `Name Type = "value"` declarations."""
    return dict(re.findall(rf'(\w+)\s+{type_name}\s*=\s*"([^"]+)"', text))


def _set_values(text, var, consts):
    """The values of a `var = map[T]struct{}{A: {}, B: {}}` set."""
    body = re.search(rf"var {var} = map\[\w+\]struct\{{\}}\{{(.*?)\n\}}", text, re.S).group(1)
    return {consts[name] for name in re.findall(r"(\w+):\s*\{\}", body)}


def account_kinds():
    t = _read("enums.go")
    return _set_values(t, "accountKindValues", _consts(t, "AccountKind"))


def tax_wrappers():
    t = _read("enums.go")
    return _set_values(t, "taxWrapperValues", _consts(t, "TaxWrapper"))


def management_styles():
    t = _read("enums.go")
    return _set_values(t, "managementStyleValues", _consts(t, "ManagementStyle"))


def tx_kinds():
    t = _read("enums.go")
    return _set_values(t, "txKindValues", _consts(t, "TxKind"))


def taxonomy_pairs():
    """The admitted (asset_class, vehicle) pairs."""
    enums, tax = _read("enums.go"), _read("taxonomy.go")
    classes = {**_consts(enums, "AssetClass"), **_consts(tax, "AssetClass")}
    vehicles = _consts(tax, "Vehicle")
    body = re.search(r"var validTaxonomyPairs = [^{]*\{(.*?)\n\}", tax, re.S).group(1)
    pairs = set()
    for cls, vs in re.findall(r"(\w+):\s*setOf\(([^)]*)\)", body):
        for v in re.findall(r"\w+", vs):
            pairs.add((classes[cls], vehicles[v]))
    return pairs


def canonical_signs():
    """{kind: +1 | -1} for the kinds sign.go pins a sign on."""
    t = _read("sign.go")
    kinds = _consts(_read("enums.go"), "TxKind")
    out = {}
    for cases, sign in re.findall(r"case ([\w,\s]+):\s*return ([+-]1)", t):
        for name in re.findall(r"\w+", cases):
            out[kinds[name]] = int(sign)
    return out


def spend_categories():
    """{detailed value: family} for every row of canonical.SpendCategories."""
    t = _read("spendtaxonomy.go")
    consts = dict(re.findall(r'(\w+)\s*=\s*"([^"]+)"', t))
    rows = re.findall(r'\{\s*("[A-Za-z_]+"|\w+),\s*("[A-Za-z_]+"|\w+),\s*"(?:[^"\\]|\\.)*",\s*(Family\w+)\}', t, re.S)

    def value(tok):
        return tok.strip('"') if tok.startswith('"') else consts[tok]

    return {value(d): fam for _, d, fam in rows}


def spending_values():
    return {d for d, fam in spend_categories().items() if fam in ("FamilySpending", "FamilyBoth")}


def income_values():
    return {d for d, fam in spend_categories().items() if fam in ("FamilyIncome", "FamilyBoth")}
