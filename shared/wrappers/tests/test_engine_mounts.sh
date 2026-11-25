#!/usr/bin/env bash
# Behaviour test: the engine wrapper mounts the config FILE for every
# subcommand but `config`, which mounts the config's folder (and, for a
# config that is a link, the folder of the file it names) and gets the
# host's locale and TERM. The subcommand is found past the global flags.
# A link into a folder that does not exist is refused before anything is
# created. The config is the -c path, else WEALTHDB_CONFIG, else the XDG
# default, and every path the container sees is absolute. `config`
# creates no file: the setup writes it, and `config validate` on a path
# with no file leaves none. Every run names host.docker.internal, where
# an AI model on this computer is reached. `web`, `mcp` and `collect`
# reach their host-side scripts, also behind global flags, and a -c path
# reaches them as WEALTHDB_CONFIG. A config that is not the default one
# is a setup of its own: its folder is the data root, mounted once, and
# a host-side script is handed that folder and the words its printed
# commands start with. A stubbed docker on PATH prints the argv; nothing
# real runs.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
TMP="$(cd "$TMP" && pwd)"
# No ambient config path reaches the wrapper: a case names its own.
unset WEALTHDB_CONFIG

TREE="$TMP/tree"
mkdir -p "$TREE/wealthdb" "$TREE/shared/wrappers" "$TMP/bin" "$TMP/home/cfg" "$TMP/dots" "$TMP/work"
# Every run starts in a scratch folder, so a relative path lands there.
cd "$TMP/work" || exit 1
cp "$ROOT/wealthdb/wealthdb" "$TREE/wealthdb/"
cp "$ROOT/shared/wrappers/image-tag.sh" "$ROOT/shared/wrappers/engine-mode.sh" "$TREE/shared/wrappers/"
# The host-side scripts are stand-ins that print their name, the config
# path they are told and their arguments. With STUB_SHOW_SETUP they
# print what a setup of its own hands them instead of the arguments.
mkdir -p "$TREE/web" "$TREE/mcp" "$TREE/bin"
for s in web/web mcp/mcp bin/wealthdb-collect; do
    cat > "$TREE/$s" <<EOF
#!/bin/sh
echo "host:$s"
echo "config:\${WEALTHDB_CONFIG-unset}"
if [ -n "\${STUB_SHOW_SETUP:-}" ]; then
    echo "root:\${WEALTHDB_DATA_ROOT-unset}"
    echo "collect:\${WEALTHDB_COLLECT-unset}"
    echo "program:\${WEALTHDB_PROGRAM-unset}"
    exit 0
fi
for a in "\$@"; do printf '%s\n' "\$a"; done
EOF
    chmod +x "$TREE/$s"
done
cat > "$TMP/bin/docker" <<'EOF'
#!/bin/sh
if [ "$1 $2" = "image inspect" ] && [ -n "${STUB_NO_IMAGE:-}" ]; then
    echo "Error response from daemon: No such image: wealthdb:t" >&2
    exit 1
fi
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
for args in "status" "-r load -a" "--config=$CFG holdings global" "help config"; do
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
for args in "config" "-r config banks" "--config=$CFG config" "-c $CFG config --wizard" "-config $CFG config"; do
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
has_item "...which the engine checks too" "WEALTHDB_MOUNTS=$CFG_DIR:$TMP/dots:$DATA" "$out"
out="$(run status)"
has_item "status mounts the link, which docker follows" "$CFG:$CFG" "$out"
has_item "the link folder is a mount the engine checks" "WEALTHDB_MOUNTS=$CFG:$DATA" "$out"
lacks_item "status mounts no other folder" "$TMP/dots:$TMP/dots" "$out"
[ -L "$CFG" ] && ok "the link stays a link" || bad "the link is gone"

rm -f "$CFG"
ln -s "$TMP/nowhere/wealthdb.cfg" "$CFG"
out="$(run config 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "a link into no folder exits 2" || bad "a link into no folder exits $rc"
has "it says why" "whose folder does not exist; nothing is changed" "$out"
lacks_item "docker never runs" "wealthdb:t" "$out"
[ ! -e "$TMP/nowhere" ] && ok "nothing is created" || bad "the folder was created"

