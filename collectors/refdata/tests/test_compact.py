"""compact: the complete runs' files in the form download stores them
in, with nothing a parser reads gone. Every file is invented."""

import hashlib
import json
import os

import compact
from collectorkit import bronze, compress
from providers.base import read_bytes

SUB = json.dumps({"cik": 900001, "filings": {"recent": {"form": ["10-Q"] * 50}}}).encode()


def complete_run(data, slug, status="complete"):
    run = data / slug
    run.mkdir(parents=True)
    bronze.atomic_write_json(run / "run.json", {"status": status, "requests": []})
    return run


def put(path, body: bytes):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(body)
    return path


def tree(data) -> dict:
    """relative path -> (sha256, inode) of every file under data."""
    return {str(p.relative_to(data)): (hashlib.sha256(p.read_bytes()).hexdigest(), p.stat().st_ino)
            for p in sorted(data.rglob("*")) if p.is_file()}


def layout(data):
    run = complete_run(data, "20260701T010000Z")
    edgar = run / "securities" / "sec-edgar" / "symbol_EXMP"
    put(edgar / "submissions.json", SUB)
    put(edgar / "meta.json", b'{"key": "symbol:EXMP"}')
    page = run / "funds" / "ishares" / "isin_XX0000000001" / "current"
    put(page / "page.html", b"<html>" + b"<p>a fund page</p>" * 200 + b"</html>")
    put(page / "holdings.csv", b"Ticker,Weight\n" + b"EXA,1.0\n" * 200)
    put(run / "funds" / "spdr" / "isin_XX0000000002" / "current" / "holdings.xlsx", b"PK\x03\x04 a workbook")
    put(run / "funds" / "inbox" / "isin_XX0000000003" / "2026-06-30" / "holdings.csv", b"isin,weight\n")
    put(run / "screenshots" / "http-trace.json", b"{}")
    return run, edgar, page


def test_compact_compresses_the_text_a_provider_stored_and_nothing_else(tmp_path, capsys):
    data = tmp_path / "data"
    run, edgar, page = layout(data)
    busy = complete_run(data, "20260702T010000Z", status="in-progress")
    put(busy / "securities" / "sec-edgar" / "symbol_EXMP" / "submissions.json", SUB)
    assert compact.run(data, dry_run=False, min_age_hours=0) == 0
    names = {p.relative_to(run).as_posix() for p in run.rglob("*") if p.is_file()}
    assert names == {"run.json", "securities/sec-edgar/symbol_EXMP/submissions.json.zst",
                     "securities/sec-edgar/symbol_EXMP/meta.json",
                     "funds/ishares/isin_XX0000000001/current/page.html.zst",
                     "funds/ishares/isin_XX0000000001/current/holdings.csv.zst",
                     "funds/spdr/isin_XX0000000002/current/holdings.xlsx",
                     "funds/inbox/isin_XX0000000003/2026-06-30/holdings.csv", "screenshots/http-trace.json"}
    assert read_bytes(edgar, "submissions.json") == SUB
    # A run that is not complete is left alone.
    assert (busy / "securities" / "sec-edgar" / "symbol_EXMP" / "submissions.json").is_file()
    out = capsys.readouterr().out
    assert "compress: changed 3 files" in out and "20260702T010000Z/" in out


def test_a_second_run_changes_nothing_and_a_dry_run_writes_nothing(tmp_path, capsys):
    data = tmp_path / "data"
    layout(data)
    before = tree(data)
    compact.run(data, dry_run=True, min_age_hours=0)
    assert tree(data) == before
    dry = capsys.readouterr().out
    compact.run(data, dry_run=False, min_age_hours=0)
    done = tree(data)
    assert done != before
    # The dry run told the bytes the run then freed.
    assert dry.replace("would change", "changed").replace("frees", "freed") == capsys.readouterr().out.replace(
        "a `load --force` rebuild gives the same silver\n", "")
    compact.run(data, dry_run=False, min_age_hours=0)
    assert tree(data) == done
    assert "compress: changed 0 files" in capsys.readouterr().out


def test_a_twin_left_by_a_stopped_run_is_kept_where_it_reads_back_and_made_again_where_not(tmp_path):
    data = tmp_path / "data"
    run, edgar, page = layout(data)
    compress.compress_file(edgar / "submissions.json", remove_original=False)
    put(page / "page.html.zst", b"not a frame")
    compact.run(data, dry_run=False, min_age_hours=0)
    assert not (edgar / "submissions.json").exists() and read_bytes(edgar, "submissions.json") == SUB
    assert not (page / "page.html").exists() and read_bytes(page, "page.html").startswith(b"<html>")


def test_a_run_touched_within_the_window_or_symlinked_is_left_alone(tmp_path, capsys):
    data = tmp_path / "data"
    run, edgar, _ = layout(data)
    elsewhere, _, _ = layout(tmp_path / "elsewhere")
    os.symlink(elsewhere, data / "20260703T010000Z")
    before = tree(data)
    compact.run(data, dry_run=False, min_age_hours=1)
    assert tree(data) == before and (edgar / "submissions.json").is_file()
    assert (elsewhere / "securities" / "sec-edgar" / "symbol_EXMP" / "submissions.json").is_file()
    out = capsys.readouterr().out
    assert "min old" in out and "symlinked run dir" in out


def test_a_file_two_runs_share_counts_its_bytes_once(tmp_path, capsys):
    data = tmp_path / "data"
    run, edgar, _ = layout(data)
    later = complete_run(data, "20260702T010000Z") / "securities" / "sec-edgar" / "symbol_EXMP"
    later.mkdir(parents=True)
    os.link(edgar / "submissions.json", later / "submissions.json")
    compact.run(data, dry_run=True, min_age_hours=0)
    shared = capsys.readouterr().out
    os.unlink(later / "submissions.json")
    compact.run(data, dry_run=True, min_age_hours=0)
    alone = capsys.readouterr().out
    assert "would change 4 files" in shared and "would change 3 files" in alone
    before = [line.split(", ")[1].split(" ->")[0] for line in (shared, alone)]
    assert before[0] == before[1]
