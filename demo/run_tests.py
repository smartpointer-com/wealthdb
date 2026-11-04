"""Runs the demo's tests side by side.

One process runs each test class, so the engine cases that build and
load a household overlap. A module that builds a household once for all
its classes (setUpModule) runs as one process instead. Each process's
output prints once it ends, in discovery order, and the run fails when
any process fails.

    python3 demo/run_tests.py [-j JOBS]
"""

import argparse
import concurrent.futures
import os
import pathlib
import re
import subprocess
import sys
import time
import unittest

DEMO = pathlib.Path(__file__).resolve().parent


def groups():
    """The test names to run, one per process, in discovery order."""
    sys.path.insert(0, str(DEMO))
    suite = unittest.TestLoader().discover(str(DEMO / "tests"), top_level_dir=str(DEMO))
    names = []

    def walk(s):
        for t in s:
            if isinstance(t, unittest.TestSuite):
                walk(t)
                continue
            module, cls = type(t).__module__, type(t).__name__
            if module == "unittest.loader":
                # A module that failed to import: run it, so it fails.
                name = t.id().removeprefix("unittest.loader._FailedTest.")
            elif hasattr(sys.modules[module], "setUpModule"):
                name = module
            else:
                name = f"{module}.{cls}"
            if name not in names:
                names.append(name)

    walk(suite)
    return names


def run(name):
    start = time.monotonic()
    out = subprocess.run([sys.executable, "-m", "unittest", name], cwd=DEMO,
                         capture_output=True, text=True)
    return out.returncode, out.stdout + out.stderr, time.monotonic() - start


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("-j", "--jobs", type=int, default=os.cpu_count() or 4)
    args = ap.parse_args()
    names = groups()
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        results = list(pool.map(run, names))
    failed, ran = [], 0
    for name, (code, output, secs) in zip(names, results, strict=True):
        print(f"--- {name} ({secs:.1f}s)")
        print(output.rstrip())
        m = re.search(r"^Ran (\d+) tests?", output, re.M)
        ran += int(m.group(1)) if m else 0
        if code != 0:
            failed.append(name)
    print(f"demo tests: {ran} tests in {len(names)} processes, {len(failed)} failed")
    for name in failed:
        print(f"  failed: {name}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
