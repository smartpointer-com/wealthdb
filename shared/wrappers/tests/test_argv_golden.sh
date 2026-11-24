#!/usr/bin/env bash
# Golden command lines of every collector wrapper and entrypoint.
#
# Wrapper side: each collectors/<name>/<name> runs from a copy of itself,
# its setup.txt and shared/wrappers/*.sh in a temp tree, so it finds no
# .venv. Its environment is empty but for a sandboxed HOME, TMPDIR, XDG
# dirs and stub programs: `docker`, `python3` and `lsof` record their
# argv and start nothing. The secrets dir holds an env file of invented
# values, and the stubs print each forwarded variable, since `-e VAR`
# passes a name and the value travels in the environment. Every setup is
# complete, so the setup check runs and prints nothing.
#
# Entrypoint side: each collectors/<name>/entrypoint.sh runs under a bash
# whose BASH_ENV defines a `source` that loads a recording library in
# place of /opt/entrypoint-lib.sh. That library sources the real one with
# its file commands recorded, then replaces start_xvfb and start_x11vnc
# with recorders. `python3`, `firefox` and `openssl` are stubs. The
# environment is the wrapper side's, so a file an entrypoint writes
# under TMPDIR lands in the temp root.
#
# Each case records the calls in order, the exit status, stdout and
# stderr, with the temp root, the checkout and uid:gid masked. The cases
# are help, every verb the help text names and the four every collector
# answers, each verb's --help on the entrypoint side, login --check, a
# dry-run download, both spellings of each dir flag on the wrapper side,
# and an unknown verb. The collectors run side by side, one job per file.
#
# The files under golden/ are the expected record: <name>.wrapper and
# <name>.entry. UPDATE_GOLDEN=1 rewrites them from this tree. A
# difference is a changed command line: review it, then rewrite.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
REPO="$(cd "$HERE/../../.." && pwd -P)"
GOLDEN="$HERE/golden"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
TMP="$(cd "$TMP" && pwd -P)"
UIDGID="$(id -u):$(id -g)"

# ---- stubs ------------------------------------------------------------