echo "the engine learns the host and the folders it mounts:"
rm -f "$CFG"
out="$(run status)"
has_item "status: the host's system" "WEALTHDB_HOST_OS=$(uname -s)" "$out"
has_item "status: the config file and the data root" "WEALTHDB_MOUNTS=$CFG:$DATA" "$out"
out="$(run config)"
has_item "config: the config folder and the data root" "WEALTHDB_MOUNTS=$CFG_DIR:$DATA" "$out"

echo "a -c path names the config, made absolute:"
rm -f "$CFG"
for args in "-c x status" "--c x status" "-config x status" "--config x status" "-c=x status" "--config=x status"; do
    # shellcheck disable=SC2086
    out="$(run $args)"
    has_item "$args: mounts the -c file" "$TMP/work/x:$TMP/work/x" "$out"
    lacks_item "$args: not the default file" "$CFG:$CFG" "$out"
    has "$args: the engine gets the absolute path" "$TMP/work/x" "$(grep -A1 -x -e '-c' -e '--c' -e '-config' -e '--config' <<<"$out"; grep -e '=.*work/x$' <<<"$out")"
    lacks_item "$args: no relative path is left" "x" "$out"
    lacks "$args: WEALTHDB_CONFIG is not set from a flag" "WEALTHDB_CONFIG=" "$out"
done
[ ! -e "$CFG" ] && ok "a -c run creates no default config" || bad "a -c run created the default config"
out="$(run -c sub/my.cfg config)"
has_item "config with -c mounts the -c folder" "$TMP/work/sub:$TMP/work/sub" "$out"
[ -d "$TMP/work/sub" ] && ok "config makes the -c folder" || bad "the -c folder is missing"
[ ! -e "$TMP/work/sub/my.cfg" ] && ok "...and no file: the setup writes it" || bad "config made an empty file"
out="$(run -c typo/none.cfg config validate)"
has_item "config validate mounts the -c folder" "$TMP/work/typo:$TMP/work/typo" "$out"
[ ! -e "$TMP/work/typo/none.cfg" ] && ok "config validate on no file leaves none" || bad "config validate made a file"
rm -rf "$TMP/work/typo"
out="$(run -r -c x holdings global -c y)"
has_item "only a -c before the subcommand counts" "$TMP/work/x:$TMP/work/x" "$out"
has_item "a later -c is the subcommand's, left as it is" "y" "$out"
rm -rf "$TMP/work/x" "$TMP/work/sub"

echo "WEALTHDB_CONFIG names the config:"
runw() { WEALTHDB_CONFIG="$1" run "${@:2}"; }
out="$(runw "$TMP/other/my.cfg" status)"
has_item "status mounts that file" "$TMP/other/my.cfg:$TMP/other/my.cfg" "$out"
has_item "the engine gets it" "WEALTHDB_CONFIG=$TMP/other/my.cfg" "$out"
lacks_item "not the default file" "$CFG:$CFG" "$out"
has_item "the XDG config dir is forwarded as it is" "XDG_CONFIG_HOME=$CFG_DIR" "$out"
[ ! -e "$CFG" ] && ok "no default config is created" || bad "the default config was created"
out="$(runw "$TMP/other/my.cfg" config)"
has_item "config mounts its folder" "$TMP/other:$TMP/other" "$out"
out="$(runw rel/my.cfg status)"
has_item "a relative WEALTHDB_CONFIG is made absolute" "WEALTHDB_CONFIG=$TMP/work/rel/my.cfg" "$out"
has_item "...and mounted there" "$TMP/work/rel/my.cfg:$TMP/work/rel/my.cfg" "$out"
out="$(runw "$TMP/other/my.cfg" -c "$TMP/flag.cfg" status)"
has_item "a -c flag wins over WEALTHDB_CONFIG" "$TMP/flag.cfg:$TMP/flag.cfg" "$out"
rm -rf "$TMP/other" "$TMP/work/rel" "$TMP/flag.cfg"

