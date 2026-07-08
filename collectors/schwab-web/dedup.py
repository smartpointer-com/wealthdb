#!/usr/bin/env python3
"""Parse-equivalence dedup for the schwab-web statement bronze.

Schwab re-renders a statement PDF on **every** download — the same logical
statement comes back with fresh bytes each run (a new creation timestamp and
object layout inside the PDF), so the byte-identical `wealthdb-collect dedup`
sweep can never collapse them and the `statements/` tree grows one full copy
per run. This verb reclaims that bloat by collapsing copies that are *parse*-
equivalent: it parses each statement PDF exactly as `load` does
(`pdf_parsers.parse_statement_pdf`) and, within one logical statement across
runs, hardlinks every copy whose parsed content matches onto the oldest copy.

Silver-safe, not byte-safe. It is LOSSY at the byte level (the re-rendered
bytes of the newer copies are discarded — the oldest copy's bytes back them
all), but silver is untouched because `load` never re-reads a statement PDF's
on-disk bytes against the manifest: it keys the `documents` table off the
manifest sha256, locates the PDF by filename, and re-parses whatever bytes are
there, gating on the logical key `(suffix, doc_date, doc_kind, filename)` — see
[load.py](load.py). So as long as the collapsed bytes parse to the SAME
statement, `load --force` reproduces byte-identical silver. That parse-
equivalence is exactly what this verb verifies before it collapses anything,
and it NEVER touches `run.json`, so the manifest-derived `documents` rows are
unchanged regardless.

Because it rewrites load inputs and depends on the parser, it is a deliberate,
manually-invoked one-off (not wired into orchestration) and runs IN-CONTAINER
(it needs the image's `pypdfium2` for text extraction), unlike the pure-stdlib
`prune`. Run it once after a backlog of re-downloaded statements has
accumulated:

    <wrapper> dedup --dry-run        # the evidence report — collapses nothing
    <wrapper> dedup                  # collapse the parse-equivalent copies

Divergence is a hard stop, not a collapse: if the copies of one logical
statement do NOT all parse alike (a genuine restatement, or a parser
nondeterminism), that group is reported as DIVERGENT and left entirely alone.

Safety envelope (shared with `collectorkit.dedup`): complete + quiescent dumps
only (`--min-age-hours`, default 1), symlinks never followed or replaced,
atomic verified hardlink replace, equivalence re-verified immediately before
each irreversible replace, own per-inode byte accounting.
"""
from __future__ import annotations

import argparse
import difflib
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

import pdf_parsers as pp
from collectorkit import bronze, dedup, prune
from load import (_DOC_KIND_BY_TYPE, _format_from_filename, canonical_json,
                  parse_doc_date)


