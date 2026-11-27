#!/usr/bin/env bash
# Behaviour test: a config that is not the default one is a setup of its
# own, kept in the config's folder (wealthdb_engine_paths in
# image-tag.sh).
#
# The default config, wealthdb.cfg in the XDG config dir, resolves from
# the environment: the data root from WEALTHDB_DATA_ROOT, else the XDG
# data dir. Any other config, by its clean path, has its folder as the data
# root, whatever WEALTHDB_DATA_ROOT holds, a name for its servers from
# that folder, and `wealthdb -c CFG` as the start of every command to
# run, the path quoted as the engine quotes it.
#
# The same rule reaches the collectors: their data folder
# (paths-lib.sh), and through the dispatcher (bin/wealthdb-collect) the
# words their printed commands start with. No collector prints a bare
# `wealthdb-collect` command of its own. The launcher's part is in
# test_engine_mounts.sh, the setup texts' in test_setup_check.sh, and
# the servers' in web/test_web.sh and mcp/test_mcp.sh.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
is() { if [ "$3" = "$2" ]; then ok "$1"; else bad "$1 (want '$2', got '$3')"; fi; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
TMP="$(cd "$TMP" && pwd -P)"
sandbox_env "$TMP"
export HOME="$TMP/home"
mkdir -p "$HOME" "$TMP/work"
cd "$TMP/work" || exit 1

# shellcheck source=../image-tag.sh
. "$ROOT/shared/wrappers/image-tag.sh"

# paths [CONFIG]: what wealthdb_engine_paths resolves, one value a line.
paths() {
    (
        wealthdb_engine_paths "$@"
        printf '%s\n' "own=$OWN_SETUP" "root=$DATA_ROOT" "cfg=$CFG" "dir=$SETUP_DIR" \
            "name=$SETUP_NAME" "program=$PROGRAM" "xdg=$XDG_CFG" "cfgdir=$CFG_DIR"
    )
}

echo "the default config:"
out="$(HOME=/home/u XDG_CONFIG_HOME='' XDG_DATA_HOME='' paths)"
has_item "it is wealthdb.cfg in ~/.config" "cfg=/home/u/.config/wealthdb.cfg" "$out"
has_item "its data root is the XDG data dir" "root=/home/u/.local/share/wealthdb" "$out"
has_item "it is no setup of its own" "own=0" "$out"
has_item "its commands start with wealthdb" "program=wealthdb" "$out"
has_item "it has no folder name" "name=" "$out"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg XDG_DATA_HOME=/xdg paths)"
has_item "XDG_CONFIG_HOME moves it" "cfg=/cfg/wealthdb.cfg" "$out"
has_item "XDG_DATA_HOME moves the data root" "root=/xdg/wealthdb" "$out"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data paths)"
has_item "WEALTHDB_DATA_ROOT names the data root" "root=/data" "$out"
has_item "...and the config stays the default one" "own=0" "$out"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=rel/data paths)"
has_item "a relative data root is made absolute" "root=$TMP/work/rel/data" "$out"
for spelled in /cfg/wealthdb.cfg /cfg//wealthdb.cfg /cfg/./wealthdb.cfg /cfg/x/../wealthdb.cfg; do
    out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG="$spelled" paths)"
    has_item "WEALTHDB_CONFIG=$spelled is the default config" "own=0" "$out"
    has_item "...with the data root of the environment" "root=/data" "$out"
    out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data paths "$spelled")"
    has_item "-c $spelled is the default config" "own=0 program=wealthdb" "$(grep -e '^own' -e '^program' <<<"$out" | tr '\n' ' ' | sed 's/ $//')"
done
out="$(cd /tmp && HOME=/home/u XDG_CONFIG_HOME=rel WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG=rel/wealthdb.cfg paths)"
has_item "a relative default config is the default one" "own=0" "$out"

echo "a demo's call (the config dir, the config and the data root are one folder):"
out="$(HOME=/home/u XDG_CONFIG_HOME=/srv/demo WEALTHDB_CONFIG=/srv/demo/wealthdb.cfg WEALTHDB_DATA_ROOT=/srv/demo paths)"
has_item "the config is the default one under that config dir" "own=0" "$out"
has_item "the data root is the one it names" "root=/srv/demo" "$out"
has_item "its commands start with wealthdb" "program=wealthdb" "$out"