echo "the host-side commands:"
# is_out DESC WANT GOT: the script's whole output.
is_out() {
    if [ "$3" = "$2" ]; then ok "$1"; else bad "$1 (want: $(printf '%s ' $2), got: $(printf '%s ' $3))"; fi
}
is_out "web reaches its script" "host:web/web
config:unset
start" "$(run web start)"
is_out "-c FILE web: the script, the flag dropped, the path as WEALTHDB_CONFIG" "host:web/web
config:$TMP/work/h.cfg
start" "$(run -c h.cfg web start)"
is_out "--config=FILE mcp" "host:mcp/mcp
config:$TMP/work/h.cfg
url" "$(run --config=h.cfg mcp url)"
is_out "-r -c FILE collect, a later -c left to the collector" "host:bin/wealthdb-collect
config:$TMP/work/h.cfg
testcol
load
-c
y" "$(run -r -c h.cfg collect testcol load -c y)"
is_out "-r mcp: an ambient WEALTHDB_CONFIG stays as it is" "host:mcp/mcp
config:rel/my.cfg
status" "$(runw rel/my.cfg -r mcp status)"
is_out "a -c flag wins over WEALTHDB_CONFIG" "host:web/web
config:$TMP/work/h.cfg
status" "$(runw "$TMP/other/my.cfg" -c h.cfg web status)"
is_out "a -c path that is a command's name is the path" "host:mcp/mcp
config:$TMP/work/web
status" "$(run -c web mcp status)"
[ ! -e "$TMP/work/h.cfg" ] && [ ! -e "$TMP/work/web" ] && [ ! -e "$CFG" ] &&
    ok "a host-side command makes no config file" || bad "a host-side command made a config file"
[ ! -e "$TMP/other" ] && ok "...and no config folder" || bad "a host-side command made a config folder"

echo "a setup of its own:"
OWN="$TMP/try"
out="$(run -c "$OWN/wealthdb.cfg" status)"
has_item "-c elsewhere: the config's folder is the data root" "$OWN:$OWN" "$out"
has_item "...forwarded so" "WEALTHDB_DATA_ROOT=$OWN" "$out"
lacks_item "...and the data root of the environment is not mounted" "$DATA:$DATA" "$out"
lacks_item "...nor forwarded" "WEALTHDB_DATA_ROOT=$DATA" "$out"
has_item "the config file is mounted in it" "$OWN/wealthdb.cfg:$OWN/wealthdb.cfg" "$out"
has_item "the engine checks both mounts" "WEALTHDB_MOUNTS=$OWN/wealthdb.cfg:$OWN" "$out"
[ -d "$OWN" ] && ok "the folder is made" || bad "no folder $OWN"
out="$(runw "$OWN/wealthdb.cfg" status)"
has_item "WEALTHDB_CONFIG elsewhere: the same data root" "$OWN:$OWN" "$out"
has_item "...forwarded so" "WEALTHDB_DATA_ROOT=$OWN" "$out"
lacks_item "...and never the environment's" "WEALTHDB_DATA_ROOT=$DATA" "$out"
out="$(run -c "$OWN/./x/..//wealthdb.cfg" status)"
has_item "a path spelled another way: the clean folder" "$OWN:$OWN" "$out"
rm -f "$OWN/wealthdb.cfg"
for args in "-c $OWN/wealthdb.cfg config" "-c $OWN/./wealthdb.cfg config"; do
    # shellcheck disable=SC2086
    out="$(run $args)"
    n="$(grep -c -x -F -- "$OWN:$OWN" <<<"$out")"
    [ "$n" = 1 ] && ok "$args: the folder is mounted once, as the data root" || bad "$args: the folder is mounted $n times"
    lacks_item "$args: the folder is not mounted under another spelling" "$OWN/.:$OWN/." "$out"
    lacks_item "$args: the config is not mounted as a file" "$OWN/wealthdb.cfg:$OWN/wealthdb.cfg" "$out"
done
[ ! -e "$OWN/wealthdb.cfg" ] && ok "config makes no file in it" || bad "config made an empty file"
out="$(run -c "$CFG" status)"
has_item "-c on the default config: the data root of the environment" "$DATA:$DATA" "$out"
has_item "...forwarded so" "WEALTHDB_DATA_ROOT=$DATA" "$out"
# A demo's call: the config dir, the config and the data root are one
# folder, so the config is the default one under that config dir.
demo() {
    env PATH="$TMP/bin:$PATH" HOME="$TMP/home" XDG_CONFIG_HOME="$TMP/demo" WEALTHDB_DATA_ROOT="$TMP/demo" \
        WEALTHDB_IMAGE_TAG=t LANG=C TERM=xterm "$TREE/wealthdb/wealthdb" "$@"
}
out="$(demo -c "$TMP/demo/wealthdb.cfg" load -a)"
has_item "a demo's call: its root is the data root" "$TMP/demo:$TMP/demo" "$out"
has_item "...and its config the file mounted" "$TMP/demo/wealthdb.cfg:$TMP/demo/wealthdb.cfg" "$out"
out="$(demo config)"
n="$(grep -c -x -F -- "$TMP/demo:$TMP/demo" <<<"$out")"
[ "$n" = 1 ] && ok "a config folder that is the data root is mounted once" || bad "the demo folder is mounted $n times"

