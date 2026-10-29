# envfile-lib.sh — read a bash env or config file the way AGENTS.md §3
# sets: check it with `bash -n`, then source it with every assignment
# exported. POSIX sh; sourced by wrapper-lib.sh, host-lib.sh, mcp and
# web. Each caller decides what a file that does not parse means: a
# warning and a skip, or a stop.

# wealthdb_source_bash FILE: source FILE with `set -a` when `bash -n`
# accepts it, and return 1, sourcing nothing, when it does not. The
# check's error output is dropped: bash quotes the offending line, and
# in an env file that line may hold a secret.
wealthdb_source_bash() {
    bash -n "$1" 2>/dev/null || return 1
    set -a
    # shellcheck disable=SC1090
    . "$1"
    set +a
}
