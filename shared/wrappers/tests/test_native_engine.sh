#!/usr/bin/env bash
# Behaviour test of a tree of the native engine (engine-mode.sh):
#   - ./configure records the kind of engine and the tools, and refuses
#     what make or the wrappers could not read back;
#   - the launcher runs the program beside it, tells it the paths and its
#     kind, starts no container, and says how to build a missing engine;
#   - `wealthdb build` is a `go build` with the version stamp, which a
#     source archive states in .archive-version;
#   - `wealthdb collect` reaches the collectors' dispatcher in both kinds
#     of tree;
#   - wealthdb-go runs the recorded Go, and tells it the harness's gold
#     paths as absolute ones;
#   - a collector's verb that writes its database runs through the
#     engine's hold-lock, on the lock file beside that database;
#   - prune and dedup run the recorded Python.
# Go, Java, docker and the engine are stubs that record how they were
# called. The tree is a copy, so the checkout's own config.mk plays no
# part.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
# This test names each tree's own record.
unset WEALTHDB_BUILD_CONFIG

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# The interpreter ./configure is given, as an absolute path: the one
# make names for the wrapper tests, which is new enough for it.
PY="$(command -v "${PYTHON:-python3}")"
unset WEALTHDB_CONFIG WEALTHDB_ENGINE WEALTHDB_ENTRY WEALTHDB_IMAGE_TAG \
      WEALTHDB_GOLD_VERIFY WEALTHDB_GOLD_BEFORE WEALTHDB_REPO GO PYTHON JAVA JAVA_HOME
export HOME="$TMP/home"
sandbox_env "$TMP"
mkdir -p "$HOME" "$TMP/bin"

# The stubs. Each appends its name, then its argv one item per line, to
# $STUB_LOG.
stub() { # NAME [BODY]
    cat > "$TMP/bin/$1" <<STUB
#!/usr/bin/env bash
{ echo "$1"; printf '%s\n' "\$@"; } >> "\$STUB_LOG"
${2:-}
STUB
    chmod +x "$TMP/bin/$1"
}
stub docker
# go also records a harness path it is told, when it is told one.
stub go 'case "$1" in env) echo "go${STUB_GO_VERSION:-1.99.0}" ;; esac
[ -z "${WEALTHDB_GOLD_VERIFY:-}" ] || echo "env:GOLD_VERIFY=$WEALTHDB_GOLD_VERIFY" >> "$STUB_LOG"'
stub java 'case "$1" in -version) { [ -z "${STUB_JAVA_NOTE:-}" ] || echo "Picked up _JAVA_OPTIONS: $STUB_JAVA_NOTE"
    echo "openjdk version \"${STUB_JAVA_VERSION:-21.0.1}\" 2026-01-01"; } >&2 ;; esac'
stub cc
# A Python that only records: the one a hand-written config.mk names.
stub pystub
# python3.15, the first name ./configure looks for, runs the real one.
stub python3.15 "exec \"$PY\" \"\$@\""
export STUB_LOG="$TMP/log"
REAL_PATH="$PATH"
export PATH="$TMP/bin:$PATH"

