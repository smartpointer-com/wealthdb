"""compact: the complete runs' files in the form download stores them
in, with nothing a parser reads gone. Every file is invented."""

import hashlib
import io
import json
import os
import time
import zipfile
from pathlib import Path

import compact
from collectorkit import bronze, compress
from providers import sec_calendar
from providers.base import read_bytes

FIXTURE = Path(__file__).parent / "fixtures" / "edgar_10q_dividends.htm"
PRIMARY = "exmp-20260930.htm"
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


def whole_archive(method=zipfile.ZIP_DEFLATED) -> bytes:
    """A filing's whole XBRL archive: its inline document, a second one,
    an exhibit, an image and the XBRL instance."""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", method) as z:
        for name, body in ((PRIMARY, FIXTURE.read_bytes()), ("exmp-ex311.htm", b"<html><p>Certification</html>"),
                           ("exmp-10q2.htm", FIXTURE.read_bytes()), ("exmp-logo.jpg", b"\xff\xd8 an image"),
                           ("exmp-20260930_htm.xml", b"<xbrl/>")):
            z.writestr(zipfile.ZipInfo(name, (2026, 10, 30, 16, 5, 2)), body)
    return buf.getvalue()


def filing_run(data, slug="20260701T010000Z", archive=None, documents=(PRIMARY,)):
    """A complete run whose sec-edgar request stored a filing's whole
    archive, its meta.json naming it once per primary document of
    `documents`."""
    rel = "securities/sec-edgar/symbol_EXMP"
    run = data / slug
    folder = run / rel
    folder.mkdir(parents=True)
    bronze.atomic_write_json(run / "run.json", {"status": "complete", "requests": [
        {"provider": "sec-edgar", "kind": "security", "status": "ok", "dir": rel, "ref": "symbol:EXMP"}]})
    put(folder / "0000000007-26-000011-xbrl.zip", archive or whole_archive())
    put(folder / "0000000007-26-000012-xbrl.zip", b"PK an archive no meta.json names")
    bronze.atomic_write_json(folder / "meta.json", {"key": "symbol:EXMP", "calendar": {"filings": [
        {"accession": "0000000007-26-000011", "form": "10-Q", "filed": "2026-10-30",
         "file": "0000000007-26-000011-xbrl.zip", "document": d} for d in documents]}})
    return folder


def test_compact_trims_a_whole_archive_to_the_documents_the_calendar_reads(tmp_path, capsys):
    data = tmp_path / "data"
    folder = filing_run(data)
    meta = json.loads((folder / "meta.json").read_text())
    before = sec_calendar.parse(folder, meta, provider="sec-edgar")
    assert compact.run(data, dry_run=False, min_age_hours=0) == 0
    assert sorted(p.name for p in folder.iterdir()) == [
        "0000000007-26-000011-xbrl.zip.zst", "0000000007-26-000012-xbrl.zip", "meta.json"]
    kept = read_bytes(folder, "0000000007-26-000011-xbrl.zip")
    assert kept == sec_calendar.trimmed(whole_archive(), PRIMARY)
    assert sec_calendar.parse(folder, meta, provider="sec-edgar") == before and before
    assert "trim: changed 1 files" in capsys.readouterr().out


def test_an_archive_that_does_not_trim_is_kept_whole_compressed(tmp_path, caplog):
    data = tmp_path / "data"
    # A document whose bytes fail their CRC.
    broken = whole_archive(zipfile.ZIP_STORED).replace(b"contextRef", b"contextReg", 1)
    folder = filing_run(data, archive=broken)
    compact.run(data, dry_run=False, min_age_hours=0)
    assert read_bytes(folder, "0000000007-26-000011-xbrl.zip") == broken
    assert not (folder / "0000000007-26-000011-xbrl.zip").exists()
    assert "kept whole, as it does not trim" in caplog.text


