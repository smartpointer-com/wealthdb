#!/usr/bin/env python3
"""Time each dashboard tile's query on a gold database, with the DuckDB
release and the settings Metabase runs it with.

    python3 web/tile_timings.py DUMP DB OUT.tsv [--wide] [--today DAY]
                                [--timeout SECONDS] [--python PATH]

DUMP is the payload dump of `provision.py --dump`. Each native tile runs
at its dashboard's defaults: a picker mapped onto a template tag gives
its default, and any other tag keeps its own. Field filters and dates
render as Metabase renders them. With --wide no tile takes a time window,
so each reads the whole history. Each model's query runs too. An MBQL
tile is listed as skipped: Metabase compiles its SQL.

Each query runs in a fresh process, one after the other. The process
opens DB read-only with the connection settings provisioning gives the
gold database, and spills to a temporary directory of its own. OUT.tsv
gets one line per query: dashboard, card, kind, status, exit status,
query seconds, process seconds, peak memory in MB and row count. The
peak is the process's own, Python included. No value of a row and no
error text is written: a failed query keeps only the name of its error.

Metabase reads gold through its DuckDB driver, which bundles one DuckDB
release. The driver's version is that release with one more part, so the
driver that web/Dockerfile pins names the release this tool runs. A
query runs under a Python whose duckdb module is that release. By
default it is the virtual environment
$XDG_CACHE_HOME/wealthdb/tile-timings/duckdb-RELEASE, which the first
run creates with pip ($HOME/.cache when XDG_CACHE_HOME is unset).
--python names another interpreter. The run stops before the first
query when that interpreter's duckdb is another release.
"""
import argparse
import contextlib
import datetime as dt
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import venv

HERE = pathlib.Path(__file__).resolve().parent

# The DuckDB settings provisioning gives the gold database
# (provision.ensure_database). The driver spills to "<database>.tmp";
# a read-only gold may sit where nothing can be written, so a query
# spills to a temporary directory instead.
MEMORY_LIMIT, THREADS = "4GB", "8"

COLUMNS = ("dashboard", "card", "kind", "status", "exit", "query_seconds",
           "process_seconds", "peak_rss_mb", "rows")

# The process that runs one query: argv is the database, the spill
# directory and the release it must run; the SQL arrives on stdin. It
# prints one JSON object: rows and seconds, or the name of the error.
CHILD = """
import json, sys, time
import duckdb
db, spill, release, memory, threads = sys.argv[1:6]
if duckdb.__version__ != release:
    print(json.dumps({"release": duckdb.__version__}))
    sys.exit(3)
sql = sys.stdin.read()
con = duckdb.connect(db, read_only=True,
                     config={"memory_limit": memory, "threads": threads, "temp_directory": spill})
# No progress bar: on a query of more than a few seconds it writes to
# stdout, after the one JSON line the parent reads.
con.execute("SET enable_progress_bar = false")
start = time.perf_counter()
try:
    cur = con.execute(sql)
    rows = 0
    while True:
        chunk = cur.fetchmany(2048)
        if not chunk:
            break
        rows += len(chunk)
except duckdb.Error as e:
    print(json.dumps({"error": type(e).__name__, "seconds": time.perf_counter() - start}))
    sys.exit(1)
print(json.dumps({"rows": rows, "seconds": time.perf_counter() - start}))
"""


def duckdb_release(dockerfile=HERE / "Dockerfile"):
    """The DuckDB release the driver in `dockerfile` bundles: its
    version without the last part (driver 1.5.3.0 bundles 1.5.3)."""
    m = re.search(r"metabase_duckdb_driver/releases/download/(\d+\.\d+\.\d+)\.\d+/",
                  pathlib.Path(dockerfile).read_text())
    if not m:
        raise SystemExit(f"tile_timings: no DuckDB driver pin in {dockerfile}")
    return m.group(1)


def cache_python(release):
    """The interpreter of the cached environment for `release`, created
    with pip on first use."""
    root = pathlib.Path(os.environ.get("XDG_CACHE_HOME") or pathlib.Path.home() / ".cache")
    env = root / "wealthdb" / "tile-timings" / f"duckdb-{release}"
    python = env / "bin" / "python"
    if python.exists():
        return python
    env.parent.mkdir(parents=True, exist_ok=True)
    # Built beside its place and moved in whole, so a failed install
    # leaves nothing a later run would take for ready.
    work = pathlib.Path(tempfile.mkdtemp(prefix=env.name + ".", dir=env.parent))
    try:
        venv.create(work, with_pip=True)
        subprocess.run([str(work / "bin" / "python"), "-m", "pip", "install", "--quiet", "--disable-pip-version-check",
                        f"duckdb=={release}"], check=True)
        work.rename(env)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    return python


def module_release(python):
    """The release of `python`'s duckdb module, or None without one."""
    out = subprocess.run([str(python), "-I", "-c", "import duckdb; print(duckdb.__version__)"],
                         capture_output=True, text=True)
    return out.stdout.strip() if out.returncode == 0 else None


# ---- rendering a native query ----------------------------------------

