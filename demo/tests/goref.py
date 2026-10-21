"""The engine's vocabularies, read from its Go source.

The generator stamps taxonomy values, account kinds, tax wrappers,
transaction kinds, composition bases, risk tenors, the companies'
period-fact fields, the funds' figures, the keys of the records that
state a bond's terms and a balance's rate, and the yield config's and
ledger's vocabularies, all of which the engine validates or reads by
name. Reading them from the Go files
rather than restating them here means a value renamed in the engine
fails these tests instead of silently becoming an uncategorised row in
gold, or a figure gold ignores.
"""

import pathlib
import re

INTERNAL = pathlib.Path(__file__).resolve().parents[2] / "wealthdb" / "internal"
CANONICAL = INTERNAL / "canonical"


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


def composition_bases():
    """What a fund composition's lines can describe (CompositionBasis)."""
    return set(_consts(_read("refdata.go"), "CompositionBasis").values())


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


def _list(text, var, consts=None):
    """The values of a `var Name = []T{A, B}` slice: names looked up in
    consts, or quoted strings and numbers as they are."""
    body = re.search(rf"var {var} = \[\]\w+\{{(.*?)\}}", text, re.S).group(1)
    if consts is None:
        return [tok.strip('"') for tok in re.findall(r'"[^"]*"|-?\d+', body)]
    return [consts[name] for name in re.findall(r"\w+", body)]


def risk_tenors():
    """The tenors a fund's rate sensitivity is stated at (RiskTenors)."""
    return _list(_read("refdata.go"), "RiskTenors")


def no_currency():
    """The currency of a fund's credit-spread risk rows (NoCurrency)."""
    return re.search(r'const NoCurrency = "([^"]+)"', _read("refdata.go")).group(1)


def period_fields():
    """The security_period_facts fields gold's wanted list counts, over
    every family of gold's periodFamilies."""
    t = (INTERNAL / "gold" / "refdata_wanted.go").read_text()
    body = re.search(r"var periodFamilies = \[\]struct \{.*?\n\}\{(.*?)\n\}", t, re.S).group(1)
    return {f for fields in re.findall(r"\[\]string\{([^}]*)\}", body) for f in re.findall(r'"([^"]+)"', fields)}


def equity_sectors():
    """The eleven broad sectors a yield cycle maps."""
    return set(_list(_read("exposure.go"), "equitySectors", _consts(_read("exposure.go"), "Sector")))


def rating_buckets():
    """The credit-rating buckets, best first."""
    t = _read("yield.go")
    return _list(t, "ratingBuckets", _consts(t, "RatingBucket"))


def withholding_default():
    """The wrapper of the rate a country withholds from every wrapper it
    names no rate for (WithholdingDefault)."""
    return re.search(r'const WithholdingDefault = "([^"]+)"', _read("yield.go")).group(1)


def cycles():
    return set(_consts(_read("yield.go"), "Cycle").values())


def day_counts():
    return set(_consts(_read("yield.go"), "DayCount").values())


def coupon_frequencies():
    return {int(n) for n in _list(_read("yield.go"), "couponFrequencies")}


def yield_records():
    """The yield records a declarations-ledger line may carry
    (DeclarationYieldKinds)."""
    t = _read("exposure_config.go")
    return _list(t, "DeclarationYieldKinds", dict(re.findall(r'(Declaration\w+)\s*=\s*"([^"]+)"', t)))


def fund_fields():
    """The fund_characteristics fields the yield report reads (gold's
    fundFields)."""
    t = (INTERNAL / "gold" / "yield_inputs.go").read_text()
    body = re.search(r"var fundFields = map\[string\].*?\{\n(.*?)\n\}", t, re.S).group(1)
    return set(re.findall(r'^\s*"(\w+)":', body, re.M))


def page_breakdowns():
    """The fields of a fund page's breakdowns the exposure views split a
    leaf by."""
    text = "".join(p.read_text() for p in sorted((INTERNAL / "gold" / "migrations").glob("*.sql")))
    return set(re.findall(r"exposure_page_breakdown\(\w+, '(\w+)'\)", text))


def payload_term_keys():
    """The keys the yield pass reads a bond's terms by, from an
    instrument's record or a holding's (canonical.PayloadTermsKeys)."""
    t = _read("payloadterms.go")
    return set(_list(t, "payloadTermsKeys", dict(re.findall(r'(Payload\w+)\s*=\s*"([^"]+)"', t))))


def balance_rate_keys():
    """The keys the yield pass reads a cash balance's stated rate by:
    the ones it names whole, and each side's keys (CdtIntrstRate1 …)
    expanded from the templates its SQL builds them by, a side's prefix
    first and a tier's number last."""
    t = (INTERNAL / "gold" / "yield_pass.go").read_text()
    prefixes = [prefix for _, prefix in re.findall(r'\{"(credit|debit)", "(\w+)"\}', t)]
    tiers = range(1, int(re.search(r"tier <= (\d+)", t).group(1)) + 1)
    keys = set(re.findall(r"payload, '([A-Z]\w*)'\)", t))
    for name, tiered in re.findall(r"'%(?:\[2\])?s([A-Za-z]+)(%(?:\[3\])?d)?'", t):
        for prefix in prefixes:
            keys |= {f"{prefix}{name}{n}" for n in tiers} if tiered else {prefix + name}
    return keys
