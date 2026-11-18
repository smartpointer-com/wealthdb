"""Credential + .env handling shared across collectors.

An env file is a bash script (repo AGENTS.md §3), so it is read the way a
shell reads it. `bash -n` checks its syntax first. Then a bash subprocess
sources it under `set -a` and prints its environment with `env -0`.
Quoting and escaping are bash's:

- single quotes keep every character as written;
- inside double quotes, `$`, backticks and backslashes stay special;
- an unquoted `#` after a space starts a comment, and an unquoted space
  ends the value.

No parser here stands in for bash, so a file means the same to a
collector as to the wrapper that sources it.

:func:`read_env_file` returns what the file set. :func:`load` picks the
file and applies it to ``os.environ`` by one rule: the file's values win
over the environment's.

Credentials are resolved with the value-with-env-fallback pattern (an
explicit flag value wins, else the env var), never a `--x-env NAME`
indirection.
"""
from __future__ import annotations

import errno
import os
import re
import subprocess
from pathlib import Path

# Shell bookkeeping variables, and the variables that steer process lookup,
# never taken from an env file into the collector's environment.
BASH_VAR_BLOCKLIST = frozenset({
    "PWD", "OLDPWD", "SHLVL", "_", "PATH", "SHELL", "HOME", "PIPESTATUS",
})

# Variables that make a non-interactive bash read a startup file. Dropped
# from the subprocess's environment, so only the env file itself runs.
_STARTUP_FILE_VARS = ("BASH_ENV", "ENV")

# Prints the environment before and after sourcing the file, separated by
# one extra NUL. `env -0` ends every entry with a NUL, so the separator
# reads as the one empty entry. The file is `$0`, so it receives no
# positional arguments. Its stdout is dropped: only `env -0` writes there.
# `command -p` finds `env` even when the file sets PATH.
_SOURCE_SCRIPT = (r'command -p env -0; printf "\0"; set -a; '
                  r'source "$0" >/dev/null; set +a; command -p env -0')

_BASH = ("bash", "--noprofile", "--norc")


def _error_lines(stderr: bytes) -> str:
    """`` at line N, M`` from bash's error output, or "". Only the numbers
    are kept: bash quotes the offending line, and it may hold a secret."""
    lines = sorted({int(n) for n in re.findall(rb": line (\d+): ", stderr)})
    return f" at line {', '.join(map(str, lines))}" if lines else ""


def _parse_env0(entries: list[bytes]) -> dict[str, str]:
    env = {}
    for entry in entries:
        k, _sep, v = entry.partition(b"=")
        try:
            env[k.decode("utf-8")] = v.decode("utf-8")
        except UnicodeDecodeError:
            continue
    return env


def read_env_file(path: Path) -> dict[str, str]:
    """The variables the env file at `path` sets or changes, as bash reads it.

    The subprocess inherits this process's environment, so the file can
    refer to it (``$HOME``). Only what the file set to a new value is
    returned: never the inherited environment, nor a key in
    :data:`BASH_VAR_BLOCKLIST`. A variable the file unsets is ignored.

    Raises FileNotFoundError when `path` is no file. Raises ValueError when
    bash reports a syntax error, or an error while sourcing (a command not
    found, an unquoted space in a value). The error names the file and the
    line numbers only, never the line: it may hold a secret.
    """
    path = Path(path)
    if not path.is_file():
        raise FileNotFoundError(errno.ENOENT, "no env file", str(path))
    chk = subprocess.run([*_BASH, "-n", str(path)], capture_output=True)
    if chk.returncode != 0:
        raise ValueError(f"env file {path} is not valid bash"
                         f"{_error_lines(chk.stderr)}. The line is not "
                         f"shown, since it may hold a secret.")
    env = {k: v for k, v in os.environ.items() if k not in _STARTUP_FILE_VARS}
    # Absolute: `source` looks a slash-free name up in PATH first.
    result = subprocess.run(
        [*_BASH, "-c", _SOURCE_SCRIPT, str(path.absolute())],
        env=env, capture_output=True)
    if result.returncode != 0 or result.stderr:
        raise ValueError(f"env file {path} failed when sourced"
                         f"{_error_lines(result.stderr)}. The line is not "
                         f"shown, since it may hold a secret.")
    # No `env -0` entry is empty, so the first empty one is the separator.
    entries = result.stdout.split(b"\x00")
    cut = entries.index(b"")
    old = _parse_env0(entries[:cut])
    new = _parse_env0([e for e in entries[cut + 1:] if e])
    return {k: v for k, v in new.items()
            if k not in BASH_VAR_BLOCKLIST and old.get(k) != v}


def load(explicit, *, env_var: str | None = None, candidates=(),
         logger) -> Path | None:
    """Source one env file into ``os.environ``, its values winning. Returns
    the path sourced, or None when there is none to source.

    The file is the first of:

    - `explicit`, an ``--env-file`` flag;
    - the path in the environment variable `env_var`;
    - the first of `candidates` that is a file. A None candidate is
      skipped, and so is one that does not exist.

    A file named by the flag or the variable that does not exist exits: a
    file that was asked for is never silently replaced by a candidate or by
    the environment. A bash error exits too, with :func:`read_env_file`'s
    message.

    A value the file replaces is logged when the environment held a
    different one, by length only, never the value.
    """
    if explicit is not None:
        path, named = Path(explicit), "--env-file"
    elif env_var and os.environ.get(env_var):
        path, named = Path(os.environ[env_var]), env_var
    else:
        path = next((Path(c) for c in candidates
                     if c is not None and Path(c).is_file()), None)
        named = None
    if path is None:
        return None
    if named is not None and not path.is_file():
        raise SystemExit(f"{named} names no file: {path}")
    try:
        values = read_env_file(path)
    except ValueError as e:
        raise SystemExit(str(e)) from e
    for key, value in values.items():
        prior = os.environ.get(key)
        if prior is not None and prior != value:
            logger.warning(
                "%s from the environment (len=%d) differs from the value "
                "in %s (len=%d); using the file's value.",
                key, len(prior), path, len(value))
        os.environ[key] = value
    logger.info("env sourced from %s", path)
    return path


def resolve_credential(value, env_name: str, flag_name: str) -> str:
    """Return the credential: an explicit `value` wins, else the env var
    `env_name`, else exit with a helpful message naming `flag_name`."""
    if value:
        return value
    env_value = os.environ.get(env_name)
    if env_value:
        return env_value
    raise SystemExit(
        f"Missing credential: pass {flag_name} or set {env_name}."
    )
