#!/usr/bin/env bash
# Behaviour test: a `login --check` probe reads the stdin its caller was
# given, never the fold notice. It covers every host wrapper that calls
# host_login_probe and every entrypoint that calls login_folds. Each runs
# with one line on stdin, and a `python3` stub prints the first line it
# reads. The host wrappers run from a copy of the tree, so they find no
# .venv. The entrypoints run as in test_argv_golden.sh: a BASH_ENV
# `source` loads the real entrypoint-lib.sh with its file commands and
# its displays stubbed.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
REPO="$(cd "$HERE/../../.." && pwd -P)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

BIN="$TMP/bin"
mkdir -p "$BIN" "$TMP/home/.secrets" "$TMP/tree/shared/wrappers"
cat > "$BIN/python3" <<'STUB'
#!/usr/bin/env bash
line=
IFS= read -r line || true
echo "probe stdin: ${line:-<none>}"
STUB
chmod +x "$BIN/python3"
cp -p "$REPO/shared/wrappers/"*.sh "$TMP/tree/shared/wrappers/"

cat > "$TMP/bashenv.sh" <<'EOF'
source() {
    if [ "${1:-}" = /opt/entrypoint-lib.sh ]; then
        mkdir() { :; }; rm() { :; }; ln() { :; }
        builtin source "$REALLIB"
        unset -f mkdir rm ln
        start_xvfb() { :; }
        start_x11vnc() { :; }
    else
        builtin source "$@"
    fi
}
EOF

SANDBOX=(env -i PATH="$BIN:/usr/bin:/bin" HOME="$TMP/home" LC_ALL=C TMPDIR="$TMP"
    XDG_DATA_HOME="$TMP/xdgdata" XDG_CACHE_HOME="$TMP/xdgcache"
    XDG_CONFIG_HOME="$TMP/xdgconfig")

# probe DESC CMD...: CMD, given one line on stdin, runs a probe that reads
# that line.
probe() {
    local desc="$1" out
    shift
    out="$(printf 'from-caller\n' | "$@" 2>&1)"
    if grep -qxF "probe stdin: from-caller" <<<"$out"; then
        ok "$desc"
    else
        bad "$desc (got: $(tr '\n' ' ' <<<"$out"))"
    fi
}

# A call to the helper, not a mention of it in a comment.
calls() { grep -qE "^[[:space:]]*$1 " "$2"; }

echo "login --check reads the caller's stdin:"
hosts=0 entries=0
for d in "$REPO"/collectors/*/; do
    n="$(basename "$d")"
    if [[ -x "$d$n" ]] && calls host_login_probe "$d$n"; then
        mkdir -p "$TMP/tree/collectors/$n"
        cp -p "$d$n" "$TMP/tree/collectors/$n/"
        probe "$n wrapper" "${SANDBOX[@]}" "$TMP/tree/collectors/$n/$n" login --check
        hosts=$((hosts + 1))
    fi
    if [[ -f "${d}entrypoint.sh" ]] && calls login_folds "${d}entrypoint.sh"; then
        probe "$n entrypoint" "${SANDBOX[@]}" BASH_ENV="$TMP/bashenv.sh" \
            REALLIB="$REPO/shared/images/entrypoint-lib.sh" \
            bash "${d}entrypoint.sh" login --check
        entries=$((entries + 1))
    fi
done
[[ $hosts -gt 0 ]] && ok "found $hosts host wrappers with a probe" \
    || bad "found no host wrapper with a probe"
[[ $entries -gt 0 ]] && ok "found $entries entrypoints with a probe" \
    || bad "found no entrypoint with a probe"

echo
summary "login probe stdin tests"