echo "a config elsewhere is a setup of its own:"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG=/home/u/try/wealthdb.cfg paths)"
has_item "it is one" "own=1" "$out"
has_item "its data root is the config's folder" "root=/home/u/try" "$out"
lacks "WEALTHDB_DATA_ROOT is not read" "/data" "$out"
has_item "the folder" "dir=/home/u/try" "$out"
has_item "the name its servers take" "name=try" "$out"
has_item "its commands name the config" "program=wealthdb -c /home/u/try/wealthdb.cfg" "$out"
has_item "the XDG config dir stays" "xdg=/cfg" "$out"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data paths /home/u/try/wealthdb.cfg)"
has_item "a -c path is one too" "own=1" "$out"
has_item "...with its folder as the data root" "root=/home/u/try" "$out"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg WEALTHDB_CONFIG=/home/u/try/wealthdb.cfg paths /other/w.cfg)"
has_item "a -c path wins over WEALTHDB_CONFIG" "root=/other" "$out"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg WEALTHDB_CONFIG=/home/u/try/wealthdb.cfg paths /cfg/wealthdb.cfg)"
has_item "...also when it names the default config" "own=0" "$out"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg paths /cfg/other.cfg)"
has_item "another file in the config dir is a setup of its own" "own=1" "$out"
has_item "...kept in that folder" "root=/cfg" "$out"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg paths try/wealthdb.cfg)"
has_item "a relative path is made absolute" "root=$TMP/work/try" "$out"
has_item "...in the command too" "program=wealthdb -c $TMP/work/try/wealthdb.cfg" "$out"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg paths /x/./a//b/../try/wealthdb.cfg)"
has_item "the folder is the clean path's" "root=/x/a/try" "$out"
has_item "the config keeps its spelling, as the engine is told it" "cfg=/x/./a//b/../try/wealthdb.cfg" "$out"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg paths /wealthdb.cfg)"
has_item "a config in the root folder: the data root is /" "root=/" "$out"
has_item "...and its name is setup" "name=setup" "$out"

echo "the name of a setup's servers:"
is "the last part of the folder" "try" "$(wealthdb_setup_name /home/u/try)"
is "in lower case" "mytry" "$(wealthdb_setup_name /home/u/MyTry)"
is "a blank reads as -" "my-try-2" "$(wealthdb_setup_name '/home/u/My Try 2')"
is "what a container name cannot hold reads as -" "a-b-c" "$(wealthdb_setup_name '/x/a+b@c')"
is "_ . and - stay" "a_b.c-d" "$(wealthdb_setup_name /x/a_b.c-d)"
is "a trailing slash is dropped" "try" "$(wealthdb_setup_name /home/u/try/)"
is "...and a doubled one" "try" "$(wealthdb_setup_name /home/u//try//)"
is "the root folder gives setup" "setup" "$(wealthdb_setup_name /)"
is "a name outside ASCII reads as - for each byte, in any locale" "caf--" \
    "$(LC_ALL=en_US.UTF-8 wealthdb_setup_name "/x/caf$(printf '\303\251')")"
is "...the same without one" "caf--" "$(LC_ALL=C wealthdb_setup_name "/x/caf$(printf '\303\251')")"
out="$(HOME=/home/u XDG_CONFIG_HOME=/cfg paths '/home/u/My Try/wealthdb.cfg')"
has_item "a folder with a blank: its name" "name=my-try" "$out"
has_item "...its data root" "root=/home/u/My Try" "$out"
has_item "...and its command, quoted for a shell" 'program=wealthdb -c "/home/u/My Try/wealthdb.cfg"' "$out"

echo "a path as one word of a command (the engine's shellWord):"
while IFS='|' read -r in want; do
    is "$in" "$want" "$(wealthdb_shell_word "$in")"
