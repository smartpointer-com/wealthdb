# demo-root.sh — the directory a checkout builds its demo household
# into. POSIX sh; read by the Makefile and demo/check_dashboards.py.
#
# WEALTHDB_DEMO_ROOT wins when it is set. Otherwise the first line of
# .demo-root at the repository root names the directory: the file is
# git-ignored, so a second checkout pins its own demo once and no shell
# in it can build into another checkout's. Without the file it is
# ~/wealthdb-demo. A variable or a .demo-root that names no directory is
# an error, never a fall back to the default.

# wealthdb_demo_root ROOT: print the demo root for the checkout at ROOT,
# a leading ~ as written; exit status 2, with a message on stderr, when
# WEALTHDB_DEMO_ROOT or ROOT/.demo-root is unusable.
wealthdb_demo_root() {
    if [ -n "${WEALTHDB_DEMO_ROOT:-}" ]; then
        _wdr_dir="$WEALTHDB_DEMO_ROOT"
        _wdr_from=WEALTHDB_DEMO_ROOT
    else
        _wdr_from="$1/.demo-root"
        if [ ! -e "$_wdr_from" ] && [ ! -L "$_wdr_from" ]; then
            printf '%s\n' '~/wealthdb-demo'
            return 0
        fi
        if [ ! -f "$_wdr_from" ] || [ ! -r "$_wdr_from" ]; then
            printf '%s is not a readable file\n' "$_wdr_from" >&2
            return 2
        fi
        _wdr_dir="$(sed -n '1{s/^[[:space:]]*//;s/[[:space:]]*$//;p;}' "$_wdr_from")"
    fi
    case "$_wdr_dir" in
        '' | *[[:space:]]*)
            printf '%s names no directory (%s); name one path without spaces\n' \
                "$_wdr_from" "'$_wdr_dir'" >&2
            return 2
            ;;
    esac
    printf '%s\n' "$_wdr_dir"
}

# wealthdb_demo_name DIR: the name the demo's containers take from its
# root DIR: the last part of the path less a leading wealthdb-, each
# character a container name cannot hold read as -. The default root,
# ~/wealthdb-demo, gives demo: containers wealthdb-metabase-demo and
# wealthdb-mcp-demo. Demo roots whose last parts differ get their own
# containers. Every demo serves on the same ports, so one runs at a time.
wealthdb_demo_name() {
    _wdn_name="$(basename "$1")"
    _wdn_name="$(printf '%s' "${_wdn_name#wealthdb-}" | tr -c 'A-Za-z0-9_.-' '-')"
    printf '%s\n' "${_wdn_name:-demo}"
}
