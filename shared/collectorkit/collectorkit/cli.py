"""Shared CLI helpers: the logging format every collector used and a
common ``--verbose`` argument."""
from __future__ import annotations

import argparse
import logging

LOG_FORMAT = "%(asctime)s %(levelname)s %(name)s: %(message)s"


def configure_logging(verbose: bool = False) -> None:
    """Set up root logging with the shared collector format."""
    logging.basicConfig(
        level=logging.DEBUG if verbose else logging.INFO,
        format=LOG_FORMAT,
    )


def add_common_args(parser: argparse.ArgumentParser) -> None:
    """Add the flags every collector CLI shares."""
    parser.add_argument(
        "-v", "--verbose", action="store_true", help="DEBUG-level logging.",
    )