done <<'EOF'
plain/path.cfg|plain/path.cfg
/x/a_b-c.d:e@f%g+h=i,j/try.cfg|/x/a_b-c.d:e@f%g+h=i,j/try.cfg
a b|"a b"
~/x|"~/x"
a*b|"a*b"
a'b'c|"a'b'c"
a&b|"a&b"
$HOME/x|'$HOME/x'
a"b|'a"b'
a`b|'a`b'
a\b|'a\b'
a!b|'a!b'
it's $5|'it'\''s $5'
EOF
is "nothing is an empty word" "''" "$(wealthdb_shell_word '')"
is "a letter outside ASCII is quoted, in any locale" "\"$(printf '\303\251').cfg\"" \
    "$(LC_ALL=en_US.UTF-8 wealthdb_shell_word "$(printf '\303\251').cfg")"
# The quoted word reads back as the path it was made from.
for p in "/x/my setup/try.cfg" "/x/it's/try.cfg" '/x/$HOME & co/"q".cfg' \
    "/x/a\`b\\c!d/try.cfg" "$(printf '/x/two\nlines')"; do
    eval "back=$(wealthdb_shell_word "$p")"
    is "a shell reads $p back" "$p" "$back"
done

echo "a clean path:"
is "no doubled slash, no dot, no dot-dot" "/a/c/d/e" "$(wealthdb_clean_path /a/b/../c//d/./e/)"
is "the root" "/" "$(wealthdb_clean_path /)"
is "above the root is the root" "/" "$(wealthdb_clean_path /../..)"
is "a relative path is made absolute" "$TMP/work/x" "$(wealthdb_clean_path ./x)"
is "a pattern is no pattern" "/a/*/c" "$(wealthdb_clean_path '/a/*/c')"
is "a blank stays" "/a b/c" "$(wealthdb_clean_path '/a b//c')"

echo "what a host-side command is handed:"
exported() {
    (
        wealthdb_engine_paths "$@"
        wealthdb_export_setup
        env | grep -e '^WEALTHDB_DATA_ROOT=' -e '^WEALTHDB_PROGRAM=' -e '^WEALTHDB_COLLECT=' | sort
    )
}
out="$(XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data exported '/home/u/My Try/wealthdb.cfg')"
is "a setup of its own: its data root and the words of its commands" \
    'WEALTHDB_COLLECT=wealthdb -c "/home/u/My Try/wealthdb.cfg" collect
WEALTHDB_DATA_ROOT=/home/u/My Try
WEALTHDB_PROGRAM=wealthdb -c "/home/u/My Try/wealthdb.cfg"' "$out"
out="$(XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_COLLECT=stale WEALTHDB_PROGRAM=stale exported)"
is "the default config: the data root of the environment, and no words, also not inherited ones" \
    "WEALTHDB_DATA_ROOT=/data" "$out"
out="$(unset WEALTHDB_DATA_ROOT; XDG_CONFIG_HOME=/cfg exported)"
is "the default config with no data root set: none is set" "" "$out"

echo "a collector's data folder (paths-lib.sh):"
# data_dir: the data folder collector_default_paths gives testcol.
data_dir() {
    (
        set -euo pipefail
        NAME=testcol
        # shellcheck source=../paths-lib.sh
        . "$ROOT/shared/wrappers/paths-lib.sh"
        collector_default_paths
        printf '%s\n' "$DATA_DIR"
    )
}
is "the default config: the XDG data dir" "$TMP/xdgdata/wealthdb/testcol" "$(data_dir)"
is "the default config: WEALTHDB_DATA_ROOT" "/data/testcol" "$(WEALTHDB_DATA_ROOT=/data/ data_dir)"
is "...also when WEALTHDB_CONFIG names the default config" "/data/testcol" \
    "$(WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG="$TMP/xdgconfig/wealthdb.cfg" data_dir)"
is "a setup of its own: a folder in the config's" "/home/u/try/testcol" \
    "$(WEALTHDB_CONFIG=/home/u/try/wealthdb.cfg data_dir)"
is "...whatever WEALTHDB_DATA_ROOT holds" "/home/u/try/testcol" \
    "$(WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG=/home/u/try/wealthdb.cfg data_dir)"
is "...and for a config in the root folder" "/testcol" "$(WEALTHDB_CONFIG=/wealthdb.cfg data_dir)"
is "the collector's own variable still wins" "/mine" \
    "$(TESTCOL_DATA_DIR=/mine WEALTHDB_CONFIG=/home/u/try/wealthdb.cfg data_dir)"
