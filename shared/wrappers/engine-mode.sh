# engine-mode.sh — what ./configure recorded for a tree: the kind of
# engine it runs, and the tools a native engine needs. POSIX sh; sourced
# by the engine wrappers, the collector wrappers, web and mcp.
#
# The record is config.mk at the tree's root, `KEY=value` lines that the
# Makefile includes. It is read with sed here and never sourced. A tree
# with no config.mk runs the container engine. No environment variable
# names the mode: every shell that runs one tree runs one kind of engine.
# The tests alone name another record in WEALTHDB_BUILD_CONFIG, so that
# they run the same in a checkout of either kind.

# wealthdb_build_setting ROOT KEY: print the value config.mk at ROOT
# gives KEY, or nothing. Blanks around the `=` and after the value are
# no part of it, so a line edited by hand reads as it does to make.
wealthdb_build_setting() {
    _wbs="${WEALTHDB_BUILD_CONFIG:-$1/config.mk}"
    [ -f "$_wbs" ] || return 0
    sed -n "s/^$2[[:blank:]]*=[[:blank:]]*//p" "$_wbs" | sed -n -e 's/[[:blank:]]*$//' -e '1p'
}

# wealthdb_engine_mode ROOT: print `container` or `native`; exit status
# 2, with a message on stderr, when config.mk names another.
wealthdb_engine_mode() {
    _wem="$(wealthdb_build_setting "$1" ENGINE)"
    case "${_wem:-container}" in
        container | native) printf '%s\n' "${_wem:-container}" ;;
        *)
            printf '%s/config.mk: ENGINE=%s is no kind of engine; run ./configure again\n' "$1" "$_wem" >&2
            return 2
            ;;
    esac
}

# wealthdb_native_engine ROOT: print the native engine's path in the
# tree at ROOT. It is built by `wealthdb build`.
wealthdb_native_engine() {
    printf '%s/wealthdb/wealthdb-engine\n' "$1"
}

# wealthdb_require_native_engine WHO ROOT: succeed when the native engine
# is built; else say how to build it, and fail.
wealthdb_require_native_engine() {
    [ -x "$(wealthdb_native_engine "$2")" ] && return 0
    printf '%s: the engine is not built yet. Run: make all\n' "$1" >&2
    return 1
}

# wealthdb_tool ROOT KEY FALLBACK: print the tool config.mk records as
# KEY (GO, PYTHON, JAVA), else FALLBACK.
wealthdb_tool() {
    _wt="$(wealthdb_build_setting "$1" "$2")"
    printf '%s\n' "${_wt:-$3}"
}
