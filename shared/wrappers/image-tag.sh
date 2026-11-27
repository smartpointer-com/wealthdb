# image-tag.sh — the tag of every image a checkout builds, runs and
# removes, and how a build decides to skip and waits its turn: the
# inputs hash and its label (wealthdb_inputs_hash,
# wealthdb_build_unchanged) and the build lock (wealthdb_build_lock).
# It also holds the engine's host paths and the rule of a setup of its
# own (wealthdb_engine_paths), which the engine wrapper, mcp, web and the
# collectors' path helper resolve alike. POSIX sh; sourced by the engine
# wrappers, the collector wrappers, mcp and web, and by the Makefile.
#
# WEALTHDB_IMAGE_TAG wins when it is set. Otherwise the first line of
# .image-tag at the repository root names the tag: the file is
# git-ignored, so a second checkout pins its own tag once and no shell
# in it can build or run another checkout's images. Without the file the
# tag is latest. A .image-tag that names no valid tag is an error, never
# a fall back to latest.

# wealthdb_image_tag ROOT: print the tag for the checkout at ROOT; exit
# status 2, with a message on stderr, when ROOT/.image-tag is malformed.
wealthdb_image_tag() {
    if [ -n "${WEALTHDB_IMAGE_TAG:-}" ]; then
        printf '%s\n' "$WEALTHDB_IMAGE_TAG"
        return 0
    fi
    _wit_file="$1/.image-tag"
    if [ ! -e "$_wit_file" ] && [ ! -L "$_wit_file" ]; then
        printf 'latest\n'
        return 0
    fi
    # Present but no readable file (a directory, a dangling link): an
    # error like a malformed tag, never latest.
    if [ ! -f "$_wit_file" ] || [ ! -r "$_wit_file" ]; then
        printf '%s is not a readable file\n' "$_wit_file" >&2
        return 2
    fi
    _wit_tag="$(sed -n '1{s/^[[:space:]]*//;s/[[:space:]]*$//;p;}' "$_wit_file")"
    case "$_wit_tag" in
        '' | [.-]* | *[!A-Za-z0-9_.-]*)
            printf '%s names no image tag (%s); write one tag such as feature-x on its first line\n' \
                "$_wit_file" "'$_wit_tag'" >&2
            return 2
            ;;
    esac
    if [ "${#_wit_tag}" -gt 128 ]; then
        printf '%s names a tag longer than 128 characters\n' "$_wit_file" >&2
        return 2
    fi
    printf '%s\n' "$_wit_tag"
}

# wealthdb_from_refs DOCKERFILE prints the image each of its FROM lines
# names, one per line, with ${WEALTHDB_IMAGE_TAG} replaced; nothing when
# the file is not there.
wealthdb_from_refs() {
    [ -f "$1" ] || return 0
    sed -n 's/^FROM[[:space:]][[:space:]]*\([^[:space:]]*\).*/\1/p' "$1" |
        sed "s/\${WEALTHDB_IMAGE_TAG}/${WEALTHDB_IMAGE_TAG}/g"
}

# wealthdb_from_ref DOCKERFILE prints the image its first FROM names,
# with ${WEALTHDB_IMAGE_TAG} replaced, or "missing" when the file is
# not there.
wealthdb_from_ref() {
    if [ ! -f "$1" ]; then
        echo missing
        return 0
    fi
    wealthdb_from_refs "$1" | sed -n '1p'
}

# wealthdb_dockerfile_arg DOCKERFILE NAME prints the default the file
# gives `ARG NAME=`, or nothing: the place a pinned version or checksum
# is read from by everything that is not `docker build`.
wealthdb_dockerfile_arg() {
    sed -n "s/^ARG $2=//p" "$1" 2>/dev/null | sed -n '1p'
}

# wealthdb_from_id DOCKERFILE prints the id of that image, or "missing"
# when it is not local.
wealthdb_from_id() {
    _wfrom="$(wealthdb_from_ref "$1")"
    if [ -z "$_wfrom" ] || [ "$_wfrom" = missing ]; then
        echo missing
        return 0
    fi
    docker image inspect -f '{{.Id}}' "$_wfrom" 2>/dev/null || echo missing
}

