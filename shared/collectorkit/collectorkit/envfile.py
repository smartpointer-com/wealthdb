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

:func:`read_env_file` returns what the file set. Two loaders apply it to
``os.environ``:

- :func:`source_env_file` lets either the environment or the file win,
  for every key.
- :func:`load_env_file` lets the file win for named credential keys, and
  the environment win for the rest.

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


def source_env_file(path: Path, prefer_file: bool = False) -> bool:
    """Source `path` as bash and merge what it sets into ``os.environ``.

    By default an existing environment value wins (``setdefault``
    semantics, for the case where the calling shell already exported the
    credentials). ``prefer_file=True`` lets the file's value win instead.
    Returns False if the file is absent. Raises ValueError on a bash error
    (see :func:`read_env_file`).
    """
    path = Path(path)
    if not path.is_file():
        return False
    for key, val in read_env_file(path).items():
        if prefer_file:
            os.environ[key] = val
        else:
            os.environ.setdefault(key, val)
    return True


def source_named_env_file(explicit: Path | None, env_var: str) -> Path | None:
    """Source the env file a run names, so that its values win.

    The file is `explicit` (an ``--env-file`` flag), else the path in the
    environment variable `env_var`. Neither set returns None and sources
    nothing. A named file that does not exist exits: a file that was asked
    for is never silently replaced by whatever the environment holds. A
    bash error exits too, with :func:`read_env_file`'s message. Returns the
    path sourced.
    """
    path = explicit or (Path(os.environ[env_var])
                        if os.environ.get(env_var) else None)
    if path is None:
        return None
    if not Path(path).is_file():
        raise SystemExit(f"{'--env-file' if explicit else env_var} names no file: {path}")
    try:
        source_env_file(path, prefer_file=True)
    except ValueError as e:
        raise SystemExit(str(e)) from e
    return Path(path)


def resolve_env_file(arg_path, candidates) -> Path | None:
    """Return `arg_path` if given, else the first existing path in
    `candidates`, else None."""
    if arg_path is not None:
        return Path(arg_path)
    for candidate in candidates:
        if Path(candidate).is_file():
            return Path(candidate)
    return None


def load_env_file(path, override_vars, *, logger,
                  warn_on_override: bool = False) -> None:
    """Source `path` as bash and merge what it sets into ``os.environ``.

    ``override_vars`` names the keys whose file value wins over a value
    already in the environment (credentials: the file is authoritative).
    Every other key uses ``setdefault`` (the file is a fallback). With
    ``warn_on_override``, an inherited value that differs from the file's
    is logged via ``logger``, by length only, never the secret.

    Raises FileNotFoundError when `path` is no file. A bash error raises
    SystemExit with :func:`read_env_file`'s message.
    """
    path = Path(path)
    logger.debug("loading env file: %s", path)
    try:
        values = read_env_file(path)
    except ValueError as e:
        raise SystemExit(str(e)) from e
    for key, value in values.items():
        if key not in override_vars:
            os.environ.setdefault(key, value)
            continue
        prior = os.environ.get(key)
        if warn_on_override and prior is not None and prior != value:
            logger.warning(
                "%s from the environment (len=%d) differs from the value "
                "in %s (len=%d); using the file's value.",
                key, len(prior), path, len(value))
        os.environ[key] = value


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