BIN="$TMP/bin"
stub_docker "$BIN"
# A recorder appends "<name>", then its argv one item per line, to
# STUB_LOG.
recorder() {
    cat > "$BIN/$1" <<STUB
#!/usr/bin/env bash
{ echo $1; if [ \$# -gt 0 ]; then printf '  %s\n' "\$@"; fi; $2; } >> "\$STUB_LOG"
$3
STUB
    chmod +x "$BIN/$1"
}
# python3 also lists the invented variables it was given, so a case
# shows whether the env file was sourced on the host.
recorder python3 "env | LC_ALL=C sort | sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*=invented-\)/  [env] \1/p'" ""
recorder firefox ":" ""
recorder openssl ":" "echo 0123456789abcdef"
# lsof finds every port free.
recorder lsof ":" "exit 1"
ln -s "$BASH" "$BIN/bash"

# The entrypoint side's BASH_ENV, and the library it loads.
cat > "$TMP/bashenv.sh" <<'EOF'
_rec() { { echo "$1"; shift; if [ $# -gt 0 ]; then printf '  %s\n' "$@"; fi; } >> "$STUB_LOG"; }
source() {
    if [ "${1:-}" = /opt/entrypoint-lib.sh ]; then
        shift
        builtin source "$RECLIB" "$@"
    else
        builtin source "$@"
    fi
}
EOF
cat > "$TMP/reclib.sh" <<'EOF'
mkdir() { _rec "lib: mkdir" "$@"; }
rm() { _rec "lib: rm" "$@"; }
ln() { _rec "lib: ln" "$@"; }
builtin source "$REALLIB"
unset -f mkdir rm ln
start_xvfb() { _rec start_xvfb "$@"; export DISPLAY=:99; }
start_x11vnc() { _rec start_x11vnc "$@"; }
EOF

# ---- helpers ----------------------------------------------------------

# mask FILE: FILE with the temp root, the checkout and uid:gid as
# placeholders, and without the line numbers of bash's own messages.
mask() {
    sed -e "s|$TMP|<TMP>|g" -e "s|$REPO|<REPO>|g" -e "s|^  $UIDGID\$|  <UID>:<GID>|" \
        -e 's/: line [0-9][0-9]*: /: line N: /' "$1"
}

# The verbs every collector answers, named in its help or not.
FLEET_VERBS="login download load prune"

# verbs: the verbs a help text names, then the fleet's, one per line.
# A verb line is indented by two spaces and holds one verb or several
# joined by `|`. Then comes the end of the line, or a description after
# two spaces or one before a capital, or an `[args]` or a flag before
# it.
verbs() {
    { sed -nE 's/^  ([a-z][a-z0-9_-]*( ?\| ?[a-z][a-z0-9_-]*)*)( {2,}[^ ].*| [A-Z].*| \[[^]]*\]( +[^ ].*)?| --?[a-z][a-z-]*( [A-Z]+)?( {2,}[^ ].*)?)?$/\1/p' |
        tr '|' '\n' | tr -d ' '; printf '%s\n' $FLEET_VERBS; } | awk 'NF && $0 != "help" && !seen[$0]++'
}

# case_run ROOT DESC CMD...: run CMD in the sandbox of ROOT with stdin
# closed, and append its record to ROOT/record, unmasked. The stubs
# append the calls to the record as they run.
case_run() {
    local root="$1" desc="$2" rc
    shift 2
    printf '=== %s\n--- calls\n' "$desc" >> "$root/record"
    "$@" < /dev/null > "$root/stdout" 2> "$root/stderr"
    rc=$?
    printf 'exit: %s\n--- stdout\n' "$rc" >> "$root/record"
    cat "$root/stdout" - "$root/stderr" <<< "--- stderr" >> "$root/record"
}

# sandbox ROOT: the dirs a case may write, the working dir, and the
# case's empty environment.
sandbox() {
    mkdir -p "$1/work" "$1/home/.secrets" "$1/alt-secrets" "$1/tmp"
    : > "$1/record"
    cd "$1/work" || exit 1
    SANDBOX_ENV=(env -i PATH="$BIN:/usr/bin:/bin" HOME="$1/home" LC_ALL=C
        TMPDIR="$1/tmp" XDG_DATA_HOME="$1/xdgdata" XDG_CACHE_HOME="$1/xdgcache"
        XDG_CONFIG_HOME="$1/xdgconfig" STUB_LOG="$1/record")
}

# wrapper_golden NAME OUT: every case of collectors/NAME/NAME. It runs as
# a job of its own, so the sandbox's working dir stays its own.
wrapper_golden() {
    local name="$1" out="$2" root="$TMP/w-$1" w v
    # bash 3.2 runs the caller's EXIT trap when a background function
    # ends, which would remove $TMP under the other jobs.
    trap - EXIT
    sandbox "$root"
    mkdir -p "$root/tree/collectors/$name" "$root/tree/shared/wrappers"
    cp -p "$REPO/collectors/$name/$name" "$REPO/collectors/$name/setup.txt" \
        "$root/tree/collectors/$name/"
    cp -p "$REPO/shared/wrappers/"*.sh "$root/tree/shared/wrappers/"
    w="$root/tree/collectors/$name/$name"
    local decl="$root/tree/collectors/$name/setup.txt"
    # The env file sets each variable the wrapper forwards, and one more
    # that only a host-side source passes on. refdata reads finra.env too.
    # Every case is a complete setup, so the setup check prints nothing:
    # the env file also sets each other variable setup.txt names, and its
    # key files are there. Those values are not `invented-`, so no stub
    # lists them.
    {
        ( eval "$(awk '/ENV_VARS=\(/ {f=1} f {print} f && /\)/ {exit}' "$w")"
          for v in ${ENV_VARS[@]+"${ENV_VARS[@]}"}; do echo "$v=invented-$v"; done
          for v in $(awk '$1 == "var" {print $2}' "$decl"); do
              [[ " ${ENV_VARS[*]-} " == *" $v "* ]] || echo "$v=complete-$v"
          done )
        echo "GOLDEN_ENV_FILE=invented-$name"
    } > "$root/home/.secrets/$name.env"
    echo "GOLDEN_FINRA=invented-finra" > "$root/home/.secrets/finra.env"
    for v in $(awk '$1 == "keyfile" {print $2}' "$decl"); do
        : > "$root/home/.secrets/$v"
    done
    # A download that the setup check runs before finds a complete setup
    # in the other secrets dir too. No wrapper of such a download sources
    # the env file on the host, so the record does not change.
    if awk '$1 == "checks"' "$decl" | grep -qw download; then
        awk '$1 == "var" {print $2 "=complete-" $2}' "$decl" > "$root/alt-secrets/$name.env"
    fi
    run() { case_run "$root" "$*" "${SANDBOX_ENV[@]}" "$w" "$@"; }
    run
    run help
    for v in $(verbs < "$root/stdout"); do
        run "$v"
    done
    run login --check
    run download --dry-run --lookback 1w
    run load --silver-db "$root/silver/x.db"
    run load --silver-db="$root/silver/x.db"
    run load --data-dir "$root/alt-data"
    run load --data-dir="$root/alt-data"
    run download --secrets-dir "$root/alt-secrets"
    run download --secrets-dir="$root/alt-secrets"
    run bogus-verb
    mask "$root/record" > "$out"
}

# entry_golden NAME OUT: every case of collectors/NAME/entrypoint.sh.
entry_golden() {
    local name="$1" out="$2" root="$TMP/e-$1" e v
    trap - EXIT
    sandbox "$root"
    e="$REPO/collectors/$name/entrypoint.sh"
    run() {
        case_run "$root" "$*" "${SANDBOX_ENV[@]}" BASH_ENV="$TMP/bashenv.sh" \
            RECLIB="$TMP/reclib.sh" REALLIB="$REPO/shared/images/entrypoint-lib.sh" \
            bash "$e" "$@"
    }
    run
    run --help
    for v in $(verbs < "$root/stdout"); do
        case "$v" in
            sh|bash) run "$v" -c 'echo shell' ;;
            *) run "$v"; run "$v" --help ;;
        esac
    done
    run login --check
    run download --dry-run --lookback 1w
    run bogus-verb
    mask "$root/record" > "$out"
}

# ---- run --------------------------------------------------------------

mkdir -p "$TMP/out"
names=()
for d in "$REPO"/collectors/*/; do
    n="$(basename "$d")"
    [[ -x "$d$n" ]] || continue
    names+=("$n.wrapper")
    wrapper_golden "$n" "$TMP/out/$n.wrapper" &
    if [[ -f "${d}entrypoint.sh" ]]; then
        names+=("$n.entry")
        entry_golden "$n" "$TMP/out/$n.entry" &
    fi
done
wait

echo "wrapper and entrypoint golden command lines:"
if [[ "${UPDATE_GOLDEN:-}" == 1 ]]; then
    mkdir -p "$GOLDEN"
    rm -f "$GOLDEN"/*.wrapper "$GOLDEN"/*.entry
    for f in "${names[@]}"; do cp "$TMP/out/$f" "$GOLDEN/$f"; done
    ok "rewrote ${#names[@]} golden files"
else
    for f in "${names[@]}"; do
        if [[ ! -f "$GOLDEN/$f" ]]; then
            bad "$f has no golden file (UPDATE_GOLDEN=1 writes it)"
        elif d="$(diff -u "$GOLDEN/$f" "$TMP/out/$f")"; then
            ok "$f"
        else
            bad "$f differs from its golden file:"
            echo "$d" | head -60
        fi
    done
    for g in "$GOLDEN"/*.wrapper "$GOLDEN"/*.entry; do
        [[ -e "$g" ]] || continue
        f="$(basename "$g")"
        [[ " ${names[*]} " == *" $f "* ]] || bad "$f is a golden file of no collector"
    done
fi

echo
summary "argv golden tests"