# wealthdb_export_image_tag ROOT: set WEALTHDB_IMAGE_TAG to the tag for
# the checkout at ROOT and export it, so a wrapper the caller starts
# reads the same tag; exit status 2 as wealthdb_image_tag's.
wealthdb_export_image_tag() {
    WEALTHDB_IMAGE_TAG="$(wealthdb_image_tag "$1")" || return 2
    export WEALTHDB_IMAGE_TAG
}

# wealthdb_inputs_hash PATH... [-- LITERAL...] prints a sha256 of those
# files and directories: each file's name, mode and bytes, and a
# symbolic link's target rather than what it points to. A name is
# relative to the PATH it was found under, kept with that PATH's last
# component only, so moving the checkout does not change the hash.
# Arguments after -- are text, never paths, so a version string is not
# read as a file in the working directory. A path that is not there is
# still part of the hash, so a missing input does not look like an
# empty one. A directory skips a virtualenv, bytecode and egg-info.
# shared/.dockerignore excludes the same names from the base images'
# context; keep the two lists in step.
# --ignore FILE, first, also leaves out of each directory what FILE, the
# .dockerignore of its build context, leaves out of that context. It
# reads two forms of line: a name at the directory's top (docs) and a
# file suffix at any depth (**/*.md). Any other line fails the hash, so
# an image whose context the hash cannot follow is always built. A
# missing FILE leaves nothing out.
wealthdb_inputs_hash() {
    python3 - "$@" <<'PY'
import hashlib, os, re, stat, sys
h = hashlib.sha256()
skip = {".venv", "__pycache__", ".pytest_cache", ".git", ".mypy_cache"}
ignore_top, ignore_suffixes = set(), ()

def add_file(path, name, st):
    h.update(b"%s\0%o\0" % (name.encode(), st.st_mode))
    if stat.S_ISLNK(st.st_mode):
        h.update(os.readlink(path).encode())
        return
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)

def add(path):
    top = os.path.normpath(os.path.abspath(path))
    label = os.path.basename(top)
    if os.path.isfile(path):
        add_file(path, label, os.stat(path))
        return
    if not os.path.isdir(path):
        h.update(label.encode())
        h.update(b"\0missing")
        return
    for root, dirs, files in os.walk(path):
        dirs[:] = sorted(
            d for d in dirs if d not in skip and not d.endswith(".egg-info")
            and not (root == path and d in ignore_top))
        if root == path:
            files = [f for f in files if f not in ignore_top]
        files = [f for f in files if not f.endswith(ignore_suffixes)]
        # os.walk lists a link to a directory with the directories and
        # does not enter it; it is hashed as a link.
        links = [d for d in dirs if os.path.islink(os.path.join(root, d))]
        for name in sorted(files + links):
            if name.endswith((".pyc", ".egg-link")):
                continue
            full = os.path.join(root, name)
            rel = os.path.relpath(full, path)
            add_file(full, label + "/" + rel, os.lstat(full))

args = sys.argv[1:]
if args[:1] == ["--ignore"]:
    ignore_file, args = args[1], args[2:]
    suffixes = []
    if os.path.isfile(ignore_file):
        for line in open(ignore_file):
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            m = re.fullmatch(r"\*\*/\*(\.[A-Za-z0-9]+)", line)
            if m:
                suffixes.append(m.group(1))
            elif re.fullmatch(r"[A-Za-z0-9_.-]+/?", line) and line not in (".", ".."):
                ignore_top.add(line.rstrip("/"))
            else:
                sys.exit(f"wealthdb_inputs_hash: {ignore_file}: cannot follow the line {line!r}")
    ignore_suffixes = tuple(suffixes)
if "--" in args:
    cut = args.index("--")
    paths, literals = args[:cut], args[cut + 1:]
else:
    paths, literals = args, []
for p in paths:
    add(p)
for lit in literals:
    h.update(b"\0lit\0")
    h.update(lit.encode())
print(h.hexdigest())
PY
}

