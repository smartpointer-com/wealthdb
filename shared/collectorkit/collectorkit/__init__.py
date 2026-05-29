"""collectorkit — shared utilities for wealthdb silver collectors.

Bundles the infrastructure every collector repeated by hand: bash-sourced
env-file loading + credential resolution, the SQLite migration runner and
connection setup, bronze-artifact writing, and CLI/logging helpers.
"""
from collectorkit import bronze, cli, envfile, silver  # noqa: F401

__all__ = ["bronze", "cli", "envfile", "silver"]