def _shift_months(day, months):
    m = day.year * 12 + day.month - 1 + months
    return dt.date(m // 12, m % 12 + 1, 1)


def _unit_start(day, unit):
    """The first day of the `unit` that holds `day`. A week starts on
    Sunday, Metabase's default."""
    if unit == "day":
        return day
    if unit == "week":
        return day - dt.timedelta(days=(day.weekday() + 1) % 7)
    if unit == "month":
        return day.replace(day=1)
    if unit == "quarter":
        return dt.date(day.year, (day.month - 1) // 3 * 3 + 1, 1)
    return dt.date(day.year, 1, 1)


def _add_units(start, unit, n):
    """`start`, the first day of a `unit`, moved by `n` of them."""
    if unit == "day":
        return start + dt.timedelta(days=n)
    if unit == "week":
        return start + dt.timedelta(weeks=n)
    return _shift_months(start, n * {"month": 1, "quarter": 3, "year": 12}[unit])


def date_window(value, today):
    """(first day, last day) of a date picker value, either end None
    where the value leaves it open: a relative window (`past12months~`,
    `thisyear`, `thisday`), a range `A~B`, `A~` or `~B`, or one day."""
    m = re.fullmatch(r"(past|next)(\d+)(day|week|month|quarter|year)s(~)?", value)
    if m:
        way, n, unit, current = m.group(1), int(m.group(2)), m.group(3), bool(m.group(4))
        start = _unit_start(today, unit)
        if way == "past":
            first = _add_units(start, unit, -n)
            last = _add_units(start, unit, 1 if current else 0) - dt.timedelta(days=1)
        else:
            first = start if current else _add_units(start, unit, 1)
            last = _add_units(start, unit, n + 1) - dt.timedelta(days=1)
        return first, last
    m = re.fullmatch(r"this(day|week|month|quarter|year)", value)
    if m:
        start = _unit_start(today, m.group(1))
        return start, _add_units(start, m.group(1), 1) - dt.timedelta(days=1)
    if "~" in value:
        a, b = value.split("~", 1)
        return (dt.date.fromisoformat(a) if a else None), (dt.date.fromisoformat(b) if b else None)
    day = dt.date.fromisoformat(value)
    return day, day


def _literal(v):
    return "'" + str(v).replace("'", "''") + "'"


def _present(v):
    return v is not None and v != []


def render(query, values, fields, today):
    """The SQL of native `query` (a dataset_query) with its template
    tags filled: tag name -> value in `values`, else the tag's default.
    A variable becomes a literal, a field filter a predicate on its
    column (`fields`, field id -> (table, column)), and an optional
    clause whose tags are not all given drops out. A field filter with
    no value outside such a clause reads 1 = 1."""
    sql, tags = query["native"]["query"], query["native"]["template-tags"]

    def value(name):
        v = values[name] if name in values else tags[name].get("default")
        return v if _present(v) else None

    def fill(name):
        tag, v = tags[name], value(name)
        if tag["type"] == "dimension":
            if v is None:
                return "1 = 1"
            table, col = fields[tag["dimension"][1]]
            ref = f'"main"."{table}"."{col}"'
            if tag.get("widget-type", "").startswith("date"):
                first, last = date_window(v[0] if isinstance(v, list) else v, today)
                ends = ([f"{ref} >= DATE '{first}'"] if first else []) + \
                       ([f"{ref} < DATE '{last + dt.timedelta(days=1)}'"] if last else [])
                return "(" + " AND ".join(ends or ["1 = 1"]) + ")"
            vs = v if isinstance(v, list) else [v]
            lits = [str(x) if tag.get("widget-type", "").startswith("number") else _literal(x) for x in vs]
            return f"{ref} = {lits[0]}" if len(lits) == 1 else f"{ref} IN ({', '.join(lits)})"
        if isinstance(v, list):
            v = v[0]
        if v is None:
            return "NULL"
        return str(v) if tag["type"] == "number" else _literal(v)

    def clause(m):
        names = re.findall(r"\{\{\s*(\w+)\s*\}\}", m.group(1))
        if any(value(n) is None for n in names):
            return ""
        return re.sub(r"\{\{\s*(\w+)\s*\}\}", lambda x: fill(x.group(1)), m.group(1))

    sql = re.sub(r"\[\[(.*?)\]\]", clause, sql, flags=re.S)
    return re.sub(r"\{\{\s*(\w+)\s*\}\}", lambda x: fill(x.group(1)), sql)


# ---- the queries of a dump --------------------------------------------

def queries(dump, wide=False):
    """(dashboard, card, kind, query, tag values) for each tile of the
    dump's dashboards, in name and layout order, then for each model.
    kind is native, mbql or model; tag values are those of the tile's
    pickers at their defaults (none of a time window with `wide`)."""
    names = {i: n for n, i in dump["ids"]["cards"].items()}
    out = []
    for dname, dash in sorted(dump["dashboards"].items()):
        params = {x["id"]: x for x in dash["parameters"]}
        for dc in dash["dashcards"]:
            if not dc.get("card_id"):
                continue
            name = names[dc["card_id"]]
            query = dump["cards"][name]["dataset_query"]
            if query.get("type") != "native":
                out.append((dname, name, "mbql", query, {}))
                continue
            tags = query["native"]["template-tags"]
            values = {}
            for m in dc["parameter_mappings"]:
                target = m["target"][1]
                default = params.get(m["parameter_id"], {}).get("default")
                if target[0] == "template-tag" and target[1] in tags and _present(default):
                    values[target[1]] = default
            if wide:
                for tname, tag in tags.items():
                    if tag.get("widget-type") == "date/all-options":
                        values[tname] = None
            out.append((dname, name, "native", query, values))
    for mname, model in sorted(dump["models"].items()):
        out.append(("", mname, "model", model["dataset_query"], {}))
    return out


def run_query(python, db, sql, release, timeout):
    """One query in a fresh process: (status, exit status, query
    seconds, process seconds, peak MB, rows), the seconds and rows None
    where the query did not finish."""
    with tempfile.TemporaryDirectory(prefix="tile-timings.") as spill:
        start = time.monotonic()
        proc = subprocess.Popen([str(python), "-I", "-c", CHILD, str(db), spill, release, MEMORY_LIMIT, THREADS],
                                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        killed = threading.Event()

        def kill():
            killed.set()
            proc.kill()
        timer = threading.Timer(timeout, kill)
        timer.start()
        try:
            # A process that dies before it reads its SQL closes the pipe.
            with contextlib.suppress(BrokenPipeError):
                proc.stdin.write(sql.encode())
                proc.stdin.close()
            out = proc.stdout.read()
            # wait4 reaps the process with its own resource usage.
            _pid, status, usage = os.wait4(proc.pid, 0)
        finally:
            timer.cancel()
        proc.returncode = code = os.waitstatus_to_exitcode(status)
        proc.stdout.close()
        elapsed = time.monotonic() - start
    # ru_maxrss is bytes on macOS, kilobytes on Linux.
    peak = usage.ru_maxrss / (1 << 20 if sys.platform == "darwin" else 1 << 10)
    try:
        res = json.loads(out.decode().strip().splitlines()[-1])
    except (ValueError, IndexError):
        res = {}
    if code == 0 and "rows" in res:
        status = "ok"
    elif "error" in res:
        status = "error:" + res["error"]
    elif "release" in res:
        status = "release:" + res["release"]
    elif killed.is_set():
        status = "timeout"
    else:
        status = "died"
    return (status, code, res.get("seconds") if status == "ok" else None,
            elapsed, peak, res.get("rows"))


def _cell(v):
    if v is None:
        return ""
    if isinstance(v, float):
        return f"{v:.3f}"
    return str(v)


def main(argv=None):
    release = duckdb_release()
    ap = argparse.ArgumentParser(
        description=__doc__.split("\n\n")[0].replace("\n", " ")
        + f" This tree's driver bundles DuckDB {release}.",
        epilog="The DuckDB release: " + " ".join(__doc__.split("\n\n")[-1].split()),
        usage="%(prog)s DUMP DB OUT.tsv [--wide] [--today DAY] [--timeout SECONDS] [--python PATH]")
    ap.add_argument("dump", metavar="DUMP", help="the payload dump of provision.py --dump")
    ap.add_argument("db", metavar="DB", help="the gold database, opened read-only")
    ap.add_argument("out", metavar="OUT.tsv", help="the timings, one line per query")
    ap.add_argument("--wide", action="store_true", help="give no tile a time window")
    ap.add_argument("--today", type=dt.date.fromisoformat, default=dt.date.today(), metavar="DAY",
                    help="the day relative dates count from (default: today)")
    ap.add_argument("--timeout", type=float, default=300, metavar="SECONDS", help="seconds before a query is killed (300)")
    ap.add_argument("--python", metavar="PATH", help=f"an interpreter whose duckdb module is {release}")
    a = ap.parse_args(argv)

    if not os.path.isfile(a.db):
        ap.error(f"no database at {a.db}")
    dump = json.loads(pathlib.Path(a.dump).read_text(encoding="utf-8"))
    python = pathlib.Path(a.python) if a.python else cache_python(release)
    got = module_release(python)
    if got != release:
        raise SystemExit(f"tile_timings: {python} has duckdb {got or '(none)'}, the driver bundles {release}")
    fields = {i: tuple(key.split(".", 1)) for key, i in dump["ids"]["fields"].items()}

    lines, failed = ["\t".join(COLUMNS)], 0
    for dname, card, kind, query, values in queries(dump, a.wide):
        if kind == "mbql":
            lines.append("\t".join([dname, card, kind, "skipped", "", "", "", "", ""]))
            continue
        sql = render(query, values, fields, a.today)
        status, code, qs, ps, peak, rows = run_query(python, a.db, sql, release, a.timeout)
        failed += status != "ok"
        lines.append("\t".join([dname, card, kind, status] + [_cell(v) for v in (code, qs, ps, peak, rows)]))
    pathlib.Path(a.out).write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"tile_timings: {len(lines) - 1} queries on DuckDB {release}, {failed} did not run; "
          f"timings in {a.out}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