# wealthdb_labeled IMAGE HASH: true when IMAGE was built from HASH. An
# empty HASH matches nothing, so a missing or unlabeled image is never
# taken as built from it.
wealthdb_labeled() {
    _wlab="$(docker image inspect -f '{{index .Config.Labels "wealthdb.inputs"}}' "$1" 2>/dev/null || true)"
    [ -n "$2" ] && [ "$_wlab" = "$2" ]
}

# wealthdb_build_unchanged IMAGE HASH ARGC: true, printing "build: IMAGE
# unchanged", when a build of IMAGE can be skipped: ARGC (the count of
# extra build arguments) is 0, HASH is not empty and IMAGE was built
# from HASH. Any extra argument, such as --no-cache, always builds.
wealthdb_build_unchanged() {
    [ "$3" -eq 0 ] && wealthdb_labeled "$1" "$2" || return 1
    printf 'build: %s unchanged\n' "$1"
}

# wealthdb_build_lock PATH: wait for the lock file PATH, then hold it on
# file descriptor 9 until this shell exits. Two builds of one image
# never run at once. Every command the shell starts inherits the
# descriptor, so the lock is free once the holder and the commands it
# started have exited, however they end: an interrupted or killed build
# leaves no lock behind, and a command that outlives its build (none of
# the builds starts one) would keep it. The file names the PID of the
# shell that holds it, and a wait past three seconds prints the path
# and that PID on stderr. A lock file removed while held (make clean)
# is opened again, so a second build never takes a file the first
# still holds.
wealthdb_build_lock() {
    mkdir -p "$(dirname "$1")" || return 1
    while :; do
        exec 9>>"$1"
        python3 - "$1" <<'PY'
import fcntl, os, sys, time
path = sys.argv[1]
start, said = time.monotonic(), False
while True:
    try:
        fcntl.flock(9, fcntl.LOCK_EX | fcntl.LOCK_NB)
        break
    except BlockingIOError:
        if not said and time.monotonic() - start >= 3:
            try:
                with open(path) as fh:
                    holder = fh.read().strip()
            except OSError:
                holder = ""
            print(f"build: waiting for {path}" + (f", held by pid {holder}" if holder else ""),
                  file=sys.stderr, flush=True)
            said = True
        time.sleep(0.2)
# A file removed while it was held is not the lock any more.
try:
    same = os.path.samestat(os.fstat(9), os.stat(path))
except FileNotFoundError:
    same = False
if not same:
    sys.exit(75)
os.ftruncate(9, 0)
os.write(9, f"{os.getppid()}\n".encode())
PY
        case $? in
            0) return 0 ;;
            75) mkdir -p "$(dirname "$1")" || return 1 ;;
            *) return 1 ;;
        esac
    done
}

# wealthdb_clean_build_locks DIR: remove the lock files under DIR that no
# build holds, and DIR once it is empty. A held lock stays, so a build
# that starts during a clean still waits for the one that holds it.
wealthdb_clean_build_locks() {
    [ -d "$1" ] || return 0
    python3 - "$1" <<'PY'
import fcntl, os, sys
top = sys.argv[1]
for name in os.listdir(top):
    path = os.path.join(top, name)
    if not os.path.isfile(path):
        continue
    with open(path, "a") as fh:
        try:
            fcntl.flock(fh, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print(f"build: {path} is held, kept", file=sys.stderr)
            continue
        os.unlink(path)
try:
    os.rmdir(top)
except OSError:
    pass
PY
}

# wealthdb_image_state IMAGE asks Docker for IMAGE, once, and sets
# IMAGE_STATE to the answer: present (IMAGE_ID then holds the image's
# id), missing, noclient (no docker program on PATH, so nothing was
# asked) or down (Docker did not answer; DOCKER_ERROR holds the first
# line of what it said instead).
wealthdb_image_state() {
    IMAGE_ID='' DOCKER_ERROR=''
    if ! command -v docker >/dev/null 2>&1; then
        IMAGE_STATE=noclient
        return 0
    fi
    if IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$1" 2>&1)"; then
        IMAGE_STATE=present
        return 0
    fi
    case "$IMAGE_ID" in
        *"No such image"*|*"No such object"*) IMAGE_STATE=missing ;;
        *)  IMAGE_STATE=down
            DOCKER_ERROR="$(printf '%s\n' "$IMAGE_ID" | sed -n '1p')" ;;
    esac
    IMAGE_ID=''
}

