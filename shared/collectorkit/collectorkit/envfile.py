"""Credential + .env handling shared across collectors.

Env files are *sourced via bash* (honoring full bash syntax — quoting,
`export`, variable references) rather than hand-parsed with a lossy
KEY=VALUE splitter. Credentials are resolved with the value-with-env-
fallback pattern (an explicit flag value wins, else the env var), never a
`--x-env NAME` indirection.
"""
from __future__ import annotations

import os
import shlex
import subprocess
from pathlib import Path

# Shell bookkeeping variables to drop when merging a sourced env, so they
# don't leak into the child process environment.
BASH_VAR_BLOCKLIST = frozenset({
    "PWD", "OLDPWD", "SHLVL", "_", "PATH", "SHELL", "HOME", "PIPESTATUS",
})


def source_env_file(path: Path) -> bool:
    """Syntax-check then source `path` as bash and merge its KEY=VALUE
    bindings into ``os.environ`` via ``setdefault`` (anything already in the
    environment wins). Returns False if the file is absent; raises
    ValueError on a bash syntax error.
    """
    path = Path(path)
    if not path.is_file():
        return False
    chk = subprocess.run(
        ["bash", "--noprofile", "--norc", "-n", str(path)],
        capture_output=True,
    )
    if chk.returncode != 0:
        stderr = (chk.stderr or b"").decode("utf-8", errors="replace").rstrip()
        raise ValueError(f"env file {path} has bash syntax errors:\n{stderr}")
    quoted = shlex.quote(str(path))
    result = subprocess.run(
        ["bash", "--noprofile", "--norc", "-c",
         f"set -a; source {quoted}; set +a; env -0"],
        env={"PATH": os.environ.get("PATH", "/usr/bin:/bin")},
        capture_output=True, check=True,
    )
    for entry in result.stdout.split(b"\x00"):
        if not entry:
            continue
        k, _sep, v = entry.partition(b"=")
        try:
            key, val = k.decode("utf-8"), v.decode("utf-8")
        except UnicodeDecodeError:
            continue
        if key in BASH_VAR_BLOCKLIST:
            continue
        os.environ.setdefault(key, val)
    return True


def resolve_env_file(arg_path, candidates) -> Path | None:
    """Return `arg_path` if given, else the first existing path in
    `candidates`, else None."""
    if arg_path is not None:
        return Path(arg_path)
    for candidate in candidates:
        if Path(candidate).is_file():
            return Path(candidate)
    return None


def load_env(arg_path, candidates) -> Path | None:
    """Resolve (flag → candidates) and source an env file. Returns the path
    that was sourced, or None if none was found."""
    path = resolve_env_file(arg_path, candidates)
    if path is not None and source_env_file(path):
        return path
    return None


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