# A tree in the checkout's layout: the launchers, the shared helpers,
# ./configure and what it reads, and one Docker collector.
TREE="$TMP/tree"
mkdir -p "$TREE/wealthdb" "$TREE/shared/wrappers" "$TREE/bin" "$TREE/collectors/testcol"
cp "$ROOT/wealthdb/wealthdb" "$ROOT/wealthdb/wealthdb-go" "$ROOT/wealthdb/go.mod" "$TREE/wealthdb/"
cp "$ROOT"/shared/wrappers/*.sh "$TREE/shared/wrappers/"
cp "$ROOT/configure" "$TREE/"
cat > "$TREE/bin/wealthdb-collect" <<'STUB'
#!/usr/bin/env bash
{ echo "collect"; printf '%s\n' "$@"; } >> "$STUB_LOG"
STUB
chmod +x "$TREE/bin/wealthdb-collect"
: > "$TREE/collectors/testcol/Dockerfile"
cat > "$TREE/collectors/testcol/testcol" <<'WRAP'
#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
NAME="testcol"
ENV_VARS=()
source "$HERE/../../shared/wrappers/wrapper-lib.sh"
wrapper_init
wrapper_main "$@"
WRAP
chmod +x "$TREE/collectors/testcol/testcol"

# engine_stub: the native engine of TREE, a stub that records its argv
# and the variables the launcher tells it.
engine_stub() {
    cat > "$TREE/wealthdb/wealthdb-engine" <<'STUB'
#!/usr/bin/env bash
{ echo "engine"; printf '%s\n' "$@"
  echo "env:ENGINE=${WEALTHDB_ENGINE:-}"; echo "env:DATA_ROOT=${WEALTHDB_DATA_ROOT:-}"
  echo "env:XDG_CONFIG_HOME=${XDG_CONFIG_HOME:-}"; echo "env:CONFIG=${WEALTHDB_CONFIG:-}"
  echo "env:LOCALE=${WEALTHDB_LOCALE-unset}"; echo "env:ENTRY=${WEALTHDB_ENTRY:-}"
  # From the environment: bash gives itself a TERM when it has none.
  echo "env:TERM=$(env | sed -n 's/^TERM=//p')"; } >> "$STUB_LOG"
# hold-lock FILE -- COMMAND...: run the command, as the engine does.
if [ "${1:-}" = hold-lock ]; then shift 3; exec "$@"; fi
STUB
    chmod +x "$TREE/wealthdb/wealthdb-engine"
}
# run CMD...: run CMD with a fresh log, and print the log. rc prints the
# status of the last run, which a command substitution would lose.
run() { : > "$STUB_LOG"; "$@" > "$TMP/out" 2> "$TMP/err"; echo "$?" > "$TMP/rc"; cat "$STUB_LOG"; }
rc() { cat "$TMP/rc"; }
# configure ARGS...: ./configure in TREE, with the interpreter of this
# test unless ARGS name another. search_python leaves the choice to it.
configure() { (cd "$TREE" && ./configure PYTHON="$PY" "$@") > "$TMP/out" 2> "$TMP/err"; }
search_python() { (cd "$TREE" && ./configure "$@") > "$TMP/out" 2> "$TMP/err"; }

echo "./configure:"
configure --engine=native || bad "configure --engine=native failed: $(cat "$TMP/err")"
mk="$(cat "$TREE/config.mk")"
has "native is recorded" "ENGINE=native" "$mk"
has "the Go on PATH is recorded, absolute" "GO=$TMP/bin/go" "$mk"
has "the Java on PATH is recorded, absolute" "JAVA=$TMP/bin/java" "$mk"
has "Python is recorded" "PYTHON=/" "$mk"
lacks "no prefix is recorded when none is given" "PREFIX=" "$mk"
lacks "and no bin directory" "BINDIR=" "$mk"
has "it names the next command" "Run: make all" "$(cat "$TMP/out")"
has "the usage says what still needs Docker" "collectors that run in a container need Docker." \
    "$(cd "$TREE" && ./configure --help)"

configure --engine=native --prefix=/opt/w
mk="$(cat "$TREE/config.mk")"
has "a prefix is recorded" "PREFIX=/opt/w" "$mk"
has "the bin directory follows the prefix" "BINDIR=/opt/w/bin" "$mk"
configure --engine=native --prefix=/opt/w --bindir=/opt/tools/bin
has "a bin directory that is named is recorded as given" "BINDIR=/opt/tools/bin" "$(cat "$TREE/config.mk")"
configure --engine=native --bindir=/opt/tools/bin
mk="$(cat "$TREE/config.mk")"
has "also with no prefix" "BINDIR=/opt/tools/bin" "$mk"
lacks "which is then not recorded" "PREFIX=" "$mk"

search_python --engine=native || bad "configure with no PYTHON= failed: $(cat "$TMP/err")"
recorded="$(sed -n 's/^PYTHON=//p' "$TREE/config.mk")"
[ -n "$recorded" ] && "$recorded" -c 'import sys; sys.exit(0 if sys.version_info >= (3, 10) else 1)' &&
    ok "with no PYTHON=, a Python of 3.10 or later is found and recorded" || bad "with no PYTHON=: recorded '$recorded'"
lacks "the interpreter is recorded, not the name it was found under" "PYTHON=$TMP/bin/python3.15" "$(cat "$TREE/config.mk")"
# A virtual environment needs no network to make when it gets no pip.
if "$PY" -m venv --without-pip "$TMP/venv" >/dev/null 2>&1; then
    configure --engine=native PYTHON="$TMP/venv/bin/python" || bad "a virtual environment's Python is refused: $(cat "$TMP/err")"
    mk="$(cat "$TREE/config.mk")"
    lacks "a virtual environment's Python is not what is recorded" "PYTHON=$TMP/venv" "$mk"
    has "its base interpreter is" "PYTHON=/" "$mk"
else
    echo "  skip a virtual environment's Python ($PY makes none)"
fi

STUB_JAVA_NOTE=-Dx=y STUB_JAVA_VERSION=21.0.13.1 configure --engine=native JAVA=java ||
    bad "a Java that states its options before its version is refused: $(cat "$TMP/err")"
has "a Java that states its options first is read by its version line" "JAVA=$TMP/bin/java" "$(cat "$TREE/config.mk")"

STUB_JAVA_VERSION=17.0.2 configure --engine=native
lacks "a Java older than 21 is not recorded" "JAVA=" "$(cat "$TREE/config.mk")"
has "and the dashboards' need is stated" "the dashboards need Java 21" "$(cat "$TMP/out")"
if STUB_JAVA_VERSION=17.0.2 configure --engine=native JAVA=java 2>/dev/null; then
    bad "a Java older than 21, named with JAVA=, is accepted"
else ok "a Java older than 21, named with JAVA=, is refused"; fi

mkdir -p "$TMP/jdk/bin"
cp "$TMP/bin/java" "$TMP/jdk/bin/java"
JAVA_HOME="$TMP/jdk" configure --engine=native || bad "configure with JAVA_HOME failed: $(cat "$TMP/err")"
has "the Java of JAVA_HOME comes before the one on PATH" "JAVA=$TMP/jdk/bin/java" "$(cat "$TREE/config.mk")"
JAVA_HOME=/nonexistent configure --engine=native ||
    bad "a JAVA_HOME that names no Java stops configure: $(cat "$TMP/err")"
lacks "a JAVA_HOME that names no Java records none" "JAVA=" "$(cat "$TREE/config.mk")"
has "and the dashboards' need is stated" "the dashboards need Java 21" "$(cat "$TMP/out")"

before="$(cat "$TREE/config.mk")"
# refused DESC NEEDLE ARGS...: ./configure ARGS fails with a message that
# holds NEEDLE, and leaves config.mk as it was.
refused() {
    local desc="$1" needle="$2"; shift 2
    if configure "$@"; then bad "$desc: accepted"; else has "$desc: refused" "$needle" "$(cat "$TMP/err")"; fi
    [ "$(cat "$TREE/config.mk")" = "$before" ] || bad "$desc: config.mk changed"
}
refused "an unknown kind of engine" "--engine=docker is no kind of engine" --engine=docker
refused "an unknown argument" "unknown argument: --with-magic" --with-magic
refused "a relative prefix" "--prefix=opt is not an absolute path" --engine=native --prefix=opt
refused "a relative bin directory" "--bindir=bin is not an absolute path" --engine=native --bindir=bin
refused "a prefix with a space" "holds a character other than" --engine=native "--prefix=/opt/my wealthdb"
refused "a prefix with a character the shell reads" "holds a character other than" --engine=native '--prefix=/opt/w;x'
refused "a bin directory with a backtick" "holds a character other than" --engine=native '--bindir=/opt/`x`'
refused "a Go that is no program" "no Go found (/nonexistent/go)" --engine=native GO=/nonexistent/go
refused "a Python that is no program" "PYTHON=/nonexistent/python is no program" --engine=native PYTHON=/nonexistent/python
refused "a Java that is no program" "JAVA=/nonexistent/java is not Java 21" --engine=native JAVA=/nonexistent/java
STUB_GO_VERSION=1.20.1 refused "a Go older than go.mod's" "is Go 1.20.1" --engine=native

configure || bad "a bare configure failed: $(cat "$TMP/err")"
mk="$(cat "$TREE/config.mk")"
has "a bare configure records the container engine" "ENGINE=container" "$mk"
lacks "and no Go" "GO=" "$mk"

echo "the launcher, native:"
configure --engine=native
rm -f "$TREE/wealthdb/wealthdb-engine"
out="$(run "$TREE/wealthdb/wealthdb" holdings global)"
[ "$(rc)" = 2 ] && ok "no engine built: exit 2" || bad "no engine built: exit $(rc)"
has "no engine built: says how to build it" "Run: make all" "$(cat "$TMP/err")"
lacks "no engine built: no container" "docker" "$out"

engine_stub
export XDG_CONFIG_HOME="$TMP/cfg" WEALTHDB_DATA_ROOT="$TMP/data"
out="$(run "$TREE/wealthdb/wealthdb" -r holdings global)"
has_item "the engine runs" "engine" "$out"
has_item "with the arguments as given" "holdings" "$out"
has_item "and the flags before them" "-r" "$out"
lacks "no container" "docker" "$out"
has_item "it is told its kind" "env:ENGINE=native" "$out"
has_item "it is told the data root" "env:DATA_ROOT=$TMP/data" "$out"
has_item "it is told the config folder" "env:XDG_CONFIG_HOME=$TMP/cfg" "$out"
has_item "no locale outside config" "env:LOCALE=unset" "$out"
[ -d "$TMP/data" ] && ok "the data root is made" || bad "the data root is not made"
[ ! -e "$TMP/cfg/wealthdb.cfg" ] && ok "no empty config file is made" || bad "an empty config file was made"

out="$(cd "$TMP" && run "$TREE/wealthdb/wealthdb" -c rel/my.cfg status)"
has_item "a relative -c is made absolute" "$TMP/rel/my.cfg" "$out"
out="$(WEALTHDB_CONFIG="$TMP/other/w.cfg" run "$TREE/wealthdb/wealthdb" status)"
has_item "WEALTHDB_CONFIG reaches the engine" "env:CONFIG=$TMP/other/w.cfg" "$out"
out="$(LANG=de_CH.UTF-8 run "$TREE/wealthdb/wealthdb" config)"
lacks_item "config gets a locale" "env:LOCALE=unset" "$out"
out="$(run env -u TERM "$TREE/wealthdb/wealthdb" config)"
has_item "config with no TERM gets dumb, as in a container" "env:TERM=dumb" "$out"
out="$(TERM=xterm run "$TREE/wealthdb/wealthdb" config)"
has_item "config keeps a TERM that is set" "env:TERM=xterm" "$out"
out="$(run env -u TERM "$TREE/wealthdb/wealthdb" status)"
has_item "no TERM is made up outside config" "env:TERM=" "$out"

echo "the command a server is started by (WEALTHDB_ENTRY):"
out="$(run "$TREE/wealthdb/wealthdb" status)"
has_item "an absolute path is kept" "env:ENTRY=$TREE/wealthdb/wealthdb" "$out"
out="$(cd "$TREE" && run wealthdb/wealthdb status)"
has_item "a relative path is made absolute" "env:ENTRY=$TREE/wealthdb/wealthdb" "$out"
ln -s "$TREE/wealthdb/wealthdb" "$TMP/bin/wealthdb"
out="$(run wealthdb status)"
has_item "a link on PATH is kept as the link" "env:ENTRY=$TMP/bin/wealthdb" "$out"
has_item "and the engine beside its target runs" "engine" "$out"
out="$(cd "$TREE/wealthdb" && run sh wealthdb status)"
has_item "a bare name is looked up on PATH" "env:ENTRY=$TMP/bin/wealthdb" "$out"
out="$(WEALTHDB_ENTRY=/opt/w/bin/wealthdb run "$TREE/wealthdb/wealthdb" status)"
has_item "a value that is set is kept" "env:ENTRY=/opt/w/bin/wealthdb" "$out"
rm -f "$TMP/bin/wealthdb"

printf 'ENGINE = native  \n' > "$TREE/config.mk"
out="$(run "$TREE/wealthdb/wealthdb" status)"
has_item "blanks around the = of config.mk are no part of the value" "env:ENGINE=native" "$out"
configure --engine=native

echo "build, collect and go:"
out="$(run "$TREE/wealthdb/wealthdb" build)"
has_item "build runs go" "go" "$out"
has_item "go builds" "build" "$out"
has_item "into the engine beside the launcher" "$TREE/wealthdb/wealthdb-engine" "$out"
has_item "the engine's main package" "./cmd/wealthdb" "$out"
lacks "build starts no docker build" "docker" "$out"
out="$(run "$TREE/wealthdb/wealthdb" build -tags x)"
[ "$(printf '%s\n' "$out" | tail -n 4)" = "$TREE/wealthdb/wealthdb-engine
-tags
x
./cmd/wealthdb" ] && ok "extra arguments go to go build, before the package" || bad "build -tags x: $(printf '%s ' $out)"
printf 'commit: 0123456789abcdef0123456789abcdef01234567\ndescribe: v7.8.9\n' > "$TREE/wealthdb/.archive-version"
out="$(run "$TREE/wealthdb/wealthdb" build)"
has "a source archive of a release states its tag" "internal/version.Tag=v7.8.9 " "$out"
has "and its commit" "internal/version.Commit=0123456" "$out"
# The archive unpacked inside another project's git repository.
git -C "$TMP" init -q 2>/dev/null
out="$(run "$TREE/wealthdb/wealthdb" build)"
[ "$(rc)" = 0 ] && ok "a git repository around the archive does not stop the build" || bad "an archive inside a git repository: exit $(rc)"
has "nor is it asked: the archive's tag stands" "internal/version.Tag=v7.8.9 " "$out"
has "and its commit" "internal/version.Commit=0123456" "$out"
lacks "which is not marked as changed" "-dirty" "$out"
rm -rf "$TMP/.git"
printf 'commit: 0123456789abcdef0123456789abcdef01234567\ndescribe: v7.8.9-4-gabc1234\n' > "$TREE/wealthdb/.archive-version"
out="$(run "$TREE/wealthdb/wealthdb" build)"
has "an archive past a release states no tag" "internal/version.Tag= " "$out"
has "and the release as its base" "internal/version.Base=v7.8.9 " "$out"
cp "$ROOT/wealthdb/.archive-version" "$TREE/wealthdb/.archive-version"
out="$(run "$TREE/wealthdb/wealthdb" build)"
has "a file git did not fill in states nothing" "internal/version.Tag= -X" "$out"
has "and no commit" "internal/version.Commit=" "$out"
lacks "(not the placeholder)" 'Format' "$out"

out="$(run "$TREE/wealthdb/wealthdb" collect testcol load --x)"
[ "$out" = "collect
testcol
load
--x" ] && ok "collect reaches the dispatcher with its arguments" || bad "collect: $out"
out="$(cd "$TMP" && run "$TREE/wealthdb/wealthdb" -r -c rel2/host.cfg collect testcol load -c y)"
[ "$out" = "collect
testcol
load
-c
y" ] && ok "collect behind global flags reaches the dispatcher, the flags dropped" || bad "-r -c FILE collect: $out"
[ ! -e "$TMP/rel2" ] && ok "...and makes no config folder" || bad "-c FILE collect made $TMP/rel2"
out="$(run "$TREE/wealthdb/wealthdb-go" vet ./...)"
[ "$out" = "go
vet
./..." ] && ok "wealthdb-go runs the recorded Go" || bad "wealthdb-go: $out"
mkdir -p "$TMP/gold/a"
out="$(cd "$TMP/gold" && WEALTHDB_GOLD_VERIFY=a/x.db run "$TREE/wealthdb/wealthdb-go" test ./internal/gold)"
has_item "a harness's relative gold path reaches Go absolute" "env:GOLD_VERIFY=$TMP/gold/a/x.db" "$out"
(cd "$TMP/gold" && WEALTHDB_GOLD_VERIFY=missing/x.db run "$TREE/wealthdb/wealthdb-go" test ./internal/gold) > /dev/null
[ "$(rc)" = 2 ] && ok "a gold path in no directory exits 2" || bad "a gold path in no directory: exit $(rc)"

echo "a collector's database lock:"
mkdir -p "$HOME/.secrets" "$TMP/data/testcol"
col() { run "$TREE/collectors/testcol/testcol" "$@"; }
out="$(col load)"
want="engine
hold-lock
$TMP/data/testcol/.wealthdb-silver.lock
--
docker
run"
# The image check comes first; the engine's call follows it.
[ "$(printf '%s\n' "$out" | sed -n '/^engine$/,$p' | sed -n '1,6p')" = "$want" ] && ok "load runs its container through hold-lock, beside the database" ||
    bad "load: $(printf '%s ' $out | cut -c1-300)"
out="$(col download)"
lacks "download takes no lock" "hold-lock" "$out"
has_item "download runs its container" "docker" "$out"
mkdir -p "$TMP/elsewhere"
out="$(col load --silver-db "$TMP/elsewhere/s.db")"
has_item "an explicit database path moves the lock beside it" "$TMP/elsewhere/.wealthdb-silver.lock" "$out"
rm -f "$TREE/wealthdb/wealthdb-engine"
out="$(col load)"
lacks "no engine built: no lock" "hold-lock" "$out"
has_item "no engine built: the container runs" "docker" "$out"

echo "the recorded Python:"
: > "$TREE/collectors/testcol/prune.py"
printf 'ENGINE=native\nPYTHON=%s\n' "$TMP/bin/pystub" > "$TREE/config.mk"
out="$(col prune --dry-run)"
[ "$(printf '%s\n' "$out" | sed -n '1,2p')" = "pystub
$TREE/collectors/testcol/prune.py" ] && ok "prune runs prune.py on it" || bad "prune: $(printf '%s ' $out)"
has_item "with its arguments" "--dry-run" "$out"
out="$(WEALTHDB_REPO="$TREE" run "$ROOT/bin/wealthdb-collect" dedup --dry-run)"
[ "$out" = "pystub
-m
collectorkit.dedup
--dry-run" ] && ok "dedup runs the sweep on it" || bad "dedup: $(printf '%s ' $out)"

echo "the container engine:"
engine_stub
configure
out="$(col load)"
lacks "a collector takes no lock" "hold-lock" "$out"
out="$(run "$TREE/wealthdb/wealthdb" status)"
has_item "the launcher starts a container" "docker" "$out"
lacks_item "and not the program" "engine" "$out"
has_item "the container is told its kind" "WEALTHDB_ENGINE=container" "$out"
out="$(run "$TREE/wealthdb/wealthdb" collect list)"
[ "$out" = "collect
list" ] && ok "collect reaches the dispatcher" || bad "collect: $out"
printf 'ENGINE=podman\n' > "$TREE/config.mk"
run "$TREE/wealthdb/wealthdb" status > /dev/null
[ "$(rc)" = 2 ] && ok "an unknown kind in config.mk stops the launcher" || bad "an unknown kind: exit $(rc)"
has "and names ./configure" "run ./configure again" "$(cat "$TMP/err")"
out="$(col load)"
[ "$(rc)" = 2 ] && ok "and a collector's verb that writes its database" || bad "an unknown kind, col load: exit $(rc)"
has "which names ./configure too" "run ./configure again" "$(cat "$TMP/err")"
lacks_item "and runs no container" "run" "$out"

PATH="$REAL_PATH"
summary "native engine tests"