# wealthdb_require_image WHO IMAGE HINT — return 1, with a message that
# ends in HINT, the sentence that says how IMAGE is built, when Docker
# has no image of that name. Each run passes --pull=never as well: a
# missing image is never fetched from a registry, where the name may
# belong to anyone, and whatever came back would run with this
# computer's folders mounted. Any other failure (no daemon) returns 0
# and is left to the run, which says what is wrong.
wealthdb_require_image() {
    wealthdb_image_state "$2"
    [ "$IMAGE_STATE" = missing ] || return 0
    echo "$1: the image $2 is not built yet. $3" >&2
    return 1
}

# wealthdb_require_local_bases WHO DOCKERFILE HINT — return 1, with a
# message that ends in HINT, the sentence that says how the base images
# are built, unless Docker holds every wealthdb/ image DOCKERFILE builds
# FROM. `docker build` has no --pull=never: it asks a registry for a
# base that is not local, and a wealthdb/ name there may belong to
# anyone. So a build FROM such an image runs only behind this check. A
# base of another name (python, golang) is left to the build. Docker's
# own failure (no daemon) is refused too, with its message.
wealthdb_require_local_bases() {
    for _wlb in $(wealthdb_from_refs "$2"); do
        case "$_wlb" in
            wealthdb/*) ;;
            *) continue ;;
        esac
        _wlb_err="$(docker image inspect --format '{{.Id}}' "$_wlb" 2>&1)" && continue
        case "$_wlb_err" in
            *"No such image"*|*"No such object"*|'')
                echo "$1: the base image $_wlb is not built yet. $3" >&2
                ;;
            *)
                echo "$1: Docker could not say whether the base image $_wlb is built: $_wlb_err" >&2
                ;;
        esac
        return 1
    done
    return 0
}

# wealthdb_abs_path PATH prints PATH made absolute against the working
# directory. A container mounts a host path at the same path, and docker
# reads a relative source as a volume name, so every path the engine's
# container sees is absolute.
wealthdb_abs_path() {
    case "$1" in
        /*) printf '%s\n' "$1" ;;
        *)  printf '%s\n' "$PWD/$1" ;;
    esac
}

# wealthdb_clean_path PATH prints PATH absolute and lexically clean: no
# empty part, no `.`, and each `..` taken with the part before it. No
# link is followed and no file is read, so two spellings of one path
# compare equal.
wealthdb_clean_path() (
    IFS=/
    set -f
    clean=
    for part in $(wealthdb_abs_path "$1"); do
        case "$part" in
            '' | .) ;;
            ..) clean="${clean%/*}" ;;
            *)  clean="$clean/$part" ;;
        esac
    done
    printf '%s\n' "${clean:-/}"
)

# wealthdb_shell_word TEXT prints TEXT as one word of a shell command: as
# it is when it holds only letters, digits and _ . / : @ % + = , -, which
# no shell acts on; else in double quotes, so a message that quotes the
# command in single quotes quotes it once. Inside double quotes a shell
# still acts on $ ` " \ and !, so TEXT with one of them, a line break, or
# nothing at all goes in single quotes instead: a single quote of its own
# ends the quoted piece, stands escaped, and starts the next piece. The
# engine quotes a path by the same rule (shellWord), so a command reads
# the same from either.
wealthdb_shell_word() (
    LC_ALL=C
    export LC_ALL
    nl='
'
    case "$1" in
        *[\$\`\"\\!]* | *"$nl"* | '')
            printf "'%s'\n" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
            ;;
        *[!A-Za-z0-9_./:@%+=,-]*) printf '"%s"\n' "$1" ;;
        *) printf '%s\n' "$1" ;;
    esac
)

# wealthdb_setup_name DIR prints the name a setup kept in DIR gives its
# servers: the last part of DIR's clean path in lower case, each byte
# outside a-z 0-9 _ . - read as -, and `setup` when nothing is left (the
# root folder). It reads bytes, never the locale, so a server started at
# login gets the name a terminal gave it.
wealthdb_setup_name() (
    LC_ALL=C
    export LC_ALL
    dir="$(wealthdb_clean_path "$1")"
    name="$(printf '%s' "${dir##*/}" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9_.-' '-')"
    printf '%s\n' "${name:-setup}"
)