def test_a_trim_that_stopped_half_way_is_done_again(tmp_path):
    data = tmp_path / "data"
    folder = filing_run(data)
    name = "0000000007-26-000011-xbrl.zip"
    # What a stop leaves: the trimmed file, its frame, the archive's twin.
    put(folder / f"{name}.trim", b"half")
    put(folder / f"{name}.trim.zst", b"half")
    put(folder / f"{name}.zst", b"half")
    compact.run(data, dry_run=False, min_age_hours=0)
    assert sorted(p.name for p in folder.iterdir() if p.name.startswith(name)) == [f"{name}.zst"]
    assert read_bytes(folder, name) == sec_calendar.trimmed(whole_archive(), PRIMARY)


def test_a_dry_run_trims_nothing_and_a_second_run_changes_nothing(tmp_path, capsys):
    data = tmp_path / "data"
    filing_run(data)
    before = tree(data)
    compact.run(data, dry_run=True, min_age_hours=0)
    assert tree(data) == before
    dry = capsys.readouterr().out
    compact.run(data, dry_run=False, min_age_hours=0)
    done = tree(data)
    assert dry.split("\n")[0].replace("would change", "changed").replace("frees", "freed") == \
        capsys.readouterr().out.split("\n")[0]
    compact.run(data, dry_run=False, min_age_hours=0)
    assert tree(data) == done


def test_an_archive_named_twice_trims_once_and_with_two_primaries_is_kept_whole(tmp_path, caplog):
    folder = filing_run(tmp_path / "a", documents=(PRIMARY, PRIMARY))
    compact.run(tmp_path / "a", dry_run=False, min_age_hours=0)
    assert read_bytes(folder, "0000000007-26-000011-xbrl.zip") == sec_calendar.trimmed(whole_archive(), PRIMARY)
    folder = filing_run(tmp_path / "b", documents=(PRIMARY, "other.htm"))
    compact.run(tmp_path / "b", dry_run=False, min_age_hours=0)
    assert read_bytes(folder, "0000000007-26-000011-xbrl.zip") == whole_archive()
    assert "named with two primary documents; kept whole" in caplog.text


def test_an_archive_whose_filing_names_no_primary_document_trims_as_download_does(tmp_path):
    folder = filing_run(tmp_path / "data", documents=(None,))
    compact.run(tmp_path / "data", dry_run=False, min_age_hours=0)
    kept = read_bytes(folder, "0000000007-26-000011-xbrl.zip")
    assert kept == sec_calendar.trimmed(whole_archive(), None) != whole_archive()
    with zipfile.ZipFile(io.BytesIO(kept)) as z:
        assert z.namelist() == [PRIMARY, "exmp-10q2.htm"]


def test_documents_that_read_otherwise_trimmed_keep_the_archive_whole(tmp_path, monkeypatch, caplog):
    folder = filing_run(tmp_path / "data")
    # A trim that loses a document: here, every one.
    empty = io.BytesIO()
    zipfile.ZipFile(empty, "w").close()
    monkeypatch.setattr(sec_calendar, "trimmed", lambda data, primary: empty.getvalue())
    compact.run(tmp_path / "data", dry_run=False, min_age_hours=0)
    assert read_bytes(folder, "0000000007-26-000011-xbrl.zip") == whole_archive()
    assert "its documents read otherwise trimmed; kept whole" in caplog.text


def test_every_step_plans_from_the_runs_quiet_at_the_start(tmp_path):
    # The trim makes the run look young; the compression still runs.
    data = tmp_path / "data"
    folder = filing_run(data)
    put(folder / "submissions.json", SUB)
    young = filing_run(data, slug="20260702T010000Z")
    old = time.time() - 7200
    for p in [data / "20260701T010000Z", *(data / "20260701T010000Z").rglob("*")]:
        os.utime(p, (old, old), follow_symlinks=False)
    compact.run(data, dry_run=False, min_age_hours=1)
    assert sorted(p.name for p in folder.iterdir()) == [
        "0000000007-26-000011-xbrl.zip.zst", "0000000007-26-000012-xbrl.zip", "meta.json", "submissions.json.zst"]
    assert (young / "0000000007-26-000011-xbrl.zip").is_file()