out="$( (set -euo pipefail; NAME=testcol; . "$ROOT/shared/wrappers/paths-lib.sh"; collector_default_paths
         echo "${OWN_SETUP-unset} ${DATA_ROOT-unset} ${CFG-unset}") )"
is "the rule's variables stay out of the wrapper" "unset unset unset" "$out"

echo "the dispatcher (bin/wealthdb-collect):"
# A tree with a stand-in collector that prints what it is handed.
TREE="$TMP/tree"
mkdir -p "$TREE/collectors/testcol" "$TREE/shared/wrappers" "$TREE/bin"
cp "$ROOT/bin/wealthdb-collect" "$TREE/bin/"
cp "$ROOT/shared/wrappers/"*.sh "$TREE/shared/wrappers/"
cat > "$TREE/collectors/testcol/testcol" <<'EOF'
#!/bin/sh
echo "root=${WEALTHDB_DATA_ROOT-unset}"
echo "collect=${WEALTHDB_COLLECT-unset}"
echo "program=${WEALTHDB_PROGRAM-unset}"
echo "args=$*"
EOF
chmod +x "$TREE/collectors/testcol/testcol"
out="$(WEALTHDB_DATA_ROOT=/data "$TREE/bin/wealthdb-collect" testcol load -v)"
is "the default config: the environment as it is, and no words" \
    "root=/data
collect=unset
program=unset
args=load -v" "$out"
out="$(WEALTHDB_COLLECT=stale WEALTHDB_PROGRAM=stale "$TREE/bin/wealthdb-collect" testcol load)"
has_item "the default config: inherited words go" "collect=unset" "$out"
has_item "...both" "program=unset" "$out"
has_item "...and no data root is made up" "root=unset" "$out"
out="$(WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG="$TMP/try/wealthdb.cfg" "$TREE/bin/wealthdb-collect" testcol load)"
is "a setup of its own: its folder and the words of its commands" \
    "root=$TMP/try
collect=wealthdb -c $TMP/try/wealthdb.cfg collect
program=wealthdb -c $TMP/try/wealthdb.cfg
args=load" "$out"
[ ! -e "$TMP/try" ] && ok "the dispatcher makes no folder" || bad "the dispatcher made $TMP/try"

echo "no collector prints a bare wealthdb-collect command:"
# A text a collector prints names its command through WEALTHDB_COLLECT,
# with wealthdb-collect as the fallback. A Python string that holds the
# bare word and more is such a text, unless it is a docstring. The
# fallback itself is the word alone.
out="$(python3 - "$ROOT/collectors" <<'PY'
import ast, pathlib, sys
skip = {"tests", ".venv", "__pycache__"}
for path in sorted(pathlib.Path(sys.argv[1]).rglob("*.py")):
    if skip & set(path.parts) or path.name.startswith("test_") or path.name == "conftest.py":
        continue
    tree = ast.parse(path.read_text(encoding="utf-8"))
    docs = set()
    for node in ast.walk(tree):
        if isinstance(node, (ast.Module, ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)):
            body = node.body
            if body and isinstance(body[0], ast.Expr) and isinstance(body[0].value, ast.Constant):
                docs.add(id(body[0].value))
    for node in ast.walk(tree):
        if (isinstance(node, ast.Constant) and isinstance(node.value, str)
                and id(node) not in docs
                and "wealthdb-collect" in node.value and node.value != "wealthdb-collect"):
            print(f"{path.relative_to(sys.argv[1])}:{node.lineno}")
PY
)"
is "no Python text names it bare" "" "$out"
# A wrapper's text: every line of code that names the command reads the
# variable. The fleet sweep `wealthdb-collect dedup` is a name in a help
# text, never a command of one setup.
out="$(for w in "$ROOT"/collectors/*/; do
        n="$(basename "$w")"
        [ -f "$w$n" ] || continue
        grep -n 'wealthdb-collect' "$w$n" | grep -v -e '^[0-9]*:[[:space:]]*#' -e 'WEALTHDB_COLLECT' -e 'wealthdb-collect dedup' |
            sed "s|^|$n:|"
    done)"
is "no wrapper text names it bare" "" "$out"

summary "own setup tests"
