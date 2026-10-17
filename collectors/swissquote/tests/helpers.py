"""Helpers the swissquote test modules share. Each test module puts the
collector root on sys.path before importing this one."""

from __future__ import annotations

import sqlite3
from pathlib import Path

import xlwt

import load

MIGRATIONS = Path(load.__file__).resolve().parent / "migrations"


def apply_all_migrations(db_path: Path) -> sqlite3.Connection:
    """A silver DB at `db_path` with every migration applied."""
    conn = load.open_db(db_path)
    load.silver.apply_migrations(conn, MIGRATIONS)
    return conn


def write_xls(path: Path, rows: list[list]) -> Path:
    """An .xls workbook at `path` whose one sheet holds `rows`."""
    wb = xlwt.Workbook()
    sheet = wb.add_sheet("export")
    for r, row in enumerate(rows):
        for c, val in enumerate(row):
            sheet.write(r, c, val)
    wb.save(str(path))
    return path
