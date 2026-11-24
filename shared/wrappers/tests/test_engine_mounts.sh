#!/usr/bin/env bash
# Behaviour test: the engine wrapper mounts the config FILE for every
# subcommand but `config`, which mounts the config's folder (and, for a
# config that is a link, the folder of the file it names) and gets the
# host's locale and TERM. The subcommand is found past the global flags.
# A link into a folder that does not exist is refused before anything is
# created. A stubbed docker on PATH prints the argv; nothing real runs.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
TMP="$(cd "$TMP" && pwd)"

TREE="$TMP/tree"
mkdir -p "$TREE/wealthdb" "$TREE/shared/wrappers" "$TMP/bin" "$TMP/home/cfg" "$TMP/dots"
cp "$ROOT/wealthdb/wealthdb" "$TREE/wealthdb/"
cp "$ROOT/shared/wrappers/image-tag.sh" "$TREE/shared/wrappers/"
cat > "$TMP/bin/docker" <<'EOF'
#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done
EOF
chmod +x "$TMP/bin/docker"

CFG_DIR="$TMP/home/cfg"
CFG="$CFG_DIR/wealthdb.cfg"
DATA="$TMP/home/data"
run() {
    env PATH="$TMP/bin:$PATH" HOME="$TMP/home" XDG_CONFIG_HOME="$CFG_DIR" WEALTHDB_DATA_ROOT="$DATA" \
        WEALTHDB_IMAGE_TAG=t LANG=de_CH.UTF-8 LC_ALL= LC_MONETARY= TERM="${STUB_TERM-xterm}" \
        "$TREE/wealthdb/wealthdb" "$@"
}

echo "the file mount:"
for args in "status" "-c x status" "-r load -a" "--config=$CFG holdings global" "help config"; do
    # shellcheck disable=SC2086
    out="$(run $args)"
    has_item "$args: mounts the config file" "$CFG:$CFG" "$out"
    lacks_item "$args: not the folder" "$CFG_DIR:$CFG_DIR" "$out"
    lacks "$args: no locale" "WEALTHDB_LOCALE=" "$out"
    has_item "$args: the data root" "$DATA:$DATA" "$out"
    has_item "$args: forwards the data root" "WEALTHDB_DATA_ROOT=$DATA" "$out"
    has_item "$args: forwards the config dir" "XDG_CONFIG_HOME=$CFG_DIR" "$out"
done

echo "config mounts its folder:"
for args in "config" "-r config banks" "--config=x config" "-c x config --wizard" "-config x config"; do
    # shellcheck disable=SC2086
    out="$(run $args)"
    has_item "$args: mounts the config folder" "$CFG_DIR:$CFG_DIR" "$out"
    lacks_item "$args: not the file" "$CFG:$CFG" "$out"
    has "$args: forwards a locale" "WEALTHDB_LOCALE=" "$out"
    has_item "$args: forwards TERM" "TERM=xterm" "$out"
    has_item "$args: the image precedes the argv" "wealthdb:t" "$out"
done
if [ "$(uname -s)" != Darwin ]; then
    has_item "the locale is the host's LANG" "WEALTHDB_LOCALE=de_CH.UTF-8" "$(run config)"
fi
has_item "no TERM is dumb" "TERM=dumb" "$(STUB_TERM= run config)"
out="$(run config)"
n="$(grep -c -x -F -- "$CFG_DIR:$CFG_DIR" <<<"$out")"
[ "$n" = 1 ] && ok "a plain config mounts its folder once" || bad "the folder is mounted $n times"

echo "a config that is a link:"
rm -f "$CFG"
printf '{}\n' > "$TMP/dots/wealthdb.cfg"
ln -s ../../dots/wealthdb.cfg "$CFG"
out="$(run config)"
has_item "config mounts the config folder" "$CFG_DIR:$CFG_DIR" "$out"
has_item "config mounts the folder the link names" "$TMP/dots:$TMP/dots" "$out"
out="$(run status)"
has_item "status mounts the link, which docker follows" "$CFG:$CFG" "$out"
lacks_item "status mounts no other folder" "$TMP/dots:$TMP/dots" "$out"
[ -L "$CFG" ] && ok "the link stays a link" || bad "the link is gone"

rm -f "$CFG"
ln -s "$TMP/nowhere/wealthdb.cfg" "$CFG"
out="$(run config 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "a link into no folder exits 2" || bad "a link into no folder exits $rc"
has "it says why" "whose folder does not exist; nothing is changed" "$out"
lacks_item "docker never runs" "wealthdb:t" "$out"
[ ! -e "$TMP/nowhere" ] && ok "nothing is created" || bad "the folder was created"

summary "engine mount tests"