def _read_manifest(run_dir: Path) -> dict | None:
    try:
        meta = json.loads((run_dir / "run.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return meta if isinstance(meta, dict) else None


def build_statement_catalog(bronze_dir: Path) -> dict[str, tuple[str, int]]:
    """Map each on-disk statement-PDF path to ``(logical_id, year_hint)``.

    A document counts as a statement PDF exactly as `load`'s statement walk
    decides: its manifest ``type`` maps to ``doc_kind == "statement"`` and its
    on-disk extension is ``pdf`` (extension trusted over the manifest's format
    claim, as load does). ``logical_id`` is ``"<suffix>/<filename>"`` — the same
    logical statement lands at the same id in every run. The ``logical_id`` is
    ``"<suffix>/<doc_date>/<filename>"`` — load's own logical statement key
    (`load.py`'s ``logical_doc_key``) minus the constant ``doc_kind``, so copies
    collapse only when they are the same logical statement load would treat as
    one. ``year_hint`` is the year of the manifest doc-date, the fallback
    `parse_statement_pdf` uses when a pre-2025 statement has no period header — so
    this verb parses each copy with the SAME hint load would, making "equivalent"
    mean "load yields the same rows". Keyed by the run-dir path so a mis-keyed
    collision is impossible. Selection is congruent with load's statement walk,
    not a superset: same ``doc_kind``, on-disk ``.pdf`` extension, parseable
    doc-date, AND a manifest ``sha256`` (load requires all four).
    """
    catalog: dict[str, tuple[str, int]] = {}
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        if run_dir.is_symlink():
            continue
        manifest = _read_manifest(run_dir)
        if manifest is None:
            continue
        for acct in manifest.get("statements", []):
            suffix = acct.get("suffix")
            if not suffix:
                continue
            for doc in acct.get("documents", []):
                filename = doc.get("filename")
                # A filename AND a manifest sha256 — load skips a doc missing
                # either (load.py), so requiring both keeps this congruent.
                if not filename or not doc.get("sha256"):
                    continue
                raw_type = doc.get("type") or "Unknown"
                doc_kind = _DOC_KIND_BY_TYPE.get(raw_type, raw_type.lower())
                if doc_kind != "statement" or _format_from_filename(filename) != "pdf":
                    continue
                doc_date = parse_doc_date(doc.get("date") or "")
                if doc_date is None:
                    continue  # load skips an undated statement too
                year = datetime.fromtimestamp(doc_date, tz=timezone.utc).year
                path = run_dir / "statements" / suffix / filename
                catalog[str(path)] = (f"{suffix}/{doc_date}/{filename}", year)
    return catalog


def parse_key(path, year_hint: int) -> str:
    """The silver-equivalence key: the canonical JSON of `parse_statement_pdf`
    output with its volatile ``path`` field (the run-dir file path, which
    differs run-to-run) removed. Everything that remains — statement period,
    transactions, positions, cash summary, account registration — is what silver
    consumes, so equal keys ⇒ byte-identical silver."""
    parsed = pp.parse_statement_pdf(str(path), statement_year=year_hint)
    parsed.pop("path", None)
    return canonical_json(parsed)


def _diverging_fields(members, catalog) -> list[str]:
    """Top-level parse fields that are NOT identical across a divergent group's
    members (for the evidence report). A member that failed to parse is named
    explicitly."""
    dicts = []
    for path_str, key, err in members:
        if err is not None or key is None:
            return [f"parse error on {Path(path_str).name}: {err}"]
        year = catalog[path_str][1]
        d = pp.parse_statement_pdf(path_str, statement_year=year)
        d.pop("path", None)
        dicts.append(d)
    fields = set().union(*(d.keys() for d in dicts))
    return sorted(f for f in fields
                  if len({canonical_json(d.get(f)) for d in dicts}) > 1)


def _text_layer_diff(canonical: Path, dup: Path, max_lines: int) -> list[str]:
    """Unified diff of the raw PDF text layers, truncated — the 'text-layer'
    half of the stratified equivalence check: confirms that two PARSE-equal
    copies differ, in their raw text, only in volatile render lines."""
    a = pp._extract_pdf_text(str(canonical)).splitlines()
    b = pp._extract_pdf_text(str(dup)).splitlines()
    out = [ln for ln in difflib.unified_diff(
        a, b, fromfile="canonical", tofile="dup", lineterm="")
        if ln and ln[0] in "+-" and not ln.startswith(("+++", "---"))]
    return out[:max_lines]


def main(argv=None) -> int:
    p = argparse.ArgumentParser(
        prog="schwab-web dedup",
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--bronze-dir", type=Path, default=Path("/data"),
                   help="schwab-web bronze root (default: %(default)s).")
    p.add_argument("--dry-run", action="store_true",
                   help="Emit the evidence report (per-group plan, divergences, "
                        "reclaimable bytes) and change nothing.")
    p.add_argument("--min-age-hours", type=float, default=1.0,
                   help="Skip dumps touched within this window — an in-flight "
                        "guard so a running download is never raced "
                        "(default: %(default)s).")
    p.add_argument("--sample-text-diff", type=int, default=0, metavar="K",
                   help="For up to K EQUIVALENT groups, print the raw text-layer "
                        "diff (canonical vs one copy) to show it is confined to "
                        "volatile render lines. Off by default.")
    args = p.parse_args(argv)

    if not args.bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {args.bronze_dir}")

    catalog = build_statement_catalog(args.bronze_dir)
    if not catalog:
        print("no statement PDFs found in any manifest")
        return 0

    def group_of(run_dir, path):
        entry = catalog.get(str(path))
        return entry[0] if entry is not None else None

    def key_of(path):
        return parse_key(path, catalog[str(path)][1])

    groups, divergent, skipped = dedup.plan_equivalence(
        args.bronze_dir, group_of=group_of, key_of=key_of,
        min_age_s=args.min_age_hours * 3600.0, min_size=1)

    verb = "would collapse" if args.dry_run else "collapsing"
    reclaimable = 0
    for g in groups:
        reclaimable += g.reclaim
        print(f"{verb}  {len(g.dups)} re-render(s) of {g.group_id}  "
              f"(reclaim {prune.human_size(g.reclaim)})")

    # Divergences are the red flag — always shown, never collapsed.
    for d in divergent:
        fields = _diverging_fields(d.members, catalog)
        print(f"DIVERGENT  {d.group_id}: {len(d.members)} copies do not parse "
              f"alike — NOT collapsed; differing: {', '.join(fields) or '?'}")

    # Optional text-layer half of the stratified diff.
    shown = 0
    for g in groups:
        if shown >= args.sample_text_diff:
            break
        diff = _text_layer_diff(g.canonical.path, g.dups[0].path, max_lines=20)
        print(f"\n-- text-layer diff for {g.group_id} "
              f"(canonical vs 1 copy; expect only render lines) --")
        for ln in diff:
            print(f"   {ln}")
        shown += 1

    if skipped:
        by_reason: dict[str, int] = {}
        for _path, reason in skipped:
            key = ("recently written (possibly in flight)"
                   if reason.startswith("only ") else reason)
            by_reason[key] = by_reason.get(key, 0) + 1
        for reason, count in sorted(by_reason.items(), key=lambda kv: -kv[1]):
            print(f"skipping  {count} dump(s): {reason}")

    print(f"\nsummary: {len(groups)} collapsible group(s), "
          f"{len(divergent)} DIVERGENT group(s), "
          f"{'would reclaim' if args.dry_run else 'reclaim'} "
          f"{prune.human_size(reclaimable)}")
    if divergent:
        print("NOTE: divergent groups were left untouched — investigate before "
              "trusting a real run over them.")

    if not args.dry_run and groups:
        # key_of re-verifies parse-equivalence at the last moment before each
        # irreversible replace (belt-and-braces beyond the quiescence guard).
        reclaimed, n = dedup.collapse_equiv_groups(
            groups, args.bronze_dir, key_of=key_of)
        print(f"collapsed {n} re-render(s); reclaimed {prune.human_size(reclaimed)}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