# wealthdb_engine_paths [CONFIG] sets the engine's host paths, which the
# engine wrapper mounts at the same path in its container, each
# absolute: XDG_CFG ($XDG_CONFIG_HOME, else ~/.config), CFG (the config
# file: CONFIG, else WEALTHDB_CONFIG, else wealthdb.cfg in XDG_CFG),
# CFG_DIR, the folder that holds CFG, and DATA_ROOT (gold and every
# silver). The engine wrapper passes the path of its -c flag as CONFIG.
#
# The default config is wealthdb.cfg in XDG_CFG. For it, OWN_SETUP is 0,
# DATA_ROOT is WEALTHDB_DATA_ROOT, else $XDG_DATA_HOME/wealthdb, else
# ~/.local/share/wealthdb, and PROGRAM, the words a command to run
# starts with, is `wealthdb`.
#
# Any other config, by its clean path, is a setup of its own, kept in
# the config's folder: OWN_SETUP is 1, SETUP_DIR is that folder and
# DATA_ROOT the same, whatever WEALTHDB_DATA_ROOT holds, SETUP_NAME is
# the name its servers take (wealthdb_setup_name), and PROGRAM is
# `wealthdb -c CFG`, the path as one shell word, so a command a message
# names acts on this setup.
wealthdb_engine_paths() {
    XDG_CFG="$(wealthdb_abs_path "${XDG_CONFIG_HOME:-$HOME/.config}")"
    CFG="$(wealthdb_abs_path "${1:-${WEALTHDB_CONFIG:-$XDG_CFG/wealthdb.cfg}}")"
    CFG_DIR="$(dirname "$CFG")"
    _wep_cfg="$(wealthdb_clean_path "$CFG")"
    if [ "$_wep_cfg" = "$(wealthdb_clean_path "$XDG_CFG/wealthdb.cfg")" ]; then
        OWN_SETUP=0 SETUP_DIR='' SETUP_NAME='' PROGRAM=wealthdb
        DATA_ROOT="$(wealthdb_abs_path "${WEALTHDB_DATA_ROOT:-${XDG_DATA_HOME:-$HOME/.local/share}/wealthdb}")"
    else
        OWN_SETUP=1
        SETUP_DIR="${_wep_cfg%/*}"
        SETUP_DIR="${SETUP_DIR:-/}"
        SETUP_NAME="$(wealthdb_setup_name "$SETUP_DIR")"
        DATA_ROOT="$SETUP_DIR"
        PROGRAM="wealthdb -c $(wealthdb_shell_word "$CFG")"
    fi
}

# wealthdb_export_setup hands the commands this shell starts the setup
# that wealthdb_engine_paths resolved. A setup of its own exports its
# data root as WEALTHDB_DATA_ROOT, and the words a command to run starts
# with, so a command that a server or a collector prints acts on this
# setup when it is pasted: WEALTHDB_PROGRAM before an engine command
# (`wealthdb -c CFG`) and WEALTHDB_COLLECT before a collector's source
# and verb (`wealthdb -c CFG collect`). The default config sets neither
# and removes one it inherited: a reader then prints `wealthdb` and
# `wealthdb-collect`.
wealthdb_export_setup() {
    if [ "$OWN_SETUP" = 1 ]; then
        export WEALTHDB_DATA_ROOT="$DATA_ROOT" WEALTHDB_PROGRAM="$PROGRAM" WEALTHDB_COLLECT="$PROGRAM collect"
    else
        unset WEALTHDB_PROGRAM WEALTHDB_COLLECT
    fi
}