# What the host-side scripts are handed.
shown() { STUB_SHOW_SETUP=1 "$@"; }
SP="$TMP/my try/wealthdb.cfg"
for sub in web mcp collect; do
    script="$sub/$sub"
    [ "$sub" = collect ] && script=bin/wealthdb-collect
    is_out "-c elsewhere, $sub: the folder and the words of its commands" "host:$script
config:$SP
root:$TMP/my try
collect:wealthdb -c '$SP' collect
program:wealthdb -c '$SP'" "$(shown run -c "$SP" "$sub" start)"
done
is_out "WEALTHDB_CONFIG elsewhere: the same, and the variable as it was set" "host:web/web
config:$OWN/wealthdb.cfg
root:$OWN
collect:wealthdb -c $OWN/wealthdb.cfg collect
program:wealthdb -c $OWN/wealthdb.cfg" "$(shown runw "$OWN/wealthdb.cfg" web status)"
is_out "the default config: the data root of the environment, and no words" "host:bin/wealthdb-collect
config:unset
root:$DATA
collect:unset
program:unset" "$(shown run collect testcol load)"
is_out "...also behind -c, and inherited words go" "host:mcp/mcp
config:$CFG
root:$DATA
collect:unset
program:unset" "$(WEALTHDB_COLLECT=stale WEALTHDB_PROGRAM=stale shown run -c "$CFG" mcp status)"
[ ! -e "$TMP/my try" ] && ok "a host-side command makes no folder for the setup" || bad "a host-side command made the setup's folder"
rm -rf "$OWN" "$TMP/demo"

echo "relative and spaced folders:"
rel() {
    env PATH="$TMP/bin:$PATH" HOME="$TMP/home" XDG_CONFIG_HOME="$1" WEALTHDB_DATA_ROOT="$2" \
        WEALTHDB_IMAGE_TAG=t LANG=C TERM=xterm "$TREE/wealthdb/wealthdb" "${@:3}"
}
out="$(rel relcfg reldata status)"
has_item "a relative XDG_CONFIG_HOME is made absolute" "$TMP/work/relcfg/wealthdb.cfg:$TMP/work/relcfg/wealthdb.cfg" "$out"
has_item "...and forwarded so" "XDG_CONFIG_HOME=$TMP/work/relcfg" "$out"
has_item "a relative data root is made absolute" "$TMP/work/reldata:$TMP/work/reldata" "$out"
has_item "...and forwarded so" "WEALTHDB_DATA_ROOT=$TMP/work/reldata" "$out"
out="$(rel "$TMP/my config" "$TMP/my data" config)"
has_item "a folder with a space is one mount" "$TMP/my config:$TMP/my config" "$out"
has_item "a data root with a space is one mount" "$TMP/my data:$TMP/my data" "$out"
[ -d "$TMP/my config" ] && ok "the spaced folder is made" || bad "no spaced folder"

echo "the host's name:"
for args in "status" "config"; do
    has_item "$args: host.docker.internal is this computer" "--add-host=host.docker.internal:host-gateway" "$(run $args)"
done

echo "colours:"
has_item "NO_COLOR goes to the engine" "NO_COLOR=1" "$(NO_COLOR=1 run status)"
lacks "...and only when set" "NO_COLOR" "$(run status)"

echo "a missing image:"
out="$(STUB_NO_IMAGE=1 run status 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "a missing image exits 2" || bad "a missing image exits $rc"
has "...naming the build" "wealthdb: the image wealthdb:t is not built yet. Build it with: wealthdb build" "$out"
lacks_item "...and nothing runs" "run" "$out"
has_item "a run never pulls" "--pull=never" "$(run status)"

summary "engine mount tests"
