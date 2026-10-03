"""Credential + .env handling shared across collectors.

Two loaders live here, for two different needs:

- :func:`source_env_file` *sources the file via bash* (honoring full bash
  syntax — quoting, `export`, variable references). Preferred for env
  files that hold ordinary configuration.

- :func:`load_env_file` *hand-parses* KEY=VALUE, reading each value
  byte-for-byte and stripping only a matched pair of outer quotes. This
  exists for the browser-login collectors' credential files: bash
  sourcing would `$`-expand, run backtick/`$()` command substitution, and
  strip unquoted trailing `#` comments, silently mangling a password that
  contains those characters even when it is *double-quoted*. Measured
  divergences of a bash source vs. this loader, for shapes a real
  credential file can hit:
    * ``PW="ab$cd"`` / ``PW=ab$cd``  → bash yields ``ab`` (``$cd`` expands)
    * ``PW="a`cmd`b"``               → bash runs ``cmd`` (substitution)
    * CRLF line endings             → bash keeps a trailing ``\\r``
    * ``KEY=value # note``          → bash strips the inline comment
    * ``KEY = value`` / unquoted spaces → bash treats it as a command
  Single-quoted values match under both. The hand parser preserves the
  literal bytes, so credentials survive regardless of quoting style.

Credentials are resolved with the value-with-env-fallback pattern (an
explicit flag value wins, else the env var), never a `--x-env NAME`
indirection.
"""
from __future__ import annotations

import os
import re
import shlex
import subprocess
from pathlib import Path

# Shell bookkeeping variables to drop when merging a sourced env, so they
# don't leak into the child process environment.
BASH_VAR_BLOCKLIST = frozenset({
    "PWD", "OLDPWD", "SHLVL", "_", "PATH", "SHELL", "HOME", "PIPESTATUS",
})


def source_env_file(path: Path, prefer_file: bool = False) -> bool:
    """Syntax-check then source `path` as bash and merge its KEY=VALUE
    bindings into ``os.environ``. By default an existing environment
    value wins (``setdefault`` semantics, for the cron/nightly case
    where the parent shell already exported credentials). Pass
    ``prefer_file=True`` to let the file's value win instead — the
    Playwright login collectors do this so a password with shell
    metacharacters reaches the browser exactly as written in the
    env file rather than as a possibly-mangled inherited value.
    Returns False if the file is absent; raises ValueError on a bash
    syntax error. The error names the file and the line numbers only:
    bash quotes the offending line, and that line may hold a secret.
    """
    path = Path(path)
    if not path.is_file():
        return False
    chk = subprocess.run(
        ["bash", "--noprofile", "--norc", "-n", str(path)],
        capture_output=True,
    )
    if chk.returncode != 0:
        lines = sorted({int(n) for n in
                        re.findall(rb": line (\d+): ", chk.stderr)})
        where = (f" at line {', '.join(map(str, lines))}" if lines else "")
        raise ValueError(f"env file {path} is not valid bash{where}. The "
                         f"line is not shown, since it may hold a secret.")
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
        if prefer_file:
            os.environ[key] = val
        else:
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


def _strip_outer_quotes(s: str) -> str:
    """Strip a matched pair of leading+trailing single or double quotes
    from ``s``. Single-side strips (e.g. ``"foo``) are left alone — they
    are more likely a real value than a syntax slip."""
    if len(s) >= 2 and s[0] == s[-1] and s[0] in ("'", '"'):
        return s[1:-1]
    return s


def load_env_file(path, override_vars, *, logger,
                  warn_on_override: bool = False) -> None:
    """Hand-parse KEY=VALUE pairs from ``path`` into ``os.environ``.

    Unlike :func:`source_env_file` this does NOT hand the file to bash;
    it reads each value byte-for-byte, stripping only a matched pair of
    outer quotes, so a credential that contains ``$``, backticks, ``#``
    or shell metacharacters survives intact regardless of quoting (see
    the module docstring for the bash-source divergence table).

    ``override_vars`` names the keys whose file value wins over an
    already-set host env var (credentials — the file is authoritative);
    every other key uses ``setdefault`` (the env-file is a fallback).
    With ``warn_on_override`` a mismatch between an inherited value and
    the file value is logged via ``logger`` (lengths only, never the
    secret).

    Lines beginning with ``#`` and blank lines are ignored; a leading
    ``export`` is stripped; malformed lines raise ``SystemExit``.
    """
    path = Path(path)
    logger.debug("loading env file: %s", path)
    with path.open("r", encoding="utf-8") as fh:
        for lineno, raw in enumerate(fh, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("export "):
                line = line[len("export "):]
            if "=" not in line:
                # Not shown: such a line is often a value pasted on a
                # line of its own, and the value may be a secret.
                raise SystemExit(
                    f"env file {path}:{lineno}: not a KEY=VALUE line"
                )
            key, _, value = line.partition("=")
            key = key.strip()
            value = _strip_outer_quotes(value.strip())
            if not key:
                raise SystemExit(f"env file {path}:{lineno}: empty key")
            if key in override_vars:
                if warn_on_override:
                    prior = os.environ.get(key)
                    if prior is not None and prior != value:
                        logger.warning(
                            "%s inherited from host env (len=%d) differs "
                            "from %s file value (len=%d); using file value. "
                            "(Use SINGLE quotes for values containing $/!/"
                            "backtick to avoid host `source` mangling.)",
                            key, len(prior), path, len(value),
                        )
                os.environ[key] = value
            else:
                os.environ.setdefault(key, value)


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
