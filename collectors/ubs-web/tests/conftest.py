"""Pytest configuration for ubs-web-dump.

Puts the repo root on `sys.path` so test modules can `import
pdf_parsers`, `load`, etc. without an installable package layout.
"""
from __future__ import annotations
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
